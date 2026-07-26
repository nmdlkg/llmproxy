package access

import (
	"testing"

	useraccess "github.com/router-for-me/CLIProxyAPI/v7/internal/access/user_access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

func TestApplyAccessProvidersOrdersAndDisablesTenantProvider(t *testing.T) {
	sdkaccess.UnregisterProvider(useraccess.ProviderName)
	sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
	t.Cleanup(func() {
		sdkaccess.UnregisterProvider(useraccess.ProviderName)
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
	})

	store, errOpen := tenancy.OpenSQLitePath(":memory:")
	if errOpen != nil {
		t.Fatalf("OpenSQLitePath() error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("Close() error = %v", errClose)
		}
	})

	manager := sdkaccess.NewManager()
	enabled := &config.Config{Tenancy: config.TenancyConfig{Enabled: true}}
	enabled.APIKeys = []string{"service-key"}
	if _, errApply := ApplyAccessProviders(manager, nil, enabled, store); errApply != nil {
		t.Fatalf("ApplyAccessProviders(enabled) error = %v", errApply)
	}
	providers := manager.Providers()
	if len(providers) != 2 {
		t.Fatalf("enabled providers = %d, want 2", len(providers))
	}
	if got := providers[0].Identifier(); got != useraccess.ProviderName {
		t.Fatalf("first provider = %q, want %q", got, useraccess.ProviderName)
	}

	disabled := &config.Config{}
	disabled.APIKeys = []string{"service-key"}
	if _, errApply := ApplyAccessProviders(manager, enabled, disabled, nil); errApply != nil {
		t.Fatalf("ApplyAccessProviders(disabled) error = %v", errApply)
	}
	for _, provider := range manager.Providers() {
		if provider.Identifier() == useraccess.ProviderName {
			t.Fatal("tenant provider remains registered while tenancy is disabled")
		}
	}
}
