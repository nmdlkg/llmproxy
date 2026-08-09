package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/otelusage"
	sdkpluginstore "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
	log "github.com/sirupsen/logrus"
)

// NormalizePluginsConfig applies default plugin configuration values.
func (cfg *Config) NormalizePluginsConfig() {
	if cfg == nil {
		return
	}
	cfg.Plugins.Dir = strings.TrimSpace(cfg.Plugins.Dir)
	if cfg.Plugins.Dir == "" {
		cfg.Plugins.Dir = defaultPluginsDir
	}
	if len(cfg.Plugins.StoreSources) > 0 {
		sources := make([]string, 0, len(cfg.Plugins.StoreSources))
		for _, source := range cfg.Plugins.StoreSources {
			source = strings.TrimSpace(source)
			if source == "" {
				continue
			}
			sources = append(sources, source)
		}
		cfg.Plugins.StoreSources = sources
	}
	cfg.Plugins.StoreAuth = sdkpluginstore.NormalizeAuthConfigs(cfg.Plugins.StoreAuth)
	if cfg.Plugins.Configs == nil {
		cfg.Plugins.Configs = map[string]PluginInstanceConfig{}
	}
}

// SanitizeCodexHeaderDefaults trims surrounding whitespace from the
// configured Codex header fallback values.
func (cfg *Config) SanitizeCodexHeaderDefaults() {
	if cfg == nil {
		return
	}
	cfg.CodexHeaderDefaults.UserAgent = strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent)
	cfg.CodexHeaderDefaults.BetaFeatures = strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
}

// SanitizeClaudeHeaderDefaults trims surrounding whitespace from the
// configured Claude fingerprint baseline values.
func (cfg *Config) SanitizeClaudeHeaderDefaults() {
	if cfg == nil {
		return
	}
	cfg.ClaudeHeaderDefaults.UserAgent = strings.TrimSpace(cfg.ClaudeHeaderDefaults.UserAgent)
	cfg.ClaudeHeaderDefaults.PackageVersion = strings.TrimSpace(cfg.ClaudeHeaderDefaults.PackageVersion)
	cfg.ClaudeHeaderDefaults.RuntimeVersion = strings.TrimSpace(cfg.ClaudeHeaderDefaults.RuntimeVersion)
	cfg.ClaudeHeaderDefaults.OS = strings.TrimSpace(cfg.ClaudeHeaderDefaults.OS)
	cfg.ClaudeHeaderDefaults.Arch = strings.TrimSpace(cfg.ClaudeHeaderDefaults.Arch)
	cfg.ClaudeHeaderDefaults.Timeout = strings.TrimSpace(cfg.ClaudeHeaderDefaults.Timeout)
}

// SanitizeOAuthModelAlias normalizes and deduplicates global OAuth model name aliases.
// It trims whitespace, normalizes channel keys to lower-case, drops empty entries,
// allows multiple aliases per upstream name, and ensures aliases are unique within each channel.
func (cfg *Config) SanitizeOAuthModelAlias() {
	if cfg == nil || len(cfg.OAuthModelAlias) == 0 {
		return
	}
	out := make(map[string][]OAuthModelAlias, len(cfg.OAuthModelAlias))
	for rawChannel, aliases := range cfg.OAuthModelAlias {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" || len(aliases) == 0 {
			continue
		}
		seenAlias := make(map[string]struct{}, len(aliases))
		clean := make([]OAuthModelAlias, 0, len(aliases))
		for _, entry := range aliases {
			name := strings.TrimSpace(entry.Name)
			alias := strings.TrimSpace(entry.Alias)
			if name == "" || alias == "" {
				continue
			}
			if strings.EqualFold(name, alias) {
				continue
			}
			aliasKey := strings.ToLower(alias)
			if _, ok := seenAlias[aliasKey]; ok {
				continue
			}
			seenAlias[aliasKey] = struct{}{}
			clean = append(clean, OAuthModelAlias{
				Name:         name,
				Alias:        alias,
				Fork:         entry.Fork,
				DisplayName:  strings.TrimSpace(entry.DisplayName),
				ForceMapping: entry.ForceMapping,
			})
		}
		if len(clean) > 0 {
			out[channel] = clean
		}
	}
	cfg.OAuthModelAlias = out
}

