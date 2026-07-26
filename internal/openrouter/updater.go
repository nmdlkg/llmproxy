package openrouter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

const fallbackRefreshInterval = 6 * time.Hour

// UpdaterOptions configures background catalog refresh. DisableRemote is the
// caller-facing --local-model seam: when true, embedded data remains available
// but no goroutine or network request is started.
type UpdaterOptions struct {
	Config        *config.Config
	LocalModels   []string
	DisableRemote bool
}

// StartUpdater starts the process-wide updater at most once.
func StartUpdater(ctx context.Context, options UpdaterOptions) {
	defaultCatalog.StartUpdater(ctx, options)
}

// StartUpdater starts this catalog's updater at most once. Disabled OpenRouter
// config is completely inert: it creates no goroutine, performs no network
// request, and emits no mapping warning.
func (c *Catalog) StartUpdater(ctx context.Context, options UpdaterOptions) {
	if c == nil || options.Config == nil {
		return
	}

	cfg := options.Config.CloneForRuntime()
	localModels := append([]string(nil), options.LocalModels...)
	if !cfg.OpenRouter.Enabled {
		c.logUnpricedModels(localModels, cfg)
		return
	}
	if options.DisableRemote {
		c.logUnresolvedMappings(localModels, cfg.OpenRouter.ModelMap)
		c.logUnpricedModels(localModels, cfg)
		return
	}

	c.startOnce.Do(func() {
		go c.runUpdater(ctx, cfg, localModels)
	})
}

func (c *Catalog) runUpdater(ctx context.Context, cfg *config.Config, localModels []string) {
	client := NewClient(ctx, cfg)
	if errRefresh := c.refresh(ctx, client, cfg.OpenRouter); errRefresh != nil {
		log.WithError(errRefresh).Warn("openrouter: startup catalog refresh failed; retaining previous snapshot")
	}
	c.logUnresolvedMappings(localModels, cfg.OpenRouter.ModelMap)
	c.logUnpricedModels(localModels, cfg)

	interval := fallbackRefreshInterval
	if parsed, errParse := time.ParseDuration(cfg.OpenRouter.RefreshInterval); errParse == nil && parsed > 0 {
		interval = parsed
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.WithField("interval", interval).Info("openrouter: periodic catalog refresh started")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if errRefresh := c.refresh(ctx, client, cfg.OpenRouter); errRefresh != nil {
				log.WithError(errRefresh).Warn("openrouter: catalog refresh failed; retaining previous snapshot")
			}
		}
	}
}

func (c *Catalog) refresh(ctx context.Context, client *Client, cfg config.OpenRouterConfig) error {
	pricing, errPricing := client.FetchPricing(ctx)
	if errPricing != nil {
		return fmt.Errorf("refresh pricing: %w", errPricing)
	}

	current := c.Snapshot()
	quality := current.Quality
	source := "openrouter-models"
	benchmarkSource := BenchmarkSource(strings.TrimSpace(cfg.BenchmarkSource))
	if benchmarkSource != "" {
		query := BenchmarkQuery{Source: benchmarkSource, MaxResults: 100}
		if benchmarkSource == BenchmarkDesignArena {
			query.Arena = "models"
		}
		var errBenchmarks error
		quality, errBenchmarks = client.FetchBenchmarks(ctx, query)
		if errBenchmarks != nil {
			return fmt.Errorf("refresh %s benchmarks: %w", benchmarkSource, errBenchmarks)
		}
		source += "+" + string(benchmarkSource)
	}

	c.replace(Snapshot{
		Pricing:   pricing,
		Quality:   quality,
		UpdatedAt: time.Now().UTC(),
		Source:    source,
	})
	return nil
}

func (c *Catalog) logUnresolvedMappings(localModels []string, explicit map[string]string) {
	if len(localModels) == 0 {
		return
	}
	c.mappingLogOnce.Do(func() {
		mappings := ResolveModels(localModels, c.Snapshot().ModelIDs(), explicit)
		unresolved := sortedUnresolvedMappings(mappings)
		if len(unresolved) == 0 {
			return
		}
		log.WithField("models", unresolved).Warn(
			"openrouter: local models have no unique OpenRouter mapping; catalog prices and benchmarks remain unknown",
		)
	})
}

func (c *Catalog) logUnpricedModels(localModels []string, cfg *config.Config) {
	if len(localModels) == 0 || cfg == nil {
		return
	}
	unpriced := make([]string, 0)
	for _, model := range localModels {
		if modelHasEffectivePrice(model, c.Snapshot(), cfg) {
			continue
		}
		unpriced = append(unpriced, model)
	}
	unpriced = uniqueSortedIDs(unpriced)
	if len(unpriced) == 0 {
		return
	}
	c.pricingLogOnce.Do(func() {
		log.WithFields(log.Fields{
			"cost_basis": cfg.OpenRouter.CostBasis,
			"models":     unpriced,
		}).Warn("openrouter: local models have no effective price; configure model-price-overrides")
	})
}

func modelHasEffectivePrice(model string, snapshot Snapshot, cfg *config.Config) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	known := overrideHasKnownPrice(cfg.Tenancy.Quota.ModelPriceOverrides["*"]) ||
		overrideHasKnownPrice(cfg.Tenancy.Quota.ModelPriceOverrides[model])
	if cfg.OpenRouter.CostBasis != "openrouter" {
		return known
	}
	pricing, _, ok := snapshot.PricingForLocalModel(model, cfg.OpenRouter.ModelMap)
	if !ok {
		return known
	}
	return known ||
		pricing.PromptNanoUSDPerToken.Known ||
		pricing.CompletionNanoUSDPerToken.Known ||
		pricing.InternalReasoningNanoUSDPerToken.Known ||
		pricing.InputCacheReadNanoUSDPerToken.Known ||
		pricing.InputCacheWriteNanoUSDPerToken.Known
}

func overrideHasKnownPrice(price config.ModelPriceOverride) bool {
	return price.Prompt != nil ||
		price.Completion != nil ||
		price.Reasoning != nil ||
		price.CacheRead != nil ||
		price.CacheWrite != nil
}
