package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/autoroute"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

func TestUserQuotaMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name             string
		tenancyEnabled   bool
		enforce          bool
		quotaFallback    bool
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
			name:             "forced fallback",
			tenancyEnabled:   true,
			enforce:          true,
			quotaFallback:    true,
			tenantCaller:     true,
			retryAfter:       time.Minute,
			wantStatus:       http.StatusNoContent,
			wantQuotaCalls:   1,
			wantForced:       true,
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
			}
			cfg.AutoRouting.QuotaFallback = test.quotaFallback
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
