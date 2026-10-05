package user

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/tenancy"
)

func TestUsageForecastIsTenantScopedAndRedactsCredentialIdentity(t *testing.T) {
	h := newUserTestHarness(t, true)
	now := time.Now().UTC()
	for i := 1; i <= 7*24; i++ {
		h.seedUsage(t, "user-a", tenancy.UsageEntry{AuthID: "secret-auth-a", Provider: "claude", Model: "claude-sonnet", InputTokens: 10, OutputTokens: 5, OccurredAt: now.Add(-time.Duration(i) * time.Hour)})
		h.seedUsage(t, "user-b", tenancy.UsageEntry{AuthID: "secret-auth-b", Provider: "claude", Model: "claude-sonnet", InputTokens: 1000, OutputTokens: 1000, OccurredAt: now.Add(-time.Duration(i) * time.Hour)})
	}
	response := h.request(t, "user-a", http.MethodGet, "/v0/user/usage/forecast", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("forecast status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if containsAny(body, "secret-auth-a", "secret-auth-b", "secret-auth-owner") {
		t.Fatalf("credential identity leaked: %s", body)
	}
	payload := decodeBody(t, response.Body.Bytes())
	if payload["advisory"] != true || payload["algorithm_version"] != "ewma-v1" || payload["truncated"] != false {
		t.Fatalf("forecast contract = %#v", payload)
	}
	rows, ok := payload["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("rows = %#v, want one tenant row", payload["rows"])
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if len(needle) > 0 && contains(value, needle) {
			return true
		}
	}
	return false
}

func contains(value, needle string) bool {
	for i := 0; i+len(needle) <= len(value); i++ {
		if value[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
