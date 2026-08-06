package tenancy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/openrouter"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	defaultUsageBatchSize     = 64
	defaultUsageFlushInterval = 2 * time.Second
)

// UserResolver attributes a usage record to a tenant.
type UserResolver func(ctx context.Context, record usage.Record) (*User, error)

// UsagePlugin durably batches usage records and tracks provider quota windows.
type UsagePlugin struct {
	store     Store
	cfg       config.TenancyConfig
	resolver  UserResolver
	validator *credentialValidator

	batchSize     int
	flushInterval time.Duration
	now           func() time.Time

	quotaWindowUpdated func()

	mu                  sync.Mutex
	pending             []UsageEntry
	unpricedModels      map[string]struct{}
	unpricedWarningOnce sync.Once

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

var _ usage.Plugin = (*UsagePlugin)(nil)

// NewUsagePlugin creates and starts a durable usage sink.
func NewUsagePlugin(store Store, cfg config.TenancyConfig, resolver UserResolver) *UsagePlugin {
	plugin := &UsagePlugin{
		store:          store,
		cfg:            cfg,
		resolver:       resolver,
		batchSize:      defaultUsageBatchSize,
		flushInterval:  defaultUsageFlushInterval,
		now:            time.Now,
		unpricedModels: make(map[string]struct{}),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	go plugin.run()
	return plugin
}

// ResolveAPIKeyUser returns a resolver that hashes Record.APIKey before lookup.
func ResolveAPIKeyUser(store Store) UserResolver {
	return func(_ context.Context, record usage.Record) (*User, error) {
		if store == nil {
			return nil, fmt.Errorf("tenancy usage: store is nil")
		}
		if record.APIKey == "" {
			return nil, fmt.Errorf("%w: empty usage API key", ErrNotFound)
		}
		return store.LookupByAPIKey(HashAPIKey(record.APIKey))
	}
}

// HandleUsage implements usage.Plugin.
func (p *UsagePlugin) HandleUsage(ctx context.Context, record usage.Record) {
	if p == nil || p.store == nil || p.resolver == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	user, errResolve := p.resolver(ctx, record)
	if errResolve != nil {
		if !errors.Is(errResolve, ErrNotFound) {
			log.WithError(errResolve).Debug("tenancy usage: user attribution failed")
		}
		return
	}
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return
	}

	occurredAt := record.RequestedAt.UTC()
	if occurredAt.IsZero() {
		occurredAt = p.now().UTC()
	}
	pricing := ModelPricingFor(record.Model, p.cfg)
	if hasUnpricedUsage(record, pricing) {
		p.noteUnpricedModel(record.Model)
	}
	entry := UsageEntry{
		UserID:       user.ID,
		AuthID:       record.AuthID,
		Provider:     strings.ToLower(strings.TrimSpace(record.Provider)),
		Model:        record.Model,
		CostNanoUSD:  CalculateWeightedTokens(record, pricing),
		InputTokens:  record.Detail.InputTokens,
		OutputTokens: record.Detail.OutputTokens,
		Failed:       record.Failed,
		OccurredAt:   occurredAt,
	}
	if batch := p.enqueue(entry); len(batch) > 0 {
		p.warnUnpricedModels()
		if errAppend := p.store.AppendUsage(context.Background(), batch); errAppend != nil {
			p.restore(batch)
			log.WithError(errAppend).Error("tenancy usage: append batch failed")
		}
	}

	if p.validator != nil {
		if errRecord := p.validator.recordOutcome(
			user.ID,
			record.AuthID,
			record.Failed,
			record.Fail.StatusCode,
			occurredAt,
		); errRecord != nil {
			log.WithError(errRecord).
				WithField("auth_id", record.AuthID).
				Warn("tenancy usage: record credential validation outcome")
		}
	}
	p.captureQuotaWindow(record, occurredAt)
}

// Flush writes all currently buffered usage rows.
func (p *UsagePlugin) Flush(ctx context.Context) error {
	if p == nil || p.store == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	batch := p.takePending()
	if len(batch) == 0 {
		return nil
	}
	p.warnUnpricedModels()
	if errAppend := p.store.AppendUsage(ctx, batch); errAppend != nil {
		p.restore(batch)
		return fmt.Errorf("tenancy usage: flush batch: %w", errAppend)
	}
	return nil
}

// Close stops periodic flushing and performs a final flush.
func (p *UsagePlugin) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		close(p.stop)
		<-p.done
	})
	return p.Flush(context.Background())
}

