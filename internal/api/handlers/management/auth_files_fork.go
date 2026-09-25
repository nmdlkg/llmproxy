package management

import (
	"context"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Exported wrappers around the upstream auth file persistence helpers. They let
// the tenant user API (via authfiles.RecordPersister) reuse the exact upstream
// parsing, ID, and upsert rules without copying them, so upstream fixes apply to
// tenant uploads automatically.

// BuildAuthFromFileData parses one auth file into its runtime record.
func (h *Handler) BuildAuthFromFileData(path string, data []byte) (*coreauth.Auth, error) {
	return h.buildAuthFromFileData(path, data)
}

// UpsertAuthRecord updates or registers a parsed auth record.
func (h *Handler) UpsertAuthRecord(ctx context.Context, auth *coreauth.Auth) error {
	return h.upsertAuthRecord(ctx, auth)
}

// AuthIDForPath returns the stable runtime auth ID for an auth file path.
func (h *Handler) AuthIDForPath(path string) string {
	return h.authIDForPath(path)
}
