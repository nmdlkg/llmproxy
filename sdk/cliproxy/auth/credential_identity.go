package auth

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// CredentialIdentityKeys identifies an upstream account independently of its
// filename, owner and subscription plan. Token values are never returned raw.
//
// For providers such as Codex, account_id names a shared workspace rather than a
// person, and one person may belong to several workspaces. When both an
// account_id and a user identifier (email, falling back to sub) are present,
// they are combined into a single seat key so distinct members of one workspace
// and one member across workspaces are not treated as the same account.
func CredentialIdentityKeys(auth *Auth) []string {
	if auth == nil {
		return nil
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	valueFor := func(field string) string {
		value, _ := auth.Metadata[field].(string)
		if strings.TrimSpace(value) == "" {
			value = auth.Attributes[field]
		}
		value = strings.TrimSpace(value)
		if field == "email" {
			value = strings.ToLower(value)
		}
		return value
	}
	key := func(field, value string) string {
		return fmt.Sprintf("%s:%s:%x", provider, field, sha256.Sum256([]byte(value)))
	}

	keys := make([]string, 0, 5)
	accountID := valueFor("account_id")
	userField, userValue := "email", valueFor("email")
	if userValue == "" {
		userField, userValue = "sub", valueFor("sub")
	}
	switch {
	case accountID != "" && userValue != "":
		keys = append(keys, key("seat", "account_id\x00"+accountID+"\x00"+userField+"\x00"+userValue))
	case accountID != "":
		keys = append(keys, key("account_id", accountID))
	default:
		for _, field := range []string{"email", "sub"} {
			if value := valueFor(field); value != "" {
				keys = append(keys, key(field, value))
			}
		}
	}
	for _, field := range []string{"access_token", "refresh_token"} {
		if value := valueFor(field); value != "" {
			keys = append(keys, key(field, value))
		}
	}
	return keys
}
