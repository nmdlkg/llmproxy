package config

import (
	"strings"
)

// Auto-routing defaults shared by the file loader and the byte parser.
const (
	defaultAutoRoutingTimeoutMS       = 800
	defaultAutoRoutingMinConfidence   = 0.5
	defaultAutoRoutingCacheTTLSeconds = 300
	defaultAutoRoutingMaxPromptChars  = 4000
)

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
