package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/autoroute"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestUserQuotaMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name             string
		tenancyEnabled   bool
		enforce          bool
		autoRouteEnabled bool
		quotaFallback    bool
		fallbackModel    string
		homeEnabled      bool
		tenantCaller     bool
		allowed          bool
		retryAfter       time.Duration
		wantStatus       int
		wantRetryAfter   string
		wantQuotaCalls   int
		wantForced       bool
		wantHandlerCalls int
	}{
		{
			name:             "allow",
			tenancyEnabled:   true,
			enforce:          true,
			tenantCaller:     true,
			allowed:          true,
			wantStatus:       http.StatusNoContent,
			wantQuotaCalls:   1,
			wantHandlerCalls: 1,
		},
		{
			name:             "deny with retry after",
			tenancyEnabled:   true,
			enforce:          true,
			tenantCaller:     true,
			retryAfter:       1500 * time.Millisecond,
			wantStatus:       http.StatusTooManyRequests,
			wantRetryAfter:   "2",
			wantQuotaCalls:   1,
			wantHandlerCalls: 0,
		},
		{
			name:             "forced fallback with auto routing disabled",
			tenancyEnabled:   true,
			enforce:          true,
			quotaFallback:    true,
			fallbackModel:    "fallback-live",
			tenantCaller:     true,
			retryAfter:       time.Minute,
			wantStatus:       http.StatusNoContent,
			wantQuotaCalls:   1,
			wantForced:       true,
			wantHandlerCalls: 1,
		},
		{
			name:             "forced fallback with auto routing enabled",
			tenancyEnabled:   true,
			enforce:          true,
			autoRouteEnabled: true,
			quotaFallback:    true,
			fallbackModel:    "fallback-live",
			tenantCaller:     true,
			retryAfter:       time.Minute,
			wantStatus:       http.StatusNoContent,
			wantQuotaCalls:   1,
			wantForced:       true,
			wantHandlerCalls: 1,
		},
		{
			name:             "empty fallback model denies",
			tenancyEnabled:   true,
			enforce:          true,
			quotaFallback:    true,
			tenantCaller:     true,
			retryAfter:       time.Minute,
			wantStatus:       http.StatusTooManyRequests,
			wantRetryAfter:   "60",
			wantQuotaCalls:   1,
			wantHandlerCalls: 0,
		},
		{
			name:             "Home mode denies because it cannot downgrade",
			tenancyEnabled:   true,
			enforce:          true,
			quotaFallback:    true,
			fallbackModel:    "fallback-live",
			homeEnabled:      true,
			tenantCaller:     true,
			retryAfter:       time.Minute,
			wantStatus:       http.StatusTooManyRequests,
			wantRetryAfter:   "60",
			wantQuotaCalls:   1,
			wantHandlerCalls: 0,
		},
		{
			name:             "allowed request ignores configured fallback",
			tenancyEnabled:   true,
			enforce:          true,
			autoRouteEnabled: true,
			quotaFallback:    true,
			fallbackModel:    "fallback-live",
			tenantCaller:     true,
			allowed:          true,
			wantStatus:       http.StatusNoContent,
			wantQuotaCalls:   1,
			wantHandlerCalls: 1,
		},
		{
			// Observe-only: usage is still attributed, but Quota.Check must not
			// even be consulted, so a store error can never deny a request.
			name:             "observe only skips quota check",
			tenancyEnabled:   true,
			enforce:          false,
			tenantCaller:     true,
			retryAfter:       time.Minute,
			wantStatus:       http.StatusNoContent,
			wantQuotaCalls:   0,
			wantHandlerCalls: 1,
		},
		{
			name:             "tenancy disabled passthrough",
			tenantCaller:     true,
			wantStatus:       http.StatusNoContent,
			wantQuotaCalls:   0,
			wantHandlerCalls: 1,
		},
		{
			name:             "admin service key passthrough",
			tenancyEnabled:   true,
			tenantCaller:     false,
			wantStatus:       http.StatusNoContent,
			wantQuotaCalls:   0,
			wantHandlerCalls: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{
				Tenancy: config.TenancyConfig{
					Enabled: test.tenancyEnabled,
					Quota:   config.TenancyQuota{Enforce: test.enforce},
				},
				Home: config.HomeConfig{Enabled: test.homeEnabled},
			}
			cfg.AutoRouting.Enabled = test.autoRouteEnabled
			cfg.AutoRouting.QuotaFallback = test.quotaFallback
			cfg.AutoRouting.FallbackModel = test.fallbackModel
			checker := &quotaCheckerStub{
				allowed:    test.allowed,
				retryAfter: test.retryAfter,
			}
			manager := sdkaccess.NewManager()
			if test.tenantCaller {
				manager.SetProviders([]sdkaccess.Provider{tenantAccessProviderStub{}})
			} else {
				manager.SetProviders([]sdkaccess.Provider{serviceAccessProviderStub{}})
			}

			engine := gin.New()
			engine.Use(AuthMiddleware(manager))
			engine.Use(UserQuotaMiddleware(func() *config.Config { return cfg }, checker))
			handlerCalls := 0
			engine.GET("/test", func(c *gin.Context) {
				handlerCalls++
				if got := autoroute.ForcedFallback(c.Request.Context()); got != test.wantForced {
					t.Errorf("ForcedFallback() = %t, want %t", got, test.wantForced)
				}
				if test.tenantCaller {
					user, ok := tenancy.UserFromGin(c)
					if !ok || user.ID != "user-1" {
						t.Errorf("UserFromGin() = %#v, %t", user, ok)
					}
					contextUser, contextOK := tenancy.UserFromContext(c.Request.Context())
					if !contextOK || contextUser.ID != "user-1" {
						t.Errorf("UserFromContext() = %#v, %t", contextUser, contextOK)
					}
					if ginUserID, exists := c.Get(tenancy.GinUserIDKey); !exists || ginUserID != "user-1" {
						t.Errorf("userID Gin value = %#v, exists=%t", ginUserID, exists)
					}
					if apiKey, exists := c.Get("userApiKey"); !exists || apiKey != "user-1" {
						t.Errorf("userApiKey Gin value = %#v, exists=%t", apiKey, exists)
					}
				} else if _, exists := c.Get(tenancy.GinUserIDKey); exists {
					t.Error("admin service key unexpectedly resolved a tenancy user")
				}
				c.Status(http.StatusNoContent)
			})

			request := httptest.NewRequest(http.MethodGet, "/test", nil)
			if test.tenantCaller {
				request.Header.Set("Authorization", "Bearer tenant-secret")
			} else {
				request.Header.Set("Authorization", "Bearer service-key")
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if got := response.Header().Get("Retry-After"); got != test.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, test.wantRetryAfter)
			}
			if checker.calls != test.wantQuotaCalls {
				t.Fatalf("quota calls = %d, want %d", checker.calls, test.wantQuotaCalls)
			}
			if handlerCalls != test.wantHandlerCalls {
				t.Fatalf("handler calls = %d, want %d", handlerCalls, test.wantHandlerCalls)
			}
			if checker.calls > 0 && checker.userID != "user-1" {
				t.Fatalf("quota user ID = %q, want user-1", checker.userID)
			}
			if test.wantStatus == http.StatusTooManyRequests &&
				!strings.Contains(response.Body.String(), "quota data is temporarily unavailable") {
				t.Fatalf("429 body does not surface fail-closed quota state: %s", response.Body.String())
			}
		})
	}
}

