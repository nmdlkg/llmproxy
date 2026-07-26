package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	ginUserKey = "tenancyUser"

	// GinUserIDKey is the stable tenant ID stored on authenticated Gin requests.
	GinUserIDKey = "userID"
)

type userContextKey struct{}

// Service owns the tenancy store, quota calculator, and durable usage sink.
type Service struct {
	store       Store
	quota       *Quota
	usagePlugin *UsagePlugin
	usageSink   *serviceUsageSink

	closeOnce sync.Once
	closeErr  error
}

// NewService starts tenancy persistence and accounting when enabled. Disabled
// tenancy returns a nil service without opening a database or starting a
// goroutine.
func NewService(cfg config.TenancyConfig, authDir string, authManager *coreauth.Manager) (*Service, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	store, errOpen := OpenSQLite(cfg, authDir)
	if errOpen != nil {
		return nil, fmt.Errorf("tenancy service: open store: %w", errOpen)
	}

	quota, errQuota := NewQuota(store, cfg.Quota, credentialResolver(authManager))
	if errQuota != nil {
		if errClose := store.Close(); errClose != nil {
			return nil, errors.Join(
				fmt.Errorf("tenancy service: create quota: %w", errQuota),
				fmt.Errorf("tenancy service: close store after quota failure: %w", errClose),
			)
		}
		return nil, fmt.Errorf("tenancy service: create quota: %w", errQuota)
	}

	usagePlugin := NewUsagePlugin(store, cfg, withContextUser(ResolveAPIKeyUser(store)))
	usageSink := &serviceUsageSink{plugin: usagePlugin}
	usage.RegisterPlugin(usageSink)

	// TODO: Do not install Balancer through Manager.SetPriorityResolver here.
	// The scheduler invokes that resolver while holding its mutex, so the
	// resolver must use precomputed state and must never call authManager.List.
	return &Service{
		store:       store,
		quota:       quota,
		usagePlugin: usagePlugin,
		usageSink:   usageSink,
	}, nil
}

// Store returns the active tenancy persistence surface.
func (s *Service) Store() Store {
	if s == nil {
		return nil
	}
	return s.store
}

// Quota returns the active quota calculator.
func (s *Service) Quota() *Quota {
	if s == nil {
		return nil
	}
	return s.quota
}

// Check delegates to Quota.Check and denies when an enabled service is
// unexpectedly missing its quota calculator.
func (s *Service) Check(userID string) (bool, time.Duration) {
	if s == nil || s.quota == nil {
		return false, quotaStoreErrorRetryAfter
	}
	return s.quota.Check(userID)
}

// Close flushes usage before closing the tenancy store.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		var closeErrors []error
		usagePlugin := s.usagePlugin
		if s.usageSink != nil {
			usagePlugin = s.usageSink.detach()
		}
		if usagePlugin != nil {
			if errClosePlugin := usagePlugin.Close(); errClosePlugin != nil {
				closeErrors = append(closeErrors, fmt.Errorf("tenancy service: close usage plugin: %w", errClosePlugin))
			}
		}
		if s.store != nil {
			if errCloseStore := s.store.Close(); errCloseStore != nil {
				closeErrors = append(closeErrors, fmt.Errorf("tenancy service: close store: %w", errCloseStore))
			}
		}
		s.closeErr = errors.Join(closeErrors...)
	})
	return s.closeErr
}

type serviceUsageSink struct {
	mu     sync.RWMutex
	plugin *UsagePlugin
}

func withContextUser(fallback UserResolver) UserResolver {
	return func(ctx context.Context, record usage.Record) (*User, error) {
		if user, ok := UserFromContext(ctx); ok {
			return user, nil
		}
		if ctx != nil {
			if ginContext, ok := ctx.Value("gin").(*gin.Context); ok {
				if user, found := UserFromGin(ginContext); found {
					return user, nil
				}
			}
		}
		return fallback(ctx, record)
	}
}

func (s *serviceUsageSink) HandleUsage(ctx context.Context, record usage.Record) {
	if s == nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.plugin != nil {
		s.plugin.HandleUsage(ctx, record)
	}
}

