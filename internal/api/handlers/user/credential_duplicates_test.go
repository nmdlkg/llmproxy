package user

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestUploadRejectsExistingAccount(t *testing.T) {
	for _, owner := range []string{"", "user-a", "user-b"} {
		t.Run("owner="+owner, func(t *testing.T) {
			h := newUserTestHarness(t, true)
			h.writeCredential(t, "existing.json", owner, true)
			before, errRead := os.ReadFile(filepath.Join(h.cfg.AuthDir, "existing.json"))
			if errRead != nil {
				t.Fatal(errRead)
			}
			for _, name := range []string{"existing.json", "renamed.json"} {
				body := []byte(`{"type":"claude","email":"EXISTING.JSON@example.com","access_token":"new-token"}`)
				response := h.request(t, "user-b", http.MethodPost, "/v0/user/credentials?name="+name, body)
				if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "already registered") {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
			}
			after, errRead := os.ReadFile(filepath.Join(h.cfg.AuthDir, "existing.json"))
			if errRead != nil || string(before) != string(after) {
				t.Fatal("existing credential changed")
			}
			if _, errStat := os.Stat(filepath.Join(h.cfg.AuthDir, "renamed.json")); !os.IsNotExist(errStat) {
				t.Fatal("duplicate was persisted")
			}
		})
	}
}

func TestConcurrentRenamedAccountUploads(t *testing.T) {
	h := newUserTestHarness(t, true)
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i, user := range []string{"user-a", "user-b"} {
		wg.Add(1)
		go func(i int, user string) {
			defer wg.Done()
			body := []byte(`{"type":"codex","account_id":"same-account","email":"same@example.com","access_token":"same-token"}`)
			statuses <- h.request(t, user, http.MethodPost, fmt.Sprintf("/v0/user/credentials?name=copy-%d.json", i), body).Code
		}(i, user)
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("statuses=%v", counts)
	}
}
