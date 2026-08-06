package tenancy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestResolveDBPath(t *testing.T) {
	t.Parallel()

	authDir := filepath.Join("var", "lib", "proxy", "auths")
	if got, want := ResolveDBPath(config.TenancyConfig{}, authDir), filepath.Join("var", "lib", "proxy", "tenancy.db"); got != want {
		t.Fatalf("ResolveDBPath() = %q, want %q", got, want)
	}
	explicit := filepath.Join("tmp", "custom.db")
	if got := ResolveDBPath(config.TenancyConfig{DBPath: explicit}, authDir); got != explicit {
		t.Fatalf("ResolveDBPath(explicit) = %q, want %q", got, explicit)
	}
}

func TestSQLiteSchemaIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	if errSchema := store.EnsureSchema(context.Background()); errSchema != nil {
		t.Fatalf("EnsureSchema() second call error = %v", errSchema)
	}

	rows, errQuery := store.db.Query(`
		SELECT name
		FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if errQuery != nil {
		t.Fatalf("query schema: %v", errQuery)
	}

	var tables []string
	for rows.Next() {
		var name string
		if errScan := rows.Scan(&name); errScan != nil {
			t.Fatalf("scan schema: %v", errScan)
		}
		tables = append(tables, name)
	}
	if errRows := rows.Close(); errRows != nil {
		t.Fatalf("close schema rows: %v", errRows)
	}
	want := []string{
		"credential_validation",
		"quota_windows",
		"usage_ledger",
		"user_api_keys",
		"users",
	}
	if !reflect.DeepEqual(tables, want) {
		t.Fatalf("tables = %#v, want %#v", tables, want)
	}

	expectedColumns := map[string][]string{
		"users": {
			"id", "email", "display_name", "role", "tier", "disabled", "created_at", "updated_at",
		},
		"user_api_keys": {
			"key_hash", "user_id", "label", "created_at", "last_used_at", "revoked_at",
		},
		"usage_ledger": {
			"id", "user_id", "auth_id", "provider", "model", "cost_nano_usd",
			"input_tokens", "output_tokens", "failed", "occurred_at",
		},
		"quota_windows": {
			"auth_id", "provider", "window_start", "window_end",
			"used_units", "limit_units", "source", "updated_at",
		},
		"credential_validation": {
			"auth_id", "user_id", "last_attempt_at", "last_ok_at", "last_status",
		},
	}
	for table, expected := range expectedColumns {
		columnRows, errColumns := store.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
		if errColumns != nil {
			t.Fatalf("query %s columns: %v", table, errColumns)
		}
		var columns []string
		for columnRows.Next() {
			var position, notNull, primaryKey int
			var name, columnType string
			var defaultValue any
			if errScan := columnRows.Scan(
				&position,
				&name,
				&columnType,
				&notNull,
				&defaultValue,
				&primaryKey,
			); errScan != nil {
				_ = columnRows.Close()
				t.Fatalf("scan %s columns: %v", table, errScan)
			}
			columns = append(columns, name)
		}
		if errRows := columnRows.Close(); errRows != nil {
			t.Fatalf("close %s columns: %v", table, errRows)
		}
		if !reflect.DeepEqual(columns, expected) {
			t.Fatalf("%s columns = %#v, want %#v", table, columns, expected)
		}
	}

	var costColumnType string
	if errType := store.db.QueryRow(`
		SELECT type
		FROM pragma_table_info('usage_ledger')
		WHERE name = 'cost_nano_usd'
	`).Scan(&costColumnType); errType != nil {
		t.Fatalf("query usage_ledger cost type: %v", errType)
	}
	if costColumnType != "INTEGER" {
		t.Fatalf("usage_ledger.cost_nano_usd type = %q, want INTEGER", costColumnType)
	}
}

func TestUserCRUDAndRoleValidation(t *testing.T) {
	store := newTestStore(t)
	user := &User{
		Email:       "admin@example.com",
		DisplayName: "Admin",
		Role:        RoleAdmin,
		Tier:        "staff",
	}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	loaded, errGet := store.GetUser(user.ID)
	if errGet != nil {
		t.Fatalf("GetUser() error = %v", errGet)
	}
	if loaded.Role != RoleAdmin || loaded.Tier != "staff" {
		t.Fatalf("GetUser() = %#v, want admin/staff", loaded)
	}

	user.DisplayName = "Updated Admin"
	user.Role = RoleUser
	user.Tier = "default"
	user.Disabled = true
	if errUpdate := store.UpdateUser(user); errUpdate != nil {
		t.Fatalf("UpdateUser() error = %v", errUpdate)
	}
	users, errList := store.ListUsers()
	if errList != nil {
		t.Fatalf("ListUsers() error = %v", errList)
	}
	if len(users) != 1 ||
		users[0].DisplayName != user.DisplayName ||
		users[0].Role != RoleUser ||
		!users[0].Disabled {
		t.Fatalf("ListUsers() = %#v, want updated disabled user", users)
	}

	if errDelete := store.DeleteUser(user.ID); errDelete != nil {
		t.Fatalf("DeleteUser() error = %v", errDelete)
	}
	if _, errGet = store.GetUser(user.ID); !errors.Is(errGet, ErrNotFound) {
		t.Fatalf("GetUser(deleted) error = %v, want ErrNotFound", errGet)
	}
	if errCreate := store.CreateUser(&User{
		Email: "bad-role@example.com",
		Role:  "owner",
	}); !errors.Is(errCreate, ErrInvalidRole) {
		t.Fatalf("CreateUser(invalid role) error = %v, want ErrInvalidRole", errCreate)
	}
}

func TestUserAndAPIKeyLifecycle(t *testing.T) {
	store := newTestStore(t)
	user := &User{
		Email:       "person@example.com",
		DisplayName: "Person",
		Role:        RoleUser,
		Tier:        "staff",
	}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	if user.ID == "" {
		t.Fatal("CreateUser() did not generate an ID")
	}

	plaintext, key, errIssue := store.IssueAPIKey(user.ID, "laptop")
	if errIssue != nil {
		t.Fatalf("IssueAPIKey() error = %v", errIssue)
	}
	if plaintext == "" || plaintext == key.KeyHash {
		t.Fatalf("IssueAPIKey() returned invalid plaintext/hash pair")
	}
	if got := HashAPIKey(plaintext); got != key.KeyHash {
		t.Fatalf("HashAPIKey() = %q, want %q", got, key.KeyHash)
	}

	var storedHash string
	if errScan := store.db.QueryRow(`SELECT key_hash FROM user_api_keys`).Scan(&storedHash); errScan != nil {
		t.Fatalf("query stored API key: %v", errScan)
	}
	if storedHash != key.KeyHash || storedHash == plaintext {
		t.Fatalf("stored key = %q, want hash only", storedHash)
	}

	lookedUp, errLookup := store.LookupByAPIKey(key.KeyHash)
	if errLookup != nil {
		t.Fatalf("LookupByAPIKey() error = %v", errLookup)
	}
	if lookedUp.ID != user.ID {
		t.Fatalf("LookupByAPIKey().ID = %q, want %q", lookedUp.ID, user.ID)
	}
	keys, errList := store.ListAPIKeys(user.ID)
	if errList != nil {
		t.Fatalf("ListAPIKeys() error = %v", errList)
	}
	if len(keys) != 1 || keys[0].LastUsed == nil || keys[0].RevokedAt != nil {
		t.Fatalf("ListAPIKeys() = %#v, want one active used key", keys)
	}

	if errRevoke := store.RevokeAPIKey(key.KeyHash); errRevoke != nil {
		t.Fatalf("RevokeAPIKey() error = %v", errRevoke)
	}
	if _, errLookup = store.LookupByAPIKey(key.KeyHash); !errors.Is(errLookup, ErrNotFound) {
		t.Fatalf("LookupByAPIKey(revoked) error = %v, want ErrNotFound", errLookup)
	}
}

func TestCredentialValidationRoundTrip(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	lastOK := now.Add(-time.Minute)
	input := CredentialValidation{
		AuthID:        "auth-1",
		UserID:        "user-1",
		LastAttemptAt: now,
		LastOKAt:      &lastOK,
		LastStatus:    "ok",
	}
	if errUpsert := store.UpsertCredentialValidation(context.Background(), input); errUpsert != nil {
		t.Fatalf("UpsertCredentialValidation() error = %v", errUpsert)
	}
	got, errGet := store.GetCredentialValidation(context.Background(), input.AuthID)
	if errGet != nil {
		t.Fatalf("GetCredentialValidation() error = %v", errGet)
	}
	if got.AuthID != input.AuthID || got.UserID != input.UserID || got.LastStatus != input.LastStatus {
		t.Fatalf("GetCredentialValidation() = %#v, want %#v", got, input)
	}
	if !got.LastAttemptAt.Equal(now) || got.LastOKAt == nil || !got.LastOKAt.Equal(lastOK) {
		t.Fatalf("GetCredentialValidation() timestamps = %#v, want attempt=%v ok=%v", got, now, lastOK)
	}
}

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, errOpen := OpenSQLitePath(filepath.Join(t.TempDir(), "tenancy.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLitePath() error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("Close() error = %v", errClose)
		}
	})
	return store
}

// TestResolveDBPathExpandsTilde pins the path contract. A literal "~" would put
// the database under a directory named "~" relative to the process working
// directory, so the same config would open different databases depending on
// where the server was started from -- silently losing every user and usage row.
func TestResolveDBPathExpandsTilde(t *testing.T) {
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		t.Skipf("no home directory available: %v", errHome)
	}

	tests := []struct {
		name    string
		cfg     config.TenancyConfig
		authDir string
		want    string
	}{
		{
			name:    "tilde auth dir with empty db path (the live deployment shape)",
			authDir: "~/.local/share/cliproxyapi/auths",
			want:    filepath.Join(home, ".local/share/cliproxyapi/tenancy.db"),
		},
		{
			name: "tilde db path",
			cfg:  config.TenancyConfig{DBPath: "~/cliproxy/tenancy.db"},
			want: filepath.Join(home, "cliproxy/tenancy.db"),
		},
		{
			name: "absolute db path is untouched",
			cfg:  config.TenancyConfig{DBPath: "/var/lib/cliproxy/tenancy.db"},
			want: "/var/lib/cliproxy/tenancy.db",
		},
		{
			name:    "absolute auth dir is untouched",
			authDir: "/var/lib/cliproxy/auths",
			want:    "/var/lib/cliproxy/tenancy.db",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ResolveDBPath(test.cfg, test.authDir)
			if got != test.want {
				t.Fatalf("ResolveDBPath() = %q, want %q", got, test.want)
			}
			if strings.Contains(got, "~") {
				t.Fatalf("ResolveDBPath() = %q still contains a literal tilde", got)
			}
			if !filepath.IsAbs(got) {
				t.Fatalf("ResolveDBPath() = %q is not absolute; the path would depend on the working directory", got)
			}
		})
	}
}
