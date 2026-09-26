package auth

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// schedulerSharingMetadata exposes only scalar sharing fields to scheduler
// plugins. Credential metadata may also contain tokens and must not be copied.
func schedulerSharingMetadata(src map[string]any) map[string]any {
	var out map[string]any
	for _, key := range []string{"owner_user_id", "shared"} {
		value, exists := src[key]
		if !exists {
			continue
		}
		switch value.(type) {
		case string, bool:
			if out == nil {
				out = make(map[string]any, 2)
			}
			out[key] = value
		}
	}
	return out
}

// schedulerCandidateMetadata returns the allowlisted metadata forwarded to
// scheduler plugins: sharing fields plus an opaque account identity.
func schedulerCandidateMetadata(auth *Auth) map[string]any {
	if auth == nil {
		return nil
	}
	out := schedulerSharingMetadata(auth.Metadata)
	if identity := schedulerAccountIdentity(auth); identity != "" {
		if out == nil {
			out = make(map[string]any, 1)
		}
		out["account_identity"] = identity
	}
	return out
}

// schedulerAccountIdentity derives a stable, non-reversible upstream account
// identity so duplicate credential files of one account are not treated as
// independent capacity. Only account identifiers are used, never tokens.
func schedulerAccountIdentity(auth *Auth) string {
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	for _, field := range []string{"account_id", "email", "sub"} {
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
		return fmt.Sprintf("%x", sha256.Sum256([]byte("scheduler-account\x00"+provider+"\x00"+field+"\x00"+value)))
	}
	return ""
}