// SanitizeOpenAICompatibility removes OpenAI-compatibility provider entries that are
// not actionable, specifically those missing a BaseURL. It trims whitespace before
// evaluation and preserves the relative order of remaining entries.
func (cfg *Config) SanitizeOpenAICompatibility() {
	if cfg == nil || len(cfg.OpenAICompatibility) == 0 {
		return
	}
	out := make([]OpenAICompatibility, 0, len(cfg.OpenAICompatibility))
	for i := range cfg.OpenAICompatibility {
		e := cfg.OpenAICompatibility[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Prefix = normalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.Headers = NormalizeHeaders(e.Headers)
		if e.BaseURL == "" {
			// Skip providers with no base-url; treated as removed
			continue
		}
		out = append(out, e)
	}
	cfg.OpenAICompatibility = out
}

// SanitizeCodexKeys removes Codex API key entries missing a BaseURL.
// It trims whitespace and preserves order for remaining entries.
func (cfg *Config) SanitizeCodexKeys() {
	if cfg == nil {
		return
	}
	cfg.CodexKey = sanitizeCodexKeyEntries(cfg.CodexKey)
}

// SanitizeXAIKeys removes xAI API key entries missing a BaseURL.
// It applies the same normalization rules as codex-api-key.
func (cfg *Config) SanitizeXAIKeys() {
	if cfg == nil {
		return
	}
	cfg.XAIKey = sanitizeCodexKeyEntries(cfg.XAIKey)
}

func sanitizeCodexKeyEntries(entries []CodexKey) []CodexKey {
	if len(entries) == 0 {
		return entries
	}
	out := make([]CodexKey, 0, len(entries))
	for i := range entries {
		e := entries[i]
		e.Prefix = normalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.Headers = NormalizeHeaders(e.Headers)
		e.ExcludedModels = NormalizeExcludedModels(e.ExcludedModels)
		if e.BaseURL == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// SanitizeClaudeKeys normalizes headers for Claude credentials.
func (cfg *Config) SanitizeClaudeKeys() {
	if cfg == nil || len(cfg.ClaudeKey) == 0 {
		return
	}
	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
	}
}

func sanitizeGeminiKeyEntries(entries []GeminiKey) []GeminiKey {
	seen := make(map[string]struct{}, len(entries))
	out := entries[:0]
	for i := range entries {
		entry := entries[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		uniqueKey := entry.APIKey + "|" + entry.BaseURL
		if _, exists := seen[uniqueKey]; exists {
			continue
		}
		seen[uniqueKey] = struct{}{}
		out = append(out, entry)
	}
	return out
}

// SanitizeGeminiKeys deduplicates and normalizes Gemini credentials.
// It uses API key + base URL as the uniqueness key.
func (cfg *Config) SanitizeGeminiKeys() {
	if cfg == nil {
		return
	}
	cfg.GeminiKey = sanitizeGeminiKeyEntries(cfg.GeminiKey)
}

// SanitizeInteractionsKeys deduplicates and normalizes native Interactions credentials.
// It uses API key + base URL as the uniqueness key.
func (cfg *Config) SanitizeInteractionsKeys() {
	if cfg == nil {
		return
	}
	cfg.InteractionsKey = sanitizeGeminiKeyEntries(cfg.InteractionsKey)
}

func normalizeModelPrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "/") {
		return ""
	}
	return trimmed
}

// NormalizeHeaders trims header keys and values and removes empty pairs.
func NormalizeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	clean := make(map[string]string, len(headers))
	for k, v := range headers {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			continue
		}
		clean[key] = val
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

// NormalizeExcludedModels trims, lowercases, and deduplicates model exclusion patterns.
// It preserves the order of first occurrences and drops empty entries.
func NormalizeExcludedModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, raw := range models {
		trimmed := strings.ToLower(strings.TrimSpace(raw))
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeOAuthExcludedModels cleans provider -> excluded models mappings by normalizing provider keys
// and applying model exclusion normalization to each entry.
func NormalizeOAuthExcludedModels(entries map[string][]string) map[string][]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]string, len(entries))
	for provider, models := range entries {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			continue
		}
		normalized := NormalizeExcludedModels(models)
		if len(normalized) == 0 {
			continue
		}
		out[key] = normalized
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Tenancy, auto-routing, and OpenRouter defaults. Kept here so both the file
// loader and the byte parser converge on identical values.
const (
	defaultTenancyValidationInterval = "1h"
	// DefaultTenancyQuotaWindow is one week. Go's duration parser has no day
	// unit, so this must be expressed in hours. Exported so the tenancy package
	// uses the same value for its own fallback and the two cannot drift.
	DefaultTenancyQuotaWindow    = "168h"
	defaultTenancyUrgencyHorizon = "30m"
	defaultTenancyHighWater      = 0.9
	defaultTenancyUrgencyBonus   = 1

	defaultAutoRoutingTimeoutMS       = 800
	defaultAutoRoutingMinConfidence   = 0.5
	defaultAutoRoutingCacheTTLSeconds = 300
	defaultAutoRoutingMaxPromptChars  = 4000

	defaultOpenRouterBaseURL         = "https://openrouter.ai/api/v1"
	defaultOpenRouterRefreshInterval = "6h"
	defaultOpenRouterCostBasis       = "static"
)

