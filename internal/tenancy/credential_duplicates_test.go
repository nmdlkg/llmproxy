package tenancy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCredentialResolverExcludesDuplicateContributions(t *testing.T) {
	for _, owner := range []string{"", "other-user", "user"} {
		t.Run("existing-owner="+owner, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			for _, record := range []*coreauth.Auth{
				{ID: "original", Provider: "codex", Attributes: map[string]string{"owner_user_id": owner, "shared": "true", "plan_type": "prolite"}, Metadata: map[string]any{"account_id": "same-account"}},
				{ID: "copy", Provider: "codex", Attributes: map[string]string{"owner_user_id": "user", "shared": "true", "plan_type": "prolite"}, Metadata: map[string]any{"account_id": "same-account"}},
				{ID: "unique", Provider: "codex", Attributes: map[string]string{"owner_user_id": "user", "shared": "true"}, Metadata: map[string]any{"account_id": "unique-account"}},
			} {
				if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			credentials, errResolve := credentialResolver(manager)("user")
			if errResolve != nil {
				t.Fatal(errResolve)
			}
			want := 1
			if owner == "user" {
				want = 2
			}
			if len(credentials) != want {
				t.Fatalf("contributions=%v want count=%d", credentials, want)
			}
		})
	}
}
