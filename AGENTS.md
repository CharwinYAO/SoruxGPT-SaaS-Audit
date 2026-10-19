# PROJECT KNOWLEDGE BASE

**Generated:** 2026-05-20
**Commit:** 2dc206c
**Branch:** main

## OVERVIEW
`auditlimit` — SoruxGPT SaaS audit/rate-limit sidecar. Single-binary Go service (GoFrame v2 / `gogf/gf`) that gates ChatGPT-proxy traffic by validating session via upstream OAuth, screening prompts (forbidden-words + OpenAI Moderation), and enforcing per-(token,model) token-bucket limits with a **reservation lifecycle** (pre-debit → commit/release) so failed/streaming-aborted upstream calls don't burn user quota.

## STRUCTURE
```
.
├── main.go                 # entry: g.Server, binds /audit_limit, /audit_limit/after, /audit_limit/release; init() loads data/keywords.txt
├── api/                    # request handler + rate-limiter + reservation lifecycle (see api/AGENTS.md)
├── config/                 # global vars + env-driven init (config.go) + dev defaults (config.yaml)
├── data/keywords.txt       # newline-separated forbidden words (currently empty)
├── manifest/docker/        # alt Dockerfile that ADDs prebuilt ./temp/linux_amd64/main
├── temp/linux_amd64/       # output dir for prebuilt binary used by manifest/docker
├── Dockerfile              # multi-stage: golang:alpine → scratch
├── docker-compose.yaml     # production compose (image charwin00/soruxgpt-saas-audit, port 9612→8080)
└── release.sh              # buildx push v1.2 to charwin00/soruxgpt-saas-audit
```

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Add/modify a route | `main.go` `s.BindHandler` (no router file — flat) |
| Change pre-call auth/limit/moderation logic | `api/audit_limit.go` `AuditLimit` |
| Change post-call commit/release logic | `api/audit_limit.go` `AuditLimitAfter` / `AuditLimitRelease` |
| Reservation lifecycle / TTL sweeper | `api/reservation.go` |
| Tune per-model rate format | `api/limit.go` `GetVisitorWithModel` (`"40/3h"` form, multiplied by tier) |
| Add/change subscription tiers (multipliers) | `api/audit_limit.go` `tierMultiplier` |
| Refund-after-the-fact mechanism | `api/limit.go` `ModelLimiter` (credit pool) |
| Add an env var | `config/config.go` `init()` (use `g.Cfg().MustGetWithEnv`) |
| Map referer host → auth host | env `AUTHURL_MAP="hostA=authA,hostB=authB"` |
| Update forbidden words | `data/keywords.txt` (loaded once at startup in `main.go init()`) |
| Static 429/Moderation JSON | `api/result.go` |

## CODE MAP
| Symbol | Type | Location | Role |
|---|---|---|---|
| `main` | func | main.go:13 | Boots `g.Server`, binds `/`, `/audit_limit`, `/audit_limit/after`, `/audit_limit/release` |
| `Index` | handler | main.go:21 | Health/banner string |
| `init` | func | main.go:25 | Loads `./data/keywords.txt` → `config.ForbiddenWords` |
| `AuditLimit` | handler | api/audit_limit.go | Pre-call: auth→keywords→moderation→pre-debit; emits `X-Reservation-Id` |
| `AuditLimitAfter` | handler | api/audit_limit.go | Post-call commit (drop reservation, token stays debited) |
| `AuditLimitRelease` | handler | api/audit_limit.go | Post-call refund (drop reservation + `Refund()`) |
| `extractToken` | func | api/audit_limit.go | `Authorization[7:]` with length guard |
| `authenticate` | func | api/audit_limit.go | Shared OAUTH check (referer→authUrl→OAUTH POST) |
| `getAuthUrlFromReferer` | func | api/audit_limit.go | Resolves auth host via `config.AuthUrlMap` |
| `containsAny` | func | api/audit_limit.go | substring scan of `ForbiddenWords` |
| `ModelLimiter` | type | api/limit.go | Wraps `*rate.Limiter` with `credit` pool for delayed refunds |
| `ModelLimiter.Take/Tokens/Refund/ReserveDelay` | methods | api/limit.go | Atomic limiter ops; prefer credit before bucket |
| `GetVisitor` / `getVisitorByKey` | func | api/limit.go | Per-key `*ModelLimiter` factory / lookup-only |
| `GetVisitorWithModel` | func | api/limit.go | Reads `"<MODEL>"` env (uppercased) → `"<n>/<duration>"` |
| `CleanupVisitors` | func | api/limit.go | Evicts entries idle > Per; goroutine every 7d |
| `reservation` / `reservations` | type/var | api/reservation.go | Pre-debit registry (sync.Map keyed by random hex ID) |
| `storeReservation` / `popReservation` / `newReservationID` | funcs | api/reservation.go | Reservation CRUD + ID generation (crypto/rand) |
| `reservationSweeper` | func | api/reservation.go | TTL sweeper; **commits** (drops without refund) on timeout |
| `config.*` globals | vars | config/config.go:10 | All runtime state: `PORT`, `OAUTH`, `OAIKEY`, `MODERATION`, `ShareUrl`, `AdminApiKey`, `SyncAdmitPostUrl`, `AuthUrlMap`, `ForbiddenWords`, `PlusModels`, `ReservationTTL` |

