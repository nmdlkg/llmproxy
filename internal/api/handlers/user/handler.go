package user

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type oauthHandlers interface {
	RequestAnthropicToken(*gin.Context)
	RequestCodexToken(*gin.Context)
	RequestAntigravityToken(*gin.Context)
	RequestKimiToken(*gin.Context)
	RequestXAIToken(*gin.Context)
	PostOAuthCallback(*gin.Context)
	GetAuthStatus(*gin.Context)
}

// Handler serves tenancy-scoped user operations.
type Handler struct {
	cfg          *config.Config
	authManager  *coreauth.Manager
	service      *tenancy.Service
	tokenStore   coreauth.Store
	oauth        oauthHandlers
	credentialMu sync.Mutex
}

// NewHandler creates the user API handler from the active server services.
func NewHandler(
	cfg *config.Config,
	authManager *coreauth.Manager,
	service *tenancy.Service,
	tokenStore coreauth.Store,
	oauth oauthHandlers,
) *Handler {
	if tokenStore != nil && cfg != nil {
		if baseDirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
			baseDirSetter.SetBaseDir(cfg.AuthDir)
		}
	}
	return &Handler{
		cfg:         cfg,
		authManager: authManager,
		service:     service,
		tokenStore:  tokenStore,
		oauth:       oauth,
	}
}

// RegisterRoutes attaches user routes only when tenancy is enabled.
func (h *Handler) RegisterRoutes(engine *gin.Engine, authMiddleware gin.HandlerFunc) {
	if h == nil || engine == nil || h.cfg == nil || !h.cfg.Tenancy.Enabled || h.service == nil {
		return
	}
	group := engine.Group("/v0/user")
	if authMiddleware != nil {
		group.Use(authMiddleware)
	}
	group.Use(h.requireUser())
	{
		group.GET("/me", h.GetMe)
		group.GET("/credentials", h.ListCredentials)
		group.POST("/credentials", h.UploadCredential)
		group.PATCH("/credentials/:id", h.PatchCredential)
		group.DELETE("/credentials/:id", h.DeleteCredential)

		group.GET("/anthropic-auth-url", h.RequestAnthropicToken)
		group.GET("/claude-auth-url", h.RequestAnthropicToken)
		group.GET("/codex-auth-url", h.RequestCodexToken)
		group.GET("/antigravity-auth-url", h.RequestAntigravityToken)
		group.GET("/kimi-auth-url", h.RequestKimiToken)
		group.GET("/xai-auth-url", h.RequestXAIToken)
		group.POST("/oauth-callback", h.PostOAuthCallback)
		group.GET("/auth-status", h.GetAuthStatus)

		group.GET("/usage", h.GetUsage)
		group.GET("/api-keys", h.ListAPIKeys)
		group.POST("/api-keys", h.IssueAPIKey)
		group.DELETE("/api-keys", h.DeleteAPIKey)

		admin := group.Group("/admin")
		admin.Use(h.requireAdmin())
		{
			admin.GET("/users", h.ListUsers)
			admin.POST("/users", h.CreateUser)
			admin.PATCH("/users/:id", h.UpdateUser)
			admin.DELETE("/users/:id", h.DeleteUser)
		}
	}
}

func (h *Handler) requireUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := tenancy.UserFromGin(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "user authentication required"})
			return
		}
		if h.store() != nil {
			storedUser, errUser := h.store().GetUser(user.ID)
			if errUser != nil {
				if errors.Is(errUser, tenancy.ErrNotFound) {
					c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "user authentication required"})
					return
				}
				log.WithError(errUser).WithField("user_id", user.ID).Error("user API: load authenticated user")
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "user store unavailable"})
				return
			}
			user = storedUser
			tenancy.SetUserOnGin(c, storedUser)
		}
		if user.Disabled {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "user authentication required"})
			return
		}
		role := strings.ToLower(strings.TrimSpace(user.Role))
		if role != tenancy.RoleUser && role != tenancy.RoleAdmin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "user role required"})
			return
		}
		c.Next()
	}
}

func (h *Handler) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := tenancy.UserFromGin(c)
		if !ok || user.Disabled || !strings.EqualFold(strings.TrimSpace(user.Role), tenancy.RoleAdmin) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin role required"})
			return
		}
		c.Next()
	}
}

func currentUser(c *gin.Context) (*tenancy.User, bool) {
	return tenancy.UserFromGin(c)
}

func (h *Handler) store() tenancy.Store {
	if h == nil || h.service == nil {
		return nil
	}
	return h.service.Store()
}

func (h *Handler) invalidateQuota(userID string) {
	if h == nil || h.service == nil || h.service.Quota() == nil {
		return
	}
	h.service.Quota().Invalidate(userID)
}
