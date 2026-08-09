package otelusage

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestTokenCategoriesSumToTotalAndOmitZero(t *testing.T) {
	breakdown := coreusage.TokenBreakdown{
		SchemaVersion: coreusage.TokenAccountingSchemaVersion,
		Quality:       coreusage.TokenAccountingQualityUnclassified,
		TotalTokens:   26,
		Input: coreusage.TokenInputBreakdown{
			TotalTokens:      13,
			UncachedTokens:   10,
			CacheReadTokens:  3,
			CacheWriteTokens: 0,
		},
		Output: coreusage.TokenOutputBreakdown{
			TotalTokens:        9,
			NonReasoningTokens: 7,
			ReasoningTokens:    2,
		},
		UnclassifiedTokens: 4,
	}

	categories := tokenCategories(breakdown)
	got := make(map[string]int64, len(categories))
	var sum int64
	for _, category := range categories {
		got[category.name] = category.tokens
		sum += category.tokens
	}
	want := map[string]int64{
		"input":        10,
		"cacheRead":    3,
		"output":       7,
		"reasoning":    2,
		"unclassified": 4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("token categories = %#v, want %#v", got, want)
	}
	if sum != breakdown.TotalTokens {
		t.Fatalf("token category sum = %d, want total %d", sum, breakdown.TotalTokens)
	}
	if _, exists := got["cacheCreation"]; exists {
		t.Fatal("zero cacheCreation category was emitted")
	}
	if _, exists := got["total"]; exists {
		t.Fatal("forbidden total category was emitted")
	}
}

func TestHandleUsageEmitsOnlyAllowedAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	plugin, errPlugin := newPluginWithMeterProvider(meterProvider, func(context.Context, coreusage.Record) (string, bool) {
		return "user@example.com", true
	})
	if errPlugin != nil {
		t.Fatalf("newPluginWithMeterProvider() error = %v", errPlugin)
	}
	t.Cleanup(func() {
		if errShutdown := plugin.Shutdown(context.Background()); errShutdown != nil {
			t.Errorf("Shutdown() error = %v", errShutdown)
		}
	})

	breakdown := coreusage.NewIndependentTokenBreakdown(10, 3, 2, 7, 4, 26)
	plugin.HandleUsage(context.Background(), coreusage.Record{
		Provider:  "codex",
		Model:     "gpt-5.6",
		Alias:     "client-controlled-alias",
		APIKey:    "secret-key",
		AuthID:    "rotating-auth",
		AuthIndex: "59",
		Latency:   1250 * time.Millisecond,
		Failed:    true,
		Fail:      coreusage.Failure{StatusCode: 503},
		Detail:    coreusage.Detail{TokenBreakdown: breakdown},
	})

	metrics := collectMetrics(t, reader)
	expectedKeys := map[string][]string{
		"llmproxy.token.usage":      {"model", "provider", "token_type", "user_email"},
		"llmproxy.request.count":    {"failed", "model", "status_class", "user_email"},
		"llmproxy.request.duration": {"model", "user_email"},
	}
	forbidden := map[string]struct{}{
		"auth_id":    {},
		"auth_index": {},
		"api_key":    {},
		"request_id": {},
		"alias":      {},
	}

	seen := make(map[string]int)
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			sets := metricAttributeSets(t, metric)
			if len(sets) == 0 {
				t.Fatalf("metric %q has no data points", metric.Name)
			}
			for _, set := range sets {
				keys := attributeKeys(set)
				if !reflect.DeepEqual(keys, expectedKeys[metric.Name]) {
					t.Fatalf("metric %q attribute keys = %v, want %v", metric.Name, keys, expectedKeys[metric.Name])
				}
				modelValue, hasModel := set.Value(attribute.Key("model"))
				if !hasModel {
					t.Fatalf("metric %q has no model attribute", metric.Name)
				}
				if gotModel := modelValue.AsString(); gotModel != "gpt-5.6" {
					t.Fatalf("metric %q model = %q, want resolved model gpt-5.6", metric.Name, gotModel)
				}
				for _, key := range keys {
					if _, disallowed := forbidden[key]; disallowed {
						t.Fatalf("metric %q emitted forbidden attribute %q", metric.Name, key)
					}
				}
				seen[metric.Name]++
			}
		}
	}
	for metricName := range expectedKeys {
		if seen[metricName] == 0 {
			t.Fatalf("metric %q was not emitted", metricName)
		}
	}
}

func TestHandleUsageDropsMissingEmail(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	plugin, errPlugin := newPluginWithMeterProvider(meterProvider, func(context.Context, coreusage.Record) (string, bool) {
		return "", false
	})
	if errPlugin != nil {
		t.Fatalf("newPluginWithMeterProvider() error = %v", errPlugin)
	}
	t.Cleanup(func() {
		if errShutdown := plugin.Shutdown(context.Background()); errShutdown != nil {
			t.Errorf("Shutdown() error = %v", errShutdown)
		}
	})

	plugin.HandleUsage(context.Background(), coreusage.Record{
		Model:   "gpt-5.6",
		Latency: time.Second,
		Detail:  coreusage.Detail{TotalTokens: 10},
	})

	metrics := collectMetrics(t, reader)
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if sets := metricAttributeSets(t, metric); len(sets) != 0 {
				t.Fatalf("metric %q emitted %d data points for missing email", metric.Name, len(sets))
			}
		}
	}
}