// CalculateWeightedTokens calculates exact nano-USD from the canonical v2 token
// buckets. The historical name is retained for callers, but the result is now
// money rather than a normalized token weight.
func CalculateWeightedTokens(record usage.Record, pricing openrouter.ModelPricing) int64 {
	detail := usage.EnsureTokenBreakdownForProvider(
		record.Detail,
		record.Provider,
		record.ExecutorType,
	)
	breakdown := detail.TokenBreakdown
	reasoningPrice := pricing.InternalReasoningNanoUSDPerToken
	if !reasoningPrice.Known {
		// Providers conventionally bill reasoning as completion when they do not
		// publish a separate reasoning price.
		reasoningPrice = pricing.CompletionNanoUSDPerToken
	}

	var cost int64
	cost = addTokenCost(cost, breakdown.Input.UncachedTokens, pricing.PromptNanoUSDPerToken)
	cost = addTokenCost(cost, breakdown.Input.CacheReadTokens, pricing.InputCacheReadNanoUSDPerToken)
	cost = addTokenCost(cost, breakdown.Input.CacheWriteTokens, pricing.InputCacheWriteNanoUSDPerToken)
	cost = addTokenCost(cost, breakdown.Output.NonReasoningTokens, pricing.CompletionNanoUSDPerToken)
	cost = addTokenCost(cost, breakdown.Output.ReasoningTokens, reasoningPrice)
	// Unclassified tokens use the prompt rate because their input/output origin
	// is unknown; this makes the fallback explicit and deterministic.
	cost = addTokenCost(cost, breakdown.UnclassifiedTokens, pricing.PromptNanoUSDPerToken)
	return cost
}

// ModelPricingFor resolves the effective per-token prices for a local model.
// Static mode uses only overrides. OpenRouter mode lets catalog prices win and
// uses exact-name then "*" overrides only for missing fields.
func ModelPricingFor(model string, cfg config.TenancyConfig) openrouter.ModelPricing {
	model = strings.ToLower(strings.TrimSpace(model))
	pricing := openrouter.ModelPricing{}
	applyModelPriceOverride(&pricing, cfg.Quota.ModelPriceOverrides["*"], false)
	applyModelPriceOverride(&pricing, cfg.Quota.ModelPriceOverrides[model], true)

	// openrouter.enabled is the master switch: when it is false the catalog is
	// documented as fully inert, so cost-basis: openrouter degrades to
	// overrides-only rather than silently pricing from the embedded snapshot.
	if cfg.Pricing.Enabled && strings.EqualFold(strings.TrimSpace(cfg.Pricing.CostBasis), "openrouter") {
		catalogPricing, _, ok := openrouter.CurrentSnapshot().PricingForLocalModel(
			model,
			cfg.Pricing.ModelMap,
		)
		if ok {
			overlayKnownCatalogPricing(&pricing, catalogPricing)
		}
	}
	return pricing
}

func applyModelPriceOverride(pricing *openrouter.ModelPricing, override config.ModelPriceOverride, overwrite bool) {
	applyOverridePrice(&pricing.PromptNanoUSDPerToken, override.Prompt, overwrite)
	applyOverridePrice(&pricing.CompletionNanoUSDPerToken, override.Completion, overwrite)
	applyOverridePrice(&pricing.InternalReasoningNanoUSDPerToken, override.Reasoning, overwrite)
	applyOverridePrice(&pricing.InputCacheReadNanoUSDPerToken, override.CacheRead, overwrite)
	applyOverridePrice(&pricing.InputCacheWriteNanoUSDPerToken, override.CacheWrite, overwrite)
}

