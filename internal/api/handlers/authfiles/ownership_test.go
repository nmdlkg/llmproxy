package authfiles

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestStampOwnerFromContextUsesResolvedUser(t *testing.T) {
	auth := &coreauth.Auth{
		Metadata:   map[string]any{"owner_user_id": "client-value"},
		Attributes: map[string]string{"owner_user_id": "client-value"},
	}
	ctx := tenancy.WithUser(context.Background(), &tenancy.User{ID: "resolved-user"})
	if errStamp := StampOwnerFromContext(ctx, auth); errStamp != nil {
		t.Fatalf("StampOwnerFromContext() error = %v", errStamp)
	}
	if got := OwnerUserID(auth); got != "resolved-user" {
		t.Fatalf("owner = %q, want resolved-user", got)
	}
}

func TestStampOwnerFromContextLeavesManagementAuthUnowned(t *testing.T) {
	auth := &coreauth.Auth{Metadata: map[string]any{"type": "codex"}}
	if errStamp := StampOwnerFromContext(context.Background(), auth); errStamp != nil {
		t.Fatalf("StampOwnerFromContext() error = %v", errStamp)
	}
	if got := OwnerUserID(auth); got != "" {
		t.Fatalf("management owner = %q, want empty", got)
	}
}