func TestQuotaFallbackHTTPExecutionUsesFallbackForExplicitModel(t *testing.T) {
	for _, autoRoutingEnabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(autoRoutingEnabled), func(t *testing.T) {
			server, apiKey, executor := newQuotaFallbackTestServer(t, autoRoutingEnabled)
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(`{"model":"quota-original","messages":[{"role":"user","content":"hello"}]}`),
			)
			request.Header.Set("Authorization", "Bearer "+apiKey)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.engine.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			if executor.executeCalls != 1 || executor.model != "quota-fallback" {
				t.Fatalf("executor calls/model = %d/%q, want 1/quota-fallback", executor.executeCalls, executor.model)
			}
		})
	}
}

func TestQuotaFallbackUnsupportedDirectRoutesFailClosed(t *testing.T) {
	server, apiKey, executor := newQuotaFallbackTestServer(t, true)
	tests := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPost, path: "/v1/alpha/search", body: `{"model":"quota-original"}`},
		{method: http.MethodPost, path: "/backend-api/codex/alpha/search", body: `{"model":"quota-original"}`},
		{method: http.MethodPost, path: "/v1/live", body: `{"model":"quota-original"}`},
		{method: http.MethodPost, path: "/v1/realtime/calls", body: `{"model":"quota-original"}`},
		{method: http.MethodGet, path: "/v1/live/call-1"},
		{method: http.MethodGet, path: "/v1/realtime/calls/call-1"},
		{method: http.MethodGet, path: "/v1/realtime?call_id=call-1"},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
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
		t.Fatalf("unsupported routes reached executor: execute=%d http=%d", executor.executeCalls, executor.httpCalls)
	}
}

