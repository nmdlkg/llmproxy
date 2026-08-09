package authfiles

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const MaxUploadBytes int64 = 4 << 20

var lastRefreshKeys = []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"}

// IsUnsafeAuthFileName rejects names that could escape the configured auth directory.
func IsUnsafeAuthFileName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	if strings.ContainsAny(name, "/\\") {
		return true
	}
	return filepath.VolumeName(name) != ""
}

// AuthIDForPath returns the same stable auth ID used by the management API.
func AuthIDForPath(cfg *config.Config, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		if abs, errAbs := filepath.Abs(path); errAbs == nil {
			path = abs
		}
	}
	id := path
	if cfg != nil {
		authDir := strings.TrimSpace(cfg.AuthDir)
		if resolvedAuthDir, errResolve := util.ResolveAuthDir(authDir); errResolve == nil && resolvedAuthDir != "" {
			authDir = resolvedAuthDir
		}
		if authDir != "" {
			authDir = filepath.Clean(authDir)
			if !filepath.IsAbs(authDir) {
				if abs, errAbs := filepath.Abs(authDir); errAbs == nil {
					authDir = abs
				}
			}
			if rel, errRel := filepath.Rel(authDir, path); errRel == nil && rel != "" {
				id = rel
			}
		}
	}
	if runtime.GOOS == "windows" {
		id = strings.ToLower(id)
	}
	return id
}

// BuildAuthFromFileData parses one auth file and synthesizes its runtime record.
func BuildAuthFromFileData(cfg *config.Config, manager *coreauth.Manager, path string, data []byte) (*coreauth.Auth, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("auth path is empty")
	}
	if data == nil {
		var errRead error
		data, errRead = os.ReadFile(path)
		if errRead != nil {
			return nil, fmt.Errorf("failed to read auth file: %w", errRead)
		}
	}
	metadata := make(map[string]any)
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return nil, fmt.Errorf("invalid auth file: %w", errUnmarshal)
	}
	provider, _ := metadata["type"].(string)
	if provider == "" {
		provider = "unknown"
	}
	label := provider
	if email, ok := metadata["email"].(string); ok && email != "" {
		label = email
	}

	authID := AuthIDForPath(cfg, path)
	if authID == "" {
		authID = path
	}
	var auth *coreauth.Auth
	if cfg != nil {
		synthesisContext := &synthesizer.SynthesisContext{
			Config:      cfg,
			AuthDir:     cfg.AuthDir,
			Now:         time.Now(),
			IDGenerator: synthesizer.NewStableIDGenerator(),
		}
		if generated := synthesizer.SynthesizeAuthFile(synthesisContext, path, data); len(generated) > 0 && generated[0] != nil {
			auth = generated[0].Clone()
		}
	}
	if auth == nil {
		auth = &coreauth.Auth{
			ID:       authID,
			Provider: provider,
			Label:    label,
			Status:   coreauth.StatusActive,
			Attributes: map[string]string{
				coreauth.AttributePath:   path,
				coreauth.AttributeSource: path,
			},
			Metadata:  metadata,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
	}
	auth.ID = authID
	auth.FileName = filepath.Base(path)
	lastRefresh, hasLastRefresh := extractLastRefreshTimestamp(metadata)
	if hasLastRefresh {
		auth.LastRefreshedAt = lastRefresh
	}
	if manager != nil {
		if existing, ok := manager.GetByID(authID); ok {
			auth.CreatedAt = existing.CreatedAt
			if !hasLastRefresh {
				auth.LastRefreshedAt = existing.LastRefreshedAt
			}
			auth.NextRefreshAfter = existing.NextRefreshAfter
			auth.Runtime = existing.Runtime
		}
	}
	coreauth.ApplyCustomHeadersFromMetadata(auth)
	return auth, nil
}

func extractLastRefreshTimestamp(metadata map[string]any) (time.Time, bool) {
	for _, key := range lastRefreshKeys {
		if value, ok := metadata[key]; ok {
			if timestamp, valid := parseLastRefreshValue(value); valid {
				return timestamp, true
			}
		}
	}
	return time.Time{}, false
}

