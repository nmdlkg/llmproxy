package authfiles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRegistrationMatchesAccountIdentity(t *testing.T) {
	for _, field := range []string{"account_id", "email", "access_token", "refresh_token"} {
		t.Run(field, func(t *testing.T) {
			cfg := &config.Config{AuthDir: t.TempDir()}
			raw := []byte(fmt.Sprintf(`{"type":"codex",%q:"same-value","disabled":true}`, field))
			if errWrite := WriteAuthFile(context.Background(), cfg, nil, "admin.json", raw); errWrite != nil {
				t.Fatal(errWrite)
			}
			record := &coreauth.Auth{ID: "renamed.json", Provider: "codex", Metadata: map[string]any{field: "same-value"}}
			ctx := tenancy.WithUser(context.Background(), &tenancy.User{ID: "user"})
			RegistrationMu.Lock()
			defer RegistrationMu.Unlock()
			if errCheck := CheckUserRegistration(ctx, cfg, nil, record); !errors.Is(errCheck, ErrCredentialExists) {
				t.Fatalf("duplicate err=%v", errCheck)
			}
			record.Provider = "claude"
			if errCheck := CheckUserRegistration(ctx, cfg, nil, record); errCheck != nil {
				t.Fatalf("other provider err=%v", errCheck)
			}
		})
	}
}

func TestConcurrentRegistrationBeforeWatcherLoad(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := tenancy.WithUser(context.Background(), &tenancy.User{ID: fmt.Sprintf("user-%d", i)})
			results <- WriteAuthFile(ctx, cfg, nil, fmt.Sprintf("copy-%d.json", i), []byte(`{"type":"codex","account_id":"same-account"}`))
		}(i)
	}
	wg.Wait()
	close(results)
	successes, duplicates := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrCredentialExists) {
			duplicates++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("successes=%d duplicates=%d", successes, duplicates)
	}
}

func TestRegistrationIgnoresFileAuthWithoutBackingFile(t *testing.T) {
	for _, withPathAttribute := range []bool{true, false} {
		t.Run(fmt.Sprintf("path_attribute=%t", withPathAttribute), func(t *testing.T) {
			cfg := &config.Config{AuthDir: t.TempDir()}
			manager := coreauth.NewManager(nil, nil, nil)
			ghost := &coreauth.Auth{
				ID:         "codex-ghost.json",
				Provider:   "codex",
				FileName:   "codex-ghost.json",
				Attributes: map[string]string{},
				Metadata:   map[string]any{"account_id": "same-account"},
			}
			if withPathAttribute {
				ghost.Attributes[coreauth.AttributePath] = filepath.Join(cfg.AuthDir, ghost.FileName)
			}
			if _, errRegister := manager.Register(context.Background(), ghost); errRegister != nil {
				t.Fatal(errRegister)
			}

			record := &coreauth.Auth{ID: "codex-new.json", Provider: "codex", Metadata: map[string]any{"account_id": "same-account"}}
			ctx := tenancy.WithUser(context.Background(), &tenancy.User{ID: "user"})
			RegistrationMu.Lock()
			defer RegistrationMu.Unlock()
			if errCheck := CheckUserRegistration(ctx, cfg, manager, record); errCheck != nil {
				t.Fatalf("stale record blocked registration: %v", errCheck)
			}
		})
	}
}

func TestRegistrationStillRejectsFileAuthPresentOnDisk(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	manager := coreauth.NewManager(nil, nil, nil)
	path := filepath.Join(cfg.AuthDir, "codex-live.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"codex","account_id":"same-account"}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	live := &coreauth.Auth{
		ID:         "codex-live.json",
		Provider:   "codex",
		FileName:   "codex-live.json",
		Attributes: map[string]string{coreauth.AttributePath: path},
		Metadata:   map[string]any{"account_id": "same-account"},
	}
	if _, errRegister := manager.Register(context.Background(), live); errRegister != nil {
		t.Fatal(errRegister)
	}

	record := &coreauth.Auth{ID: "codex-new.json", Provider: "codex", Metadata: map[string]any{"account_id": "same-account"}}
	ctx := tenancy.WithUser(context.Background(), &tenancy.User{ID: "user"})
	RegistrationMu.Lock()
	defer RegistrationMu.Unlock()
	if errCheck := CheckUserRegistration(ctx, cfg, manager, record); !errors.Is(errCheck, ErrCredentialExists) {
		t.Fatalf("duplicate err=%v", errCheck)
	}
}