func applyOverridePrice(target *openrouter.KnownNanoUSD, value *config.USDPerMillionTokens, overwrite bool) {
	if value == nil || (target.Known && !overwrite) {
		return
	}
	*target = openrouter.KnownNanoUSD{
		NanoUSD: value.NanoUSDPerToken(),
		Known:   true,
	}
}

func overlayKnownCatalogPricing(target *openrouter.ModelPricing, catalog openrouter.ModelPricing) {
	target.OpenRouterID = catalog.OpenRouterID
	target.ContextLength = catalog.ContextLength
	target.SupportedParameters = append([]string(nil), catalog.SupportedParameters...)
	overlayKnownPrice(&target.PromptNanoUSDPerToken, catalog.PromptNanoUSDPerToken)
	overlayKnownPrice(&target.CompletionNanoUSDPerToken, catalog.CompletionNanoUSDPerToken)
	overlayKnownPrice(&target.InternalReasoningNanoUSDPerToken, catalog.InternalReasoningNanoUSDPerToken)
	overlayKnownPrice(&target.InputCacheReadNanoUSDPerToken, catalog.InputCacheReadNanoUSDPerToken)
	overlayKnownPrice(&target.InputCacheWriteNanoUSDPerToken, catalog.InputCacheWriteNanoUSDPerToken)
	overlayKnownPrice(&target.RequestNanoUSD, catalog.RequestNanoUSD)
	overlayKnownPrice(&target.ImageNanoUSD, catalog.ImageNanoUSD)
	overlayKnownPrice(&target.WebSearchNanoUSD, catalog.WebSearchNanoUSD)
}

func overlayKnownPrice(target *openrouter.KnownNanoUSD, catalog openrouter.KnownNanoUSD) {
	if catalog.Known {
		*target = catalog
	}
}

func addTokenCost(current, tokens int64, price openrouter.KnownNanoUSD) int64 {
	if tokens <= 0 || !price.Known || price.NanoUSD <= 0 {
		return current
	}
	if tokens > math.MaxInt64/price.NanoUSD {
		return math.MaxInt64
	}
	increment := tokens * price.NanoUSD
	if current > math.MaxInt64-increment {
		return math.MaxInt64
	}
	return current + increment
}

func hasUnpricedUsage(record usage.Record, pricing openrouter.ModelPricing) bool {
	detail := usage.EnsureTokenBreakdownForProvider(
		record.Detail,
		record.Provider,
		record.ExecutorType,
	)
	breakdown := detail.TokenBreakdown
	if (breakdown.Input.UncachedTokens > 0 || breakdown.UnclassifiedTokens > 0) &&
		!pricing.PromptNanoUSDPerToken.Known {
		return true
	}
	if breakdown.Input.CacheReadTokens > 0 && !pricing.InputCacheReadNanoUSDPerToken.Known {
		return true
	}
	if breakdown.Input.CacheWriteTokens > 0 && !pricing.InputCacheWriteNanoUSDPerToken.Known {
		return true
	}
	if breakdown.Output.NonReasoningTokens > 0 && !pricing.CompletionNanoUSDPerToken.Known {
		return true
	}
	return breakdown.Output.ReasoningTokens > 0 &&
		!pricing.InternalReasoningNanoUSDPerToken.Known &&
		!pricing.CompletionNanoUSDPerToken.Known
}

func (p *UsagePlugin) noteUnpricedModel(model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "<unknown>"
	}
	p.mu.Lock()
	p.unpricedModels[model] = struct{}{}
	p.mu.Unlock()
}

func (p *UsagePlugin) warnUnpricedModels() {
	p.mu.Lock()
	models := make([]string, 0, len(p.unpricedModels))
	for model := range p.unpricedModels {
		models = append(models, model)
	}
	p.mu.Unlock()
	if len(models) == 0 {
		return
	}
	p.unpricedWarningOnce.Do(func() {
		sort.Strings(models)
		log.WithFields(log.Fields{
			"cost_basis": p.cfg.Pricing.CostBasis,
			"models":     models,
		}).Warn("tenancy usage: models have unpriced token categories; those categories record zero cost")
	})
}

