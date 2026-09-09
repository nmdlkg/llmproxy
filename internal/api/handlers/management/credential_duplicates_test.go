package management

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestOAuthSaveRejectsRegisteredAccountBeforeOverwrite(t *testing.T) {
	for _, filename := range []string{"existing.json", "renamed-prolite.json"} {
		t.Run(filename, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "existing.json")
			original := []byte(`{"type":"codex","account_id":"account-1","email":"person@example.com","access_token":"original","shared":true}`)
			if errWrite := os.WriteFile(path, original, 0600); errWrite != nil {
				t.Fatal(errWrite)
			}
			// No manager: the persisted record must be found even before watcher registration.
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, nil)
			ctx := tenancy.WithUser(context.Background(), &tenancy.User{ID: "user-b"})
			record := &coreauth.Auth{ID: filename, FileName: filename, Provider: "codex", Metadata: map[string]any{"account_id": "account-1", "email": "person@example.com", "access_token": "replacement"}}
			saved, errSave := h.saveTokenRecord(ctx, record)
			if !errors.Is(errSave, authfiles.ErrCredentialExists) || saved != "" {
				t.Fatalf("save=%q err=%v", saved, errSave)
			}
			after, errRead := os.ReadFile(path)
			if errRead != nil || string(after) != string(original) {
				t.Fatal("admin credential overwritten")
			}
			if authfiles.OAuthSaveError(errSave) != authfiles.ErrCredentialExists.Error() {
				t.Fatal("duplicate warning missing")
			}
		})
	}
}
