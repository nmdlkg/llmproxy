package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/userpanelasset"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
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

// UserPanelAsset is the small manager surface needed by the admin UI routes.
// Keeping this interface here avoids coupling the tenancy handler to updater
// internals and makes route tests deterministic.
type UserPanelAsset interface {
	Status() userpanelasset.Status
	Refresh(context.Context) error
}

// Handler serves tenancy-scoped user operations.
type Handler struct {
	cfg             *config.Config
	authManager     *coreauth.Manager
	service         *tenancy.Service
	tokenStore      coreauth.Store
	oauth           oauthHandlers
	panel           UserPanelAsset
	credentialMu    sync.Mutex
	quotaMu         sync.Mutex
	quotaCache      map[string]providerQuotaCacheEntry
	quotaFetches    singleflight.Group
	quotaSlotMu     sync.Mutex
	quotaFetchSlots chan struct{}
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
		cfg:             cfg,
		authManager:     authManager,
		service:         service,
		tokenStore:      tokenStore,
		oauth:           oauth,
		quotaCache:      make(map[string]providerQuotaCacheEntry),
		quotaFetchSlots: make(chan struct{}, providerQuotaParallel),
	}
}

// SetConfig updates the live config snapshot used by user operations after a
// server hot reload.
func (h *Handler) SetConfig(cfg *config.Config) {
	if h == nil || cfg == nil {
		return
	}
	h.cfg = cfg
	if h.tokenStore != nil {
		if baseDirSetter, ok := h.tokenStore.(interface{ SetBaseDir(string) }); ok {
			baseDirSetter.SetBaseDir(cfg.AuthDir)
		}
	}
}

// SetUserPanelAsset attaches the instance-owned panel manager to the admin
// routes. Passing nil leaves the routes absent.
func (h *Handler) SetUserPanelAsset(panel UserPanelAsset) {
	if h == nil {
		return
	}
	h.panel = panel
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
		group.GET("/provider-quotas", h.GetProviderQuotas)
		group.GET("/api-keys", h.ListAPIKeys)
		group.POST("/api-keys", h.RegisterAPIKeyHash)
		group.DELETE("/api-keys", h.DeleteAPIKey)
		group.DELETE("/api-keys/:hash", h.DeleteAPIKey)

		admin := group.Group("/admin")
		admin.Use(h.requireAdmin())
		{
			admin.GET("/users", h.ListUsers)
			admin.POST("/users", h.CreateUser)
			admin.PATCH("/users/:id", h.UpdateUser)
			admin.DELETE("/users/:id", h.DeleteUser)
			admin.DELETE("/users/:id/api-keys/:hash", h.RevokeUserAPIKey)
			admin.GET("/usage", h.ListUsageByUser)
			if h.panel != nil {
				admin.GET("/ui/status", h.GetUserPanelUIStatus)
				admin.POST("/ui/refresh", h.RefreshUserPanelUI)
			}
		}
	}
}

// GetUserPanelUIStatus returns local updater state without network I/O.
func (h *Handler) GetUserPanelUIStatus(c *gin.Context) {
	if h == nil || h.panel == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.JSON(http.StatusOK, h.panel.Status())
}

// RefreshUserPanelUI manually requests a signed panel update. This endpoint is
// deliberately strict: the only accepted request body is the JSON object {}.
func (h *Handler) RefreshUserPanelUI(c *gin.Context) {
	if h == nil || h.panel == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if errAuth := requireBearerAuth(c); errAuth != nil {
		c.Header("WWW-Authenticate", "Bearer")
		c.JSON(http.StatusUnauthorized, gin.H{"error": errAuth.Error()})
		return
	}
	if errContentType := requireJSONContentType(c); errContentType != nil {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": errContentType.Error()})
		return
	}
	if errBody := requireEmptyJSONBody(c); errBody != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errBody.Error()})
		return
	}
	errRefresh := h.panel.Refresh(c.Request.Context())
	if errRefresh == nil {
		c.JSON(http.StatusOK, h.panel.Status())
		return
	}
	if errors.Is(errRefresh, userpanelasset.ErrRefreshThrottled) {
		retryAfter := userpanelasset.RetryAfter(errRefresh)
		seconds := int((retryAfter + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		c.Header("Retry-After", strconv.Itoa(seconds))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": errRefresh.Error(), "retry_after_seconds": seconds})
		return
	}
	if errors.Is(errRefresh, userpanelasset.ErrRefreshBusy) {
		c.JSON(http.StatusConflict, gin.H{"error": errRefresh.Error()})
		return
	}
	if errors.Is(errRefresh, userpanelasset.ErrManualRefreshRequiresDevMode) || errors.Is(errRefresh, userpanelasset.ErrDevOverride) {
		c.JSON(http.StatusConflict, gin.H{"error": errRefresh.Error()})
		return
	}
	log.WithError(errRefresh).Warn("user panel manual refresh failed")
	c.JSON(http.StatusBadGateway, gin.H{"error": "user panel refresh failed"})
}

func requireBearerAuth(c *gin.Context) error {
	if c == nil || c.Request == nil {
		return errors.New("request must use Bearer authentication")
	}
	authorization := strings.Fields(c.GetHeader("Authorization"))
	if len(authorization) != 2 || !strings.EqualFold(authorization[0], "Bearer") || strings.TrimSpace(authorization[1]) == "" {
		return errors.New("request must use Bearer authentication")
	}
	return nil
}

func requireJSONContentType(c *gin.Context) error {
	if c == nil || c.Request == nil {
		return errors.New("request Content-Type must be application/json")
	}
	mediaType, _, errMedia := mime.ParseMediaType(c.ContentType())
	if errMedia != nil || !strings.EqualFold(mediaType, "application/json") {
		return errors.New("request Content-Type must be application/json")
	}
	return nil
}

func requireEmptyJSONBody(c *gin.Context) error {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return errors.New("request body must be JSON {}")
	}
	limited := io.LimitReader(c.Request.Body, 4097)
	body, errRead := io.ReadAll(limited)
	if errRead != nil || len(body) > 4096 {
		return errors.New("request body must be JSON {}")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload map[string]json.RawMessage
	if errDecode := decoder.Decode(&payload); errDecode != nil || payload == nil {
		if errDecode == nil {
			return errors.New("request body must be JSON {}")
		}
		return errors.New("request body must be JSON {}")
	}
	if len(payload) != 0 {
		return errors.New("request body must be JSON {}")
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); !errors.Is(errTrailing, io.EOF) {
		return errors.New("request body must be JSON {}")
	}
	return nil
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
