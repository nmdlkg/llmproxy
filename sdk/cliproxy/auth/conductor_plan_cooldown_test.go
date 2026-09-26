package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

const codexPlanUnsupportedBody = `{"detail":"The 'gpt-6-luna' model is not supported when using Codex with a ChatGPT account."}`

func newCodexPlanCooldownManager(t *testing.T, plans map[string]string) *Manager {
	t.Helper()
	previousDisabled := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	previousSeconds := codexPlanModelUnsupportedCooldownSeconds.Load()
	t.Cleanup(func() {
		quotaCooldownDisabled.Store(previousDisabled)
		codexPlanModelUnsupportedCooldownSeconds.Store(previousSeconds)
	})
	m := NewManager(nil, nil, nil)
	for id, plan := range plans {
		auth := &Auth{ID: id, Provider: "codex", Attributes: map[string]string{"plan_type": plan}}
		if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
	}
	return m
}

func codexPlanUnsupportedResult(authID string) Result {
	return Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    "gpt-6-luna",
		Error:    &Error{HTTPStatus: http.StatusBadRequest, Message: codexPlanUnsupportedBody},
	}
}

func modelRetryAfter(t *testing.T, m *Manager, authID, model string) time.Time {
	t.Helper()
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("auth %s not found", authID)
	}
	state := auth.ModelStates[model]
	if state == nil {
		return time.Time{}
	}
	return state.NextRetryAfter
}

func TestIsCodexPlanModelUnsupportedError(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		err      *Error
		want     bool
	}{
		{"codex 400 plan rejection", "codex", &Error{HTTPStatus: 400, Message: codexPlanUnsupportedBody}, true},
		{"other provider", "claude", &Error{HTTPStatus: 400, Message: codexPlanUnsupportedBody}, false},
		{"codex 400 context too large", "codex", &Error{HTTPStatus: 400, Message: `{"error":{"code":"context_too_large","message":"too many tokens"}}`}, false},
		{"codex 400 unsupported parameter", "codex", &Error{HTTPStatus: 400, Message: `{"detail":"Unsupported parameter: temperature"}`}, false},
		{"codex 503 with same text", "codex", &Error{HTTPStatus: 503, Message: codexPlanUnsupportedBody}, false},
		{"nil error", "codex", nil, false},
	}
	for _, tc := range cases {
		if got := isCodexPlanModelUnsupportedError(tc.provider, tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMarkResult_CodexPlanModelUnsupported_CoolsWholeTier(t *testing.T) {
	m := newCodexPlanCooldownManager(t, map[string]string{"edu-1": "edu", "edu-2": "edu", "plus-1": "plus"})
	SetCodexPlanModelUnsupportedCooldownSeconds(3600)

	before := time.Now()
	m.MarkResult(context.Background(), codexPlanUnsupportedResult("edu-1"))

	for _, id := range []string{"edu-1", "edu-2"} {
		next := modelRetryAfter(t, m, id, "gpt-6-luna")
		if next.Before(before.Add(59*time.Minute)) || next.After(before.Add(61*time.Minute)) {
			t.Fatalf("%s NextRetryAfter = %v, want ~1h", id, next)
		}
		auth, _ := m.GetByID(id)
		if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-6-luna", time.Now()); !blocked {
			t.Fatalf("%s should be blocked for gpt-6-luna", id)
		}
		if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-6-sol", time.Now()); blocked {
			t.Fatalf("%s must not be blocked for other models", id)
		}
	}
	if next := modelRetryAfter(t, m, "plus-1", "gpt-6-luna"); !next.IsZero() {
		t.Fatalf("plus-1 must not be cooled by an edu rejection, got %v", next)
	}
}

func TestMarkResult_CodexPlanModelUnsupported_DefaultIs24h(t *testing.T) {
	m := newCodexPlanCooldownManager(t, map[string]string{"edu-1": "edu", "edu-2": "edu"})
	SetCodexPlanModelUnsupportedCooldownSeconds(0)

	before := time.Now()
	m.MarkResult(context.Background(), codexPlanUnsupportedResult("edu-1"))
	next := modelRetryAfter(t, m, "edu-2", "gpt-6-luna")
	if next.Before(before.Add(24*time.Hour - time.Minute)) {
		t.Fatalf("edu-2 NextRetryAfter = %v, want ~24h", next)
	}
}

func TestMarkResult_CodexPlanModelUnsupported_SuccessClearsTier(t *testing.T) {
	m := newCodexPlanCooldownManager(t, map[string]string{"edu-1": "edu", "edu-2": "edu"})
	SetCodexPlanModelUnsupportedCooldownSeconds(3600)

	m.MarkResult(context.Background(), codexPlanUnsupportedResult("edu-1"))
	m.MarkResult(context.Background(), Result{AuthID: "edu-2", Provider: "codex", Model: "gpt-6-luna", Success: true})

	auth, _ := m.GetByID("edu-1")
	if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-6-luna", time.Now()); blocked {
		t.Fatal("edu-1 should be released once a tier peer succeeds")
	}
}

func TestMarkResult_CodexPlanModelUnsupported_NegativeDisablesPropagation(t *testing.T) {
	m := newCodexPlanCooldownManager(t, map[string]string{"edu-1": "edu", "edu-2": "edu"})
	SetCodexPlanModelUnsupportedCooldownSeconds(-1)

	before := time.Now()
	m.MarkResult(context.Background(), codexPlanUnsupportedResult("edu-1"))
	if next := modelRetryAfter(t, m, "edu-2", "gpt-6-luna"); !next.IsZero() {
		t.Fatalf("edu-2 must not be cooled when tier-wide cooldown is disabled, got %v", next)
	}
	// Legacy per-credential model-support cooldown still applies.
	if next := modelRetryAfter(t, m, "edu-1", "gpt-6-luna"); next.Before(before.Add(modelSupportRetryAfter - time.Hour)) {
		t.Fatalf("edu-1 NextRetryAfter = %v, want legacy ~%v", next, modelSupportRetryAfter)
	}
}
