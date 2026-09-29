package auth

import "testing"

func identityOverlaps(left, right *Auth) bool {
	keys := make(map[string]bool)
	for _, key := range CredentialIdentityKeys(left) {
		keys[key] = true
	}
	for _, key := range CredentialIdentityKeys(right) {
		if keys[key] {
			return true
		}
	}
	return false
}

func codexIdentity(meta map[string]any) *Auth {
	return &Auth{Provider: "codex", Metadata: meta}
}

func TestCredentialIdentityKeysSeparatesWorkspaceSeats(t *testing.T) {
	cases := []struct {
		name        string
		left, right map[string]any
		want        bool
	}{
		{
			name:  "same workspace different members",
			left:  map[string]any{"account_id": "ws-1", "email": "a@example.com"},
			right: map[string]any{"account_id": "ws-1", "email": "b@example.com"},
			want:  false,
		},
		{
			name:  "same member different workspaces",
			left:  map[string]any{"account_id": "ws-1", "email": "a@example.com"},
			right: map[string]any{"account_id": "ws-2", "email": "a@example.com"},
			want:  false,
		},
		{
			name:  "same seat with email casing",
			left:  map[string]any{"account_id": "ws-1", "email": "A@example.com"},
			right: map[string]any{"account_id": "ws-1", "email": "a@example.com"},
			want:  true,
		},
		{
			name:  "sub fallback seat",
			left:  map[string]any{"account_id": "ws-1", "sub": "user-1"},
			right: map[string]any{"account_id": "ws-1", "sub": "user-1"},
			want:  true,
		},
		{
			name:  "account id only",
			left:  map[string]any{"account_id": "ws-1"},
			right: map[string]any{"account_id": "ws-1"},
			want:  true,
		},
		{
			name:  "email only",
			left:  map[string]any{"email": "a@example.com"},
			right: map[string]any{"email": "a@example.com"},
			want:  true,
		},
		{
			name:  "shared refresh token across seats",
			left:  map[string]any{"account_id": "ws-1", "email": "a@example.com", "refresh_token": "rt"},
			right: map[string]any{"account_id": "ws-2", "email": "b@example.com", "refresh_token": "rt"},
			want:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := identityOverlaps(codexIdentity(tc.left), codexIdentity(tc.right)); got != tc.want {
				t.Fatalf("overlap = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCredentialIdentityKeysScopedByProvider(t *testing.T) {
	left := codexIdentity(map[string]any{"account_id": "ws-1", "email": "a@example.com"})
	right := &Auth{Provider: "claude", Metadata: map[string]any{"account_id": "ws-1", "email": "a@example.com"}}
	if identityOverlaps(left, right) {
		t.Fatal("identity keys must not match across providers")
	}
}
