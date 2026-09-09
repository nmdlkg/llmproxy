package user

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestProviderQuotaParsersNormalizeFiveHourAndWeeklyWindows(t *testing.T) {
	claude, errClaude := parseClaudeProviderQuota([]byte(`{
		"five_hour":{"utilization":12,"resets_at":"2026-08-20T17:00:00Z"},
		"seven_day":{"utilization":31,"resets_at":"2026-08-25T12:00:00Z"}
	}`))
	if errClaude != nil {
		t.Fatalf("parseClaudeProviderQuota() error = %v", errClaude)
	}
	if len(claude) != 2 || claude[0].Name != "5h" || claude[1].Name != "weekly" {
		t.Fatalf("Claude windows = %#v, want 5h and weekly", claude)
	}
	if *claude[0].UsedPercent != 12 || *claude[0].RemainingPercent != 88 {
		t.Fatalf("Claude five-hour percentages = %#v, want 12/88", claude[0])
	}

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	codex, errCodex := parseCodexProviderQuotaAt([]byte(`{
		"rate_limit":{
			"primary_window":{"used_percent":"31","limit_window_seconds":"604800","reset_at":1787659200},
			"secondary_window":{"used_percent":12,"window_minutes":300,"reset_after_seconds":60}
		}
	}`), now)
	if errCodex != nil {
		t.Fatalf("parseCodexProviderQuotaAt() error = %v", errCodex)
	}
	if len(codex) != 2 || codex[0].Name != "weekly" || codex[1].Name != "5h" {
		t.Fatalf("Codex windows = %#v, want weekly and 5h", codex)
	}
	if codex[1].WindowSeconds != 5*60*60 || codex[1].ResetAt == nil || !codex[1].ResetAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("Codex secondary window = %#v, want 5h/reset-after", codex[1])
	}
}

func TestProviderQuotaParsersRejectTrailingGarbageAndOutOfRangeTimes(t *testing.T) {
	for raw, wantOK := range map[string]bool{
		`12`:           true,
		`"12%"`:        true,
		`" 12 % "`:     true,
		`"12abc"`:      false,
		`"12%garbage"`: false,
		`"101%"`:       false,
		`"NaN"`:        false,
	} {
		if _, ok := quotaPercent([]byte(raw)); ok != wantOK {
			t.Errorf("quotaPercent(%s) ok = %v, want %v", raw, ok, wantOK)
		}
	}

	for value, wantOK := range map[string]bool{
		"946684800":   true,  // 2000-01-01
		"4102444799":  true,  // just before 2100-01-01
		"946684799":   false, // before the lower bound
		"4102444800":  false, // at the upper bound
		"1787659200x": false,
	} {
		if _, ok := parseQuotaUnix(value); ok != wantOK {
			t.Errorf("parseQuotaUnix(%q) ok = %v, want %v", value, ok, wantOK)
		}
	}
}