## CONVENTIONS
- **Framework**: GoFrame v2 (`g.Server`, `g.Cfg`, `g.Log`, `g.Client`). Don't introduce `net/http`/gin/echo.
- **Config access**: ALWAYS `g.Cfg().MustGetWithEnv(ctx, "KEY")` — env wins over `config/config.yaml`. Keys are uppercased model names (e.g. `GPT-4O`, `O1-PREVIEW`); `GetVisitorWithModel` upper-cases before lookup.
- **Rate format**: `"<count>/<go-duration>"` e.g. `"40/3h"`, `"50/24h"`. Default fallback `"40/3h"`.
- **Globals over DI**: state lives in `package config` package-level vars, populated in `init()`. Don't refactor to structs/DI without a reason.
- **Logging**: `g.Log().Debug/Info/Warning/Error(ctx, ...)`. `OAIKEY` MUST be masked as `OAIKEYLOG = "******"` in logs.
- **Error responses**: `r.Response.Status = N; r.Response.WriteJson(g.Map{"detail": g.Map{"message": "..."}})`. Pre-baked JSON strings live in `api/result.go`.
- **Auth**: Bearer token in `Authorization` header (stripped first 7 chars `"Bearer "`). Forwarded to `config.OAUTH` as `{"ticket": token, "targetUrl": authUrl}`; non-200 → 401 "会员已过期".
- **Ratelimit key**: `token + "|" + model`.

## ANTI-PATTERNS (THIS PROJECT)
- ❌ Don't switch web framework — handlers depend on `*ghttp.Request`.
- ❌ Don't replace package-level globals with config struct — `init()` ordering across `config/` and `api/` relies on them.
- ❌ Don't log `OAIKEY` directly; only `OAIKEYLOG`.
- ❌ Don't drop the `time.Header.Get("Authorization")[7:]` slice without a length guard if Bearer prefix may be missing — current code is fragile if header is non-empty but shorter than 7 (latent bug; fix carefully if touched).
- ❌ Don't add a `return` after the `GetJson` 400 branch (audit_limit.go:33-41) without verifying — current code intentionally falls through (likely bug, preserve unless the task is to fix it).
- ❌ `manifest/docker/Dockerfile` consumes `./temp/linux_amd64/main` — DO NOT delete `temp/` blindly; it's a build artifact dir for the alt image path.

## UNIQUE STYLES
- Mixed-language messages: user-facing strings include both English and Chinese in the same payload (search `AuditLimit` 429 branch). Keep both when editing.
- `PlusModels` is declared but currently unreferenced — leave for compatibility.
- `CleanupVisitors` runs once per **week** (`time.Hour*24*7`) — intentional, not a bug.
- `reservationSweeper` runs at TTL/12 (clamped to [1m, 1h]); on timeout it **commits** (drops without refund) — intentional, protects platform billing.

## COMMANDS
```bash
# Run locally (uses config/config.yaml + env)
go run .

# Test
go test ./api/...

# Build static binary
CGO_ENABLED=0 GOOS=linux go build -o main .

# Build & push image (multi-arch via buildx, tag v1.2)
./release.sh

# Run via compose (port 9612 → container 8080)
docker compose up -d
```

## ENVIRONMENT VARIABLES
| Var | Purpose | Required |
|---|---|---|
| `PORT` | Listen port (default 8080) | no |
| `OAUTH` | Upstream auth endpoint POSTed `{ticket,targetUrl}` | **yes** |
| `AUTHURL_MAP` | `host1=authHost1,host2=authHost2` | **yes** (else 400) |
| `SHARE_URL` | Legacy share URL (kept for compat) | no |
| `ADMIN_API_KEY` | Admin key (declared, currently unused in handler) | no |
| `OAIKEY` | OpenAI key; if set, enables Moderation check | no |
| `MODERATION` | Moderation endpoint (e.g. `https://api.openai.com/v1/moderations`) | only if `OAIKEY` set |
| `SYNC_ADMIT_POST_URL` | Declared, currently unused | no |
| `RESERVATION_TTL` | Reservation max lifetime (Go duration, e.g. `24h`). Default `24h`. On expiry the sweeper **commits** (no refund). | no |
| `<MODEL>` (uppercased) | Per-model rate `"N/duration"` e.g. `GPT-4O="60/3h"` | no (default `40/3h`) |

## NOTES
- README is intentionally terse (`SHARE_URL`, `ADMIN_API_KEY`) and OUT OF DATE — this file is the source of truth. Real critical envs are `OAUTH` and `AUTHURL_MAP`.
- `data/keywords.txt` is empty in repo; mount/replace at deploy.
- Two Dockerfiles: root `Dockerfile` builds from source (used by `release.sh`); `manifest/docker/Dockerfile` ships a prebuilt binary from `temp/linux_amd64/main`.
- `go.mod` declares `go 1.18` but uses no generics — safe to bump if needed.
