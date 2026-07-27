package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

func TestCredentialValidationMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name               string
		tenancyEnabled     bool
		validationInterval string
		tenantCaller       bool
		preferred          []string
		validatorErr       error
		wantCalls          int
		wantContextChange  bool
	}{
		{
			name:               "due tenant gets preferred context",
			tenancyEnabled:     true,
			validationInterval: "1h",
			tenantCaller:       true,
			preferred:          []string{"auth-1"},
			wantCalls:          1,
			wantContextChange:  true,
		},
		{
			name:               "not due passes through unchanged",
			tenancyEnabled:     true,
			validationInterval: "1h",
			tenantCaller:       true,
			wantCalls:          1,
		},
		{
			name:               "disabled tenancy adds no preference",
			validationInterval: "1h",
			tenantCaller:       true,
			preferred:          []string{"auth-1"},
		},
		{
			name:               "invalid interval adds no preference",
			tenancyEnabled:     true,
			validationInterval: "invalid",
			tenantCaller:       true,
			preferred:          []string{"auth-1"},
		},
		{
			name:               "nonpositive interval adds no preference",
			tenancyEnabled:     true,
			validationInterval: "0s",
			tenantCaller:       true,
			preferred:          []string{"auth-1"},
		},
		{
			name:               "admin caller passes through",
			tenancyEnabled:     true,
			validationInterval: "1h",
			preferred:          []string{"auth-1"},
		},
		{
			name:               "validator failure never aborts",
			tenancyEnabled:     true,
			validationInterval: "1h",
			tenantCaller:       true,
			validatorErr:       errors.New("validation store unavailable"),
			wantCalls:          1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Tenancy: config.TenancyConfig{
				Enabled:            test.tenancyEnabled,
				ValidationInterval: test.validationInterval,
			}}
			validator := &credentialValidationCheckerStub{
				preferred: test.preferred,
				err:       test.validatorErr,
			}
			manager := sdkaccess.NewManager()
			if test.tenantCaller {
				manager.SetProviders([]sdkaccess.Provider{tenantAccessProviderStub{}})
			} else {
				manager.SetProviders([]sdkaccess.Provider{serviceAccessProviderStub{}})
			}

			engine := gin.New()
			engine.Use(AuthMiddleware(manager))
			engine.Use(func(c *gin.Context) {
				c.Set("contextBeforeValidation", c.Request.Context())
				c.Next()
			})
			engine.Use(CredentialValidationMiddleware(func() *config.Config { return cfg }, validator))
			handlerCalls := 0
			contextChanged := false
			engine.GET("/test", func(c *gin.Context) {
				handlerCalls++
				before, _ := c.Get("contextBeforeValidation")
				contextChanged = before.(context.Context) != c.Request.Context()
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

			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
			}
			if handlerCalls != 1 {
				t.Fatalf("handler calls = %d, want 1", handlerCalls)
			}
			if validator.calls != test.wantCalls {
				t.Fatalf("validator calls = %d, want %d", validator.calls, test.wantCalls)
			}
			if contextChanged != test.wantContextChange {
				t.Fatalf("request context changed = %t, want %t", contextChanged, test.wantContextChange)
			}
			if validator.calls > 0 {
				if validator.userID != "user-1" {
					t.Fatalf("validator user ID = %q, want %q", validator.userID, "user-1")
				}
				if validator.interval != time.Hour {
					t.Fatalf("validator interval = %v, want %v", validator.interval, time.Hour)
				}
			}
		})
	}
}

type credentialValidationCheckerStub struct {
	preferred []string
	err       error
	calls     int
	userID    string
	interval  time.Duration
}

func (s *credentialValidationCheckerStub) PreferredAuthIDs(userID string, interval time.Duration) ([]string, error) {
	s.calls++
	s.userID = userID
	s.interval = interval
	return append([]string(nil), s.preferred...), s.err
}
