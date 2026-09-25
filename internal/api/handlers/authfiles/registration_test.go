package authfiles_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newPersister(t *testing.T) (*config.Config, *coreauth.Manager, *management.Handler) {
	t.Helper()
	cfg := &config.Config{AuthDir: t.TempDir()}
	manager := coreauth.NewManager(nil, nil, nil)
	return cfg, manager, management.NewHandlerWithoutConfigFilePath(cfg, manager)
}

func tenantContext(userID string) context.Context {
	return tenancy.WithUser(context.Background(), &tenancy.User{ID: userID})
}

func TestRegistrationMatchesAccountIdentity(t *testing.T) {
	for _, field := range []string{"account_id", "email", "access_token", "refresh_token"} {
		t.Run(field, func(t *testing.T) {
			cfg, manager, persister := newPersister(t)
			raw := []byte(fmt.Sprintf(`{"type":"codex",%q:"same-value","disabled":true}`, field))
			if errWrite := authfiles.WriteAuthFile(context.Background(), cfg, manager, persister, "admin.json", raw); errWrite != nil {
				t.Fatal(errWrite)
			}
			record := &coreauth.Auth{ID: "renamed.json", Provider: "codex", Metadata: map[string]any{field: "same-value"}}
			ctx := tenantContext("user")
			release := authfiles.LockRegistration()
			defer release()
			if errCheck := authfiles.CheckUserRegistration(ctx, cfg, nil, persister, record); !errors.Is(errCheck, authfiles.ErrCredentialExists) {
				t.Fatalf("duplicate err=%v", errCheck)
			}
			record.Provider = "claude"
			if errCheck := authfiles.CheckUserRegistration(ctx, cfg, nil, persister, record); errCheck != nil {
				t.Fatalf("other provider err=%v", errCheck)
			}
		})
	}
}

func TestConcurrentRegistrationBeforeWatcherLoad(t *testing.T) {
	cfg, _, persister := newPersister(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A nil manager models records the file watcher has not loaded yet,
			// so only the on-disk scan can detect the duplicate.
			results <- authfiles.WriteAuthFile(tenantContext(fmt.Sprintf("user-%d", i)), cfg, nil, persister, fmt.Sprintf("copy-%d.json", i), []byte(`{"type":"codex","account_id":"same-account"}`))
		}(i)
	}
	wg.Wait()
	close(results)
	successes, duplicates := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, authfiles.ErrCredentialExists) {
			duplicates++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("successes=%d duplicates=%d", successes, duplicates)
	}
}

func TestWriteAuthFileRequiresPersister(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	if errWrite := authfiles.WriteAuthFile(context.Background(), cfg, nil, nil, "a.json", []byte(`{"type":"codex"}`)); errWrite == nil {
		t.Fatal("WriteAuthFile accepted a nil persister")
	}
	if _, errStat := os.Stat(filepath.Join(cfg.AuthDir, "a.json")); !os.IsNotExist(errStat) {
		t.Fatalf("file written without persister: %v", errStat)
	}
}

