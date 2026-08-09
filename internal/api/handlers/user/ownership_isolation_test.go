package user

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestOwnershipIsolationAgainstEscapeAttempts is an independent adversarial pass over the
// credential routes, written separately from the implementation's own tests. It
// targets escape routes a field-allowlist review can miss: path traversal in the
// :id parameter, privilege fields smuggled through PATCH, and ownership
// reassignment.
func TestOwnershipIsolationAgainstEscapeAttempts(t *testing.T) {
	harness := newUserTestHarness(t, true)
	harness.writeCredential(t, "victim.json", "user-b", false)
	victimPath := filepath.Join(harness.cfg.AuthDir, "victim.json")

	// 1. Path traversal / absolute paths in the :id parameter must never reach
	//    another user's credential or anything outside AuthDir.
	traversals := []string{
		"../victim.json",
		"..%2Fvictim.json",
		"%2e%2e%2fvictim.json",
		"./victim.json",
		"/victim.json",
		"auths/../victim.json",
		"victim.json/",
	}
	for _, id := range traversals {
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			var body []byte
			if method == http.MethodPatch {
				body = []byte(`{"disabled":true}`)
			}
			response := harness.request(t, "user-a", method, "/v0/user/credentials/"+id, body)
			if response.Code >= 200 && response.Code < 300 {
				t.Errorf("%s /v0/user/credentials/%s = %d, want non-2xx (traversal reached a foreign credential)",
					method, id, response.Code)
			}
		}
	}
	if _, errStat := os.Stat(victimPath); errStat != nil {
		t.Fatalf("victim credential was destroyed by a traversal attempt: %v", errStat)
	}

	// 2. PATCH is a strict allowlist: privileged fields are REJECTED outright,
	//    not silently ignored. Verify the rejection, then verify that a
	//    legitimate PATCH still cannot reassign ownership.
	harness.writeCredential(t, "mine.json", "user-a", false)
	minePath := filepath.Join(harness.cfg.AuthDir, "mine.json")

	privileged := []string{
		`{"owner_user_id":"user-b"}`,
		`{"priority":9999}`,
		`{"proxy_url":"http://attacker.invalid"}`,
		`{"base_url":"http://attacker.invalid"}`,
		`{"contribution_tier":"pro"}`,
		`{"plan_type":"pro"}`,
		`{"model_aliases":{"a":"b"}}`,
		`{"excluded_models":["x"]}`,
		`{"disable_cooling":true}`,
		`{"headers":{"X-Evil":"1"}}`,
		`{"note":"ok","owner_user_id":"user-b"}`,
	}
	for _, body := range privileged {
		response := harness.request(t, "user-a", http.MethodPatch, "/v0/user/credentials/mine.json", []byte(body))
		if response.Code >= 200 && response.Code < 300 {
			t.Errorf("PATCH %s = %d, want non-2xx (privileged field accepted)", body, response.Code)
		}
	}

	// A legitimate PATCH must succeed and must leave ownership untouched.
	if response := harness.request(t, "user-a", http.MethodPatch, "/v0/user/credentials/mine.json",
		[]byte(`{"shared":true,"note":"ok"}`)); response.Code != http.StatusOK {
		t.Fatalf("PATCH allowed fields = %d, want 200; body=%s", response.Code, response.Body.String())
	}

	raw, errRead := os.ReadFile(minePath)
	if errRead != nil {
		t.Fatalf("read own credential: %v", errRead)
	}
	var stored map[string]any
	if errUnmarshal := json.Unmarshal(raw, &stored); errUnmarshal != nil {
		t.Fatalf("unmarshal own credential: %v", errUnmarshal)
	}
	if owner, _ := stored["owner_user_id"].(string); owner != "user-a" {
		t.Errorf("owner_user_id = %q after PATCH, want user-a (ownership was reassignable)", owner)
	}
	for _, forbidden := range []string{
		"priority", "proxy_url", "base_url", "contribution_tier",
		"plan_type", "model_aliases", "excluded_models", "disable_cooling", "headers",
	} {
		if value, exists := stored[forbidden]; exists {
			t.Errorf("privileged field %q = %v leaked into the stored credential", forbidden, value)
		}
	}

	// 3. A user must not take over a filename already owned by someone else,
	//    even though that credential is loaded in the runtime manager.
	upload := []byte(`{"type":"claude","email":"a@example.com","access_token":"attacker-token"}`)
	response := harness.request(t, "user-a", http.MethodPost, "/v0/user/credentials?name=victim.json", upload)
	if response.Code >= 200 && response.Code < 300 {
		t.Errorf("upload onto a foreign filename = %d, want non-2xx", response.Code)
	}
	victimRaw, errVictim := os.ReadFile(victimPath)
	if errVictim != nil {
		t.Fatalf("victim credential unreadable after upload attempt: %v", errVictim)
	}
	var victim map[string]any
	if errUnmarshal := json.Unmarshal(victimRaw, &victim); errUnmarshal != nil {
		t.Fatalf("unmarshal victim: %v", errUnmarshal)
	}
	if owner, _ := victim["owner_user_id"].(string); owner != "user-b" {
		t.Errorf("victim owner_user_id = %q, want user-b (ownership was stolen)", owner)
	}
	if token, _ := victim["access_token"].(string); token == "attacker-token" {
		t.Error("victim credential contents were overwritten by another user")
	}
}
