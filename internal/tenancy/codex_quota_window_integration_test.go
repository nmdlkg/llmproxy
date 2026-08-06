package tenancy

import (
	"net/http"
	"testing"
	"time"
)

// End-to-end check against the exact header set captured from a live Codex
// response, plus the empty-Reset-At trap.
func TestCodexRealHeadersDriveUrgencyBonus(t *testing.T) {
	now := time.Unix(1786011000, 0).UTC()
	h := http.Header{}
	for k, v := range map[string]string{
		"X-Codex-Active-Limit":                         "premium",
		"X-Codex-Plan-Type":                            "prolite",
		"X-Codex-Primary-Used-Percent":                 "29",
		"X-Codex-Primary-Reset-At":                     "1786498132",
		"X-Codex-Primary-Reset-After-Seconds":          "487213",
		"X-Codex-Primary-Window-Minutes":               "10080",
		"X-Codex-Primary-Over-Secondary-Limit-Percent": "0",
		"X-Codex-Secondary-Used-Percent":               "0",
		"X-Codex-Secondary-Reset-At":                   "",
		"X-Codex-Secondary-Reset-After-Seconds":        "0",
		"X-Codex-Secondary-Window-Minutes":             "0",
		"X-Codex-Credits-Has-Credits":                  "False",
		"X-Codex-Bengalfox-Primary-Used-Percent":       "0",
		"X-Codex-Bengalfox-Primary-Window-Minutes":     "10080",
	} {
		h.Set(k, v)
	}

	parsed, ok := ParseRateLimitHeaders(h, now)
	if !ok {
		t.Fatal("real Codex headers did not parse; quota_windows would stay empty")
	}
	t.Logf("parsed = %+v", parsed)

	if parsed.ResetAt.Unix() != 1786498132 {
		t.Errorf("ResetAt = %d, want 1786498132 (absolute Reset-At must win)", parsed.ResetAt.Unix())
	}
	if parsed.Limit <= 0 {
		t.Fatalf("Limit = %d, must be positive for a used/limit ratio", parsed.Limit)
	}
	ratio := float64(parsed.Remaining) / float64(parsed.Limit)
	used := 1 - ratio
	if usedFromRemaining := float64(parsed.Limit-parsed.Remaining) / float64(parsed.Limit); usedFromRemaining < 0.28 || usedFromRemaining > 0.30 {
		t.Errorf("used ratio = %.4f, want ~0.29 (29%%); remaining=%d limit=%d", usedFromRemaining, parsed.Remaining, parsed.Limit)
	}
	_ = used

	// The urgency bonus must be reachable with this window: reset far away => no bonus.
	balancing := Balancing{UrgencyHorizon: 30 * time.Minute, HighWater: 0.9, UrgencyBonus: 1}
	far := EffectivePriority(5, QuotaWindow{
		WindowEnd: parsed.ResetAt, UsedUnits: parsed.Limit - parsed.Remaining, LimitUnits: parsed.Limit,
	}, now, balancing)
	near := EffectivePriority(5, QuotaWindow{
		WindowEnd: now.Add(5 * time.Minute), UsedUnits: parsed.Limit - parsed.Remaining, LimitUnits: parsed.Limit,
	}, now, balancing)
	t.Logf("EffectivePriority far=%d near=%d", far, near)
	if far != 5 {
		t.Errorf("far-from-reset priority = %d, want base 5", far)
	}
	if near != 6 {
		t.Errorf("near-reset priority = %d, want 6 (base+bonus); urgency never fires", near)
	}
}

// The trap: Reset-At present but EMPTY must not parse as epoch 0, which would look
// like a window that reset in 1970 and keep the credential permanently "urgent".
func TestCodexEmptyResetAtIsNotEpoch(t *testing.T) {
	now := time.Unix(1786011000, 0).UTC()
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "50")
	h.Set("X-Codex-Primary-Reset-At", "")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
	h.Set("X-Codex-Primary-Window-Minutes", "60")

	parsed, ok := ParseRateLimitHeaders(h, now)
	if !ok {
		t.Fatal("empty Reset-At with a valid Reset-After-Seconds should still parse")
	}
	if parsed.ResetAt.Year() < 2000 {
		t.Fatalf("ResetAt = %v — empty Reset-At parsed as epoch; credential would look permanently about to reset", parsed.ResetAt)
	}
	if got := parsed.ResetAt.Unix(); got != now.Add(time.Hour).Unix() {
		t.Errorf("ResetAt = %d, want %d (now + Reset-After-Seconds)", got, now.Add(time.Hour).Unix())
	}
}