// TestTenantUploadUsesUpstreamParsing verifies tenant uploads share the
// management parser, including legacy credential metadata key normalization.
func TestTenantUploadUsesUpstreamParsing(t *testing.T) {
	cfg, manager, persister := newPersister(t)
	raw := []byte(`{"type":"codex","email":"tenant@example.com","base-url":"https://example.invalid/v1","owner_user_id":"user-a","shared":false}`)
	if errWrite := authfiles.WriteAuthFile(tenantContext("user-a"), cfg, manager, persister, "tenant.json", raw); errWrite != nil {
		t.Fatalf("WriteAuthFile() error = %v", errWrite)
	}
	auth, ok := authfiles.FindAuth(manager, "tenant.json")
	if !ok {
		t.Fatal("uploaded auth was not registered")
	}
	if auth.ID != persister.AuthIDForPath(filepath.Join(cfg.AuthDir, "tenant.json")) {
		t.Fatalf("auth ID = %q", auth.ID)
	}
	if _, legacy := auth.Metadata["base-url"]; legacy {
		t.Fatalf("legacy metadata key was not normalized: %#v", auth.Metadata)
	}
	if auth.Metadata["base_url"] != "https://example.invalid/v1" {
		t.Fatalf("base_url = %#v", auth.Metadata["base_url"])
	}
	if owner := authfiles.OwnerUserID(auth); owner != "user-a" {
		t.Fatalf("owner = %q", owner)
	}
	info, errStat := os.Stat(filepath.Join(cfg.AuthDir, "tenant.json"))
	if errStat != nil {
		t.Fatalf("stat: %v", errStat)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteAuthFileRejectsUnsafeDestinations(t *testing.T) {
	cfg, manager, persister := newPersister(t)
	outside := filepath.Join(t.TempDir(), "outside.json")
	if errWrite := os.WriteFile(outside, []byte(`{}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errLink := os.Symlink(outside, filepath.Join(cfg.AuthDir, "link.json")); errLink != nil {
		t.Skipf("symlinks unavailable: %v", errLink)
	}
	for _, name := range []string{"link.json", "notjson.txt", " "} {
		if errWrite := authfiles.WriteAuthFile(context.Background(), cfg, manager, persister, name, []byte(`{"type":"codex"}`)); errWrite == nil {
			t.Fatalf("WriteAuthFile(%q) succeeded", name)
		}
	}
	data, errRead := os.ReadFile(outside)
	if errRead != nil || string(data) != "{}" {
		t.Fatalf("symlink target modified: %q %v", data, errRead)
	}
}

func TestExistingFilePermissionsAreTightened(t *testing.T) {
	cfg, manager, persister := newPersister(t)
	path := filepath.Join(cfg.AuthDir, "loose.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o644); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errChmod := os.Chmod(path, 0o644); errChmod != nil {
		t.Fatal(errChmod)
	}
	if errWrite := authfiles.WriteAuthFile(context.Background(), cfg, manager, persister, "loose.json", []byte(`{"type":"codex","email":"x@example.com"}`)); errWrite != nil {
		t.Fatalf("WriteAuthFile() error = %v", errWrite)
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestFinalizeTokenRecordRestampsOwnerAndRejectsDuplicates(t *testing.T) {
	cfg, manager, persister := newPersister(t)
	if errWrite := authfiles.WriteAuthFile(context.Background(), cfg, manager, persister, "admin.json", []byte(`{"type":"codex","account_id":"shared-account"}`)); errWrite != nil {
		t.Fatal(errWrite)
	}
	release := authfiles.LockRegistration()
	defer release()

	record := &coreauth.Auth{ID: "new.json", Provider: "codex", Metadata: map[string]any{"owner_user_id": "hook-value", "account_id": "fresh"}}
	if errFinalize := authfiles.FinalizeTokenRecord(tenantContext("user-a"), cfg, manager, persister, record); errFinalize != nil {
		t.Fatalf("FinalizeTokenRecord() error = %v", errFinalize)
	}
	if record.Metadata["owner_user_id"] != "user-a" || record.Attributes["owner_user_id"] != "user-a" {
		t.Fatalf("owner not re-stamped: metadata=%#v attributes=%#v", record.Metadata, record.Attributes)
	}

	duplicate := &coreauth.Auth{ID: "dup.json", Provider: "codex", Metadata: map[string]any{"account_id": "shared-account"}}
	if errFinalize := authfiles.FinalizeTokenRecord(tenantContext("user-a"), cfg, manager, persister, duplicate); !errors.Is(errFinalize, authfiles.ErrCredentialExists) {
		t.Fatalf("duplicate err = %v", errFinalize)
	}

	managementRecord := &coreauth.Auth{ID: "mgmt.json", Provider: "codex", Metadata: map[string]any{"account_id": "shared-account"}}
	if errFinalize := authfiles.FinalizeTokenRecord(context.Background(), cfg, manager, persister, managementRecord); errFinalize != nil {
		t.Fatalf("management record rejected: %v", errFinalize)
	}
	if owner := authfiles.OwnerUserID(managementRecord); owner != "" {
		t.Fatalf("management record owned by %q", owner)
	}
}

func TestComposeOwnerStampHookRunsBeforeCustomHook(t *testing.T) {
	var seenOwner string
	hook := authfiles.ComposeOwnerStampHook(func(_ context.Context, auth *coreauth.Auth) error {
		seenOwner = authfiles.OwnerUserID(auth)
		return nil
	})
	record := &coreauth.Auth{}
	if errHook := hook(tenantContext("user-a"), record); errHook != nil {
		t.Fatalf("hook error = %v", errHook)
	}
	if seenOwner != "user-a" {
		t.Fatalf("custom hook saw owner %q", seenOwner)
	}
	errCustom := errors.New("custom failure")
	failing := authfiles.ComposeOwnerStampHook(func(context.Context, *coreauth.Auth) error { return errCustom })
	if errHook := failing(context.Background(), &coreauth.Auth{}); !errors.Is(errHook, errCustom) {
		t.Fatalf("custom hook error lost: %v", errHook)
	}
	if errHook := authfiles.ComposeOwnerStampHook(nil)(context.Background(), &coreauth.Auth{}); errHook != nil {
		t.Fatalf("nil custom hook error = %v", errHook)
	}
}
