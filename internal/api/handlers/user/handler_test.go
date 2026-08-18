package user

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/authfiles"
	management "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type userTestHarness struct {
	engine  *gin.Engine
	handler *Handler
	cfg     *config.Config
	manager *coreauth.Manager
	service *tenancy.Service
	users   map[string]*tenancy.User
}

func newUserTestHarness(t *testing.T, tenancyEnabled bool) *userTestHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	authDir := filepath.Join(root, "auths")
	cfg := &config.Config{
		AuthDir: authDir,
		Tenancy: config.TenancyConfig{
			Enabled: tenancyEnabled,
			DBPath:  filepath.Join(root, "tenancy.db"),
			Quota: config.TenancyQuota{
				Window:  "24h",
				BaseUSD: map[string]config.USDLimit{"default": 5_000_000_000},
			},
		},
	}
	fileStore := sdkAuth.NewFileTokenStore()
	fileStore.SetBaseDir(authDir)
	manager := coreauth.NewManager(fileStore, nil, nil)
	service, errService := tenancy.NewService(cfg.Tenancy, authDir, manager)
	if errService != nil {
		t.Fatalf("NewService() error = %v", errService)
	}
	if service != nil {
		t.Cleanup(func() {
			if errClose := service.Close(); errClose != nil {
				t.Errorf("service.Close() error = %v", errClose)
			}
		})
	}
	managementHandler := management.NewHandlerWithoutConfigFilePath(cfg, manager)
	handler := NewHandler(cfg, manager, service, fileStore, managementHandler)
	harness := &userTestHarness{
		engine:  gin.New(),
		handler: handler,
		cfg:     cfg,
		manager: manager,
		service: service,
		users:   make(map[string]*tenancy.User),
	}
	if service != nil {
		harness.addUser(t, "user-a", tenancy.RoleUser)
		harness.addUser(t, "user-b", tenancy.RoleUser)
		harness.addUser(t, "admin", tenancy.RoleAdmin)
	}
	handler.RegisterRoutes(harness.engine, harness.testAuthMiddleware())
	return harness
}

func (h *userTestHarness) addUser(t *testing.T, id, role string) {
	t.Helper()
	user := &tenancy.User{
		ID:          id,
		Email:       id + "@example.com",
		DisplayName: id,
		Role:        role,
		Tier:        "default",
	}
	if errCreate := h.service.Store().CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser(%s) error = %v", id, errCreate)
	}
	h.users[id] = user
}

func (h *userTestHarness) testAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		user := h.users[strings.TrimSpace(c.GetHeader("X-Test-User"))]
		if user != nil {
			tenancy.SetUserOnGin(c, user)
		}
		c.Next()
	}
}

