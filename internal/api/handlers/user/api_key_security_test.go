package user

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
)

func TestHTTPHandlersCannotIssuePlaintextAPIKeys(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	directory := filepath.Dir(filename)
	for _, sourceName := range []string{"account.go", "admin.go", "handler.go"} {
		raw, errRead := os.ReadFile(filepath.Join(directory, sourceName))
		if errRead != nil {
			t.Fatalf("read %s: %v", sourceName, errRead)
		}
		if strings.Contains(string(raw), ".IssueAPIKey(") {
			t.Fatalf("%s calls IssueAPIKey; HTTP handlers may only register browser key hashes", sourceName)
		}
	}
}

func TestHTTPAPIKeyRegistrationIsHashOnlyAndRevocable(t *testing.T) {
	harness := newUserTestHarness(t, true)
	plaintext := "cp_u_http-browser-secret"
	hash := tenancy.HashAPIKey(plaintext)
	register := harness.request(t, "user-a", http.MethodPost, "/v0/user/api-keys", []byte(`{"key_hash":"`+hash+`","label":"browser"}`))
	if register.Code != http.StatusCreated {
		t.Fatalf("register status = %d, body = %s", register.Code, register.Body.String())
	}
	if strings.Contains(register.Body.String(), plaintext) || !strings.Contains(register.Body.String(), hash) {
		t.Fatalf("register response leaked plaintext or omitted hash: %s", register.Body.String())
	}
	list := harness.request(t, "user-a", http.MethodGet, "/v0/user/api-keys", nil)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), plaintext) {
		t.Fatalf("list status/body = %d/%s", list.Code, list.Body.String())
	}
	revoke := harness.request(t, "user-a", http.MethodDelete, "/v0/user/api-keys/"+hash, nil)
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body = %s", revoke.Code, revoke.Body.String())
	}
}

func TestAdminCreateUserInitialKeyHashNeverReturnsPlaintext(t *testing.T) {
	harness := newUserTestHarness(t, true)
	plaintext := "cp_u_admin-browser-secret"
	hash := tenancy.HashAPIKey(plaintext)
	response := harness.request(t, "admin", http.MethodPost, "/v0/user/admin/users", []byte(`{"email":"hash-only@example.com","display_name":"Hash Only","role":"user","key_hash":"`+hash+`"}`))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), plaintext) || strings.Contains(response.Body.String(), `"api_key"`) {
		t.Fatalf("create response leaked plaintext: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), hash) {
		t.Fatalf("create response omitted key metadata: %s", response.Body.String())
	}
	var created struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &created); errDecode != nil || created.User.ID == "" {
		t.Fatalf("decode created user: id=%q error=%v", created.User.ID, errDecode)
	}
	path := "/v0/user/admin/users/" + created.User.ID + "/api-keys/" + hash
	if forbidden := harness.request(t, "user-a", http.MethodDelete, path, nil); forbidden.Code != http.StatusForbidden {
		t.Fatalf("non-admin revoke status = %d, body = %s", forbidden.Code, forbidden.Body.String())
	}
	if revoke := harness.request(t, "admin", http.MethodDelete, path, nil); revoke.Code != http.StatusOK {
		t.Fatalf("admin revoke status = %d, body = %s", revoke.Code, revoke.Body.String())
	}
	if _, errLookup := harness.service.Store().LookupByAPIKey(hash); errLookup == nil {
		t.Fatal("admin discard route left the initial API key active")
	}
}
