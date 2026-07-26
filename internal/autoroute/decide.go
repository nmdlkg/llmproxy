package autoroute

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

type forcedFallbackContextKey struct{}

// WithForcedFallback marks a request for direct routing to the configured fallback model.
func WithForcedFallback(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, forcedFallbackContextKey{}, true)
}

// ForcedFallback reports whether quota handling requested the fallback model.
func ForcedFallback(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	forced, _ := ctx.Value(forcedFallbackContextKey{}).(bool)
	return forced
}

// ModelAvailable reports whether a configured model has a live registration.
type ModelAvailable func(string) bool

// Decide maps a valid classification to the first live model in its configured tier.
// It is pure: callers provide the liveness snapshot through modelAvailable.
func Decide(cfg config.AutoRoutingConfig, classification Classification, forcedFallback bool, modelAvailable ModelAvailable) string {
	if modelAvailable == nil {
		return ""
	}
	if forcedFallback {
		return availableFallback(cfg, modelAvailable)
	}

	tierName := strings.ToLower(strings.TrimSpace(cfg.DefaultTier))
	category := strings.ToLower(strings.TrimSpace(classification.Category))
	if classification.Valid && category != "" && classification.Confidence >= cfg.MinConfidence {
		if mappedTier := strings.ToLower(strings.TrimSpace(cfg.Categories[category])); mappedTier != "" {
			tierName = mappedTier
		}
	}

	if tier, ok := cfg.Tiers[tierName]; ok {
		for _, model := range tier.Models {
			model = strings.TrimSpace(model)
			if model != "" && modelAvailable(model) {
				return model
			}
		}
	}
	return availableFallback(cfg, modelAvailable)
}

func availableFallback(cfg config.AutoRoutingConfig, modelAvailable ModelAvailable) string {
	fallback := strings.TrimSpace(cfg.FallbackModel)
	if fallback != "" && modelAvailable(fallback) {
		return fallback
	}
	return ""
}

type decisionCacheEntry struct {
	model     string
	configKey [32]byte
	expiresAt time.Time
}

type decisionCache struct {
	mu      sync.Mutex
	entries map[[32]byte]decisionCacheEntry
	now     func() time.Time
}

func newDecisionCache(now func() time.Time) *decisionCache {
	if now == nil {
		now = time.Now
	}
	return &decisionCache{
		entries: make(map[[32]byte]decisionCacheEntry),
		now:     now,
	}
}

func (c *decisionCache) get(text string, configKey [32]byte, modelAvailable ModelAvailable) (string, bool) {
	if c == nil {
		return "", false
	}
	key := sha256.Sum256([]byte(text))
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if entry.configKey != configKey || !c.now().Before(entry.expiresAt) || !modelAvailable(entry.model) {
		delete(c.entries, key)
		return "", false
	}
	return entry.model, true
}

func (c *decisionCache) put(text string, configKey [32]byte, model string, ttl time.Duration) {
	if c == nil || ttl <= 0 || model == "" {
		return
	}
	key := sha256.Sum256([]byte(text))
	c.mu.Lock()
	now := c.now()
	for cachedKey, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, cachedKey)
		}
	}
	c.entries[key] = decisionCacheEntry{
		model:     model,
		configKey: configKey,
		expiresAt: now.Add(ttl),
	}
	c.mu.Unlock()
}

type classifyFunc func(context.Context, config.AutoRoutingConfig, *config.SDKConfig, string) Classification

// Resolver adds classification and TTL caching around the pure decision function.
type Resolver struct {
	cache          *decisionCache
	classify       classifyFunc
	modelAvailable ModelAvailable
}

// NewResolver creates a semantic auto-routing resolver backed by the global model registry.
func NewResolver() *Resolver {
	return &Resolver{
		cache: newDecisionCache(time.Now),
		classify: func(ctx context.Context, cfg config.AutoRoutingConfig, proxyCfg *config.SDKConfig, text string) Classification {
			return NewClient(ctx, cfg, proxyCfg).Classify(ctx, text)
		},
		modelAvailable: globalModelAvailable,
	}
}

// Resolve classifies text and returns a concrete live model, or an empty string when
// routing should continue through the legacy auto-model path.
func (r *Resolver) Resolve(ctx context.Context, cfg config.AutoRoutingConfig, proxyCfg *config.SDKConfig, text string) string {
	if r == nil || !cfg.Enabled || r.classify == nil || r.modelAvailable == nil {
		return ""
	}
	if ForcedFallback(ctx) {
		return Decide(cfg, Classification{}, true, r.modelAvailable)
	}
	if strings.TrimSpace(text) == "" {
		return ""
	}

	configKey := routingConfigKey(cfg)
	if cached, ok := r.cache.get(text, configKey, r.modelAvailable); ok {
		return cached
	}

	classification := r.classify(ctx, cfg, proxyCfg, text)
	if !classification.Valid {
		return ""
	}
	model := Decide(cfg, classification, false, r.modelAvailable)
	r.cache.put(text, configKey, model, time.Duration(cfg.CacheTTLSeconds)*time.Second)
	return model
}

func routingConfigKey(cfg config.AutoRoutingConfig) [32]byte {
	encoded, errMarshal := json.Marshal(cfg)
	if errMarshal != nil {
		return [32]byte{}
	}
	return sha256.Sum256(encoded)
}

func globalModelAvailable(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, available := range registry.GetGlobalRegistry().GetAvailableModels("") {
		id, ok := available["id"].(string)
		if ok && id == model {
			return true
		}
	}
	return false
}

var defaultResolver = NewResolver()

// Resolve uses the process-wide resolver so repeated prompts share the TTL cache.
func Resolve(ctx context.Context, cfg config.AutoRoutingConfig, proxyCfg *config.SDKConfig, text string) string {
	return defaultResolver.Resolve(ctx, cfg, proxyCfg, text)
}
