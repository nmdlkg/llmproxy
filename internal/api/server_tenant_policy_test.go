package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestTenantPolicyRouteMatrix pins the set of routes that carry the tenancy
// policy chain. With quota enforced and no fallback, every policy route must
// deny an over-quota tenant before its handler runs, and non-policy realtime
// helper routes must not consult quota.
func TestTenantPolicyRouteMatrix(t *testing.T) {
	server, apiKey, executor := newQuotaFallbackTestServer(t, false)
	server.cfg.AutoRouting.QuotaFallback = false

	policyRoutes := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/v1/models"},
		{method: http.MethodPost, path: "/v1/chat/completions", body: `{"model":"quota-original","messages":[]}`},
		{method: http.MethodPost, path: "/v1/responses", body: `{"model":"quota-original"}`},
		{method: http.MethodPost, path: "/v1/alpha/search", body: `{"model":"quota-original"}`},
		{method: http.MethodPost, path: "/v1/live", body: `{"model":"quota-original"}`},
		{method: http.MethodGet, path: "/v1/live/call-1"},
		{method: http.MethodPost, path: "/openai/v1/videos", body: `{"model":"quota-original"}`},
		{method: http.MethodPost, path: "/backend-api/codex/responses", body: `{"model":"quota-original"}`},
		{method: http.MethodPost, path: "/backend-api/codex/alpha/search", body: `{"model":"quota-original"}`},
		{method: http.MethodGet, path: "/v1beta/models"},
		{method: http.MethodGet, path: "/v1/realtime?call_id=call-1"},
		{method: http.MethodPost, path: "/v1/realtime", body: `{"model":"quota-original"}`},
		{method: http.MethodPost, path: "/v1/realtime/calls", body: `{"model":"quota-original"}`},
		{method: http.MethodGet, path: "/v1/realtime/calls/call-1"},
		{method: http.MethodGet, path: "/v1/realtime/translations"},
		{method: http.MethodPost, path: "/v1/realtime/translations", body: `{"model":"quota-original"}`},
	}
	for _, route := range policyRoutes {
		t.Run("policy "+route.method+" "+route.path, func(t *testing.T) {
			request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			request.Header.Set("Authorization", "Bearer "+apiKey)
			response := httptest.NewRecorder()
			server.engine.ServeHTTP(response, request)
			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429; body=%s", response.Code, response.Body.String())
			}
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("Retry-After header is empty")
			}
		})
	}
	if executor.executeCalls != 0 || executor.httpCalls != 0 {
		t.Fatalf("over-quota requests reached executor: execute=%d http=%d", executor.executeCalls, executor.httpCalls)
	}

	nonPolicyRoutes := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/v1/realtime/client_secrets"},
		{method: http.MethodPost, path: "/v1/realtime/sessions"},
		{method: http.MethodPost, path: "/v1/realtime/transcription_sessions"},
	}
	for _, route := range nonPolicyRoutes {
		t.Run("non-policy "+route.method+" "+route.path, func(t *testing.T) {
			request := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
			request.Header.Set("Authorization", "Bearer "+apiKey)
			response := httptest.NewRecorder()
			server.engine.ServeHTTP(response, request)
			if response.Code == http.StatusTooManyRequests {
				t.Fatalf("status = 429 on a route without tenancy policy; body=%s", response.Body.String())
			}
		})
	}
}

// TestTenantPolicyFallbackGuardOnlyAffectsUnsupportedRoutes verifies that the
// group-level forced-fallback guard leaves supported group routes alone.
func TestTenantPolicyFallbackGuardOnlyAffectsUnsupportedRoutes(t *testing.T) {
	server, apiKey, executor := newQuotaFallbackTestServer(t, false)
	request := httptest.NewRequest(
		http.MethodPost,
		"/backend-api/codex/responses",
		strings.NewReader(`{"model":"quota-original","input":"hello"}`),
	)
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	if response.Code == http.StatusTooManyRequests {
		t.Fatalf("supported route was denied instead of using the fallback model; body=%s", response.Body.String())
	}
	if executor.model != "" && executor.model != "quota-fallback" {
		t.Fatalf("executor model = %q, want quota-fallback", executor.model)
	}
}

