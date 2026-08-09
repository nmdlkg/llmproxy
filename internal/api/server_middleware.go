package api

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	useraccess "github.com/router-for-me/CLIProxyAPI/v7/internal/access/user_access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/autoroute"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/safemode"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
)

var corsExposedResponseHeaders = []string{
	logging.CPATraceIDHeader,
	"X-CPA-VERSION",
	"X-CPA-COMMIT",
	"X-CPA-BUILD-DATE",
	"X-CPA-SUPPORT-PLUGIN",
	"X-CPA-HOME-VERSION",
	"X-CPA-HOME-BUILD-DATE",
	"X-SERVER-VERSION",
	"X-SERVER-BUILD-DATE",
}

var corsExposedResponseHeadersJoined = strings.Join(corsExposedResponseHeaders, ", ")

const (
	exampleAPIKeyManagementPath = "/management.html"
	exampleAPIKeyManagementURL  = "/management.html?safe-mode=configure"
)

func (s *Server) homeHeartbeatMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.cfg == nil || !s.cfg.Home.Enabled {
			c.Next()
			return
		}
		if c != nil && c.Request != nil {
			path := c.Request.URL.Path
			if strings.HasPrefix(path, "/v0/management/") || path == "/v0/management" || strings.HasPrefix(path, "/v0/resource/plugins/") || path == "/management.html" {
				c.Next()
				return
			}
		}
		client := home.Current()
		if client == nil || !client.HeartbeatOK() {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.Next()
	}
}

func (s *Server) exampleAPIKeySafeModeRequired(cfg *config.Config) bool {
	return s != nil && s.exampleAPIKeySafeModeEnabled && cfg != nil && safemode.HasExampleAPIKeys(cfg.APIKeys)
}

func (s *Server) exampleAPIKeySafeModeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || !s.exampleAPIKeySafeModeActive.Load() || c == nil || c.Request == nil || c.Request.URL == nil {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if path == exampleAPIKeyManagementPath && c.Query("safe-mode") == "configure" {
			c.Next()
			return
		}
		if (path == "/" || path == exampleAPIKeyManagementPath) && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
			s.serveExampleAPIKeyWarningPage(c)
			return
		}
		if !isExampleAPIKeySafeModeProxyPath(path) {
			c.Next()
			return
		}

		c.Header("X-CPA-SAFE-MODE", "example-api-key")
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error":   "unsafe_example_api_key",
			"message": "Proxy API endpoints are disabled because api-keys contains template values. Open /management.html?safe-mode=configure, update api-keys in Management, then retry.",
		})
	}
}

func (s *Server) serveExampleAPIKeyWarningPage(c *gin.Context) {
	cfg := s.cfg
	var keys []string
	if cfg != nil {
		keys = safemode.ExampleAPIKeys(cfg.APIKeys)
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		c.Abort()
		return
	}
	c.String(http.StatusOK, safemode.ExampleAPIKeyWarningPageHTML(keys, exampleAPIKeyManagementURL))
	c.Abort()
}

func isExampleAPIKeySafeModeProxyPath(path string) bool {
	switch {
	case path == "/v1" || strings.HasPrefix(path, "/v1/"):
		return true
	case path == "/v1beta" || strings.HasPrefix(path, "/v1beta/"):
		return true
	case path == "/openai/v1" || strings.HasPrefix(path, "/openai/v1/"):
		return true
	case path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/"):
		return true
	default:
		return false
	}
}

// corsMiddleware returns a Gin middleware handler that adds CORS headers
// to every response, allowing cross-origin requests.
//
// Returns:
//   - gin.HandlerFunc: The CORS middleware handler
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
		c.Header("Access-Control-Expose-Headers", corsExposedResponseHeadersJoined)

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// AuthMiddleware returns a Gin middleware handler that authenticates requests
// using the configured authentication providers. When no providers are available,
// it allows all requests (legacy behaviour).
func AuthMiddleware(manager *sdkaccess.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if manager == nil {
			c.Next()
			return
		}

		result, err := manager.Authenticate(c.Request.Context(), c.Request)
		if err == nil {
			if result != nil {
				c.Set("userApiKey", result.Principal)
				c.Set("accessProvider", result.Provider)
				if len(result.Metadata) > 0 {
					c.Set("accessMetadata", result.Metadata)
				}
				if user, ok := tenantUserFromAccessResult(result); ok {
					tenancy.SetUserOnGin(c, user)
				}
			}
			c.Next()
			return
		}

		statusCode := err.HTTPStatusCode()
		if statusCode >= http.StatusInternalServerError {
			log.Errorf("authentication middleware error: %v", err)
		}
		c.AbortWithStatusJSON(statusCode, gin.H{"error": err.Message})
	}
}

type userQuotaChecker interface {
	Check(userID string) (allowed bool, retryAfter time.Duration)
}