type quotaFallbackCaptureExecutor struct {
	model        string
	executeCalls int
	httpCalls    int
}

func (*quotaFallbackCaptureExecutor) Identifier() string { return "quota-fallback-test" }

func (e *quotaFallbackCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, request coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.executeCalls++
	e.model = request.Model
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *quotaFallbackCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("unexpected stream execution")
}

func (e *quotaFallbackCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("unexpected count execution")
}

func (*quotaFallbackCaptureExecutor) Refresh(_ context.Context, credential *coreauth.Auth) (*coreauth.Auth, error) {
	return credential, nil
}

func (e *quotaFallbackCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	e.httpCalls++
	return nil, errors.New("unexpected HTTP execution")
}

func newQuotaFallbackTestServer(t *testing.T, autoRoutingEnabled bool) (*Server, string, *quotaFallbackCaptureExecutor) {
	t.Helper()
	dataDir := t.TempDir()
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			AutoRouting: config.AutoRoutingConfig{
				Enabled:       autoRoutingEnabled,
				QuotaFallback: true,
				FallbackModel: "quota-fallback",
			},
		},
		Tenancy: config.TenancyConfig{
			Enabled: true,
			DBPath:  filepath.Join(dataDir, "tenancy.db"),
			Quota: config.TenancyQuota{
				Enforce: true,
				Window:  "24h",
			},
		},
		AuthDir: dataDir,
	}
	authManager := coreauth.NewManager(nil, nil, nil)
	server := NewServer(cfg, authManager, sdkaccess.NewManager(), filepath.Join(dataDir, "config.yaml"), WithTenancyService())
	if server.tenancyInitErr != nil {
		t.Fatalf("NewServer() tenancy error = %v", server.tenancyInitErr)
	}
	if server.tenancyService == nil {
		t.Fatal("NewServer() tenancy service is nil")
	}
	t.Cleanup(func() {
		if errClose := server.tenancyService.Close(); errClose != nil {
			t.Errorf("tenancy service Close() error = %v", errClose)
		}
	})

	user := &tenancy.User{Email: "quota@example.com", Role: tenancy.RoleUser, Tier: "default"}
	if errCreate := server.tenancyService.Store().CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	apiKey, _, errIssue := server.tenancyService.Store().IssueAPIKey(user.ID, "quota-test")
	if errIssue != nil {
		t.Fatalf("IssueAPIKey() error = %v", errIssue)
	}

	executor := &quotaFallbackCaptureExecutor{}
	authManager.RegisterExecutor(executor)
	credential := &coreauth.Auth{
		ID:       "quota-fallback-auth",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
	}
	if _, errRegister := authManager.Register(context.Background(), credential); errRegister != nil {
		t.Fatalf("authManager.Register() error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{
		{ID: "quota-original"},
		{ID: "quota-fallback"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(credential.ID)
	})
	return server, apiKey, executor
}

type quotaCheckerStub struct {
	allowed    bool
	retryAfter time.Duration
	calls      int
	userID     string
}

func (q *quotaCheckerStub) Check(userID string) (bool, time.Duration) {
	q.calls++
	q.userID = userID
	return q.allowed, q.retryAfter
}

type tenantAccessProviderStub struct{}

func (tenantAccessProviderStub) Identifier() string {
	return "user-api-key"
}

func (tenantAccessProviderStub) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return &sdkaccess.Result{
		Provider:  "user-api-key",
		Principal: "user-1",
		Metadata: map[string]string{
			"user_id":  "user-1",
			"email":    "user@example.com",
			"role":     tenancy.RoleUser,
			"tier":     "default",
			"key_hash": tenancy.HashAPIKey("tenant-secret"),
		},
	}, nil
}

type serviceAccessProviderStub struct{}

func (serviceAccessProviderStub) Identifier() string {
	return "config-api-key"
}

func (serviceAccessProviderStub) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return &sdkaccess.Result{
		Provider:  "config-inline",
		Principal: "service-key",
		Metadata:  map[string]string{"source": "authorization"},
	}, nil
}
