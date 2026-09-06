package user

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
)

// seedUsage appends ledger rows for one user through the real tenancy store.
func (h *userTestHarness) seedUsage(t *testing.T, userID string, entries ...tenancy.UsageEntry) {
	t.Helper()
	for index := range entries {
		entries[index].UserID = userID
	}
	if errAppend := h.service.Store().AppendUsage(context.Background(), entries); errAppend != nil {
		t.Fatalf("AppendUsage(%s) error = %v", userID, errAppend)
	}
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal() error = %v body = %s", errUnmarshal, raw)
	}
	return payload
}

func TestGetUsageIsolatesUsers(t *testing.T) {
	harness := newUserTestHarness(t, true)
	now := time.Now().UTC().Add(-time.Hour)
	harness.seedUsage(t, "user-a", tenancy.UsageEntry{
		AuthID: "auth-a", Provider: "codex", Model: "gpt-5.6-sol",
		CostNanoUSD: 500, InputTokens: 50, OutputTokens: 8, OccurredAt: now,
	})
	harness.seedUsage(t, "user-b", tenancy.UsageEntry{
		AuthID: "auth-b", Provider: "claude", Model: "claude-opus-5",
		CostNanoUSD: 900, InputTokens: 90, OutputTokens: 4, OccurredAt: now,
	})

	response := harness.request(t, "user-b", http.MethodGet, "/v0/user/usage", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /usage = %d, want 200 body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "claude-opus-5") {
		t.Errorf("user-b usage is missing its own model: %s", body)
	}
	if strings.Contains(body, "gpt-5.6-sol") {
		t.Errorf("user-b usage leaked user-a's model: %s", body)
	}

	payload := decodeBody(t, response.Body.Bytes())
	if payload["schema_version"] != float64(1) || payload["currency"] != "USD" {
		t.Fatalf("usage contract identity = schema %v currency %v", payload["schema_version"], payload["currency"])
	}
	rangePayload, ok := payload["range"].(map[string]any)
	if !ok || rangePayload["timezone"] != "UTC" {
		t.Fatalf("usage range missing UTC contract: %v", payload["range"])
	}
	start, errStart := time.Parse(time.RFC3339, rangePayload["start"].(string))
	end, errEnd := time.Parse(time.RFC3339, rangePayload["end"].(string))
	if errStart != nil || errEnd != nil || !start.Before(end) {
		t.Fatalf("usage range is not a valid half-open interval: start=%v end=%v errors=%v/%v", start, end, errStart, errEnd)
	}
	models, ok := payload["models"].([]any)
	if !ok {
		t.Fatalf("models missing from payload: %s", body)
	}
	if len(models) != 1 {
		t.Errorf("models length = %d, want 1", len(models))
	}
	totals, ok := payload["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals missing from payload: %s", body)
	}
	if attempts, _ := totals["attempts"].(float64); attempts != 1 {
		t.Errorf("totals.attempts = %v, want 1", totals["attempts"])
	}
	if cost, costIsString := totals["cost_nano_usd"].(string); !costIsString || cost != "900" {
		t.Errorf("totals.cost_nano_usd = %#v, want exact integer string 900", totals["cost_nano_usd"])
	}
	model, modelOK := models[0].(map[string]any)
	if !modelOK {
		t.Fatalf("model row is not an object: %#v", models[0])
	}
	if cost, costIsString := model["cost_nano_usd"].(string); !costIsString || cost != "900" {
		t.Errorf("model.cost_nano_usd = %#v, want exact integer string 900", model["cost_nano_usd"])
	}
}

func TestGetUsageEmptyStateReturnsOK(t *testing.T) {
	harness := newUserTestHarness(t, true)
	response := harness.request(t, "user-a", http.MethodGet, "/v0/user/usage", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /usage = %d, want 200 body = %s", response.Code, response.Body.String())
	}
	payload := decodeBody(t, response.Body.Bytes())
	models, ok := payload["models"].([]any)
	if !ok || len(models) != 0 {
		t.Errorf("models = %v, want empty list", payload["models"])
	}
	daily, ok := payload["daily"].([]any)
	if !ok || len(daily) == 0 {
		t.Fatalf("daily = %v, want dense zero buckets", payload["daily"])
	}
	for _, raw := range daily {
		row, rowOK := raw.(map[string]any)
		if !rowOK || row["cost_nano_usd"] != "0" || row["attempts"] != float64(0) {
			t.Errorf("daily row = %v, want zero-valued dense bucket", raw)
		}
	}
}

func TestGetMeExposesLimitComposition(t *testing.T) {
	harness := newUserTestHarness(t, true)
	response := harness.request(t, "user-a", http.MethodGet, "/v0/user/me", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /me = %d, want 200 body = %s", response.Code, response.Body.String())
	}
	payload := decodeBody(t, response.Body.Bytes())
	quota, ok := payload["quota"].(map[string]any)
	if !ok {
		t.Fatalf("quota missing: %s", response.Body.String())
	}
	composition, ok := quota["composition"].(map[string]any)
	if !ok {
		t.Fatalf("composition missing: %s", response.Body.String())
	}
	// With no contributed credentials the base must equal the effective limit.
	if composition["base"] != quota["limit"] {
		t.Errorf("composition.base = %v, want limit %v", composition["base"], quota["limit"])
	}
	if credentials, _ := composition["contributing_credentials"].(float64); credentials != 0 {
		t.Errorf("contributing_credentials = %v, want 0", composition["contributing_credentials"])
	}
}

func TestAdminUsageRequiresAdminRole(t *testing.T) {
	harness := newUserTestHarness(t, true)
	if response := harness.request(t, "user-a", http.MethodGet, "/v0/user/admin/usage", nil); response.Code != http.StatusForbidden {
		t.Fatalf("GET /admin/usage as user = %d, want 403", response.Code)
	}
}

func TestAdminUsageReportsEveryUser(t *testing.T) {
	harness := newUserTestHarness(t, true)
	now := time.Now().UTC().Add(-time.Hour)
	harness.seedUsage(t, "user-a", tenancy.UsageEntry{
		AuthID: "auth-a", Provider: "codex", Model: "gpt-5.6-sol",
		CostNanoUSD: 500, InputTokens: 50, OutputTokens: 8, OccurredAt: now,
	})

	response := harness.request(t, "admin", http.MethodGet, "/v0/user/admin/usage", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /admin/usage = %d, want 200 body = %s", response.Code, response.Body.String())
	}
	payload := decodeBody(t, response.Body.Bytes())
	users, ok := payload["users"].([]any)
	if !ok {
		t.Fatalf("users missing: %s", response.Body.String())
	}
	// Users without usage must still be listed.
	if len(users) != 3 {
		t.Fatalf("users length = %d, want 3", len(users))
	}
	seen := make(map[string]map[string]any, len(users))
	for _, entry := range users {
		row, valid := entry.(map[string]any)
		if !valid {
			t.Fatalf("user row is not an object: %v", entry)
		}
		id, _ := row["id"].(string)
		seen[id] = row
		if _, leaked := row["auth_id"]; leaked {
			t.Error("admin usage leaked a credential identifier")
		}
	}
	if _, exists := seen["user-b"]; !exists {
		t.Error("admin usage omitted a user with no usage")
	}
	if attempts, _ := seen["user-a"]["attempts"].(float64); attempts != 1 {
		t.Errorf("user-a attempts = %v, want 1", seen["user-a"]["attempts"])
	}
	if attempts, _ := seen["user-b"]["attempts"].(float64); attempts != 0 {
		t.Errorf("user-b attempts = %v, want 0", seen["user-b"]["attempts"])
	}
}
