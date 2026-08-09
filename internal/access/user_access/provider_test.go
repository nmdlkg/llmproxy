package useraccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

func TestProviderAuthenticatesAllKeySources(t *testing.T) {
	store, user, plaintext := newTestUserKey(t)
	provider := New(store)

	tests := []struct {
		name    string
		target  string
		headers map[string]string
	}{
		{name: "authorization bearer", target: "/", headers: map[string]string{"Authorization": "Bearer " + plaintext}},
		{name: "google header", target: "/", headers: map[string]string{"X-Goog-Api-Key": plaintext}},
		{name: "api key header", target: "/", headers: map[string]string{"X-Api-Key": plaintext}},
		{name: "query key", target: "/?key=" + plaintext},
		{name: "query auth token", target: "/?auth_token=" + plaintext},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.target, nil)
			for name, value := range test.headers {
				request.Header.Set(name, value)
			}

			result, authErr := provider.Authenticate(context.Background(), request)
			if authErr != nil {
				t.Fatalf("Authenticate() error = %v", authErr)
			}
			if result.Provider != ProviderName {
				t.Fatalf("Provider = %q, want %q", result.Provider, ProviderName)
			}
			if result.Principal != user.ID {
				t.Fatalf("Principal = %q, want %q", result.Principal, user.ID)
			}
			wantMetadata := map[string]string{
				"user_id":  user.ID,
				"email":    user.Email,
				"role":     user.Role,
				"tier":     user.Tier,
				"key_hash": tenancy.HashAPIKey(plaintext),
			}
			for key, want := range wantMetadata {
				if got := result.Metadata[key]; got != want {
					t.Fatalf("Metadata[%q] = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestProviderUnknownKeyFallsThrough(t *testing.T) {
	store, _, _ := newTestUserKey(t)
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{
		New(store),
		&fallbackProvider{key: "service-key"},
	})

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer service-key")
	result, authErr := manager.Authenticate(context.Background(), request)
	if authErr != nil {
		t.Fatalf("Authenticate() error = %v", authErr)
	}
	if result == nil || result.Provider != "fallback" {
		t.Fatalf("result = %#v, want fallback provider", result)
	}
}

func TestProviderRejectsRevokedKey(t *testing.T) {
	store, _, plaintext := newTestUserKey(t)
	if errRevoke := store.RevokeAPIKey(tenancy.HashAPIKey(plaintext)); errRevoke != nil {
		t.Fatalf("RevokeAPIKey() error = %v", errRevoke)
	}

	assertInvalidCredential(t, New(store), plaintext)
}

func TestProviderRejectsDisabledUser(t *testing.T) {
	store, user, plaintext := newTestUserKey(t)
	user.Disabled = true
	if errUpdate := store.UpdateUser(user); errUpdate != nil {
		t.Fatalf("UpdateUser() error = %v", errUpdate)
	}

	assertInvalidCredential(t, New(store), plaintext)
}

func newTestUserKey(t *testing.T) (*tenancy.SQLiteStore, *tenancy.User, string) {
	t.Helper()
	store, errOpen := tenancy.OpenSQLitePath(":memory:")
	if errOpen != nil {
		t.Fatalf("OpenSQLitePath() error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("Close() error = %v", errClose)
		}
	})

	user := &tenancy.User{
		Email: "user@example.com",
		Role:  tenancy.RoleUser,
		Tier:  "standard",
	}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	plaintext, _, errIssue := store.IssueAPIKey(user.ID, "test")
	if errIssue != nil {
		t.Fatalf("IssueAPIKey() error = %v", errIssue)
	}
	return store, user, plaintext
}

func assertInvalidCredential(t *testing.T, provider sdkaccess.Provider, plaintext string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+plaintext)
	result, authErr := provider.Authenticate(context.Background(), request)
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
	if authErr == nil || authErr.Code != sdkaccess.AuthErrorCodeInvalidCredential {
		t.Fatalf("auth error = %#v, want %q", authErr, sdkaccess.AuthErrorCodeInvalidCredential)
	}
}

type fallbackProvider struct {
	key string
}

func (p *fallbackProvider) Identifier() string {
	return "fallback"
}

func (p *fallbackProvider) Authenticate(_ context.Context, request *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if request.Header.Get("Authorization") != "Bearer "+p.key {
		return nil, sdkaccess.NewNotHandledError()
	}
	return &sdkaccess.Result{Provider: p.Identifier(), Principal: p.key}, nil
}
