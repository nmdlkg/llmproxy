package authfiles

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// StampOwnerFromContext is the built-in post-auth hook for user-initiated OAuth.
// Management requests have no tenancy user in their context and remain unchanged.
func StampOwnerFromContext(ctx context.Context, auth *coreauth.Auth) error {
	user, ok := tenancy.UserFromContext(ctx)
	if !ok || auth == nil {
		return nil
	}
	userID := strings.TrimSpace(user.ID)
	if userID == "" {
		return nil
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["owner_user_id"] = userID
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["owner_user_id"] = userID
	return nil
}

// ComposeOwnerStampHook returns a post-auth hook that stamps the resolved tenant
// owner before running the optional custom hook, so custom hooks observe the
// owner. FinalizeTokenRecord stamps again afterwards so a custom hook cannot
// replace the resolved owner.
func ComposeOwnerStampHook(next coreauth.PostAuthHook) coreauth.PostAuthHook {
	return func(ctx context.Context, auth *coreauth.Auth) error {
		if errOwner := StampOwnerFromContext(ctx, auth); errOwner != nil {
			return fmt.Errorf("owner post-auth hook failed: %w", errOwner)
		}
		if next == nil {
			return nil
		}
		return next(ctx, auth)
	}
}

// FinalizeTokenRecord runs the tenant persistence policy for an OAuth token
// record after all post-auth hooks and before it is saved: it re-stamps the
// resolved owner and rejects credentials already registered by anyone.
// Callers must hold RegistrationMu.
func FinalizeTokenRecord(ctx context.Context, cfg *config.Config, manager *coreauth.Manager, ids AuthIDResolver, record *coreauth.Auth) error {
	if errOwner := StampOwnerFromContext(ctx, record); errOwner != nil {
		return fmt.Errorf("owner post-auth hook failed: %w", errOwner)
	}
	return CheckUserRegistration(ctx, cfg, manager, ids, record)
}
