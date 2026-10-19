package config

import (
	"strings"
	"time"

	"github.com/gogf/gf/v2/container/garray"
	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/os/gctx"
)

var (
	PORT             = 8080
	ForbiddenWords   = []string{} // 禁止词
	OAIKEY           = ""         // OAIKEY
	OAIKEYLOG        = ""         // OAIKEYLOG 隐藏
	MODERATION       = ""
	ShareUrl         = ""
	PlusModels       = garray.NewStrArrayFrom([]string{"gpt-4", "gpt-4o", "gpt-4-browsing", "gpt-4-plugins", "gpt-4-mobile", "gpt-4-code-interpreter", "gpt-4-dalle", "gpt-4-gizmo", "gpt-4-magic-create", "o1-preview", "o1-mini", "video_gen", "o1", "o1-pro"})
	AdminApiKey      = ""
	SyncAdmitPostUrl = ""
	OAUTH			 = ""
	AuthUrlMap       = make(map[string]string)

	// ReservationTTL bounds how long a pre-debited reservation may live before
	// the cleanup goroutine commits it (per the timeout=commit policy: tokens
	// stay debited, the reservation entry is just dropped).
	// Streaming conversations can run for hours, so default to 24h.
	ReservationTTL = 24 * time.Hour

	// PostBilling switches the debit moment from pre-call to post-call.
	//
	//   false (default) — original pre-billing semantics: AuditLimit calls
	//     Take() up-front, AuditLimitAfter is a no-op commit, AuditLimitRelease
	//     refunds. The sweeper's drop-on-timeout = "platform keeps the debit"
	//     (protects billing if /after never arrives).
	//
	//   true — post-billing: AuditLimit does NOT call Take(); it only runs
	//     auth/moderation/soft-quota checks and registers a reservation.
	//     AuditLimitAfter performs the actual Take() at commit time. If the
	//     /after callback never arrives (sweeper timeout, or AuditLimitRelease
	//     fires) NOTHING is debited — the user is not charged for the failed
	//     conversation. This is "没收到后调成功的指令，就不进行扣费".
	//
	// Tradeoff: in post-billing mode the pre-call quota check is a soft check
	// only (Tokens() < 1) — under heavy concurrency a few requests may slip
	// past the soft check and only fail at /after. Acceptable for chat-style
	// workloads, where requests-per-user are naturally serialized.
	PostBilling = false
)

func init() {
	ctx := gctx.GetInitCtx()
	port := g.Cfg().MustGetWithEnv(ctx, "PORT").Int()
	if port > 0 {
		PORT = port
	}
	g.Log().Info(ctx, "PORT:", PORT)
	oaikey := g.Cfg().MustGetWithEnv(ctx, "OAIKEY").String()
	// oaikey 不为空
	if oaikey != "" {
		OAIKEY = oaikey
		// 日志隐藏 oaikey，有 * 代表有值
		OAIKEYLOG = "******"
	}
	g.Log().Info(ctx, "OAIKEY:", OAIKEYLOG)
	moderation := g.Cfg().MustGetWithEnv(ctx, "MODERATION").String()
	if moderation != "" {
		MODERATION = moderation
	}
	g.Log().Info(ctx, "MODERATION:", MODERATION)

	shareUrl := g.Cfg().MustGetWithEnv(ctx, "SHARE_URL").String()
	if shareUrl != "" {
		ShareUrl = shareUrl
	}
	g.Log().Info(ctx, "SHARE_URL:", ShareUrl)

	adminApiKey := g.Cfg().MustGetWithEnv(ctx, "ADMIN_API_KEY").String()
	if adminApiKey != "" {
		AdminApiKey = adminApiKey
	}
	g.Log().Info(ctx, "ADMIN_API_KEY:", AdminApiKey)

	syncAdmitPostUrl := g.Cfg().MustGetWithEnv(ctx, "SYNC_ADMIT_POST_URL").String()
	if syncAdmitPostUrl != "" {
		SyncAdmitPostUrl = syncAdmitPostUrl
	}
	g.Log().Info(ctx, "SYNC_ADMIT_POST_URL:", SyncAdmitPostUrl)

	oauth := g.Cfg().MustGetWithEnv(ctx, "OAUTH").String()
	if oauth != "" {
		OAUTH = oauth
	}
	g.Log().Info(ctx, "OAUTH:", OAUTH)

    // 格式: "share.gptpanda.net=chat2.gptpanda.net,share2.example.com=auth2.example.com"
    authMapping := g.Cfg().MustGetWithEnv(ctx, "AUTHURL_MAP").String()
	g.Log().Info(ctx, "authMapping:", authMapping)
    if authMapping != "" {
        pairs := strings.Split(authMapping, ",")
        for _, pair := range pairs {
            kv := strings.Split(pair, "=")
            if len(kv) == 2 {
                AuthUrlMap[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
            }
        }
    }
	g.Log().Info(ctx, "AuthUrlMap:", AuthUrlMap)

	reservationTTL := g.Cfg().MustGetWithEnv(ctx, "RESERVATION_TTL").String()
	if reservationTTL != "" {
		if d, err := time.ParseDuration(reservationTTL); err == nil && d > 0 {
			ReservationTTL = d
		} else {
			g.Log().Warning(ctx, "RESERVATION_TTL parse failed, keep default:", ReservationTTL, "err:", err)
		}
	}
	g.Log().Info(ctx, "ReservationTTL:", ReservationTTL)

	// POST_BILLING: env-driven toggle. Accepts the typical truthy strings
	// (`true`, `1`, `yes`, `on`); anything else (including unset) leaves
	// PostBilling=false so the default behaviour stays pre-billing.
	postBilling := g.Cfg().MustGetWithEnv(ctx, "POST_BILLING").String()
	if postBilling != "" {
		switch strings.ToLower(strings.TrimSpace(postBilling)) {
		case "1", "true", "yes", "on":
			PostBilling = true
		case "0", "false", "no", "off":
			PostBilling = false
		default:
			g.Log().Warning(ctx, "POST_BILLING unrecognized value, keep default(false):", postBilling)
		}
	}
	g.Log().Info(ctx, "PostBilling:", PostBilling)
}
