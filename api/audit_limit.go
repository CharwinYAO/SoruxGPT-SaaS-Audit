package api

import (
	"auditlimit/config"
	"net/url"
	"strings"
	"time"

	"github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"
	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gogf/gf/v2/os/gctx"
	"github.com/gogf/gf/v2/util/gconv"
)

// extractToken pulls the bearer ticket out of Authorization. The original code
// did `token[7:]` unconditionally; that panics on `Authorization: x` (non-empty,
// shorter than "Bearer "). Guard against it without changing semantics for the
// well-formed case.
func extractToken(r *ghttp.Request) string {
	token := r.Header.Get("Authorization")
	if len(token) >= 7 {
		return token[7:]
	}
	return ""
}

// tierMultiplier maps the OAUTH-reported tier string to a rate multiplier.
// Empty / unknown tiers fall back to 1x (the standard plan); known paid tiers
// scale the per-model base rate accordingly. Add new tiers here as products
// roll them out — keeping the table in code (not env) keeps the contract with
// the OAUTH side explicit.
func tierMultiplier(ctx g.Ctx, tier string) int {
	switch tier {
	case "":
		return 1
	case "pro5x":
		return 5
	case "pro20x":
		return 20
	default:
		g.Log().Warning(ctx, "unknown tier from OAUTH, defaulting to 1x:", tier)
		return 1
	}
}

// authenticate runs the upstream OAUTH check shared by all three endpoints.
// On failure it writes the response and returns ok=false; the caller should
// just return. The authUrl and the OAUTH-reported tier (raw string, may be
// empty) are returned for use by downstream rate-limit logic.
func authenticate(r *ghttp.Request, token string) (authUrl, tier string, ok bool) {
	ctx := r.Context()
	referer := r.Header.Get("referer")
	g.Log().Debug(ctx, "referer", referer)

	authUrl, err := getAuthUrlFromReferer(ctx, referer)
	if err != nil {
		g.Log().Error(ctx, "getAuthUrlFromReferer", err)
		r.Response.Status = 400
		r.Response.WriteJson(g.Map{
			"detail": g.Map{
				"message": "Invalid referer or auth configuration.",
			},
		})
		return "", "", false
	}
	g.Log().Debug(ctx, "authUrl", authUrl)

	resp, err := g.Client().SetHeaderMap(g.MapStrStr{
		"Content-Type": "application/json",
	}).Post(ctx, config.OAUTH, g.Map{
		"ticket":    token,
		"targetUrl": authUrl,
	})
	if err != nil {
		g.Log().Error(ctx, "OAUTH post error", err)
		r.Response.Status = 500
		r.Response.WriteJson(g.Map{
			"detail": g.Map{
				"message": "Internal Server Error.",
			},
		})
		return "", "", false
	}
	if resp.StatusCode != 200 {
		g.Log().Warning(ctx, "Auth failed, status code:", resp.StatusCode)
		r.Response.Status = 401
		r.Response.WriteJson(g.Map{
			"detail": g.Map{
				"message": "会员已过期",
			},
		})
		return "", "", false
	}

	// Parse the OAUTH body for the tier field. Both response shapes (fresh
	// validation and cached hit) carry "tier" at the top level, so a single
	// gjson lookup covers both. Missing / unparseable body → tier="" → 1x.
	body := resp.ReadAllString()
	if body != "" {
		tier = gjson.New(body).Get("tier").String()
	}
	g.Log().Debug(ctx, "tier", tier)
	return authUrl, tier, true
}