func parseLastRefreshValue(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return time.Time{}, false
		}
		for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00"} {
			if timestamp, errParse := time.Parse(layout, text); errParse == nil {
				return timestamp.UTC(), true
			}
		}
		if unix, errParse := strconv.ParseInt(text, 10, 64); errParse == nil && unix > 0 {
			return time.Unix(unix, 0).UTC(), true
		}
	case float64:
		if typed > 0 {
			return time.Unix(int64(typed), 0).UTC(), true
		}
	case int64:
		if typed > 0 {
			return time.Unix(typed, 0).UTC(), true
		}
	case int:
		if typed > 0 {
			return time.Unix(int64(typed), 0).UTC(), true
		}
	case json.Number:
		if unix, errParse := typed.Int64(); errParse == nil && unix > 0 {
			return time.Unix(unix, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

// UpsertAuthRecord updates or registers a parsed auth record.
func UpsertAuthRecord(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth) error {
	if manager == nil || auth == nil {
		return nil
	}
	if existing, ok := manager.GetByID(auth.ID); ok {
		auth.CreatedAt = existing.CreatedAt
		_, errUpdate := manager.Update(ctx, auth)
		return errUpdate
	}
	_, errRegister := manager.Register(ctx, auth)
	return errRegister
}

// WriteAuthFile writes a validated JSON auth file with owner-only permissions and
// updates the runtime auth manager.
func WriteAuthFile(ctx context.Context, cfg *config.Config, manager *coreauth.Manager, name string, data []byte) error {
	if cfg == nil {
		return fmt.Errorf("config is unavailable")
	}
	name = strings.TrimSpace(name)
	if IsUnsafeAuthFileName(name) {
		return fmt.Errorf("invalid name")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		return fmt.Errorf("name must end with .json")
	}
	dst := filepath.Join(cfg.AuthDir, filepath.Base(name))
	if !filepath.IsAbs(dst) {
		if abs, errAbs := filepath.Abs(dst); errAbs == nil {
			dst = abs
		}
	}
	if !PathWithinAuthDir(cfg, dst) {
		return fmt.Errorf("auth path is outside auth directory")
	}
	if info, errLstat := os.Lstat(dst); errLstat == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("auth path must not be a symbolic link")
	} else if errLstat != nil && !os.IsNotExist(errLstat) {
		return fmt.Errorf("inspect auth path: %w", errLstat)
	}
	auth, errBuild := BuildAuthFromFileData(cfg, manager, dst, data)
	if errBuild != nil {
		return errBuild
	}
	if errMkdir := os.MkdirAll(filepath.Dir(dst), 0o700); errMkdir != nil {
		return fmt.Errorf("failed to create auth directory: %w", errMkdir)
	}
	if errWrite := os.WriteFile(dst, data, 0o600); errWrite != nil {
		return fmt.Errorf("failed to write file: %w", errWrite)
	}
	if errChmod := os.Chmod(dst, 0o600); errChmod != nil {
		return fmt.Errorf("failed to secure auth file: %w", errChmod)
	}
	if errUpsert := UpsertAuthRecord(ctx, manager, auth); errUpsert != nil {
		return errUpsert
	}
	return nil
}

// OwnerUserID returns ownership only from the stored auth record.
func OwnerUserID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["owner_user_id"].(string); ok {
			if owner := strings.TrimSpace(value); owner != "" {
				return owner
			}
		}
	}
	if auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["owner_user_id"])
}

// AuthPath returns the stored backing path for an auth record.
func AuthPath(auth *coreauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes[coreauth.AttributePath])
}

// FindAuth resolves an auth by runtime ID, file name, or stored path basename.
func FindAuth(manager *coreauth.Manager, id string) (*coreauth.Auth, bool) {
	if manager == nil {
		return nil, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, false
	}
	if auth, ok := manager.GetByID(id); ok {
		return auth, true
	}
	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.FileName) == id || filepath.Base(AuthPath(auth)) == id {
			return auth, true
		}
	}
	return nil, false
}

// PathWithinAuthDir verifies that a stored path remains inside cfg.AuthDir.
func PathWithinAuthDir(cfg *config.Config, path string) bool {
	if cfg == nil || strings.TrimSpace(cfg.AuthDir) == "" || strings.TrimSpace(path) == "" {
		return false
	}
	authDir, errAuthDir := filepath.Abs(cfg.AuthDir)
	if errAuthDir != nil {
		return false
	}
	target, errTarget := filepath.Abs(path)
	if errTarget != nil {
		return false
	}
	if resolvedAuthDir, errResolvedAuthDir := filepath.EvalSymlinks(authDir); errResolvedAuthDir == nil {
		authDir = resolvedAuthDir
	}
	if resolvedTarget, errResolvedTarget := filepath.EvalSymlinks(target); errResolvedTarget == nil {
		target = resolvedTarget
	} else if resolvedParent, errResolvedParent := filepath.EvalSymlinks(filepath.Dir(target)); errResolvedParent == nil {
		target = filepath.Join(resolvedParent, filepath.Base(target))
	}
	relative, errRelative := filepath.Rel(authDir, target)
	if errRelative != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
