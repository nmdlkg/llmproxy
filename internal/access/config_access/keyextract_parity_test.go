package configaccess

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/access/keyextract"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

// TestKeyExtractParityWithConfigProvider keeps the fork's keyextract package,
// used by the tenant access provider, identical to the upstream config API key
// provider's extraction order and malformed-header semantics. When this fails
// after an upstream merge, update internal/access/keyextract to match.
func TestKeyExtractParityWithConfigProvider(t *testing.T) {
	keys := []string{"k-auth", "k-goog", "k-anth", "k-query", "k-token", "raw-header"}
	valid := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		valid[key] = struct{}{}
	}
	provider := newProvider("parity", keys)

	cases := []struct {
		name    string
		target  string
		headers map[string]string
	}{
		{name: "none", target: "/v1/models"},
		{name: "bearer", target: "/v1/models", headers: map[string]string{"Authorization": "Bearer k-auth"}},
		{name: "lowercase bearer", target: "/v1/models", headers: map[string]string{"Authorization": "bearer k-auth"}},
		{name: "bearer padded", target: "/v1/models", headers: map[string]string{"Authorization": "Bearer   k-auth  "}},
		{name: "bearer empty", target: "/v1/models", headers: map[string]string{"Authorization": "Bearer "}},
		{name: "raw authorization", target: "/v1/models", headers: map[string]string{"Authorization": "raw-header"}},
		{name: "basic scheme", target: "/v1/models", headers: map[string]string{"Authorization": "Basic k-auth"}},
		{name: "goog", target: "/v1/models", headers: map[string]string{"X-Goog-Api-Key": "k-goog"}},
		{name: "anthropic", target: "/v1/models", headers: map[string]string{"X-Api-Key": "k-anth"}},
		{name: "query key", target: "/v1/models?key=k-query"},
		{name: "query auth token", target: "/v1/models?auth_token=k-token"},
		{name: "invalid only", target: "/v1/models", headers: map[string]string{"Authorization": "Bearer nope"}},
		{name: "invalid then valid", target: "/v1/models?key=k-query", headers: map[string]string{"Authorization": "Bearer nope"}},
		{name: "precedence", target: "/v1/models?key=k-query&auth_token=k-token", headers: map[string]string{
			"Authorization":  "Bearer k-auth",
			"X-Goog-Api-Key": "k-goog",
			"X-Api-Key":      "k-anth",
		}},
		{name: "precedence without authorization", target: "/v1/models?key=k-query", headers: map[string]string{
			"X-Goog-Api-Key": "nope",
			"X-Api-Key":      "k-anth",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", tc.target, nil)
			for name, value := range tc.headers {
				request.Header.Set(name, value)
			}

			result, authErr := provider.Authenticate(context.Background(), request)

			extracted := keyextract.FromRequest(request)
			wantPrincipal, wantSource := "", ""
			for _, candidate := range extracted.Candidates {
				if candidate.Value == "" {
					continue
				}
				if _, ok := valid[candidate.Value]; ok {
					wantPrincipal, wantSource = candidate.Value, candidate.Source
					break
				}
			}
			switch {
			case !extracted.Presented:
				if authErr == nil || authErr.Code != sdkaccess.AuthErrorCodeNoCredentials {
					t.Fatalf("provider = %+v/%+v, keyextract reports no credentials", result, authErr)
				}
			case wantPrincipal == "":
				if authErr == nil || authErr.Code != sdkaccess.AuthErrorCodeInvalidCredential {
					t.Fatalf("provider = %+v/%+v, keyextract reports invalid credential", result, authErr)
				}
			default:
				if authErr != nil || result == nil {
					t.Fatalf("provider error = %+v, keyextract matched %q", authErr, wantPrincipal)
				}
				if result.Principal != wantPrincipal || result.Metadata["source"] != wantSource {
					t.Fatalf("provider = %s/%s, keyextract = %s/%s", result.Principal, result.Metadata["source"], wantPrincipal, wantSource)
				}
			}
		})
	}
}