type credentialValidationChecker interface {
	PreferredAuthIDs(userID string, interval time.Duration) ([]string, error)
}

const quotaFallbackRetryAfterKey = "quotaFallbackRetryAfterSeconds"

func (s *Server) credentialValidationMiddleware() gin.HandlerFunc {
	return CredentialValidationMiddleware(func() *config.Config {
		if s == nil {
			return nil
		}
		return s.cfg
	}, s.tenancyService)
}

// CredentialValidationMiddleware softly prefers one due user-owned credential.
// Missing users, disabled configuration, and bookkeeping failures always pass through.
func CredentialValidationMiddleware(configProvider func() *config.Config, validator credentialValidationChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		var cfg *config.Config
		if configProvider != nil {
			cfg = configProvider()
		}
		if cfg == nil || !cfg.Tenancy.Enabled || validator == nil {
			c.Next()
			return
		}
		interval, enabled := tenancy.ParseValidationInterval(cfg.Tenancy.ValidationInterval)
		if !enabled {
			c.Next()
			return
		}
		user, ok := tenancy.UserFromGin(c)
		if !ok {
			c.Next()
			return
		}

		preferred, errPreferred := validator.PreferredAuthIDs(user.ID, interval)
		if errPreferred != nil {
			log.WithError(errPreferred).
				WithField("user_id", user.ID).
				Warn("tenancy validation: preferred credential lookup failed")
			c.Next()
			return
		}
		if len(preferred) > 0 && c.Request != nil {
			ctx := sdkhandlers.WithPreferredAuthIDs(c.Request.Context(), preferred)
			c.Request = c.Request.WithContext(ctx)
		}
		c.Next()
	}
}

func (s *Server) userQuotaMiddleware() gin.HandlerFunc {
	return UserQuotaMiddleware(func() *config.Config {
		if s == nil {
			return nil
		}
		return s.cfg
	}, s.tenancyService)
}

// UserQuotaMiddleware enforces the resolved tenant's rolling quota. Requests
// authenticated with non-tenant providers have no resolved user and pass
// through unchanged.
func UserQuotaMiddleware(configProvider func() *config.Config, quota userQuotaChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		var cfg *config.Config
		if configProvider != nil {
			cfg = configProvider()
		}
		if cfg == nil || !cfg.Tenancy.Enabled || quota == nil {
			c.Next()
			return
		}

		// Observe-only mode: attribute and record usage, but never deny. The
		// quota check is skipped entirely rather than compared against a very
		// large limit, because Quota.Check fails closed on store errors and
		// observe-only must have zero user impact.
		if !cfg.Tenancy.Quota.Enforce {
			c.Next()
			return
		}

		user, ok := tenancy.UserFromGin(c)
		if !ok {
			c.Next()
			return
		}

		allowed, retryAfter := quota.Check(user.ID)
		if allowed {
			c.Next()
			return
		}
		retryAfterSeconds := int64(math.Ceil(retryAfter.Seconds()))
		if retryAfterSeconds < 1 {
			retryAfterSeconds = 1
		}
		// Home resolves models outside the SDK handler, so it cannot honor the
		// forced fallback marker. Deny instead of serving the original model.
		if cfg.AutoRouting.QuotaFallback &&
			strings.TrimSpace(cfg.AutoRouting.FallbackModel) != "" &&
			!cfg.Home.Enabled && c.Request != nil {
			c.Set(quotaFallbackRetryAfterKey, retryAfterSeconds)
			c.Request = c.Request.WithContext(autoroute.WithForcedFallback(c.Request.Context()))
			c.Next()
			return
		}

		abortUserQuotaExceeded(c, retryAfterSeconds)
	}
}

func rejectForcedQuotaFallback() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil || !autoroute.ForcedFallback(c.Request.Context()) {
			c.Next()
			return
		}
		retryAfterSeconds := int64(1)
		if stored, ok := c.Get(quotaFallbackRetryAfterKey); ok {
			if seconds, valid := stored.(int64); valid && seconds > 0 {
				retryAfterSeconds = seconds
			}
		}
		abortUserQuotaExceeded(c, retryAfterSeconds)
	}
}

func abortUserQuotaExceeded(c *gin.Context, retryAfterSeconds int64) {
	c.Header("Retry-After", strconv.FormatInt(retryAfterSeconds, 10))
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error": "User quota exceeded or quota data is temporarily unavailable",
	})
}

func tenantUserFromAccessResult(result *sdkaccess.Result) (*tenancy.User, bool) {
	if result == nil || result.Provider != useraccess.ProviderName {
		return nil, false
	}
	userID := strings.TrimSpace(result.Metadata["user_id"])
	if userID == "" || userID != strings.TrimSpace(result.Principal) {
		return nil, false
	}
	return &tenancy.User{
		ID:    userID,
		Email: result.Metadata["email"],
		Role:  result.Metadata["role"],
		Tier:  result.Metadata["tier"],
	}, true
}