// AuditLimit is the pre-call endpoint. On success it pre-debits one token and
// surfaces an X-Reservation-Id header that the caller MUST echo back to either
// /audit_limit/after (commit, no-op against tokens) or /audit_limit/release
// (refund) once the upstream conversation completes or fails.
func AuditLimit(r *ghttp.Request) {
	ctx := r.Context()

	token := extractToken(r)
	g.Log().Debug(ctx, "token", token)

	gfsessionid := r.Cookie.Get("gfsessionid").String()
	g.Log().Debug(ctx, "gfsessionid", gfsessionid)

	reqJson, err := r.GetJson()
	if err != nil {
		g.Log().Error(ctx, "GetJson", err)
		r.Response.Status = 400
		r.Response.WriteJson(g.Map{
			"detail": g.Map{
				"message": "Invalid JSON payload.",
			},
		})
		// NOTE: original code intentionally fell through here (per AGENTS.md
		// "preserve unless fixing is the task"). Keeping that behaviour.
	}
	action := reqJson.Get("action").String()
	g.Log().Debug(ctx, "action", action)

	model := reqJson.Get("model").String()
	g.Log().Debug(ctx, "model", model)
	prompt := reqJson.Get("messages.0.content.parts.0").String()
	g.Log().Debug(ctx, "prompt", prompt)

	_, tier, ok := authenticate(r, token)
	if !ok {
		return
	}

	// 判断提问内容是否包含禁止词
	if containsAny(ctx, prompt, config.ForbiddenWords) {
		r.Response.Status = 400
		r.Response.WriteJson(g.Map{
			"detail": g.Map{
				"message": "请珍惜账号,不要提问违禁内容.",
			},
		})
		return
	}

	// OPENAI Moderation 检测
	if config.OAIKEY != "" && prompt != "" {
		respVar := g.Client().SetHeaderMap(g.MapStrStr{
			"Authorization": "Bearer " + config.OAIKEY,
			"Content-Type":  "application/json",
		}).PostVar(ctx, config.MODERATION, g.Map{
			"input": prompt,
		})

		g.Dump(respVar)
		respJson := gjson.New(respVar)
		isFlagged := respJson.Get("results.0.flagged").Bool()
		g.Log().Debug(ctx, "flagged", isFlagged)
		if isFlagged {
			r.Response.Status = 400
			r.Response.WriteJson(MsgMod400)
			return
		}
	}

	multiplier := tierMultiplier(ctx, tier)
	limit, per, limiter, err := GetVisitorWithModel(ctx, token, model, multiplier)
	if err != nil {
		g.Log().Error(ctx, "GetVisitorWithModel", err)
		r.Response.Status = 500
		r.Response.WriteJson(g.Map{
			"error": err.Error(),
		})
		return
	}

	remain := limiter.Tokens()
	g.Log().Debug(ctx, token, model, "tier", tier, "x", multiplier, "remain", remain, "limit", limit, "per", per)
	if remain < 1 {
		r.Response.Status = 429
		delay := limiter.ReserveDelay()
		g.Log().Debug(ctx, "delayFrom", delay)
		r.Response.WriteJson(g.Map{
			"error": "You have triggered the usage frequency limit of " + model + ", the current limit is " + gconv.String(limit) + " times/" + gconv.String(per) + ", please wait " + gconv.String(int(delay.Seconds())) + " seconds before trying again.\n" + "您已经触发 " + model + " 使用频率限制,当前限制为 " + gconv.String(limit) + " 次/" + gconv.String(per) + ",请等待 " + gconv.String(int(delay.Seconds())) + " 秒后再试.",
			"detail": g.Map{
				"message": "You have triggered the usage frequency limit of " + model + ", the current limit is " + gconv.String(limit) + " times/" + gconv.String(per) + ", please wait " + gconv.String(int(delay.Seconds())) + " seconds before trying again.",
			},
		})
		return
	}

	// 预扣 1 个令牌。
	//
	//   PostBilling=false (默认): 立即 Take()，与历史行为一致。失败 → 429。
	//   PostBilling=true: 跳过 Take()。仅在收到 /audit_limit/after 时由提交
	//   逻辑进行扣费；若回调缺失（包括 sweeper 超时丢弃 / 调用 /release）则
	//   完全不扣费。这种模式下并发可能让若干请求越过软配额检查
	//   （Tokens() < 1），由 /after 路径在提交时再做硬性 Take() 失败兜底。
	if !config.PostBilling {
		if !limiter.Take() {
			r.Response.Status = 429
			delay := limiter.ReserveDelay()
			r.Response.WriteJson(g.Map{
				"error": "You have triggered the usage frequency limit of " + model + ", please wait " + gconv.String(int(delay.Seconds())) + " seconds before trying again.",
				"detail": g.Map{
					"message": "You have triggered the usage frequency limit of " + model + ", please wait " + gconv.String(int(delay.Seconds())) + " seconds before trying again.",
				},
			})
			return
		}
	}

	// 生成 reservation_id 并登记。失败时（极不可能）回退令牌并返回 500。
	rid, err := newReservationID()
	if err != nil {
		g.Log().Error(ctx, "newReservationID", err)
		if !config.PostBilling {
			limiter.Refund()
		}
		r.Response.Status = 500
		r.Response.WriteJson(g.Map{
			"detail": g.Map{
				"message": "Internal Server Error.",
			},
		})
		return
	}
	storeReservation(rid, &reservation{
		Token:     token,
		Model:     strings.ToUpper(model),
		Key:       limiterKey(token, model, multiplier),
		CreatedAt: time.Now(),
	})
	r.Response.Header().Set("X-Reservation-Id", rid)
	g.Log().Info(ctx, "pre-call ok", "rid", rid, "token", token, "model", strings.ToUpper(model), "tier", tier, "x", multiplier, "post_billing", config.PostBilling, "remain_after", limiter.Tokens())
	r.Response.Status = 200
}

