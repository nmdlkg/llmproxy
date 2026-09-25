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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
)

// Fork tenancy request policy: tenant resolution after authentication,
// credential validation preference, quota enforcement, and forced quota
// fallback. Kept out of server_middleware.go and server_routes.go so upstream
// middleware and route edits do not conflict with fork policy.

// quotaFallbackUnsupportedRoutes lists group routes whose handlers cannot honor
// the forced quota fallback model. They fail closed with 429 instead.
var quotaFallbackUnsupportedRoutes = map[string]struct{}{
	"/v1/alpha/search":                {},
	"/v1/live":                        {},
	"/v1/live/:call_id":               {},
	"/backend-api/codex/alpha/search": {},
}

// tenantPolicy returns the tenancy middleware chain installed on every API
// route group after authentication: credential validation, quota, and the
// fail-closed guard for routes that cannot apply the quota fallback model.
func (s *Server) tenantPolicy() []gin.HandlerFunc {
	return []gin.HandlerFunc{
		s.credentialValidationMiddleware(),
		s.userQuotaMiddleware(),
		rejectForcedQuotaFallbackOnRoutes(quotaFallbackUnsupportedRoutes),
	}
}

// tenantChain builds the handler chain for a directly registered realtime or
// live route: authentication, tenancy policy, then the handler. Every such route
// cannot apply the quota fallback model, so the forced fallback always fails closed.
func (s *Server) tenantChain(authMiddleware gin.HandlerFunc, handler gin.HandlerFunc) []gin.HandlerFunc {
	return []gin.HandlerFunc{
		authMiddleware,
		s.credentialValidationMiddleware(),
		s.userQuotaMiddleware(),
		rejectForcedQuotaFallback(),
		handler,
	}
}

// rejectForcedQuotaFallbackOnRoutes applies rejectForcedQuotaFallback only to
// the listed gin route patterns.
func rejectForcedQuotaFallbackOnRoutes(routes map[string]struct{}) gin.HandlerFunc {
	reject := rejectForcedQuotaFallback()
	return func(c *gin.Context) {
		if _, unsupported := routes[c.FullPath()]; unsupported {
			reject(c)
			return
		}
		c.Next()
	}
}

// setTenantUserFromAccessResult records the resolved tenant on the gin context
// when the request was authenticated by the tenant access provider.
func setTenantUserFromAccessResult(c *gin.Context, result *sdkaccess.Result) {
	if user, ok := tenantUserFromAccessResult(result); ok {
		tenancy.SetUserOnGin(c, user)
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
	}, s.fork.tenancy())
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
	}, s.fork.tenancy())
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