func TestCodexProviderQuotaNamesMissingDurationsByWindowKey(t *testing.T) {
	windows, errParse := parseCodexProviderQuotaAt([]byte(`{
		"rate_limit":{
			"primary_window":{"used_percent":12,"reset_after_seconds":60},
			"secondary_window":{"used_percent":31,"reset_after_seconds":120}
		}
	}`), time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	if errParse != nil {
		t.Fatalf("parseCodexProviderQuotaAt() error = %v", errParse)
	}
	if len(windows) != 2 || windows[0].Name != "5h" || windows[1].Name != "weekly" {
		t.Fatalf("windows = %#v, want primary=5h and secondary=weekly fallback names", windows)
	}
}

func TestGetProviderQuotasScopesOwnedCredentialsAndRedactsSecrets(t *testing.T) {
	harness := newUserTestHarness(t, true)
	harness.writeCredential(t, "owner-a.json", "user-a", false)
	harness.writeCredential(t, "owner-b.json", "user-b", false)

	previousFetcher := fetchProviderQuota
	defer func() { fetchProviderQuota = previousFetcher }()
	var seen []string
	fetchProviderQuota = func(_ context.Context, _ *config.Config, auth *coreauth.Auth) ([]providerQuotaWindow, error) {
		seen = append(seen, auth.ID)
		used := float64(12)
		remaining := float64(88)
		return []providerQuotaWindow{{Name: "5h", UsedPercent: &used, RemainingPercent: &remaining}}, nil
	}

	response := harness.request(t, "user-a", http.MethodGet, "/v0/user/provider-quotas", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /provider-quotas = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, "provider-secret-owner-a") || strings.Contains(body, "provider-secret-owner-b") {
		t.Fatalf("provider secret leaked in quota response: %s", body)
	}
	if strings.Contains(body, "owner-b@example.com") || strings.Contains(body, "owner-b.json") {
		t.Fatalf("other user's quota leaked: %s", body)
	}
	if len(seen) != 1 {
		t.Fatalf("fetches = %d, want one owned credential (seen=%v)", len(seen), seen)
	}
	var payload struct {
		SchemaVersion int  `json:"schema_version"`
		Truncated     bool `json:"truncated"`
		Accounts      []struct {
			Label    string `json:"label"`
			Provider string `json:"provider"`
			Windows  []struct {
				Name             string  `json:"name"`
				UsedPercent      float64 `json:"used_percent"`
				RemainingPercent float64 `json:"remaining_percent"`
			} `json:"windows"`
		} `json:"accounts"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload.SchemaVersion != 1 || payload.Truncated || len(payload.Accounts) != 1 {
		t.Fatalf("payload = %#v, want schema 1 with one account", payload)
	}
	if payload.Accounts[0].Label != "owner-a.json@example.com" || payload.Accounts[0].Provider != "claude" {
		t.Fatalf("account identity = %#v", payload.Accounts[0])
	}
	if len(payload.Accounts[0].Windows) != 1 || payload.Accounts[0].Windows[0].RemainingPercent != 88 {
		t.Fatalf("account windows = %#v", payload.Accounts[0].Windows)
	}
}

func TestProviderQuotaFetchSingleflightAndTokenAwareCache(t *testing.T) {
	harness := newUserTestHarness(t, true)
	harness.writeCredential(t, "owner-a.json", "user-a", false)

	previousFetcher := fetchProviderQuota
	defer func() { fetchProviderQuota = previousFetcher }()
	var calls atomic.Int32
	var fetchMu sync.Mutex
	fetchProviderQuota = func(_ context.Context, _ *config.Config, _ *coreauth.Auth) ([]providerQuotaWindow, error) {
		calls.Add(1)
		fetchMu.Lock()
		defer fetchMu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return []providerQuotaWindow{}, nil
	}

	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if accounts, truncated := harness.handler.providerQuotaAccounts(context.Background(), "user-a"); truncated || len(accounts) != 1 {
				t.Errorf("accounts = %d, want 1", len(accounts))
			}
		}()
	}
	workers.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider fetch calls = %d, want one singleflight call", got)
	}
	authA, _ := harness.manager.GetByID("owner-a.json")
	if authA == nil || providerQuotaCacheKey("user-a", authA) == providerQuotaCacheKey("user-b", authA) {
		t.Fatal("provider quota cache key does not include user ownership")
	}
}

func TestProviderQuotaLimitsOwnedCredentials(t *testing.T) {
	harness := newUserTestHarness(t, true)
	for index := 0; index < providerQuotaMaxCredentials+1; index++ {
		harness.writeCredential(t, fmt.Sprintf("owner-%02d.json", index), "user-a", false)
	}

	previousFetcher := fetchProviderQuota
	defer func() { fetchProviderQuota = previousFetcher }()
	var calls atomic.Int32
	fetchProviderQuota = func(_ context.Context, _ *config.Config, _ *coreauth.Auth) ([]providerQuotaWindow, error) {
		calls.Add(1)
		return []providerQuotaWindow{}, nil
	}

	response := harness.request(t, "user-a", http.MethodGet, "/v0/user/provider-quotas", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /provider-quotas = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Accounts  []providerQuotaAccount `json:"accounts"`
		Truncated bool                   `json:"truncated"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if !payload.Truncated || len(payload.Accounts) != providerQuotaMaxCredentials {
		t.Fatalf("payload has %d accounts, truncated=%v; want %d and true", len(payload.Accounts), payload.Truncated, providerQuotaMaxCredentials)
	}
	if got := calls.Load(); got != providerQuotaMaxCredentials {
		t.Fatalf("provider fetch calls = %d, want %d", got, providerQuotaMaxCredentials)
	}
}

func TestProviderQuotaSharedFetchBudgetReturnsBusyWithoutWaiting(t *testing.T) {
	harness := newUserTestHarness(t, true)
	harness.writeCredential(t, "owner-a.json", "user-a", false)
	harness.writeCredential(t, "owner-b.json", "user-b", false)
	harness.handler.quotaFetchSlots = make(chan struct{}, 1)

	previousFetcher := fetchProviderQuota
	defer func() { fetchProviderQuota = previousFetcher }()
	started := make(chan struct{})
	release := make(chan struct{})
	fetchProviderQuota = func(_ context.Context, _ *config.Config, auth *coreauth.Auth) ([]providerQuotaWindow, error) {
		if authfiles.OwnerUserID(auth) == "user-a" {
			close(started)
			<-release
		}
		return []providerQuotaWindow{}, nil
	}

	firstDone := make(chan struct{})
	go func() {
		harness.handler.providerQuotaAccounts(context.Background(), "user-a")
		close(firstDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first provider quota fetch did not start")
	}

	accounts, truncated := harness.handler.providerQuotaAccounts(context.Background(), "user-b")
	if truncated || len(accounts) != 1 || accounts[0].Status != "unavailable" || accounts[0].Error != "busy" {
		t.Fatalf("busy response = %#v, truncated=%v", accounts, truncated)
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first provider quota fetch did not finish")
	}
}

func TestProviderQuotaAccountIDUsesCodexIDTokenClaim(t *testing.T) {
	claims := `{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-from-claim"}}`
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
	auth := &coreauth.Auth{Metadata: map[string]any{"id_token": token}}
	if got := providerQuotaAccountID(auth); got != "acct-from-claim" {
		t.Fatalf("providerQuotaAccountID() = %q, want acct-from-claim", got)
	}
}

func TestProviderQuotaFetchDoesNotFollowRedirects(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer quota-secret" {
			t.Fatalf("authorization header = %q, want bearer token on fixed host", request.Header.Get("Authorization"))
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://attacker.invalid/collect"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
	auth := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"access_token": "quota-secret"},
		Attributes: map[string]string{
			"auth_kind": coreauth.AuthKindOAuth,
		},
	}
	if _, errFetch := fetchProviderQuotaHTTP(ctx, &config.Config{}, auth); errFetch == nil {
		t.Fatal("fetchProviderQuotaHTTP() error = nil, want redirect status error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("round trip calls = %d, want one request with redirect rejected", got)
	}
}

func TestProviderQuotaCachePrunesExpiredEntries(t *testing.T) {
	harness := newUserTestHarness(t, true)
	handler := harness.handler
	now := time.Now()
	handler.quotaCache = map[string]providerQuotaCacheEntry{
		"expired": {expiresAt: now.Add(-time.Second), account: providerQuotaAccount{Label: "old"}},
		"live":    {expiresAt: now.Add(time.Minute), account: providerQuotaAccount{Label: "current"}},
	}
	if _, ok := handler.cachedProviderQuota("live"); !ok {
		t.Fatal("cached live quota missing")
	}
	if _, ok := handler.quotaCache["expired"]; ok {
		t.Fatal("expired quota cache entry was not pruned")
	}
	if _, ok := handler.quotaCache["live"]; !ok {
		t.Fatal("live quota cache entry was pruned")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