func TestForkRuntimeNilSafe(t *testing.T) {
	var runtime *forkRuntime
	if errStart := runtime.startErr(); errStart != nil {
		t.Fatalf("nil startErr = %v", errStart)
	}
	runtime.start(context.Background())()
	runtime.reload(&config.Config{})
	runtime.attachManagement(nil, nil, nil)
	if errStop := runtime.stop(context.Background(), nil); errStop != nil {
		t.Fatalf("nil stop = %v", errStop)
	}
	if runtime.tenancyStore() != nil || runtime.tenancy() != nil {
		t.Fatal("nil runtime exposed tenancy")
	}
}

func TestForkRuntimeStartErrPreventsServing(t *testing.T) {
	errInit := errors.New("boom")
	server := &Server{server: &http.Server{Addr: "127.0.0.1:0"}, fork: &forkRuntime{tenancyInitErr: errInit}}
	errStart := server.Start()
	if !errors.Is(errStart, errInit) || !strings.Contains(errStart.Error(), "initialize tenancy") {
		t.Fatalf("Start() error = %v, want wrapped tenancy init error", errStart)
	}
	server.fork = &forkRuntime{otelUsageInitErr: errInit}
	errStart = server.Start()
	if !errors.Is(errStart, errInit) || !strings.Contains(errStart.Error(), "OpenTelemetry") {
		t.Fatalf("Start() error = %v, want wrapped OTel init error", errStart)
	}
}

// TestForkRuntimeStopClosesTenancyAndReloadReconfigures covers the fork
// lifecycle through the public Server API: reload reaches the tenancy service
// and user handler, and Stop closes the tenancy database.
func TestForkRuntimeStopClosesTenancyAndReloadReconfigures(t *testing.T) {
	dataDir := t.TempDir()
	cfg := &config.Config{
		Tenancy: config.TenancyConfig{
			Enabled: true,
			DBPath:  filepath.Join(dataDir, "tenancy.db"),
			Quota:   config.TenancyQuota{Window: "24h"},
		},
		AuthDir: dataDir,
	}
	server := NewServer(cfg, coreauth.NewManager(nil, nil, nil), sdkaccess.NewManager(), filepath.Join(dataDir, "config.yaml"), WithTenancyService())
	if server.fork == nil || server.fork.tenancyService == nil || server.fork.user == nil {
		t.Fatalf("fork runtime not initialized: %+v", server.fork)
	}
	if server.fork.tenancyStore() == nil {
		t.Fatal("tenancy store is nil")
	}

	next := *cfg
	next.Tenancy.Quota.Window = "48h"
	if ok := server.UpdateClientsContext(context.Background(), &next); !ok {
		t.Fatal("UpdateClientsContext returned false")
	}

	if errStop := server.Stop(context.Background()); errStop != nil {
		t.Fatalf("Stop() error = %v", errStop)
	}
	user := &tenancy.User{Email: "closed@example.com", Role: tenancy.RoleUser, Tier: "default"}
	if errCreate := server.fork.tenancyService.Store().CreateUser(user); errCreate == nil {
		t.Fatal("tenancy store accepted writes after Stop")
	}
}

func TestForkRuntimeStopPrefersHTTPShutdownError(t *testing.T) {
	dataDir := t.TempDir()
	service, errService := tenancy.NewService(config.TenancyConfig{
		Enabled: true,
		DBPath:  filepath.Join(dataDir, "tenancy.db"),
	}, dataDir, coreauth.NewManager(nil, nil, nil))
	if errService != nil {
		t.Fatalf("NewService() error = %v", errService)
	}
	runtime := &forkRuntime{tenancyService: service}
	if errStop := runtime.stop(context.Background(), errors.New("http shutdown failed")); errStop != nil {
		t.Fatalf("stop() with HTTP error = %v, want nil (HTTP error takes precedence)", errStop)
	}
	if errCreate := service.Store().CreateUser(&tenancy.User{Email: "x@example.com", Role: tenancy.RoleUser}); errCreate == nil {
		t.Fatal("tenancy store still open after stop")
	}
}
