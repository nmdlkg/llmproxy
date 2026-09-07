package auth

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// CredentialIdentityKeys identifies an upstream account independently of its
// filename, owner and subscription plan. Token values are never returned raw.
func CredentialIdentityKeys(auth *Auth) []string {
	if auth == nil {
		return nil
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	keys := make([]string, 0, 5)
	for _, field := range []string{"account_id", "email", "sub", "access_token", "refresh_token"} {
		value, _ := auth.Metadata[field].(string)
		if strings.TrimSpace(value) == "" {
			value = auth.Attributes[field]
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if field == "email" {
			value = strings.ToLower(value)
		}
		keys = append(keys, fmt.Sprintf("%s:%s:%x", provider, field, sha256.Sum256([]byte(value))))
	}
	return keys
}
