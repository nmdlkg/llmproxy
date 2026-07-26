// Package otelusage exports proxy usage metrics through OTLP/HTTP.
package otelusage

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

const (
	// DefaultExportInterval keeps proxy metrics reasonably fresh without
	// turning collector availability into a request-path concern.
	DefaultExportInterval = 15 * time.Second

	DefaultServiceName = "llmproxy"

	instrumentationName = "github.com/router-for-me/CLIProxyAPI/v7/internal/otelusage"
	exportWarnInterval  = time.Minute
)

// Options configures the OTLP/HTTP metrics pipeline.
type Options struct {
	Enabled            bool
	Endpoint           string
	ExportInterval     time.Duration
	ServiceName        string
	ServiceVersion     string
	Environment        string
	ResourceAttributes map[string]string
}

// UserEmailResolver maps one usage record to the tenant email already owned by
// the tenancy service. Returning ok=false drops the complete record.
type UserEmailResolver func(coreusage.Record) (email string, ok bool)

// Plugin records usage into an in-process MeterProvider. Its HandleUsage method
// performs no network I/O; the PeriodicReader owns all collector communication.
type Plugin struct {
	meterProvider *sdkmetric.MeterProvider
	resolver      UserEmailResolver

	tokenUsage      otelmetric.Int64Counter
	requestCount    otelmetric.Int64Counter
	requestDuration otelmetric.Float64Histogram

	active       atomic.Bool
	shutdownOnce sync.Once
	shutdownErr  error
}

var _ coreusage.Plugin = (*Plugin)(nil)