// SanitizeOTelConfig trims OTLP settings, applies safe defaults, and rejects
// enabled configurations that cannot export. It does not synthesize an absent
// section because absence enables the deprecated environment fallback.
func (cfg *Config) SanitizeOTelConfig() error {
	if cfg == nil || cfg.OTel == nil {
		return nil
	}

	o := cfg.OTel
	o.Endpoint = strings.TrimSpace(o.Endpoint)
	o.ExportInterval = strings.TrimSpace(o.ExportInterval)
	o.ServiceName = strings.TrimSpace(o.ServiceName)
	o.ServiceVersion = strings.TrimSpace(o.ServiceVersion)
	o.Environment = strings.TrimSpace(o.Environment)

	switch interval, errParse := time.ParseDuration(o.ExportInterval); {
	case o.ExportInterval == "", errParse != nil:
		o.ExportInterval = otelusage.DefaultExportInterval.String()
	case interval <= 0:
		return fmt.Errorf("otel.export-interval must be positive")
	}
	if o.ServiceName == "" {
		o.ServiceName = otelusage.DefaultServiceName
	}
	if o.Enabled && o.Endpoint == "" {
		return fmt.Errorf("otel.endpoint is required when otel.enabled is true")
	}

	resourceAttributes, errAttributes := otelusage.NormalizeResourceAttributes(o.ResourceAttributes)
	if errAttributes != nil {
		return fmt.Errorf("sanitize otel.resource-attributes: %w", errAttributes)
	}
	o.ResourceAttributes = resourceAttributes
	return nil
}

