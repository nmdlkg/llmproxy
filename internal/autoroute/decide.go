package autoroute

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/openrouter"
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

// ModelRanker reorders tier candidates best-first. Returning nil or an empty
// slice means "no opinion"; the caller then uses configured order.
type ModelRanker func(category string, candidates []string) []string

// Decide maps a valid classification to the first live model in its configured tier.
// It is pure: callers provide the liveness snapshot through modelAvailable.
func Decide(cfg config.AutoRoutingConfig, classification Classification, forcedFallback bool, modelAvailable ModelAvailable, rankers ...ModelRanker) string {
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
		var ranker ModelRanker
		if len(rankers) > 0 {
			ranker = rankers[0]
		}
		if rankedModel := firstAvailableRankedModel(category, tier.Models, modelAvailable, ranker); rankedModel != "" {
			return rankedModel
		}
		for _, model := range tier.Models {
			model = strings.TrimSpace(model)
			if model != "" && modelAvailable(model) {
				return model
			}
		}
	}
	return availableFallback(cfg, modelAvailable)
}

func firstAvailableRankedModel(category string, configured []string, modelAvailable ModelAvailable, ranker ModelRanker) string {
	if ranker == nil || len(configured) == 0 {
		return ""
	}

	allowed := make(map[string]struct{}, len(configured))
	for _, model := range configured {
		if model = strings.TrimSpace(model); model != "" {
			allowed[model] = struct{}{}
		}
	}
	ranked := ranker(category, append([]string(nil), configured...))
	for _, model := range ranked {
		model = strings.TrimSpace(model)
		if _, ok := allowed[model]; !ok {
			continue
		}
		if modelAvailable(model) {
			return model
		}
	}
	return ""
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

func modelRanker(proxyCfg *config.SDKConfig) ModelRanker {
	if proxyCfg == nil {
		return nil
	}
	return openRouterModelRanker(proxyCfg.OpenRouter, openrouter.CurrentSnapshot())
}

func openRouterModelRanker(cfg config.OpenRouterConfig, snapshot openrouter.Snapshot) ModelRanker {
	// openrouter.enabled is the master switch: when it is false the catalog is
	// documented as fully inert, so benchmark ranking must not silently reorder
	// candidates from the embedded snapshot.
	if !cfg.Enabled {
		return nil
	}
	source := openrouter.BenchmarkSource(strings.ToLower(strings.TrimSpace(cfg.BenchmarkSource)))
	switch source {
	case openrouter.BenchmarkArtificialAnalysis, openrouter.BenchmarkDesignArena:
	default:
		return nil
	}

	perDollarOptions := openrouter.RankOptions{
		Mode:     openrouter.RankByScorePerDollar,
		Source:   source,
		ModelMap: cfg.ModelMap,
	}
	rawOptions := perDollarOptions
	rawOptions.Mode = openrouter.RankByRawScore

	return func(category string, candidates []string) []string {
		task := taskTypeForCategory(category)
		ranked := openrouter.RankModels(task, candidates, snapshot, perDollarOptions)
		if !hasKnownRankingScore(ranked) {
			ranked = openrouter.RankModels(task, candidates, snapshot, rawOptions)
		}
		if !hasKnownRankingScore(ranked) {
			return nil
		}

		models := make([]string, 0, len(ranked))
		for _, model := range ranked {
			models = append(models, model.LocalModel)
		}
		return models
	}
}

func hasKnownRankingScore(ranked []openrouter.RankedModel) bool {
	for _, model := range ranked {
		if model.RankingScore.Known {
			return true
		}
	}
	return false
}

func taskTypeForCategory(category string) openrouter.TaskType {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "agent", "agents", "agentic", "tool-use", "tool_use", "tool use":
		return openrouter.TaskAgentic
	case "code", "coding", "programming", "software", "software-development", "software_development",
		"software development", "computer-science", "computer_science", "computer science", "debug", "debugging":
		return openrouter.TaskCoding
	default:
		return openrouter.TaskIntelligence
	}
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
		return Decide(cfg, Classification{}, true, r.modelAvailable, nil)
	}
	if strings.TrimSpace(text) == "" {
		return ""
	}

	configKey := routingConfigKey(cfg, proxyCfg)
	if cached, ok := r.cache.get(text, configKey, r.modelAvailable); ok {
		return cached
	}

	classification := r.classify(ctx, cfg, proxyCfg, text)
	if !classification.Valid {
		return ""
	}
	model := Decide(cfg, classification, false, r.modelAvailable, modelRanker(proxyCfg))
	r.cache.put(text, configKey, model, time.Duration(cfg.CacheTTLSeconds)*time.Second)
	return model
}

func routingConfigKey(cfg config.AutoRoutingConfig, proxyCfg *config.SDKConfig) [32]byte {
	routingConfig := struct {
		AutoRouting     config.AutoRoutingConfig `json:"auto_routing"`
		BenchmarkSource string                   `json:"benchmark_source,omitempty"`
		ModelMap        map[string]string        `json:"model_map,omitempty"`
	}{
		AutoRouting: cfg,
	}
	if proxyCfg != nil {
		routingConfig.BenchmarkSource = proxyCfg.OpenRouter.BenchmarkSource
		routingConfig.ModelMap = proxyCfg.OpenRouter.ModelMap
	}
	encoded, errMarshal := json.Marshal(routingConfig)
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