// New creates an enabled OTLP/HTTP usage plugin. Disabled options return a nil
// plugin without constructing an exporter, MeterProvider, or goroutine.
func New(ctx context.Context, options Options, resolver UserEmailResolver) (*Plugin, error) {
	if !options.Enabled {
		return nil, nil
	}
	if resolver == nil {
		return nil, fmt.Errorf("otel usage: user email resolver is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	normalized, errNormalize := normalizeOptions(options)
	if errNormalize != nil {
		return nil, errNormalize
	}

	exporter, errExporter := otlpmetrichttp.New(
		ctx,
		otlpmetrichttp.WithEndpointURL(normalized.Endpoint),
	)
	if errExporter != nil {
		return nil, fmt.Errorf("otel usage: create OTLP/HTTP metric exporter: %w", errExporter)
	}

	reader := sdkmetric.NewPeriodicReader(
		&rateLimitedExporter{Exporter: exporter, now: time.Now},
		sdkmetric.WithInterval(normalized.ExportInterval),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(resourceFromOptions(normalized)),
	)
	plugin, errPlugin := newPluginWithMeterProvider(meterProvider, resolver)
	if errPlugin != nil {
		if errShutdown := meterProvider.Shutdown(ctx); errShutdown != nil {
			return nil, errors.Join(
				errPlugin,
				fmt.Errorf("otel usage: shut down metric provider after initialization failure: %w", errShutdown),
			)
		}
		return nil, errPlugin
	}
	return plugin, nil
}

func newPluginWithMeterProvider(meterProvider *sdkmetric.MeterProvider, resolver UserEmailResolver) (*Plugin, error) {
	if meterProvider == nil {
		return nil, fmt.Errorf("otel usage: meter provider is required")
	}
	if resolver == nil {
		return nil, fmt.Errorf("otel usage: user email resolver is required")
	}

	meter := meterProvider.Meter(instrumentationName)
	tokenUsage, errTokenUsage := meter.Int64Counter(
		"llmproxy.token.usage",
		otelmetric.WithUnit("tokens"),
	)
	if errTokenUsage != nil {
		return nil, fmt.Errorf("otel usage: create token counter: %w", errTokenUsage)
	}
	requestCount, errRequestCount := meter.Int64Counter("llmproxy.request.count")
	if errRequestCount != nil {
		return nil, fmt.Errorf("otel usage: create request counter: %w", errRequestCount)
	}
	requestDuration, errRequestDuration := meter.Float64Histogram(
		"llmproxy.request.duration",
		otelmetric.WithUnit("ms"),
	)
	if errRequestDuration != nil {
		return nil, fmt.Errorf("otel usage: create request duration histogram: %w", errRequestDuration)
	}

	plugin := &Plugin{
		meterProvider:   meterProvider,
		resolver:        resolver,
		tokenUsage:      tokenUsage,
		requestCount:    requestCount,
		requestDuration: requestDuration,
	}
	plugin.active.Store(true)
	return plugin, nil
}

// HandleUsage implements usage.Plugin. Missing tenant email drops the complete
// record so unrelated users are never merged into an "unknown" time series.
func (p *Plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || !p.active.Load() || p.resolver == nil {
		return
	}
	defer func() {
		if recover() != nil {
			log.Warn("otel usage: dropped record after internal panic")
		}
	}()

	email, ok := p.resolver(record)
	email = strings.TrimSpace(email)
	if !ok || email == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	model := normalizedDimension(record.Model)
	provider := normalizedDimension(record.Provider)
	requestAttributes := attribute.NewSet(
		attribute.String("user_email", email),
		attribute.String("model", model),
		attribute.String("status_class", statusClass(record)),
		attribute.Bool("failed", record.Failed),
	)
	p.requestCount.Add(ctx, 1, otelmetric.WithAttributeSet(requestAttributes))

	durationAttributes := attribute.NewSet(
		attribute.String("user_email", email),
		attribute.String("model", model),
	)
	durationMilliseconds := float64(record.Latency) / float64(time.Millisecond)
	if durationMilliseconds < 0 {
		durationMilliseconds = 0
	}
	p.requestDuration.Record(ctx, durationMilliseconds, otelmetric.WithAttributeSet(durationAttributes))

	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	for _, category := range tokenCategories(detail.TokenBreakdown) {
		tokenAttributes := attribute.NewSet(
			attribute.String("user_email", email),
			attribute.String("model", model),
			attribute.String("provider", provider),
			attribute.String("token_type", category.name),
		)
		p.tokenUsage.Add(ctx, category.tokens, otelmetric.WithAttributeSet(tokenAttributes))
	}
}

// Shutdown stops accepting measurements, flushes pending metrics, and releases
// the OTLP exporter.
func (p *Plugin) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.shutdownOnce.Do(func() {
		p.active.Store(false)
		var shutdownErrors []error
		if p.meterProvider != nil {
			if errFlush := p.meterProvider.ForceFlush(ctx); errFlush != nil {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("otel usage: flush metrics: %w", errFlush))
			}
			if errShutdown := p.meterProvider.Shutdown(ctx); errShutdown != nil {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("otel usage: shut down metrics: %w", errShutdown))
			}
		}
		p.shutdownErr = errors.Join(shutdownErrors...)
	})
	return p.shutdownErr
}

type tokenCategory struct {
	name   string
	tokens int64
}

func tokenCategories(breakdown coreusage.TokenBreakdown) []tokenCategory {
	if !breakdown.Valid() {
		return nil
	}
	candidates := [...]tokenCategory{
		{name: "input", tokens: breakdown.Input.UncachedTokens},
		{name: "cacheRead", tokens: breakdown.Input.CacheReadTokens},
		{name: "cacheCreation", tokens: breakdown.Input.CacheWriteTokens},
		{name: "output", tokens: breakdown.Output.NonReasoningTokens},
		{name: "reasoning", tokens: breakdown.Output.ReasoningTokens},
		{name: "unclassified", tokens: breakdown.UnclassifiedTokens},
	}
	categories := make([]tokenCategory, 0, len(candidates))
	for _, category := range candidates {
		if category.tokens > 0 {
			categories = append(categories, category)
		}
	}
	return categories
}

