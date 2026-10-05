package forecast

import (
	"testing"
	"time"
)

func TestCalculateIsDeterministicAndUsesLatestCompleteBucket(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 30, 0, 0, time.UTC)
	obs := make([]Observation, 0, 24*7)
	for i := 0; i < 24*7; i++ {
		obs = append(obs, Observation{At: now.Add(-time.Duration(i+1) * time.Hour), Tokens: int64(i + 1)})
	}
	a := Calculate(obs, nil, now)
	b := Calculate(obs, nil, now)
	if a.ProjectedNextWeekTokens != b.ProjectedNextWeekTokens || a.Coverage != b.Coverage || a.AlgorithmVersion != "ewma-v1" {
		t.Fatalf("non-deterministic result: %#v %#v", a, b)
	}
	if a.Confidence != "medium" || a.Status != "ok" || a.Coverage <= 0.99 {
		t.Fatalf("result = %#v, want complete medium forecast", a)
	}
}

func TestCalculateUnknownAndNativeEntitlementSemantics(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	native := 42.0
	r := Calculate(nil, &Entitlement{NativeRemaining: &native, NativeUnit: "percent"}, now)
	if r.Status != "unknown" || r.EstimatedAvailableTokens != nil || r.NativeRemaining == nil || *r.NativeRemaining != native {
		t.Fatalf("unknown result = %#v", r)
	}
	remaining := int64(90)
	r = Calculate([]Observation{{At: now.Add(-time.Hour), Tokens: 0}}, &Entitlement{RemainingTokens: remaining, Authoritative: true, ResetAt: now.Add(time.Hour)}, now)
	if r.Status != "zero" || r.EstimatedAvailableTokens == nil || *r.EstimatedAvailableTokens != remaining || r.RemainingUntilReset == nil {
		t.Fatalf("zero result = %#v", r)
	}
}
