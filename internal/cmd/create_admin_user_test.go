package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
)

const apiKeyOutputPrefix = "UNRECOVERABLE API key (shown exactly once): "

func TestDoCreateAdminUserCreatesAndRecoversCaseInsensitively(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "tenancy.db")
	cfg := &config.Config{
		Tenancy: config.TenancyConfig{
			Enabled: true,
			DBPath:  databasePath,
		},
	}

	var firstOutput bytes.Buffer
	if errCreate := DoCreateAdminUser(cfg, " Admin@Example.com ", "", &firstOutput); errCreate != nil {
		t.Fatalf("DoCreateAdminUser(first) error = %v", errCreate)
	}
	firstKey := keyFromAdminOutput(t, firstOutput.String())
	if !strings.Contains(firstOutput.String(), "Admin user created") {
		t.Fatalf("first output = %q, want creation status", firstOutput.String())
	}

	var secondOutput bytes.Buffer
	if errCreate := DoCreateAdminUser(cfg, "admin@example.COM", "staff", &secondOutput); errCreate != nil {
		t.Fatalf("DoCreateAdminUser(existing) error = %v", errCreate)
	}
	secondKey := keyFromAdminOutput(t, secondOutput.String())
	if firstKey == secondKey {
		t.Fatal("existing-user path returned the original API key")
	}
	if !strings.Contains(secondOutput.String(), "issued a new recovery API key without creating a duplicate") {
		t.Fatalf("second output = %q, want recovery status", secondOutput.String())
	}

	store, errOpen := tenancy.OpenSQLite(cfg.Tenancy, cfg.AuthDir)
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	users, errListUsers := store.ListUsers()
	if errListUsers != nil {
		_ = store.Close()
		t.Fatalf("ListUsers() error = %v", errListUsers)
	}
	if len(users) != 1 {
		_ = store.Close()
		t.Fatalf("ListUsers() returned %d users, want 1", len(users))
	}
	// The store canonicalizes the email to lower case so the case-sensitive UNIQUE
	// constraint cannot admit two accounts for one person; the original casing is
	// preserved in DisplayName.
	if users[0].Email != "admin@example.com" || users[0].Role != tenancy.RoleAdmin || users[0].Tier != defaultAdminUserTier {
		_ = store.Close()
		t.Fatalf("created user = %#v, want normalized admin email with default tier", users[0])
	}
	keys, errListKeys := store.ListAPIKeys(users[0].ID)
	if errListKeys != nil {
		_ = store.Close()
		t.Fatalf("ListAPIKeys() error = %v", errListKeys)
	}
	if errClose := store.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
	if len(keys) != 2 {
		t.Fatalf("ListAPIKeys() returned %d keys, want 2", len(keys))
	}
	storedHashes := map[string]bool{
		keys[0].KeyHash: true,
		keys[1].KeyHash: true,
	}
	if !storedHashes[tenancy.HashAPIKey(firstKey)] || !storedHashes[tenancy.HashAPIKey(secondKey)] {
		t.Fatalf("stored hashes = %#v, want hashes of both issued keys", storedHashes)
	}
	if storedHashes[firstKey] || storedHashes[secondKey] {
		t.Fatal("plaintext API key was stored as a key hash")
	}

	databaseBytes, errRead := os.ReadFile(databasePath)
	if errRead != nil {
		t.Fatalf("ReadFile(database) error = %v", errRead)
	}
	if bytes.Contains(databaseBytes, []byte(firstKey)) || bytes.Contains(databaseBytes, []byte(secondKey)) {
		t.Fatal("database contains plaintext API key material")
	}
}

func TestDoCreateAdminUserRequiresEnabledTenancyWithoutCreatingDatabase(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "disabled", "tenancy.db")
	cfg := &config.Config{
		Tenancy: config.TenancyConfig{
			DBPath: databasePath,
		},
	}

	var output bytes.Buffer
	errCreate := DoCreateAdminUser(cfg, "admin@example.com", "default", &output)
	if errCreate == nil || !strings.Contains(errCreate.Error(), "tenancy.enabled") {
		t.Fatalf("DoCreateAdminUser() error = %v, want actionable tenancy.enabled error", errCreate)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty output", output.String())
	}
	if _, errStat := os.Stat(databasePath); !os.IsNotExist(errStat) {
		t.Fatalf("database stat error = %v, want not exist", errStat)
	}
}

func keyFromAdminOutput(t *testing.T, output string) string {
	t.Helper()
	if strings.Count(output, apiKeyOutputPrefix) != 1 {
		t.Fatalf("output = %q, want one API key label", output)
	}
	key := strings.TrimSpace(strings.SplitN(output, apiKeyOutputPrefix, 2)[1])
	if !strings.HasPrefix(key, "cp_u_") {
		t.Fatalf("output key = %q, want cp_u_ prefix", key)
	}
	if strings.Count(output, key) != 1 {
		t.Fatalf("output contains plaintext key %d times, want exactly once", strings.Count(output, key))
	}
	return key
}
