package api

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/otelusage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestOTelUsageOptionsFromEnvironmentDisabled(t *testing.T) {
	t.Setenv("LLMPROXY_OTEL_ENABLED", "false")
	t.Setenv("LLMPROXY_OTEL_ENDPOINT", "not a URL")
	t.Setenv("LLMPROXY_OTEL_INTERVAL_SECONDS", "not a number")

	options, errOptions := otelUsageOptionsFromEnvironment()
	if errOptions != nil {
		t.Fatalf("otelUsageOptionsFromEnvironment() error = %v", errOptions)
	}
	if options.Enabled {
		t.Fatal("disabled environment produced enabled options")
	}
}

func TestOTelUsageOptionsFromEnvironmentEnabled(t *testing.T) {
	t.Setenv("LLMPROXY_OTEL_ENABLED", "true")
	t.Setenv("LLMPROXY_OTEL_ENDPOINT", "http://llm-otel.example:4328")
	t.Setenv("LLMPROXY_OTEL_INTERVAL_SECONDS", "20")
	t.Setenv("LLMPROXY_OTEL_ENVIRONMENT", "staging")

	options, errOptions := otelUsageOptionsFromEnvironment()
	if errOptions != nil {
		t.Fatalf("otelUsageOptionsFromEnvironment() error = %v", errOptions)
	}
	if !options.Enabled {
		t.Fatal("enabled environment produced disabled options")
	}
	if options.Endpoint != "http://llm-otel.example:4328" {
		t.Fatalf("Endpoint = %q", options.Endpoint)
	}
	if options.ExportInterval != 20*time.Second {
		t.Fatalf("ExportInterval = %v, want 20s", options.ExportInterval)
	}
	if options.Environment != "staging" {
		t.Fatalf("Environment = %q, want staging", options.Environment)
	}
	if options.ResourceAttributes["host.name"] == "" {
		t.Fatal("host.name resource attribute is empty")
	}
	hostName, errHostName := os.Hostname()
	if errHostName != nil {
		t.Fatalf("os.Hostname() error = %v", errHostName)
	}
	if options.ResourceAttributes["host.name"] != hostName {
		t.Fatalf("host.name = %q, want %q", options.ResourceAttributes["host.name"], hostName)
	}
}

func TestRegisterOTelUsageDisabledDoesNotRegister(t *testing.T) {
	registered := false
	sink, errRegister := registerOTelUsage(
		context.Background(),
		otelusage.Options{Enabled: false, Endpoint: "not a URL"},
		nil,
		func(string, coreusage.Plugin) { registered = true },
	)
	if errRegister != nil {
		t.Fatalf("registerOTelUsage() error = %v", errRegister)
	}
	if sink != nil {
		t.Fatal("registerOTelUsage() returned a sink while disabled")
	}
	if registered {
		t.Fatal("disabled OpenTelemetry usage plugin was registered")
	}
}