func (h *userTestHarness) request(t *testing.T, userID, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if userID != "" {
		request.Header.Set("X-Test-User", userID)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	h.engine.ServeHTTP(recorder, request)
	return recorder
}

func (h *userTestHarness) writeCredential(t *testing.T, name, owner string, shared bool) {
	t.Helper()
	raw, errMarshal := json.Marshal(map[string]any{
		"type":          "claude",
		"email":         name + "@example.com",
		"access_token":  "provider-secret-" + name,
		"owner_user_id": owner,
		"shared":        shared,
	})
	if errMarshal != nil {
		t.Fatalf("marshal credential: %v", errMarshal)
	}
	if errWrite := authfiles.WriteAuthFile(context.Background(), h.cfg, h.manager, name, raw); errWrite != nil {
		t.Fatalf("WriteAuthFile(%s) error = %v", name, errWrite)
	}
}

func TestCredentialRoutesPreventCrossUserAccess(t *testing.T) {
	harness := newUserTestHarness(t, true)
	harness.writeCredential(t, "owner-a.json", "user-a", false)
	harness.writeCredential(t, "owner-b.json", "user-b", false)

	t.Run("list filters other owners and secrets", func(t *testing.T) {
		response := harness.request(t, "user-b", http.MethodGet, "/v0/user/credentials", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		body := response.Body.String()
		if strings.Contains(body, "owner-a.json") || strings.Contains(body, "provider-secret") {
			t.Fatalf("cross-user or secret data leaked: %s", body)
		}
		if !strings.Contains(body, "owner-b.json") {
			t.Fatalf("own credential missing: %s", body)
		}
	})

	t.Run("upload ignores client owner", func(t *testing.T) {
		body := []byte(`{
			"type":"codex",
			"access_token":"secret",
			"id_token":"client-controlled-plan-claims",
			"owner_user_id":"user-a",
			"shared":true,
			"disabled":true,
			"priority":999,
			"proxy_url":"http://127.0.0.1:1",
			"base_url":"https://attacker.invalid",
			"headers":{"X-Evil":"true"},
			"prefix":"stolen",
			"oauth_model_alias":{"safe":"unsafe"},
			"oauth_excluded_models":["*"],
			"contribution_tier":"enterprise",
			"using_api":true
		}`)
		response := harness.request(t, "user-b", http.MethodPost, "/v0/user/credentials?name=uploaded.json", body)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		auth, ok := authfiles.FindAuth(harness.manager, "uploaded.json")
		if !ok || authfiles.OwnerUserID(auth) != "user-b" {
			t.Fatalf("uploaded owner = %q, auth=%#v", authfiles.OwnerUserID(auth), auth)
		}
		if auth.Disabled || metadataBool(auth.Metadata, auth.Attributes, "shared") {
			t.Fatalf("client enabled disabled/shared controls: %#v", auth)
		}
		for _, field := range []string{
			"priority",
			"id_token",
			"proxy_url",
			"base_url",
			"headers",
			"prefix",
			"oauth_model_alias",
			"oauth_excluded_models",
			"contribution_tier",
			"using_api",
		} {
			if _, exists := auth.Metadata[field]; exists {
				t.Fatalf("privileged upload field %q survived in metadata: %#v", field, auth.Metadata)
			}
			if _, exists := auth.Attributes[field]; exists {
				t.Fatalf("privileged upload field %q survived in attributes: %#v", field, auth.Attributes)
			}
		}
		info, errStat := os.Stat(filepath.Join(harness.cfg.AuthDir, "uploaded.json"))
		if errStat != nil {
			t.Fatalf("stat uploaded credential: %v", errStat)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("uploaded mode = %o, want 600", info.Mode().Perm())
		}
	})

	t.Run("upload rejects unsupported providers", func(t *testing.T) {
		body := []byte(`{"type":"openai-compatibility","access_token":"secret"}`)
		response := harness.request(t, "user-b", http.MethodPost, "/v0/user/credentials?name=unsupported.json", body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	})

	t.Run("xai upload forces trusted routing endpoint", func(t *testing.T) {
		body := []byte(`{
			"type":"xai",
			"access_token":"secret",
			"base_url":"http://127.0.0.1:1",
			"token_endpoint":"https://auth.x.ai/oauth2/token",
			"auth_kind":"api",
			"using_api":true
		}`)
		response := harness.request(t, "user-b", http.MethodPost, "/v0/user/credentials?name=xai.json", body)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		auth, ok := authfiles.FindAuth(harness.manager, "xai.json")
		if !ok {
			t.Fatal("xai credential missing")
		}
		if auth.Metadata["base_url"] != "https://api.x.ai/v1" || auth.Metadata["auth_kind"] != "oauth" {
			t.Fatalf("xai routing metadata = %#v", auth.Metadata)
		}
		if _, exists := auth.Metadata["using_api"]; exists {
			t.Fatalf("xai using_api survived: %#v", auth.Metadata)
		}
	})

	t.Run("upload cannot overwrite another owner", func(t *testing.T) {
		body := []byte(`{"type":"codex","access_token":"replacement","owner_user_id":"user-b"}`)
		response := harness.request(t, "user-b", http.MethodPost, "/v0/user/credentials?name=owner-a.json", body)
		if response.Code != http.StatusConflict {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		auth, _ := authfiles.FindAuth(harness.manager, "owner-a.json")
		if authfiles.OwnerUserID(auth) != "user-a" {
			t.Fatalf("owner changed to %q", authfiles.OwnerUserID(auth))
		}
	})

	t.Run("patch returns not found for another owner", func(t *testing.T) {
		response := harness.request(t, "user-b", http.MethodPatch, "/v0/user/credentials/owner-a.json", []byte(`{"shared":true,"disabled":true,"note":"stolen"}`))
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		auth, _ := authfiles.FindAuth(harness.manager, "owner-a.json")
		if metadataBool(auth.Metadata, auth.Attributes, "shared") || auth.Disabled {
			t.Fatalf("cross-user patch changed credential: %#v", auth)
		}
	})

	t.Run("delete returns not found for another owner", func(t *testing.T) {
		response := harness.request(t, "user-b", http.MethodDelete, "/v0/user/credentials/owner-a.json", nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if _, errStat := os.Stat(filepath.Join(harness.cfg.AuthDir, "owner-a.json")); errStat != nil {
			t.Fatalf("other user's credential was deleted: %v", errStat)
		}
	})
}

func TestConcurrentCredentialUploadsCannotCrossUserOverwrite(t *testing.T) {
	harness := newUserTestHarness(t, true)
	start := make(chan struct{})
	type result struct {
		userID string
		status int
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, userID := range []string{"user-a", "user-b"} {
		workers.Add(1)
		go func(userID string) {
			defer workers.Done()
			<-start
			body := []byte(fmt.Sprintf(`{"type":"codex","access_token":"secret","owner_user_id":%q}`, userID))
			response := harness.request(t, userID, http.MethodPost, "/v0/user/credentials?name=contended.json", body)
			results <- result{userID: userID, status: response.Code}
		}(userID)
	}
	close(start)
	workers.Wait()
	close(results)

	createdBy := ""
	conflicts := 0
	for upload := range results {
		switch upload.status {
		case http.StatusCreated:
			createdBy = upload.userID
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("upload by %s returned status %d", upload.userID, upload.status)
		}
	}
	if createdBy == "" || conflicts != 1 {
		t.Fatalf("created_by = %q, conflicts = %d, want one creator and one conflict", createdBy, conflicts)
	}
	auth, ok := authfiles.FindAuth(harness.manager, "contended.json")
	if !ok || authfiles.OwnerUserID(auth) != createdBy {
		t.Fatalf("stored owner = %q, created_by = %q", authfiles.OwnerUserID(auth), createdBy)
	}
}

func TestUserAndAdminGates(t *testing.T) {
	harness := newUserTestHarness(t, true)
	if response := harness.request(t, "", http.MethodGet, "/v0/user/me", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("requireUser status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := harness.request(t, "user-a", http.MethodGet, "/v0/user/admin/users", nil); response.Code != http.StatusForbidden {
		t.Fatalf("requireAdmin user status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := harness.request(t, "admin", http.MethodGet, "/v0/user/admin/users", nil); response.Code != http.StatusOK {
		t.Fatalf("requireAdmin admin status = %d, body = %s", response.Code, response.Body.String())
	}
	profileResponse := harness.request(t, "user-a", http.MethodGet, "/v0/user/me", nil)
	if profileResponse.Code != http.StatusOK || !strings.Contains(profileResponse.Body.String(), `"$5.00"`) {
		t.Fatalf("profile quota is not formatted money: status=%d body=%s", profileResponse.Code, profileResponse.Body.String())
	}
}

func TestAdminCreateUserInitialAPIKey(t *testing.T) {
	harness := newUserTestHarness(t, true)

	t.Run("issued when requested", func(t *testing.T) {
		response := harness.request(t, "admin", http.MethodPost, "/v0/user/admin/users", []byte(`{
			"email":"new-user@example.com",
			"display_name":"New User",
			"role":"user",
			"tier":"default",
			"issue_key":true,
			"key_label":"first"
		}`))
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}

		var created struct {
			APIKey string `json:"api_key"`
			User   struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		if errDecode := json.Unmarshal(response.Body.Bytes(), &created); errDecode != nil {
			t.Fatalf("decode create response: %v", errDecode)
		}
		if created.APIKey == "" {
			t.Fatalf("create response did not contain initial API key: %s", response.Body.String())
		}
		keys, errList := harness.service.Store().ListAPIKeys(created.User.ID)
		if errList != nil {
			t.Fatalf("ListAPIKeys() error = %v", errList)
		}
		if len(keys) != 1 || keys[0].KeyHash != tenancy.HashAPIKey(created.APIKey) || keys[0].Label != "first" {
			t.Fatalf("stored keys = %#v, plaintext = %q", keys, created.APIKey)
		}
	})

	t.Run("not issued when unchecked", func(t *testing.T) {
		response := harness.request(t, "admin", http.MethodPost, "/v0/user/admin/users", []byte(`{
			"email":"no-key@example.com",
			"display_name":"No Key",
			"role":"user",
			"tier":"default",
			"issue_key":false
		}`))
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}

		var created struct {
			APIKey string `json:"api_key"`
			User   struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		if errDecode := json.Unmarshal(response.Body.Bytes(), &created); errDecode != nil {
			t.Fatalf("decode create response: %v", errDecode)
		}
		if created.APIKey != "" {
			t.Fatalf("create response unexpectedly contained an API key: %s", response.Body.String())
		}
		keys, errList := harness.service.Store().ListAPIKeys(created.User.ID)
		if errList != nil {
			t.Fatalf("ListAPIKeys() error = %v", errList)
		}
		if len(keys) != 0 {
			t.Fatalf("stored keys = %#v, want none", keys)
		}
	})
}

func TestTenancyDisabledLeavesUserRoutesAbsent(t *testing.T) {
	harness := newUserTestHarness(t, false)
	response := harness.request(t, "", http.MethodGet, "/v0/user/me", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestOAuthSessionIsBoundToInitiatingUser(t *testing.T) {
	harness := newUserTestHarness(t, true)
	state := fmt.Sprintf("state-%d", time.Now().UnixNano())
	management.RegisterOAuthSessionForUser(state, "codex", "user-a")
	body := []byte(fmt.Sprintf(`{"provider":"codex","code":"code","state":%q}`, state))

	otherResponse := harness.request(t, "user-b", http.MethodPost, "/v0/user/oauth-callback", body)
	if otherResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-user callback status = %d, body = %s", otherResponse.Code, otherResponse.Body.String())
	}
	callbackPath := filepath.Join(harness.cfg.AuthDir, ".oauth-codex-"+state+".oauth")
	if _, errStat := os.Stat(callbackPath); !os.IsNotExist(errStat) {
		t.Fatalf("cross-user callback created file: %v", errStat)
	}

	ownerResponse := harness.request(t, "user-a", http.MethodPost, "/v0/user/oauth-callback", body)
	if ownerResponse.Code != http.StatusOK {
		t.Fatalf("owner callback status = %d, body = %s", ownerResponse.Code, ownerResponse.Body.String())
	}
	if _, errStat := os.Stat(callbackPath); errStat != nil {
		t.Fatalf("owner callback file missing: %v", errStat)
	}

	statusResponse := harness.request(t, "user-b", http.MethodGet, "/v0/user/auth-status?state="+state, nil)
	if statusResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-user status = %d, body = %s", statusResponse.Code, statusResponse.Body.String())
	}
}

func TestAPIKeyIsReturnedOnceAndStoredAsHash(t *testing.T) {
	harness := newUserTestHarness(t, true)
	issueResponse := harness.request(t, "user-a", http.MethodPost, "/v0/user/api-keys", []byte(`{"label":"laptop"}`))
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("issue status = %d, body = %s", issueResponse.Code, issueResponse.Body.String())
	}
	var issued struct {
		APIKey string `json:"api_key"`
		Key    struct {
			Hash string `json:"hash"`
		} `json:"key"`
	}
	if errDecode := json.Unmarshal(issueResponse.Body.Bytes(), &issued); errDecode != nil {
		t.Fatalf("decode issue response: %v", errDecode)
	}
	if issued.APIKey == "" || issued.Key.Hash != tenancy.HashAPIKey(issued.APIKey) {
		t.Fatalf("invalid plaintext/hash response: %#v", issued)
	}
	keys, errList := harness.service.Store().ListAPIKeys("user-a")
	if errList != nil || len(keys) != 1 || keys[0].KeyHash != issued.Key.Hash {
		t.Fatalf("stored keys = %#v, error = %v", keys, errList)
	}
	listResponse := harness.request(t, "user-a", http.MethodGet, "/v0/user/api-keys", nil)
	if strings.Contains(listResponse.Body.String(), issued.APIKey) {
		t.Fatalf("plaintext key returned by list: %s", listResponse.Body.String())
	}

	crossDelete := harness.request(t, "user-b", http.MethodDelete, "/v0/user/api-keys?hash="+issued.Key.Hash, nil)
	if crossDelete.Code != http.StatusNotFound {
		t.Fatalf("cross-user key delete status = %d, body = %s", crossDelete.Code, crossDelete.Body.String())
	}
	if _, errLookup := harness.service.Store().LookupByAPIKey(issued.Key.Hash); errLookup != nil {
		t.Fatalf("cross-user delete revoked key: %v", errLookup)
	}
}