// SanitizeTenancyConfig trims tenancy values and applies defaults for unset fields.
// It never enables tenancy implicitly: Enabled must be set explicitly.
func (cfg *Config) SanitizeTenancyConfig() {
	if cfg == nil {
		return
	}
	t := &cfg.Tenancy
	t.DBPath = strings.TrimSpace(t.DBPath)
	t.ValidationInterval = strings.TrimSpace(t.ValidationInterval)
	if t.ValidationInterval == "" {
		t.ValidationInterval = defaultTenancyValidationInterval
	}

	t.Quota.Window = strings.TrimSpace(t.Quota.Window)
	if t.Quota.Window == "" {
		t.Quota.Window = DefaultTenancyQuotaWindow
	}
	t.Quota.BaseUSD = normalizeUSDLimits(t.Quota.BaseUSD, "tenancy.quota.base-usd")
	t.Quota.ModelPriceOverrides = normalizeModelPriceOverrides(t.Quota.ModelPriceOverrides)
	t.Quota.ProviderWindows = normalizeLowerKeyedString(t.Quota.ProviderWindows)
	if len(t.Quota.ContributionUSD) > 0 {
		contributions := make(map[string]map[string]USDLimit, len(t.Quota.ContributionUSD))
		for rawProvider, plans := range t.Quota.ContributionUSD {
			provider := strings.ToLower(strings.TrimSpace(rawProvider))
			if provider == "" {
				continue
			}
			normalized := normalizeUSDLimits(
				plans,
				"tenancy.quota.contribution-usd."+provider,
			)
			if len(normalized) == 0 {
				continue
			}
			contributions[provider] = normalized
		}
		if len(contributions) == 0 {
			contributions = nil
		}
		t.Quota.ContributionUSD = contributions
	}

	t.Balancing.UrgencyHorizon = strings.TrimSpace(t.Balancing.UrgencyHorizon)
	if t.Balancing.UrgencyHorizon == "" {
		t.Balancing.UrgencyHorizon = defaultTenancyUrgencyHorizon
	}
	if t.Balancing.HighWater <= 0 || t.Balancing.HighWater > 1 {
		t.Balancing.HighWater = defaultTenancyHighWater
	}
	if t.Balancing.UrgencyBonus <= 0 {
		t.Balancing.UrgencyBonus = defaultTenancyUrgencyBonus
	}
}

// SanitizeAutoRoutingConfig trims auto-routing values, applies defaults, and drops
// empty tier and category entries. Tier labels and category keys are lower-cased;
// model names keep their original case because model IDs are case-sensitive upstream.
func (cfg *Config) SanitizeAutoRoutingConfig() {
	if cfg == nil {
		return
	}
	a := &cfg.AutoRouting
	a.RouterURL = strings.TrimRight(strings.TrimSpace(a.RouterURL), "/")
	a.DefaultTier = strings.ToLower(strings.TrimSpace(a.DefaultTier))
	a.FallbackModel = strings.TrimSpace(a.FallbackModel)

	if a.TimeoutMS <= 0 {
		a.TimeoutMS = defaultAutoRoutingTimeoutMS
	}
	if a.MinConfidence < 0 || a.MinConfidence > 1 {
		a.MinConfidence = defaultAutoRoutingMinConfidence
	}
	if a.CacheTTLSeconds < 0 {
		a.CacheTTLSeconds = defaultAutoRoutingCacheTTLSeconds
	}
	if a.MaxPromptChars <= 0 {
		a.MaxPromptChars = defaultAutoRoutingMaxPromptChars
	}

	if len(a.Tiers) > 0 {
		tiers := make(map[string]AutoRoutingTier, len(a.Tiers))
		for rawTier, tier := range a.Tiers {
			label := strings.ToLower(strings.TrimSpace(rawTier))
			if label == "" {
				continue
			}
			models := make([]string, 0, len(tier.Models))
			seen := make(map[string]struct{}, len(tier.Models))
			for _, model := range tier.Models {
				model = strings.TrimSpace(model)
				if model == "" {
					continue
				}
				key := strings.ToLower(model)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				models = append(models, model)
			}
			if len(models) == 0 {
				continue
			}
			tiers[label] = AutoRoutingTier{Models: models}
		}
		if len(tiers) == 0 {
			tiers = nil
		}
		a.Tiers = tiers
	}

	if len(a.Categories) > 0 {
		categories := make(map[string]string, len(a.Categories))
		for rawCategory, rawTier := range a.Categories {
			category := strings.ToLower(strings.TrimSpace(rawCategory))
			tier := strings.ToLower(strings.TrimSpace(rawTier))
			if category == "" || tier == "" {
				continue
			}
			categories[category] = tier
		}
		if len(categories) == 0 {
			categories = nil
		}
		a.Categories = categories
	}
}