func statusClass(record coreusage.Record) string {
	statusCode := record.Fail.StatusCode
	switch {
	case statusCode >= 400 && statusCode < 500:
		return "4xx"
	case statusCode >= 500:
		return "5xx"
	case record.Failed:
		return "5xx"
	default:
		return "2xx"
	}
}

func normalizedDimension(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func normalizeOptions(options Options) (Options, error) {
	options.Endpoint = strings.TrimSpace(options.Endpoint)
	if options.Endpoint == "" {
		return Options{}, fmt.Errorf("otel usage: endpoint is required when enabled")
	}
	endpointURL, errParse := url.Parse(options.Endpoint)
	if errParse != nil {
		return Options{}, fmt.Errorf("otel usage: parse endpoint: %w", errParse)
	}
	if (endpointURL.Scheme != "http" && endpointURL.Scheme != "https") || endpointURL.Host == "" {
		return Options{}, fmt.Errorf("otel usage: endpoint must be an absolute HTTP(S) URL")
	}
	if endpointURL.User != nil || endpointURL.RawQuery != "" || endpointURL.Fragment != "" {
		return Options{}, fmt.Errorf("otel usage: endpoint must not contain user information, a query, or a fragment")
	}
	if options.ExportInterval <= 0 {
		options.ExportInterval = DefaultExportInterval
	}
	options.ServiceName = normalizedResourceValue(options.ServiceName, DefaultServiceName)
	options.ServiceVersion = normalizedResourceValue(options.ServiceVersion, "unknown")
	options.Environment = normalizedResourceValue(options.Environment, "unknown")

	resourceAttributes := make(map[string]string, len(options.ResourceAttributes))
	for key, value := range options.ResourceAttributes {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		if forbiddenAttributeName(key) {
			return Options{}, fmt.Errorf("otel usage: resource attribute %q is forbidden", key)
		}
		resourceAttributes[key] = value
	}
	options.ResourceAttributes = resourceAttributes
	return options, nil
}

func forbiddenAttributeName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "auth_id", "auth_index", "api_key", "request_id", "alias":
		return true
	default:
		return false
	}
}

func normalizedResourceValue(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func resourceFromOptions(options Options) *sdkresource.Resource {
	values := make(map[string]string, len(options.ResourceAttributes)+4)
	for key, value := range options.ResourceAttributes {
		values[key] = value
	}
	values["service.name"] = options.ServiceName
	values["service.version"] = options.ServiceVersion
	values["deployment.environment"] = options.Environment
	if strings.TrimSpace(values["host.name"]) == "" {
		values["host.name"] = "unknown"
	}

	attributes := make([]attribute.KeyValue, 0, len(values))
	for key, value := range values {
		attributes = append(attributes, attribute.String(key, value))
	}
	return sdkresource.NewSchemaless(attributes...)
}

type rateLimitedExporter struct {
	sdkmetric.Exporter

	mu        sync.Mutex
	lastWarn  time.Time
	lastError error
	now       func() time.Time
}

func (e *rateLimitedExporter) Export(ctx context.Context, metrics *metricdata.ResourceMetrics) error {
	errExport := e.Exporter.Export(ctx, metrics)
	if errExport == nil {
		e.mu.Lock()
		e.lastError = nil
		e.mu.Unlock()
		return nil
	}

	now := time.Now()
	if e.now != nil {
		now = e.now()
	}
	e.mu.Lock()
	e.lastError = errExport
	shouldWarn := e.lastWarn.IsZero() || now.Sub(e.lastWarn) >= exportWarnInterval
	if shouldWarn {
		e.lastWarn = now
	}
	e.mu.Unlock()
	if shouldWarn {
		log.WithError(errExport).Warn("otel usage: metric export failed")
	}

	// The next PeriodicReader collection is the retry boundary. Swallowing the
	// error prevents the SDK's global handler from logging every interval.
	return nil
}

func (e *rateLimitedExporter) ForceFlush(ctx context.Context) error {
	if errFlush := e.Exporter.ForceFlush(ctx); errFlush != nil {
		return errFlush
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastError
}