func TestHandleUsageContainsResolverPanic(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	plugin, errPlugin := newPluginWithMeterProvider(meterProvider, func(context.Context, coreusage.Record) (string, bool) {
		panic("malformed resolver input")
	})
	if errPlugin != nil {
		t.Fatalf("newPluginWithMeterProvider() error = %v", errPlugin)
	}
	t.Cleanup(func() {
		if errShutdown := plugin.Shutdown(context.Background()); errShutdown != nil {
			t.Errorf("Shutdown() error = %v", errShutdown)
		}
	})

	plugin.HandleUsage(context.Background(), coreusage.Record{
		Model:  "gpt-5.6",
		Detail: coreusage.Detail{TotalTokens: -1},
	})
}

func TestHandleUsagePassesCancelledContextValuesToResolver(t *testing.T) {
	type contextKey struct{}
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	var gotValue string
	plugin, errPlugin := newPluginWithMeterProvider(meterProvider, func(ctx context.Context, _ coreusage.Record) (string, bool) {
		gotValue, _ = ctx.Value(contextKey{}).(string)
		return "user@example.com", true
	})
	if errPlugin != nil {
		t.Fatalf("newPluginWithMeterProvider() error = %v", errPlugin)
	}
	t.Cleanup(func() {
		if errShutdown := plugin.Shutdown(context.Background()); errShutdown != nil {
			t.Errorf("Shutdown() error = %v", errShutdown)
		}
	})

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "tenant-context"))
	cancel()
	plugin.HandleUsage(ctx, coreusage.Record{Model: "gpt-5.6"})
	if gotValue != "tenant-context" {
		t.Fatalf("resolver context value = %q, want tenant-context", gotValue)
	}
}

func TestNewDisabledCreatesNoPlugin(t *testing.T) {
	plugin, errPlugin := New(context.Background(), Options{
		Enabled:  false,
		Endpoint: "not a URL",
	}, nil)
	if errPlugin != nil {
		t.Fatalf("New() disabled error = %v", errPlugin)
	}
	if plugin != nil {
		t.Fatal("New() disabled returned a plugin")
	}
}

func TestNormalizeOptionsRejectsForbiddenResourceAttributes(t *testing.T) {
	for _, name := range []string{
		"service.name",
		"service.version",
		"deployment.environment",
		"host.name",
		"auth_id",
		"auth_index",
		"api_key",
		"request_id",
		"alias",
	} {
		t.Run(name, func(t *testing.T) {
			_, errNormalize := normalizeOptions(Options{
				Enabled:  true,
				Endpoint: "http://llm-otel.example:4328",
				ResourceAttributes: map[string]string{
					name: "must-not-leave-host",
				},
			})
			if errNormalize == nil {
				t.Fatalf("normalizeOptions() accepted forbidden resource attribute %q", name)
			}
		})
	}
}

func TestHandleUsageDoesNotWaitForStalledExporter(t *testing.T) {
	exporter := &stalledExporter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(time.Millisecond))
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	plugin, errPlugin := newPluginWithMeterProvider(meterProvider, func(context.Context, coreusage.Record) (string, bool) {
		return "user@example.com", true
	})
	if errPlugin != nil {
		t.Fatalf("newPluginWithMeterProvider() error = %v", errPlugin)
	}
	t.Cleanup(func() {
		exporter.unblock()
		if errShutdown := plugin.Shutdown(context.Background()); errShutdown != nil {
			t.Errorf("Shutdown() error = %v", errShutdown)
		}
	})

	record := coreusage.Record{
		Provider: "codex",
		Model:    "gpt-5.6",
		Latency:  time.Second,
		Detail:   coreusage.Detail{TotalTokens: 10},
	}
	plugin.HandleUsage(context.Background(), record)
	select {
	case <-exporter.started:
	case <-time.After(time.Second):
		t.Fatal("periodic exporter did not start")
	}

	returned := make(chan struct{})
	go func() {
		plugin.HandleUsage(context.Background(), record)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("HandleUsage blocked behind stalled exporter")
	}

	exporter.unblock()
	if errShutdown := plugin.Shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("Shutdown() error = %v", errShutdown)
	}
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var metrics metricdata.ResourceMetrics
	if errCollect := reader.Collect(context.Background(), &metrics); errCollect != nil {
		t.Fatalf("Collect() error = %v", errCollect)
	}
	return metrics
}

func metricAttributeSets(t *testing.T, metric metricdata.Metrics) []attribute.Set {
	t.Helper()
	switch data := metric.Data.(type) {
	case metricdata.Sum[int64]:
		sets := make([]attribute.Set, 0, len(data.DataPoints))
		for _, point := range data.DataPoints {
			sets = append(sets, point.Attributes)
		}
		return sets
	case metricdata.Histogram[float64]:
		sets := make([]attribute.Set, 0, len(data.DataPoints))
		for _, point := range data.DataPoints {
			sets = append(sets, point.Attributes)
		}
		return sets
	default:
		t.Fatalf("metric %q has unexpected data type %T", metric.Name, metric.Data)
		return nil
	}
}

func attributeKeys(set attribute.Set) []string {
	values := set.ToSlice()
	keys := make([]string, 0, len(values))
	for _, value := range values {
		keys = append(keys, string(value.Key))
	}
	sort.Strings(keys)
	return keys
}

type stalledExporter struct {
	started     chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

func (e *stalledExporter) Temporality(kind sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(kind)
}

func (e *stalledExporter) Aggregation(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(kind)
}

func (e *stalledExporter) Export(ctx context.Context, _ *metricdata.ResourceMetrics) error {
	e.once.Do(func() { close(e.started) })
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *stalledExporter) ForceFlush(context.Context) error {
	return nil
}

func (e *stalledExporter) Shutdown(context.Context) error {
	return nil
}

func (e *stalledExporter) unblock() {
	e.releaseOnce.Do(func() { close(e.release) })
}