func (p *UsagePlugin) run() {
	defer close(p.done)
	ticker := time.NewTicker(p.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if errFlush := p.Flush(context.Background()); errFlush != nil {
				log.WithError(errFlush).Error("tenancy usage: periodic flush failed")
			}
		case <-p.stop:
			return
		}
	}
}

func (p *UsagePlugin) enqueue(entry UsageEntry) []UsageEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = append(p.pending, entry)
	if len(p.pending) < p.batchSize {
		return nil
	}
	batch := p.pending
	p.pending = nil
	return batch
}

func (p *UsagePlugin) takePending() []UsageEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	batch := p.pending
	p.pending = nil
	return batch
}

func (p *UsagePlugin) restore(batch []UsageEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	restored := make([]UsageEntry, 0, len(batch)+len(p.pending))
	restored = append(restored, batch...)
	restored = append(restored, p.pending...)
	p.pending = restored
}

func (p *UsagePlugin) captureQuotaWindow(record usage.Record, occurredAt time.Time) {
	if record.AuthID == "" || strings.TrimSpace(record.Provider) == "" {
		return
	}
	parsed, ok := ParseRateLimitHeaders(record.ResponseHeaders, p.now().UTC())
	if !ok {
		return
	}

	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	now := p.now().UTC()
	windowEnd := parsed.ResetAt
	windowDuration := parsed.WindowDuration
	hasHeaderWindow := windowDuration > 0
	hasWindowDuration := hasHeaderWindow
	if !hasWindowDuration {
		windowDuration, hasWindowDuration = p.providerWindow(provider)
	}

	var windowStart time.Time
	if hasHeaderWindow && !windowEnd.IsZero() {
		windowStart = windowEnd.Add(-windowDuration)
	} else {
		searchSince := now
		if hasWindowDuration {
			if !windowEnd.IsZero() {
				searchSince = windowEnd.Add(-windowDuration)
			} else {
				searchSince = now.Add(-windowDuration)
			}
		}
		found := false
		var errEarliest error
		windowStart, found, errEarliest = p.store.EarliestAuthUsage(
			context.Background(),
			record.AuthID,
			provider,
			searchSince,
		)
		if errEarliest != nil {
			log.WithError(errEarliest).
				WithField("auth_id", record.AuthID).
				Error("tenancy usage: infer quota window start")
			return
		}
		if !found || occurredAt.Before(windowStart) {
			windowStart = occurredAt
		}
	}
	if windowStart.IsZero() {
		windowStart = now
	}

	source := parsed.Source
	if windowEnd.IsZero() && hasWindowDuration {
		windowEnd = windowStart.Add(windowDuration)
		if !hasHeaderWindow {
			source += ":provider-window"
		}
	}
	usedUnits := int64(0)
	if parsed.Limit > 0 && parsed.Remaining <= parsed.Limit {
		usedUnits = parsed.Limit - parsed.Remaining
	}
	window := QuotaWindow{
		AuthID:      record.AuthID,
		Provider:    provider,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		UsedUnits:   usedUnits,
		LimitUnits:  parsed.Limit,
		Source:      source,
		UpdatedAt:   now,
	}
	if errUpsert := p.store.UpsertQuotaWindow(context.Background(), window); errUpsert != nil {
		log.WithError(errUpsert).
			WithField("auth_id", record.AuthID).
			Error("tenancy usage: upsert quota window")
		return
	}
	if p.quotaWindowUpdated != nil {
		p.quotaWindowUpdated()
	}
}

func (p *UsagePlugin) providerWindow(provider string) (time.Duration, bool) {
	value := strings.TrimSpace(p.cfg.Quota.ProviderWindows[provider])
	if value == "" {
		return 0, false
	}
	duration, errParse := time.ParseDuration(value)
	if errParse != nil || duration <= 0 {
		return 0, false
	}
	return duration, true
}
