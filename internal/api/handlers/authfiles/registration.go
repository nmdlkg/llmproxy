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
)

var ErrCredentialExists = errors.New("This account is already registered. You cannot add it again. Please use a different account.")

// RegistrationMu serializes OAuth and upload checks with persistence, including
// management writes. Disk inspection also covers the file watcher delay.
var RegistrationMu sync.Mutex

// CheckUserRegistration must be called under RegistrationMu before persistence.
func CheckUserRegistration(ctx context.Context, cfg *config.Config, manager *coreauth.Manager, record *coreauth.Auth) error {
	if user, ok := tenancy.UserFromContext(ctx); !ok || strings.TrimSpace(user.ID) == "" {
		return nil
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
			if matches(existing) {
				return ErrCredentialExists
			}
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
		if matches(&coreauth.Auth{ID: AuthIDForPath(cfg, path), FileName: entry.Name(), Provider: provider, Metadata: metadata}) {
			return ErrCredentialExists
		}
		return nil
	})
}

// OAuthSaveError exposes an actionable duplicate warning without leaking other
// storage errors or another user's account details.
func OAuthSaveError(err error) string {
	if errors.Is(err, ErrCredentialExists) {
		return ErrCredentialExists.Error()
	}
	return "Failed to save authentication tokens"
}
