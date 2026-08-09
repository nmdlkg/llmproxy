package authfiles

import (
	"context"
	"strings"

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
