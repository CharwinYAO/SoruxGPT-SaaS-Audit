package api_test

import (
	"auditlimit/api"
	"testing"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/os/gctx"
)

func TestGetVisitorWithModel(t *testing.T) {
	ctx := gctx.New()
	limit, per, limiter, err := api.GetVisitorWithModel(ctx, "token", "text-davinci-002-render-sha", 1)
	if err != nil {
		g.Log().Error(ctx, "GetVisitorWithModel", err)
		return
	}
	g.Dump(limiter)
	g.Log().Info(ctx, "limit:", limit, "per:", per, "limiter:", limiter)
}

// TestGetVisitorWithModel_Multiplier verifies the tier multiplier is applied
// to the base rate and that different multipliers yield distinct limiter
// instances (different cache keys), so a tier change is reflected on the very
// next request without waiting for cleanup.
func TestGetVisitorWithModel_Multiplier(t *testing.T) {
	ctx := gctx.New()
	base, _, lim1, err := api.GetVisitorWithModel(ctx, "tier-test-token", "test-model", 1)
	if err != nil {
		t.Fatalf("base lookup failed: %v", err)
	}
	scaled, _, lim5, err := api.GetVisitorWithModel(ctx, "tier-test-token", "test-model", 5)
	if err != nil {
		t.Fatalf("scaled lookup failed: %v", err)
	}
	if scaled != base*5 {
		t.Fatalf("expected scaled=%d (base*5), got %d", base*5, scaled)
	}
	if lim1 == lim5 {
		t.Fatalf("expected distinct limiters per multiplier; got the same instance")
	}
}
