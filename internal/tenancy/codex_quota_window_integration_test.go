package tenancy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
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

// TestQuotaWindowCapturedWithoutUserAttribution pins that quota windows are
// recorded for requests that resolve to no tenancy user. A window is keyed by
// (auth_id, provider) and describes the upstream credential's rate-limit state,
// which feeds balancing for the whole shared pool and is independent of who
// called. Capturing it after the user-attribution early-returns starved the
// balancer of every unattributed request -- which, in a deployment still using
// plain api-keys, is all of them.
func TestQuotaWindowCapturedWithoutUserAttribution(t *testing.T) {
	store := newTestStore(t)
	now := time.Unix(1786011000, 0).UTC()

	plugin := NewUsagePlugin(store, config.TenancyConfig{}, func(context.Context, usage.Record) (*User, error) {
		// No tenancy user for this caller, exactly like a legacy api-keys request.
		return nil, ErrNotFound
	})
	plugin.now = func() time.Time { return now }
	t.Cleanup(func() {
		if errClose := plugin.Close(); errClose != nil {
			t.Errorf("plugin.Close() error = %v", errClose)
		}
	})

	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "29")
	headers.Set("X-Codex-Primary-Reset-At", "1786498132")
	headers.Set("X-Codex-Primary-Window-Minutes", "10080")

	plugin.HandleUsage(context.Background(), usage.Record{
		AuthID:          "codex-auth-1",
		Provider:        "codex",
		Model:           "gpt-5.6-sol",
		RequestedAt:     now,
		ResponseHeaders: headers,
	})

	windows, errList := store.ListQuotaWindows(context.Background())
	if errList != nil {
		t.Fatalf("ListQuotaWindows() error = %v", errList)
	}
	if len(windows) != 1 {
		t.Fatalf("quota window count = %d, want 1 (unattributed requests must still record windows)", len(windows))
	}
	if windows[0].AuthID != "codex-auth-1" || windows[0].LimitUnits <= 0 {
		t.Fatalf("window = %+v, want auth codex-auth-1 with a positive limit", windows[0])
	}

	// The ledger must still be empty: usage attribution genuinely requires a user.
	var ledgerRows int
	if errCount := store.db.QueryRow(`SELECT COUNT(*) FROM usage_ledger`).Scan(&ledgerRows); errCount != nil {
		t.Fatalf("count usage_ledger: %v", errCount)
	}
	if ledgerRows != 0 {
		t.Fatalf("usage_ledger rows = %d, want 0 for an unattributed request", ledgerRows)
	}
}
