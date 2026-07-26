package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRemoteStoreSaveUploadsCredentialWithoutOwner(t *testing.T) {
	var gotAuthorization string
	var gotName string
	var gotPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotAuthorization = request.Header.Get("Authorization")
		gotName = request.URL.Query().Get("name")
		if errDecode := json.NewDecoder(request.Body).Decode(&gotPayload); errDecode != nil {
			t.Errorf("decode credential: %v", errDecode)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	store, errNew := NewRemoteStore(server.URL, "user-secret")
	if errNew != nil {
		t.Fatalf("NewRemoteStore() error = %v", errNew)
	}
	_, errSave := store.Save(context.Background(), &coreauth.Auth{
		ID:       "claude-person.json",
		FileName: "claude-person.json",
		Provider: "claude",
		Metadata: map[string]any{
			"access_token":  "provider-secret",
			"owner_user_id": "client-supplied-owner",
		},
	})
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	if gotAuthorization != "Bearer user-secret" {
		t.Fatalf("Authorization = %q", gotAuthorization)
	}
	if gotName != "claude-person.json" {
		t.Fatalf("name = %q", gotName)
	}
	if gotPayload["type"] != "claude" || gotPayload["access_token"] != "provider-secret" {
		t.Fatalf("payload = %#v", gotPayload)
	}
	if _, exists := gotPayload["owner_user_id"]; exists {
		t.Fatalf("payload contains client owner: %#v", gotPayload)
	}
}

func TestNewRemoteStoreRejectsIncompleteConfiguration(t *testing.T) {
	tests := []struct {
		remote string
		key    string
	}{
		{remote: "", key: "key"},
		{remote: "https://proxy.example", key: ""},
		{remote: "proxy.example", key: "key"},
		{remote: "ftp://proxy.example", key: "key"},
		{remote: "https://user@proxy.example", key: "key"},
		{remote: "http://proxy.example", key: "key"},
	}
	for _, test := range tests {
		if _, errNew := NewRemoteStore(test.remote, test.key); errNew == nil {
			t.Fatalf("NewRemoteStore(%q, key=%t) succeeded", test.remote, test.key != "")
		}
	}
}