func (s *serviceUsageSink) detach() *UsagePlugin {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	plugin := s.plugin
	s.plugin = nil
	s.mu.Unlock()
	return plugin
}

func credentialResolver(authManager *coreauth.Manager) CredentialResolver {
	return func(userID string) ([]Credential, error) {
		if authManager == nil {
			return nil, nil
		}
		userID = strings.TrimSpace(userID)
		if userID == "" {
			return nil, nil
		}

		auths := authManager.List()
		credentials := make([]Credential, 0)
		for _, auth := range auths {
			if auth == nil || auth.Disabled || auth.Attributes == nil {
				continue
			}
			if strings.TrimSpace(auth.Attributes["owner_user_id"]) != userID {
				continue
			}
			shared, errShared := strconv.ParseBool(strings.TrimSpace(auth.Attributes["shared"]))
			if errShared != nil || !shared {
				continue
			}
			planTier := strings.TrimSpace(auth.Attributes["plan_type"])
			if planTier == "" {
				planTier = strings.TrimSpace(auth.Attributes["contribution_tier"])
			}
			if planTier == "" {
				planTier = "default"
			}
			credentials = append(credentials, Credential{
				AuthID:   auth.ID,
				Provider: auth.Provider,
				PlanTier: planTier,
			})
		}
		return credentials, nil
	}
}

// APIKeyRevoked reports whether a key hash exists in a revoked state. Active
// lookup intentionally excludes revoked keys, so the access provider uses this
// narrow helper to distinguish revocation from an unknown credential.
func APIKeyRevoked(store Store, hash string) (bool, error) {
	if store == nil {
		return false, fmt.Errorf("tenancy: store is nil")
	}
	checker, ok := store.(interface {
		APIKeyRevoked(string) (bool, error)
	})
	if !ok {
		return false, nil
	}
	return checker.APIKeyRevoked(hash)
}

// APIKeyRevoked reports whether the SQLite key row is revoked.
func (s *SQLiteStore) APIKeyRevoked(hash string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("tenancy sqlite: not initialized")
	}
	hash = strings.ToLower(strings.TrimSpace(hash))
	var revokedAt sql.NullInt64
	errScan := s.db.QueryRow(
		`SELECT revoked_at FROM user_api_keys WHERE key_hash = ?`,
		hash,
	).Scan(&revokedAt)
	if errors.Is(errScan, sql.ErrNoRows) {
		return false, nil
	}
	if errScan != nil {
		return false, fmt.Errorf("tenancy sqlite: inspect API key revocation: %w", errScan)
	}
	return revokedAt.Valid, nil
}

// WithUser adds a resolved tenant to a request context.
func WithUser(ctx context.Context, user *User) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return ctx
	}
	return context.WithValue(ctx, userContextKey{}, user)
}

// UserFromContext returns the tenant attached to a request context.
func UserFromContext(ctx context.Context) (*User, bool) {
	if ctx == nil {
		return nil, false
	}
	user, ok := ctx.Value(userContextKey{}).(*User)
	return user, ok && user != nil && strings.TrimSpace(user.ID) != ""
}

// SetUserOnGin stores a resolved tenant on Gin and its underlying request
// context so non-Gin layers can read the same stable identity.
func SetUserOnGin(c *gin.Context, user *User) {
	if c == nil || user == nil || strings.TrimSpace(user.ID) == "" {
		return
	}
	c.Set(ginUserKey, user)
	c.Set(GinUserIDKey, user.ID)
	if c.Request != nil {
		c.Request = c.Request.WithContext(WithUser(c.Request.Context(), user))
	}
}

// UserFromGin returns the tenant resolved by inbound authentication.
func UserFromGin(c *gin.Context) (*User, bool) {
	if c == nil {
		return nil, false
	}
	if value, exists := c.Get(ginUserKey); exists {
		if user, ok := value.(*User); ok && user != nil && strings.TrimSpace(user.ID) != "" {
			return user, true
		}
	}
	if c.Request == nil {
		return nil, false
	}
	return UserFromContext(c.Request.Context())
}