// SanitizeOpenRouterConfig trims OpenRouter values, applies safe defaults, and
// validates consumer switches. It never enables OpenRouter implicitly.
func (cfg *Config) SanitizeOpenRouterConfig() {
	if cfg == nil {
		return
	}

	o := &cfg.OpenRouter
	o.APIKey = strings.TrimSpace(o.APIKey)
	if o.APIKey == "" {
		o.APIKey = strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	}

	o.BaseURL = strings.TrimRight(strings.TrimSpace(o.BaseURL), "/")
	if o.BaseURL == "" {
		o.BaseURL = defaultOpenRouterBaseURL
	}

	o.RefreshInterval = strings.TrimSpace(o.RefreshInterval)
	if interval, errParse := time.ParseDuration(o.RefreshInterval); errParse != nil || interval <= 0 {
		o.RefreshInterval = defaultOpenRouterRefreshInterval
	}

	if len(o.ModelMap) > 0 {
		modelMap := make(map[string]string, len(o.ModelMap))
		for rawLocalModel, rawOpenRouterModel := range o.ModelMap {
			localModel := strings.ToLower(strings.TrimSpace(rawLocalModel))
			openRouterModel := strings.TrimSpace(rawOpenRouterModel)
			if localModel == "" || openRouterModel == "" {
				continue
			}
			modelMap[localModel] = openRouterModel
		}
		if len(modelMap) == 0 {
			modelMap = nil
		}
		o.ModelMap = modelMap
	}

	o.CostBasis = strings.ToLower(strings.TrimSpace(o.CostBasis))
	switch o.CostBasis {
	case "openrouter":
	default:
		o.CostBasis = defaultOpenRouterCostBasis
	}

	o.BenchmarkSource = strings.ToLower(strings.TrimSpace(o.BenchmarkSource))
	switch o.BenchmarkSource {
	case "", "artificial-analysis", "design-arena":
	default:
		o.BenchmarkSource = ""
	}

	// NewService currently receives TenancyConfig rather than the full Config.
	// Carry the sanitized catalog settings as runtime-only data so quota costing
	// still honors openrouter.cost-basis and openrouter.model-map.
	cfg.Tenancy.Pricing = *o
	auditModelPricing(cfg)
}

func normalizeUSDLimits(in map[string]USDLimit, field string) map[string]USDLimit {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]USDLimit, len(in))
	for rawKey, value := range in {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		if key == "" {
			continue
		}
		if value < 0 {
			log.WithFields(log.Fields{
				"field": field,
				"key":   key,
			}).Warn("config: rejected negative USD amount")
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeModelPriceOverrides(in map[string]ModelPriceOverride) map[string]ModelPriceOverride {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]ModelPriceOverride, len(in))
	for rawModel, price := range in {
		model := strings.ToLower(strings.TrimSpace(rawModel))
		if model == "" {
			continue
		}
		price.Prompt = nonNegativeModelPrice(model, "prompt", price.Prompt)
		price.Completion = nonNegativeModelPrice(model, "completion", price.Completion)
		price.Reasoning = nonNegativeModelPrice(model, "reasoning", price.Reasoning)
		price.CacheRead = nonNegativeModelPrice(model, "cache-read", price.CacheRead)
		price.CacheWrite = nonNegativeModelPrice(model, "cache-write", price.CacheWrite)
		if price.Prompt == nil &&
			price.Completion == nil &&
			price.Reasoning == nil &&
			price.CacheRead == nil &&
			price.CacheWrite == nil {
			continue
		}
		out[model] = price
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nonNegativeModelPrice(model, category string, value *USDPerMillionTokens) *USDPerMillionTokens {
	if value == nil || *value >= 0 {
		return value
	}
	log.WithFields(log.Fields{
		"model":    model,
		"category": category,
	}).Warn("config: rejected negative model price override")
	return nil
}

func normalizeLowerKeyedString(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for rawKey, value := range in {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
