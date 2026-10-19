package api

import (
	"strings"
	"sync"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/text/gstr"
	"github.com/gogf/gf/v2/util/gconv"
	"golang.org/x/time/rate"
)

// ModelLimiter wraps *rate.Limiter with a hand-managed "credit" pool so callers
// can refund tokens after the fact (post-call release on streaming/upstream
// failure). The underlying rate.Limiter cannot truly cancel a long-elapsed
// reservation (Reservation.Cancel becomes a no-op once timeToAct has passed),
// so credit is the only reliable way to give a token back minutes/hours later.
//
// Take() prefers credit first; if no credit is banked, it consumes from the
// limiter as before. Refund() adds 1 to credit, capped at burst so a buggy
// caller can't inflate the quota beyond the configured maximum.
type ModelLimiter struct {
	mu      sync.Mutex
	limiter *rate.Limiter
	credit  float64 // refunded-but-unspent tokens
	burst   int
}

func newModelLimiter(limit int, per time.Duration) *ModelLimiter {
	return &ModelLimiter{
		limiter: rate.NewLimiter(rate.Every(per/time.Duration(limit)), limit),
		burst:   limit,
	}
}

// Take attempts to consume one token. Returns true on success.
func (m *ModelLimiter) Take() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credit >= 1 {
		m.credit--
		return true
	}
	return m.limiter.Allow()
}

// Tokens reports the currently available token count (credit + limiter).
// Used by the < 1 pre-check in the pre-call handler.
func (m *ModelLimiter) Tokens() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limiter.TokensAt(time.Now()) + m.credit
}

// Refund banks one token back to credit (capped at burst).
// Called by the release path or by the post-call timeout sweep when policy
// dictates a release. With timeout=commit policy, the sweep does NOT call this.
func (m *ModelLimiter) Refund() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credit < float64(m.burst) {
		m.credit++
	}
}

// ReserveDelay reports how long until the next token would be available, used
// by the 429 response to tell the client when to retry. Mirrors the behaviour
// of the original code which called limiter.ReserveN/Cancel solely to read
// Delay() without burning a token.
func (m *ModelLimiter) ReserveDelay() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := m.limiter.ReserveN(time.Now(), 1)
	if !res.OK() {
		return 0
	}
	d := res.Delay()
	res.Cancel() // immediate cancel: timeToAct == now, so this restores the token
	return d
}

type visitor struct {
	limiter  *ModelLimiter
	lastSeen time.Time
	Per      time.Duration
}

var visitors = make(map[string]*visitor)
var mtx sync.Mutex

// GetVisitor returns (or lazily creates) the ModelLimiter for a given key.
func GetVisitor(key string, limit int, per time.Duration) *ModelLimiter {
	mtx.Lock()
	defer mtx.Unlock()

	v, exists := visitors[key]
	if !exists {
		ml := newModelLimiter(limit, per)
		visitors[key] = &visitor{ml, time.Now(), per}
		return ml
	}

	v.lastSeen = time.Now()
	return v.limiter
}

// getVisitorByKey looks up an existing limiter without creating one. Used by
// the release path: if the visitor was evicted between pre-call and release,
// the refund is moot (a fresh limiter would already be at full burst).
func getVisitorByKey(key string) *ModelLimiter {
	mtx.Lock()
	defer mtx.Unlock()
	if v, ok := visitors[key]; ok {
		v.lastSeen = time.Now()
		return v.limiter
	}
	return nil
}

// limiterKey is the canonical visitors-map key for a given (token, model, tier
// multiplier) triple. Embedding the multiplier means a tier upgrade or downgrade
// takes effect on the very next request: the new multiplier yields a different
// key, so a fresh limiter is provisioned at the new rate. The stale limiter
// stays in the map idle and is evicted by CleanupVisitors after one Per window.
func limiterKey(token, model string, multiplier int) string {
	return token + "|" + gstr.ToUpper(model) + "|" + gconv.String(multiplier)
}

// GetVisitorWithModel resolves the per-model rate and applies the caller's tier
// multiplier (>=1). The base rate "<count>/<duration>" is read from env/config
// keyed by the uppercased model name; count is multiplied, duration is not.
// burst follows count so paid tiers can also burst proportionally.
func GetVisitorWithModel(ctx g.Ctx, token, model string, multiplier int) (limit int, per time.Duration, limiter *ModelLimiter, err error) {
	if multiplier < 1 {
		multiplier = 1
	}
	model = gstr.ToUpper(model)
	modelrate := g.Cfg().MustGetWithEnv(ctx, model, "40/3h").String()
	modelratearr := strings.Split(modelrate, "/")
	if len(modelratearr) != 2 {
		modelratearr = []string{"40", "3h"}
	}
	base := gconv.Int(modelratearr[0])
	per, err = time.ParseDuration(modelratearr[1])
	if err != nil {
		return 0, 0, nil, err
	}
	limit = base * multiplier
	return limit, per, GetVisitor(limiterKey(token, model, multiplier), limit, per), nil
}

func CleanupVisitors() {
	mtx.Lock()
	defer mtx.Unlock()

	for token, v := range visitors {
		if time.Since(v.lastSeen) > v.Per {
			delete(visitors, token)
		}
	}
}

func init() {
	// 每星期清理一次
	go func() {
		for {
			time.Sleep(time.Hour * 24 * 7)
			CleanupVisitors()
		}
	}()
}
