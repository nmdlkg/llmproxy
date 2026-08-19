package otelusage

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// harnessValues collects the harness attribute observed on every data point of
// one instrument.
func harnessValues(t *testing.T, metrics metricdata.ResourceMetrics, instrument string) []string {
	t.Helper()
	values := make([]string, 0, 4)
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != instrument {
				continue
			}
			for _, set := range metricAttributeSets(t, metric) {
				value, _ := set.Value("harness")
				values = append(values, value.AsString())
			}
		}
	}
	return values
}

func TestHandleUsageAttachesHarnessFromContext(t *testing.T) {
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

	ctx := constant.WithHarness(context.Background(), constant.HarnessClaudeCode)
	plugin.HandleUsage(ctx, coreusage.Record{
		Provider: "claude",
		Model:    "claude-opus-5",
		// Source carries an upstream account identifier and must not influence
		// the harness attribute.
		Source: "shared-account@example.com",
		Detail: coreusage.Detail{InputTokens: 4, OutputTokens: 2, TotalTokens: 6},
	})

	metrics := collectMetrics(t, reader)
	for _, instrument := range []string{"llmproxy.token.usage", "llmproxy.request.count", "llmproxy.request.duration"} {
		values := harnessValues(t, metrics, instrument)
		if len(values) == 0 {
			t.Fatalf("%s produced no data points", instrument)
		}
		for _, value := range values {
			if value != constant.HarnessClaudeCode {
				t.Errorf("%s harness = %q, want %q", instrument, value, constant.HarnessClaudeCode)
			}
		}
	}
}

func TestHandleUsageDefaultsHarnessToUnknown(t *testing.T) {
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

	plugin.HandleUsage(context.Background(), coreusage.Record{
		Provider: "codex",
		Model:    "gpt-5.6-sol",
		Detail:   coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})

	metrics := collectMetrics(t, reader)
	for _, value := range harnessValues(t, metrics, "llmproxy.request.count") {
		if value != constant.HarnessUnknown {
			t.Errorf("harness = %q, want %q", value, constant.HarnessUnknown)
		}
	}
}
