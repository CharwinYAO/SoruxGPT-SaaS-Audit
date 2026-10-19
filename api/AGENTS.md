# api/

The whole request pipeline + rate-limiter + reservation lifecycle. Package `api`.

## FILES
- `audit_limit.go` — Handlers `AuditLimit` (pre-call), `AuditLimitAfter` (post-call commit), `AuditLimitRelease` (post-call refund). Shared helpers: `extractToken`, `authenticate`, `tierMultiplier`, `getAuthUrlFromReferer`, `containsAny`.
- `limit.go` — In-memory `visitors map[string]*visitor` guarded by `sync.Mutex`. Key is `limiterKey(token, model, multiplier) = "token|MODEL|N"`. `ModelLimiter` wraps `*rate.Limiter` (`golang.org/x/time/rate`) with a manual `credit` pool so refunds work after `timeToAct` has elapsed (raw `Reservation.Cancel()` becomes a no-op once that boundary passes — that's why the wrapper exists).
- `reservation.go` — In-memory `reservations sync.Map` keyed by reservation ID, plus `reservationSweeper` goroutine (TTL = `config.ReservationTTL`, sweep interval = TTL/12 clamped to [1m, 1h]).
- `result.go` — Pre-baked JSON string constants (`MsgPlus429`, `MsgMod400`).
- `limit_test.go` — Smoke tests for `GetVisitorWithModel` (base lookup + multiplier scaling). Run: `go test ./api/...`.

## ROUTES (bound in main.go)
- `POST /audit_limit` — pre-call audit + pre-debit. On 200 returns header `X-Reservation-Id`. **Authenticated** via OAUTH.
- `POST /audit_limit/after` — commit. Reads `X-Reservation-Id` from request header, drops the entry. Token stays debited. **Authenticated** via OAUTH (same as pre-call).
- `POST /audit_limit/release` — refund. Reads `X-Reservation-Id`, drops the entry, calls `Refund()` on the limiter. **UNAUTHENTICATED** by design — protection comes from the 128-bit unguessable rid, and share-2026 must be able to release even when the user's session is dead. Network-layer ACLs (only allow share-2026 to reach this listener) are the appropriate gate.

`/audit_limit` and `/audit_limit/after` share `authenticate(r, token)` — same OAUTH check, same 401 on failure. `/audit_limit/release` does NOT call it.

## PIPELINE ORDER — `AuditLimit` (DO NOT REORDER)
1. `extractToken(r)` — `Authorization[7:]` with length guard (returns "" if header < 7 chars instead of panicking).
2. Parse JSON body → `action`, `model`, `messages.0.content.parts.0` (prompt). On parse error writes 400 but **does not return** (intentional fall-through, preserved from original).
3. `authenticate(r, token)` — resolves `authUrl` from Referer + `config.AuthUrlMap`, POSTs `{ticket, targetUrl}` to `config.OAUTH`. Missing mapping → 400. OAUTH non-200 → 401 `"会员已过期"`.
4. `containsAny(prompt, config.ForbiddenWords)` → 400 (custom Chinese message).
5. If `config.OAIKEY != "" && prompt != ""` → call `config.MODERATION`; flagged → 400 `MsgMod400`.
6. `GetVisitorWithModel(ctx, token, model)` → if `limiter.Tokens() < 1` → 429 (delay computed via `ReserveDelay()`).
7. `limiter.Take()` — pre-debits. False under rare concurrent race → 429.
8. `newReservationID()` + `storeReservation(rid, ...)` → set `X-Reservation-Id` response header → log `pre-call ok rid=...` at Info level → 200 (empty body). The Info log is the operator's anchor for correlating later `/after` and `/release` events to the originating pre-call.

## PIPELINE ORDER — `AuditLimitAfter`
1. `extractToken(r)` + `authenticate(r, token)` — identical to pre-call.
2. Read `X-Reservation-Id` from request header. Empty → 200 (no-op).
3. `popReservation(rid)` → just delete (token already debited at pre-call → "commit"). Logs `reservation committed rid=...` at Info on success, `after: reservation already handled or unknown` at Debug otherwise.
4. Always 200, empty body. Idempotent against unknown / already-consumed IDs.

## PIPELINE ORDER — `AuditLimitRelease`
1. **No auth.** Skip extractToken/authenticate. The unguessable rid is the credential.
2. Read `X-Reservation-Id` from request header. Empty → 200 (no-op, no log).
3. `popReservation(rid)`:
   - Hit: delete + `getVisitorByKey(res.Key).Refund()` (skip refund + Debug log if visitor was evicted). Logs `reservation released rid=... key=...` at Info.
   - Miss: Debug `release: reservation already handled or unknown` (duplicate release / unknown rid). No refund.
4. Always 200, empty body.

## RESERVATION LIFECYCLE
- ID = 16 random bytes hex-encoded (`crypto/rand`).
- TTL governed by `config.ReservationTTL` (env `RESERVATION_TTL`, default `24h`).
- **Timeout policy = COMMIT.** The sweeper deletes expired entries WITHOUT calling `Refund` — protects platform billing if a post-call never arrives. Do not change to release-on-timeout without product sign-off.

## CONVENTIONS (api-specific)
- Limiter key: `limiterKey(token, model, multiplier)` = `token + "|" + UPPER(model) + "|" + multiplier` (raw token, not hashed). `reservation.Key` stores this string for the release path. Embedding the multiplier means tier changes take effect on the next request — no in-place limiter rebuild needed.
- Tier source: parsed from OAUTH 200 response body, top-level `tier` field. Known values: `""` (default, 1x), `"pro5x"` (5x), `"pro20x"` (20x). Unknown non-empty values warn-log and fall back to 1x. Add new tiers in `tierMultiplier` in `audit_limit.go`.
- Rate parsing: `strings.Split(modelrate, "/")` → `[count, duration]`. Bad parse falls back to `["40","3h"]`. Effective `limit = count * multiplier`; duration is NOT scaled.
- `time.ParseDuration` accepts `h`, `m`, `s`, NOT `d`. Use `24h` for "day", `72h` for "3d".
- Response shape on errors: `g.Map{"detail": g.Map{"message": ...}}`. 429 also adds top-level `"error"`.
- Use `g.Client()` (GoFrame HTTP client) for upstream calls — NOT `net/http`.
- New JSON literal responses → add as `var` in `result.go`.

## ANTI-PATTERNS
- ❌ Don't read request body twice — `r.GetJson()` is called once; cache locals.
- ❌ Don't call `limiter.Take()` (or `limiter.Allow()`) before the `Tokens() < 1` check — it would consume a token before the bilingual 429 message can be assembled.
- ❌ Don't try to use `Reservation.Cancel()` for delayed (minutes/hours later) refunds. That's why `ModelLimiter.credit` exists; use `Refund()`.
- ❌ Don't replace the package-level `visitors` map with sync.Map without reviewing `CleanupVisitors` (it iterates+deletes under explicit `mtx`).
- ❌ Don't make `CleanupVisitors` run more often than weekly without thinking — `lastSeen > Per` eviction assumes long sleep.
- ❌ Don't add a `return` after the `GetJson` 400 branch in `AuditLimit` without verifying — current code intentionally falls through (latent bug, preserved unless the task is to fix it).
- ❌ Don't change `RESERVATION_TTL` sweep policy from commit to release without product sign-off — that flips who eats the cost on missing callbacks.
- ❌ Don't add `authenticate()` to `AuditLimitRelease`. The endpoint is intentionally unauthenticated: the 128-bit unguessable rid is the credential, and share-2026 must be able to release a reservation even when the user's session has already expired (which is exactly when releases are most needed). Use network-layer ACLs to restrict who can reach the listener.

## KNOWN LATENT ISSUES (preserve unless fixing is the task)
- `audit_limit.go` `GetJson` error branch writes 400 but **does not `return`** — execution continues with empty `reqJson`.
