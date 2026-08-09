package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/otelusage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
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
	if options.HostName == "" {
		t.Fatal("HostName is empty")
	}
	hostName, errHostName := os.Hostname()
	if errHostName != nil {
		t.Fatalf("os.Hostname() error = %v", errHostName)
	}
	if options.HostName != hostName {
		t.Fatalf("HostName = %q, want %q", options.HostName, hostName)
	}
}

func TestOTelUsageOptionsFromConfig(t *testing.T) {
	previousVersion := buildinfo.Version
	buildinfo.Version = "v9.8.7"
	t.Cleanup(func() {
		buildinfo.Version = previousVersion
	})

	cfg := &config.Config{OTel: &config.OTelConfig{
		Enabled:        true,
		Endpoint:       "http://collector.tailnet:4328",
		ExportInterval: "20s",
		ServiceName:    "proxy-test",
		Environment:    "staging",
		ResourceAttributes: map[string]string{
			"region": "ap-northeast-2",
		},
	}}
	options, usedEnvironmentFallback, errOptions := otelUsageOptions(cfg)
	if errOptions != nil {
		t.Fatalf("otelUsageOptions() error = %v", errOptions)
	}
	if usedEnvironmentFallback {
		t.Fatal("config section unexpectedly used environment fallback")
	}
	if !options.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if options.Endpoint != "http://collector.tailnet:4328" {
		t.Fatalf("Endpoint = %q", options.Endpoint)
	}
	if options.ExportInterval != 20*time.Second {
		t.Fatalf("ExportInterval = %v, want 20s", options.ExportInterval)
	}
	if options.ServiceName != "proxy-test" {
		t.Fatalf("ServiceName = %q, want proxy-test", options.ServiceName)
	}
	if options.ServiceVersion != "v9.8.7" {
		t.Fatalf("ServiceVersion = %q, want build version", options.ServiceVersion)
	}
	if options.Environment != "staging" {
		t.Fatalf("Environment = %q, want staging", options.Environment)
	}
	if options.ResourceAttributes["region"] != "ap-northeast-2" {
		t.Fatalf("ResourceAttributes = %#v", options.ResourceAttributes)
	}
	if options.HostName == "" {
		t.Fatal("HostName is empty")
	}
}

func TestOTelUsageEnvironmentFallbackPrecedence(t *testing.T) {
	t.Setenv("LLMPROXY_OTEL_ENABLED", "true")
	t.Setenv("LLMPROXY_OTEL_ENDPOINT", "http://environment.example:4328")

	options, usedEnvironmentFallback, errOptions := otelUsageOptions(&config.Config{})
	if errOptions != nil {
		t.Fatalf("otelUsageOptions(absent) error = %v", errOptions)
	}
	if !usedEnvironmentFallback {
		t.Fatal("absent section did not use environment fallback")
	}
	if !options.Enabled || options.Endpoint != "http://environment.example:4328" {
		t.Fatalf("environment fallback options = %#v", options)
	}

	options, usedEnvironmentFallback, errOptions = otelUsageOptions(&config.Config{
		OTel: &config.OTelConfig{Enabled: false},
	})
	if errOptions != nil {
		t.Fatalf("otelUsageOptions(disabled config) error = %v", errOptions)
	}
	if usedEnvironmentFallback {
		t.Fatal("present config section used environment fallback")
	}
	if options.Enabled {
		t.Fatal("disabled config was overridden by enabled environment")
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

func TestTenancyUsageEmailResolverUsesSharedIdentityPath(t *testing.T) {
	dataDir := t.TempDir()
	service, errService := tenancy.NewService(config.TenancyConfig{
		Enabled: true,
		DBPath:  filepath.Join(dataDir, "tenancy.db"),
		Quota: config.TenancyQuota{
			Window:  "24h",
			BaseUSD: map[string]config.USDLimit{"default": 1},
		},
	}, dataDir, nil)
	if errService != nil {
		t.Fatalf("tenancy.NewService() error = %v", errService)
	}
	t.Cleanup(func() {
		if errClose := service.Close(); errClose != nil {
			t.Errorf("tenancy service Close() error = %v", errClose)
		}
	})

	user := &tenancy.User{Email: "tenant@example.com", Role: tenancy.RoleUser, Tier: "default"}
	if errCreate := service.Store().CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	plaintext, _, errIssue := service.Store().IssueAPIKey(user.ID, "otel-test")
	if errIssue != nil {
		t.Fatalf("IssueAPIKey() error = %v", errIssue)
	}
	resolver := tenancyUsageEmailResolver(service)
	if resolver == nil {
		t.Fatal("tenancyUsageEmailResolver() = nil")
	}

	ctx, cancel := context.WithCancel(tenancy.WithUser(context.Background(), user))
	cancel()
	if email, ok := resolver(ctx, coreusage.Record{APIKey: user.ID}); !ok || email != user.Email {
		t.Fatalf("context identity resolution = %q, %t; want %q, true", email, ok, user.Email)
	}
	if email, ok := resolver(context.Background(), coreusage.Record{APIKey: plaintext}); !ok || email != user.Email {
		t.Fatalf("API-key fallback resolution = %q, %t; want %q, true", email, ok, user.Email)
	}
	if email, ok := resolver(context.Background(), coreusage.Record{APIKey: "missing-key"}); ok || email != "" {
		t.Fatalf("unresolved identity = %q, %t; want empty, false", email, ok)
	}
}
