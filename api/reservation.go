package api

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"auditlimit/config"

	"github.com/gogf/gf/v2/frame/g"
)

// reservation tracks a pre-debited token waiting on a post-call commit/release.
// Identified by an opaque ID surfaced to the share-2026 caller via the
// X-Reservation-Id response header on /audit_limit success.
type reservation struct {
	Token     string    // raw bearer ticket from pre-call
	Model     string    // uppercased model used as part of the limiter key
	Key       string    // token + "|" + Model — the visitors map key
	CreatedAt time.Time
}

// reservations is keyed by reservation ID. sync.Map is appropriate because the
// access pattern is mostly Store/LoadAndDelete with no iteration on hot paths;
// the cleanup sweeper is the only iterator and runs infrequently.
var reservations sync.Map

// newReservationID returns a 128-bit random hex ID. Collision probability is
// negligible for the lifetime of any single audit_limit instance.
func newReservationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func storeReservation(id string, r *reservation) {
	reservations.Store(id, r)
}

// popReservation atomically removes and returns the reservation. Used by both
// /after (commit: just delete, tokens stay debited) and /release (refund + delete).
// Returns (nil, false) if the ID is unknown — callers treat this as idempotent
// success per the share-2026 contract.
func popReservation(id string) (*reservation, bool) {
	v, ok := reservations.LoadAndDelete(id)
	if !ok {
		return nil, false
	}
	return v.(*reservation), true
}

// reservationSweeper enforces ReservationTTL. Per the agreed timeout=commit
// policy, expired entries are simply dropped: the pre-debited token is not
// refunded, so the platform is protected from billing leakage when a post-call
// callback never arrives.
func reservationSweeper(ctx g.Ctx) {
	ttl := config.ReservationTTL
	// Sweep every ttl/12 with sane bounds: frequent enough to keep the map
	// bounded, infrequent enough to be cheap. For a 24h TTL this is 2h.
	interval := ttl / 12
	if interval < time.Minute {
		interval = time.Minute
	}
	if interval > time.Hour {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		dropped := 0
		reservations.Range(func(k, v interface{}) bool {
			r := v.(*reservation)
			if now.Sub(r.CreatedAt) > ttl {
				if _, ok := reservations.LoadAndDelete(k); ok {
					dropped++
				}
			}
			return true
		})
		if dropped > 0 {
			g.Log().Info(ctx, "reservation sweeper: committed (dropped) expired entries:", dropped)
		}
	}
}
