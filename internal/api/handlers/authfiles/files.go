package authfiles

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const MaxUploadBytes int64 = 4 << 20

// RecordPersister parses and registers auth records through the management
// handler's upstream implementation, so tenant uploads always use the same
// parsing, ID, and metadata normalization rules as management uploads.
type RecordPersister interface {
	AuthIDResolver
	BuildAuthFromFileData(path string, data []byte) (*coreauth.Auth, error)
	UpsertAuthRecord(ctx context.Context, auth *coreauth.Auth) error
}

// AuthIDResolver maps an auth file path to its stable runtime auth ID.
type AuthIDResolver interface {
	AuthIDForPath(path string) string
}

// IsUnsafeAuthFileName rejects names that could escape the configured auth
// directory. It mirrors the management handler's isUnsafeAuthFileName; a parity
// test in the management package keeps the two identical.
func IsUnsafeAuthFileName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	if strings.ContainsAny(name, "/\\") {
		return true
	}
	return filepath.VolumeName(name) != ""
}

// LockRegistration acquires RegistrationMu and returns its release function.
func LockRegistration() func() {
	RegistrationMu.Lock()
	return RegistrationMu.Unlock
}

// PrepareAuthFileWrite validates an auth file write before it reaches disk:
// safe JSON name, destination inside the auth directory, no symbolic link, and
// no duplicate tenant registration. On success RegistrationMu stays held until
// the returned release function runs, and the auth directory exists.
func PrepareAuthFileWrite(ctx context.Context, cfg *config.Config, manager *coreauth.Manager, ids AuthIDResolver, name, dst string, auth *coreauth.Auth) (func(), error) {
	release := LockRegistration()
	if errValidate := validateAuthFileWrite(ctx, cfg, manager, ids, name, dst, auth); errValidate != nil {
		release()
		return nil, errValidate
	}
	return release, nil
}

func validateAuthFileWrite(ctx context.Context, cfg *config.Config, manager *coreauth.Manager, ids AuthIDResolver, name, dst string, auth *coreauth.Auth) error {
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
	if !PathWithinAuthDir(cfg, dst) {
		return fmt.Errorf("auth path is outside auth directory")
	}
	if info, errLstat := os.Lstat(dst); errLstat == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("auth path must not be a symbolic link")
	} else if errLstat != nil && !os.IsNotExist(errLstat) {
		return fmt.Errorf("inspect auth path: %w", errLstat)
	}
	if errCheck := CheckUserRegistration(ctx, cfg, manager, ids, auth); errCheck != nil {
		return errCheck
	}
	if errMkdir := os.MkdirAll(filepath.Dir(dst), 0o700); errMkdir != nil {
		return fmt.Errorf("failed to create auth directory: %w", errMkdir)
	}
	return nil
}

// SecureAuthFile enforces owner-only permissions after an auth file write,
// including when the file already existed with broader permissions.
func SecureAuthFile(path string) error {
	if errChmod := os.Chmod(path, 0o600); errChmod != nil {
		return fmt.Errorf("failed to secure auth file: %w", errChmod)
	}
	return nil
}

// WriteAuthFile writes a validated JSON auth file with owner-only permissions and
// updates the runtime auth manager through the management persistence path.
// Unlike management uploads, it does not run the post-auth persist hook.
func WriteAuthFile(ctx context.Context, cfg *config.Config, manager *coreauth.Manager, persister RecordPersister, name string, data []byte) error {
	if cfg == nil {
		return fmt.Errorf("config is unavailable")
	}
	if persister == nil {
		return fmt.Errorf("auth record persister is unavailable")
	}
	dst := filepath.Join(cfg.AuthDir, filepath.Base(strings.TrimSpace(name)))
	if !filepath.IsAbs(dst) {
		if abs, errAbs := filepath.Abs(dst); errAbs == nil {
			dst = abs
		}
	}
	auth, errBuild := persister.BuildAuthFromFileData(dst, data)
	if errBuild != nil {
		return errBuild
	}
	release, errPrepare := PrepareAuthFileWrite(ctx, cfg, manager, persister, name, dst, auth)
	if errPrepare != nil {
		return errPrepare
	}
	defer release()
	if errWrite := os.WriteFile(dst, data, 0o600); errWrite != nil {
		return fmt.Errorf("failed to write file: %w", errWrite)
	}
	if errSecure := SecureAuthFile(dst); errSecure != nil {
		return errSecure
	}
	return persister.UpsertAuthRecord(ctx, auth)
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
