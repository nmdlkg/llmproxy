package authfiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var ErrCredentialExists = errors.New("This account is already registered. You cannot add it again. Please use a different account.")

// RegistrationMu serializes OAuth and upload checks with persistence, including
// management writes. Disk inspection also covers the file watcher delay.
var RegistrationMu sync.Mutex

// CheckUserRegistration must be called under RegistrationMu before persistence.
// ids resolves on-disk auth file paths to runtime auth IDs so renamed files and
// pending watcher loads are matched by the same IDs the manager uses.
func CheckUserRegistration(ctx context.Context, cfg *config.Config, manager *coreauth.Manager, ids AuthIDResolver, record *coreauth.Auth) error {
	if user, ok := tenancy.UserFromContext(ctx); !ok || strings.TrimSpace(user.ID) == "" {
		return nil
	}
	if ids == nil {
		return fmt.Errorf("auth ID resolver unavailable")
	}
	keys := make(map[string]bool)
	for _, key := range coreauth.CredentialIdentityKeys(record) {
		keys[key] = true
	}
	matches := func(existing *coreauth.Auth) bool {
		if existing == nil {
			return false
		}
		if record.ID != "" && record.ID == existing.ID || record.FileName != "" && record.FileName == existing.FileName {
			return true
		}
		for _, key := range coreauth.CredentialIdentityKeys(existing) {
			if keys[key] {
				return true
			}
		}
		return false
	}
	if manager != nil {
		for _, existing := range manager.List() {
			if !matches(existing) {
				continue
			}
			if stale, path := isStaleFileAuth(cfg, existing); stale {
				// The backing file is gone, so this runtime record no longer
				// represents a registered credential. Management listings already
				// hide these, and counting them here rejects re-registration of an
				// account the user can no longer see or delete.
				log.WithFields(log.Fields{
					"auth_id": existing.ID,
					"path":    path,
				}).Warn("registration check: ignoring runtime record without backing file")
				continue
			}
			logDuplicateMatch(existing)
			return ErrCredentialExists
		}
	}
	if cfg == nil || strings.TrimSpace(cfg.AuthDir) == "" {
		return fmt.Errorf("auth directory unavailable")
	}
	return filepath.WalkDir(cfg.AuthDir, func(path string, entry os.DirEntry, errWalk error) error {
		if errWalk != nil {
			if path == cfg.AuthDir && os.IsNotExist(errWalk) {
				return nil
			}
			return fmt.Errorf("inspect registered credentials: %w", errWalk)
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			return nil
		}
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			return fmt.Errorf("read registered credential: %w", errRead)
		}
		var metadata map[string]any
		if errDecode := json.Unmarshal(data, &metadata); errDecode != nil {
			return fmt.Errorf("inspect registered credential: %w", errDecode)
		}
		provider, _ := metadata["type"].(string)
		candidate := &coreauth.Auth{ID: ids.AuthIDForPath(path), FileName: entry.Name(), Provider: provider, Metadata: metadata}
		if matches(candidate) {
			logDuplicateMatch(candidate)
			return ErrCredentialExists
		}
		return nil
	})
}

// isStaleFileAuth reports whether a file-backed runtime record lost its file.
// Records that are not file-backed (config API keys, plugin virtual auths,
// runtime-only entries) are left untouched.
func isStaleFileAuth(cfg *config.Config, auth *coreauth.Auth) (bool, string) {
	if auth == nil || coreauth.IsPluginVirtualAuth(auth) || coreauth.IsConfigAPIKeyAuth(auth) {
		return false, ""
	}
	if auth.AuthSourceKind() != coreauth.AuthSourceFile {
		return false, ""
	}
	path := AuthPath(auth)
	if path == "" {
		// Records synthesized without a path attribute are hidden by the
		// management listing, so resolve the conventional location instead of
		// treating them as registered.
		fileName := strings.TrimSpace(auth.FileName)
		if fileName == "" || cfg == nil || strings.TrimSpace(cfg.AuthDir) == "" {
			return false, ""
		}
		if IsUnsafeAuthFileName(fileName) {
			return false, ""
		}
		path = filepath.Join(cfg.AuthDir, fileName)
	}
	if _, errStat := os.Stat(path); os.IsNotExist(errStat) {
		return true, path
	}
	return false, path
}

// logDuplicateMatch records which credential caused a rejection. The duplicate
// error deliberately hides the other account, so operators need this to explain
// a 409 that has no visible counterpart in the management UI.
func logDuplicateMatch(existing *coreauth.Auth) {
	if existing == nil {
		return
	}
	log.WithFields(log.Fields{
		"auth_id":  existing.ID,
		"provider": existing.Provider,
		"path":     AuthPath(existing),
		"owner":    OwnerUserID(existing),
	}).Warn("registration check: rejected duplicate credential")
}

// OAuthSaveError exposes an actionable duplicate warning without leaking other
// storage errors or another user's account details.
func OAuthSaveError(err error) string {
	if errors.Is(err, ErrCredentialExists) {
		return ErrCredentialExists.Error()
	}
	return "Failed to save authentication tokens"
}
