package config

import (
	"os"
	"strings"
	"time"
)

// OpenRouter defaults shared by the file loader and the byte parser.
const (
	defaultOpenRouterBaseURL         = "https://openrouter.ai/api/v1"
	defaultOpenRouterRefreshInterval = "6h"
	defaultOpenRouterCostBasis       = "static"
)

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