// AuditLimitAfter is the post-call commit endpoint.
//
// In pre-billing mode (config.PostBilling=false): the token was already
// debited at pre-call time, so commit just retires the reservation entry.
//
// In post-billing mode (config.PostBilling=true): the actual debit happens
// HERE — Take() is called against the limiter recorded in the reservation.
// If the limiter is empty (concurrency race past the soft Tokens()<1 check
// in pre-call), the request has already completed upstream so we cannot
// reject it; we log the overshoot loudly and still return 200. This is the
// inherent tradeoff of post-billing.
//
// Idempotent: unknown / already-handled IDs return 200.
func AuditLimitAfter(r *ghttp.Request) {
	ctx := r.Context()
	token := extractToken(r)
	if _, _, ok := authenticate(r, token); !ok {
		return
	}
	rid := r.Header.Get("X-Reservation-Id")
	if rid == "" {
		// No reservation to commit. Per spec the header may be absent when the
		// pre-call did not return one (e.g. 4xx path). Treat as success.
		r.Response.Status = 200
		return
	}
	res, ok := popReservation(rid)
	if !ok {
		g.Log().Debug(ctx, "after: reservation already handled or unknown", rid)
		r.Response.Status = 200
		return
	}
	if config.PostBilling {
		// Post-billing: charge now. Use the Key recorded at pre-call so a tier
		// change between pre-call and commit doesn't misroute the debit.
		if limiter := getVisitorByKey(res.Key); limiter != nil {
			if limiter.Take() {
				g.Log().Info(ctx, "reservation committed (post-billing debited)", "rid", rid, "key", res.Key, "remain_after", limiter.Tokens())
			} else {
				// Soft-check overshoot: upstream already served the request.
				// Log so operators can spot abuse / mis-tuned limits, but the
				// response is already on the wire so we cannot block it.
				g.Log().Warning(ctx, "post-billing commit: limiter empty, overshoot accepted", "rid", rid, "key", res.Key)
			}
		} else {
			// Visitor evicted between pre-call and commit (unlikely on the
			// minutes-scale conversation window, but possible). Skipping the
			// debit means the user got a free request; log for visibility.
			g.Log().Warning(ctx, "post-billing commit: visitor evicted, skip debit", "rid", rid, "key", res.Key)
		}
	} else {
		g.Log().Info(ctx, "reservation committed", "rid", rid)
	}
	r.Response.Status = 200
}

