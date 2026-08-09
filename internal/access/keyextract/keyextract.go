// Package keyextract provides the shared inbound API key extraction order used
// by request authentication providers.
package keyextract

import (
	"net/http"
	"strings"
)

// Candidate is one presented API key and the request location it came from.
type Candidate struct {
	Value  string
	Source string
}

// Result contains presented credentials in authentication precedence order.
// Presented remains true for malformed non-empty credentials such as
// "Authorization: Bearer ", allowing providers to preserve invalid-credential
// behavior instead of treating them as missing.
type Result struct {
	Candidates []Candidate
	Presented  bool
}

// FromRequest extracts API keys in the repository's established order.
func FromRequest(r *http.Request) Result {
	if r == nil {
		return Result{}
	}

	authorization := r.Header.Get("Authorization")
	google := r.Header.Get("X-Goog-Api-Key")
	anthropic := r.Header.Get("X-Api-Key")
	queryKey := ""
	queryAuthToken := ""
	if r.URL != nil {
		queryKey = r.URL.Query().Get("key")
		queryAuthToken = r.URL.Query().Get("auth_token")
	}

	return Result{
		Presented: authorization != "" || google != "" || anthropic != "" || queryKey != "" || queryAuthToken != "",
		Candidates: []Candidate{
			{Value: bearerToken(authorization), Source: "authorization"},
			{Value: google, Source: "x-goog-api-key"},
			{Value: anthropic, Source: "x-api-key"},
			{Value: queryKey, Source: "query-key"},
			{Value: queryAuthToken, Source: "query-auth-token"},
		},
	}
}

func bearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if !strings.EqualFold(parts[0], "bearer") {
		return header
	}
	return strings.TrimSpace(parts[1])
}
