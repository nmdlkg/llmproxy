// Package useraccess authenticates tenant API keys against the tenancy store.
package useraccess

import (
	"context"
	"errors"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/access/keyextract"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

// ProviderName identifies tenant API key authentication results.
const ProviderName = "user-api-key"

// Register installs or removes the tenant API key provider.
func Register(store tenancy.Store) {
	if store == nil {
		sdkaccess.UnregisterProvider(ProviderName)
		return
	}
	sdkaccess.RegisterProvider(ProviderName, New(store))
}

// New constructs a tenant API key provider.
func New(store tenancy.Store) sdkaccess.Provider {
	return &provider{store: store}
}

type provider struct {
	store tenancy.Store
}

func (p *provider) Identifier() string {
	return ProviderName
}

func (p *provider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil || p.store == nil {
		return nil, sdkaccess.NewNotHandledError()
	}

	extracted := keyextract.FromRequest(r)
	if !extracted.Presented {
		return nil, sdkaccess.NewNotHandledError()
	}

	for _, candidate := range extracted.Candidates {
		if candidate.Value == "" {
			continue
		}
		keyHash := tenancy.HashAPIKey(candidate.Value)
		user, errLookup := p.store.LookupByAPIKey(keyHash)
		if errors.Is(errLookup, tenancy.ErrNotFound) {
			revoked, errRevoked := tenancy.APIKeyRevoked(p.store, keyHash)
			if errRevoked != nil {
				return nil, sdkaccess.NewInternalAuthError("User API key lookup failed", errRevoked)
			}
			if revoked {
				return nil, sdkaccess.NewInvalidCredentialError()
			}
			continue
		}
		if errLookup != nil {
			return nil, sdkaccess.NewInternalAuthError("User API key lookup failed", errLookup)
		}
		if user == nil || user.Disabled {
			return nil, sdkaccess.NewInvalidCredentialError()
		}
		return &sdkaccess.Result{
			Provider:  ProviderName,
			Principal: user.ID,
			Metadata: map[string]string{
				"user_id":  user.ID,
				"email":    user.Email,
				"role":     user.Role,
				"tier":     user.Tier,
				"key_hash": keyHash,
			},
		}, nil
	}

	return nil, sdkaccess.NewNotHandledError()
}
