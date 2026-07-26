package tenancy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestNewServiceDisabledIsNoOp(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state", "tenancy.db")
	service, errNew := NewService(config.TenancyConfig{
		Enabled: false,
		DBPath:  databasePath,
	}, t.TempDir(), coreauth.NewManager(nil, nil, nil))
	if errNew != nil {
		t.Fatalf("NewService() error = %v", errNew)
	}
	if service != nil {
		t.Fatalf("service = %#v, want nil", service)
	}
	if _, errStat := os.Stat(databasePath); !os.IsNotExist(errStat) {
		t.Fatalf("database path stat error = %v, want not-exist", errStat)
	}
}

func TestCredentialResolverFiltersOwnedSharedCredentials(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auths := []*coreauth.Auth{
		{
			ID:       "codex-owned-shared",
			Provider: "codex",
			Attributes: map[string]string{
				"owner_user_id": "user-1",
				"shared":        "true",
				"plan_type":     "pro",
			},
		},
		{
			ID:       "claude-owned-shared",
			Provider: "claude",
			Attributes: map[string]string{
				"owner_user_id":     "user-1",
				"shared":            "true",
				"contribution_tier": "team",
			},
		},
		{
			ID:       "owned-private",
			Provider: "gemini",
			Attributes: map[string]string{
				"owner_user_id": "user-1",
				"shared":        "false",
			},
		},
		{
			ID:       "other-user",
			Provider: "gemini",
			Attributes: map[string]string{
				"owner_user_id": "user-2",
				"shared":        "true",
			},
		},
		{
			ID:       "disabled",
			Provider: "gemini",
			Disabled: true,
			Attributes: map[string]string{
				"owner_user_id": "user-1",
				"shared":        "true",
			},
		},
	}
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%q) error = %v", auth.ID, errRegister)
		}
	}

	credentials, errResolve := credentialResolver(manager)("user-1")
	if errResolve != nil {
		t.Fatalf("credential resolver error = %v", errResolve)
	}
	if len(credentials) != 2 {
		t.Fatalf("credentials = %#v, want 2 entries", credentials)
	}
	got := make(map[string]Credential, len(credentials))
	for _, credential := range credentials {
		got[credential.AuthID] = credential
	}
	if credential := got["codex-owned-shared"]; credential.Provider != "codex" || credential.PlanTier != "pro" {
		t.Fatalf("codex credential = %#v", credential)
	}
	if credential := got["claude-owned-shared"]; credential.Provider != "claude" || credential.PlanTier != "team" {
		t.Fatalf("claude credential = %#v", credential)
	}
}

func TestServiceFeedsAndFlushesUsageLedger(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "tenancy.db")
	service, errNew := NewService(config.TenancyConfig{
		Enabled: true,
		DBPath:  databasePath,
		Quota: config.TenancyQuota{
			Window:  "24h",
			BaseUSD: map[string]config.USDLimit{"default": 1000},
			ModelPriceOverrides: map[string]config.ModelPriceOverride{
				"*": {
					Prompt:     modelPrice(1),
					Completion: modelPrice(1),
				},
			},
		},
	}, t.TempDir(), coreauth.NewManager(nil, nil, nil))
	if errNew != nil {
		t.Fatalf("NewService() error = %v", errNew)
	}
	t.Cleanup(func() {
		if errClose := service.Close(); errClose != nil {
			t.Errorf("Close() error = %v", errClose)
		}
	})

	store := service.Store()
	user := &User{Email: "ledger@example.com", Role: RoleUser, Tier: "default"}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	_, _, errIssue := store.IssueAPIKey(user.ID, "ledger")
	if errIssue != nil {
		t.Fatalf("IssueAPIKey() error = %v", errIssue)
	}

	service.usageSink.HandleUsage(WithUser(context.Background(), user), usage.Record{
		APIKey:      user.ID,
		Provider:    "codex",
		Model:       "gpt-test",
		RequestedAt: time.Now().UTC(),
		Detail: usage.Detail{
			InputTokens:  10,
			OutputTokens: 5,
		},
	})
	if errFlush := service.usagePlugin.Flush(context.Background()); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}
	used, errUsed := store.UsedUnits(context.Background(), user.ID, time.Now().Add(-time.Hour))
	if errUsed != nil {
		t.Fatalf("UsedUnits() error = %v", errUsed)
	}
	if used != 15 {
		t.Fatalf("used cost = %d nano-USD, want 15", used)
	}
}

func modelPrice(nanoUSDPerToken int64) *config.USDPerMillionTokens {
	value := config.USDPerMillionTokens(nanoUSDPerToken)
	return &value
}
