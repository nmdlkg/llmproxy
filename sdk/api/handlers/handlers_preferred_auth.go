package handlers

import (
	"context"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Fork seam: soft preferred-auth IDs carried from tenancy credential
// validation to the scheduler first pass. Kept out of handlers_context.go and
// handlers.go so upstream context and metadata edits do not conflict.

type preferredAuthIDsContextKey struct{}

// WithPreferredAuthIDs returns a child context that softly prefers the supplied auth IDs.
func WithPreferredAuthIDs(ctx context.Context, authIDs []string) context.Context {
	authIDs = normalizePreferredAuthIDs(authIDs)
	if len(authIDs) == 0 {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, preferredAuthIDsContextKey{}, authIDs)
}

// addPreferredAuthIDsMetadata copies the soft preferred-auth IDs from ctx into
// execution metadata for the scheduler first pass.
func addPreferredAuthIDsMetadata(ctx context.Context, meta map[string]any) {
	if meta == nil {
		return
	}
	if preferred := preferredAuthIDsFromContext(ctx); len(preferred) > 0 {
		meta[coreauth.PreferredAuthIDsMetadataKey] = preferred
	}
}

func preferredAuthIDsFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	raw := ctx.Value(preferredAuthIDsContextKey{})
	switch value := raw.(type) {
	case []string:
		return normalizePreferredAuthIDs(value)
	case string:
		return normalizePreferredAuthIDs([]string{value})
	case []byte:
		return normalizePreferredAuthIDs([]string{string(value)})
	case []any:
		authIDs := make([]string, 0, len(value))
		for _, item := range value {
			switch typed := item.(type) {
			case string:
				authIDs = append(authIDs, typed)
			case []byte:
				authIDs = append(authIDs, string(typed))
			}
		}
		return normalizePreferredAuthIDs(authIDs)
	default:
		return nil
	}
}

func normalizePreferredAuthIDs(authIDs []string) []string {
	normalized := make([]string, 0, len(authIDs))
	for _, authID := range authIDs {
		if authID = strings.TrimSpace(authID); authID != "" {
			normalized = append(normalized, authID)
		}
	}
	return normalized
}