// AuditLimitRelease is the compensating endpoint invoked on upstream/streaming
// failure. It refunds the pre-debited token and retires the reservation.
// Idempotent: unknown IDs return 200 without refunding (so a duplicate release
// can't double-credit). The reservation's stored Key already encodes the tier
// multiplier in effect at pre-call time, so a tier change between pre-call and
// release does not misroute the refund.
//
// NOTE: release is intentionally UNAUTHENTICATED. The endpoint only acts on a
// 128-bit unguessable reservation ID that was returned by a prior pre-call;
// without the ID a caller can't refund anything they didn't already pre-debit.
// share-2026 needs to be able to call this even when the user's session has
// failed/expired (which is exactly when a release is most needed), so adding
// auth here would defeat the purpose. Network-layer access control (limiting
// the listener to share-2026's network) is the appropriate gate.
func AuditLimitRelease(r *ghttp.Request) {
	ctx := r.Context()
	// Log every incoming release call before any parsing so operators can see
	// exactly what share-2026 (or any caller) sent — useful when chasing
	// "the rid I expected isn't refunding" issues. r.GetBody() returns the
	// cached body bytes and is safe to call before GetJson() below.
	g.Log().Info(ctx, "release: incoming",
		"method", r.Method,
		"remote", r.GetRemoteIp(),
		"content_type", r.Header.Get("Content-Type"),
		"header_rid", r.Header.Get("X-Reservation-Id"),
		"body", string(r.GetBody()),
	)
	// share-2026 sends the reservation id in a JSON body
	// (`{"reservation_id":"..."}`). Body is the source of truth; fall back to
	// the legacy X-Reservation-Id header so older callers keep working. Body
	// parse errors / missing field are treated as "no rid" → silent 200, same
	// idempotent contract as before.
	rid := ""
	if reqJson, err := r.GetJson(); err == nil && reqJson != nil {
		rid = reqJson.Get("reservation_id").String()
	}
	if rid == "" {
		rid = r.Header.Get("X-Reservation-Id")
	}
	if rid == "" {
		r.Response.Status = 200
		return
	}
	res, ok := popReservation(rid)
	if !ok {
		g.Log().Debug(ctx, "release: reservation already handled or unknown", rid)
		r.Response.Status = 200
		return
	}
	// Post-billing: nothing was debited at pre-call, so there's nothing to
	// refund — dropping the reservation IS the "user is not charged" effect.
	// Refunding here would over-credit the limiter and leak quota.
	if config.PostBilling {
		g.Log().Info(ctx, "reservation released (post-billing: no debit, no refund)", "rid", rid, "key", res.Key)
		r.Response.Status = 200
		return
	}
	if limiter := getVisitorByKey(res.Key); limiter != nil {
		limiter.Refund()
		g.Log().Info(ctx, "reservation released", "rid", rid, "key", res.Key)
	} else {
		// Visitor was evicted by CleanupVisitors; a fresh limiter would be at
		// full burst anyway, so skipping refund is harmless.
		g.Log().Debug(ctx, "release: visitor evicted, skip refund", rid, "key", res.Key)
	}
	r.Response.Status = 200
}

// 根据referer获取对应的鉴权地址
func getAuthUrlFromReferer(ctx g.Ctx, referer string) (string, error) {
	if referer == "" {
		return "", gerror.New("Referer is empty")
	}
	parsedUrl, err := url.Parse(referer)
	if err != nil {
		g.Log().Error(ctx, "Parse referer URL error", err)
		return "", err
	}

	host := parsedUrl.Host
	g.Log().Debug(ctx, "referer host", host)

	if authHost, exists := config.AuthUrlMap[host]; exists {
		scheme := "https"
		if parsedUrl.Scheme == "http" {
			scheme = "http"
		}
		return scheme + "://" + authHost, nil
	}

	g.Log().Warning(ctx, "No auth mapping found for host:", host, "using default ShareUrl")
	return "", gerror.New("No auth mapping found for host")
}

// 判断字符串是否包含数组中的任意一个元素
func containsAny(ctx g.Ctx, text string, array []string) bool {
	for _, item := range array {
		if strings.Contains(text, item) {
			g.Log().Debug(ctx, "containsAny", text, item)
			return true
		}
	}
	return false
}

// init starts the reservation TTL sweeper. Done here (not in main) so the
// goroutine spins up regardless of how the handlers are wired.
func init() {
	go reservationSweeper(gctx.New())
}
