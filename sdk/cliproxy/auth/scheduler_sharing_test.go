package auth

import (
	"context"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPluginSchedulerReceivesSharingMetadata(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		manager := NewManager(nil, &RoundRobinSelector{}, nil)
		manager.RegisterExecutor(schedulerTestExecutor{provider: "gemini"})
		_, errRegister := manager.Register(context.Background(), &Auth{
			ID: "owned", Provider: "gemini",
			Metadata: map[string]any{"owner_user_id": "user-1", "shared": false, "access_token": "secret", "nested": map[string]any{"token": "secret"}},
		})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		calls := 0
		manager.SetPluginScheduler(&fakePluginScheduler{pick: func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
			calls++
			if len(req.Candidates) != 1 {
				t.Fatalf("candidates = %d", len(req.Candidates))
			}
			metadata := req.Candidates[0].Metadata
			if len(metadata) != 2 || metadata["shared"] != false || metadata["owner_user_id"] != "user-1" {
				t.Fatal("sharing metadata missing or non-allowlisted metadata exposed")
			}
			metadata["shared"] = true
			return pluginapi.SchedulerPickResponse{Handled: false}, false, nil
		}})
		var selected *Auth
		var errPick error
		if mixed {
			selected, _, _, errPick = manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		} else {
			selected, _, errPick = manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		}
		if errPick != nil || selected == nil || selected.ID != "owned" || calls != 1 {
			t.Fatalf("fallback failed: calls=%d, err=%v", calls, errPick)
		}
		stored, _ := manager.GetByID("owned")
		if stored.Metadata["shared"] != false {
			t.Fatal("plugin mutated stored sharing flag")
		}
	}
}

func TestSchedulerCandidateMetadataAccountIdentity(t *testing.T) {
	first := schedulerCandidateMetadata(&Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-1", "email": "same@example.com", "access_token": "secret-a"}})
	duplicate := schedulerCandidateMetadata(&Auth{Provider: "Codex", Attributes: map[string]string{"account_id": "acct-1", "email": "SAME@example.com"}, Metadata: map[string]any{"access_token": "secret-b"}})
	other := schedulerCandidateMetadata(&Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-2"}})
	seatA := schedulerCandidateMetadata(&Auth{Provider: "codex", Metadata: map[string]any{"account_id": "workspace-1", "email": "alice@example.com"}})
	seatB := schedulerCandidateMetadata(&Auth{Provider: "codex", Metadata: map[string]any{"account_id": "workspace-1", "email": "bob@example.com"}})
	identity, _ := first["account_identity"].(string)
	if identity == "" || identity != duplicate["account_identity"] || identity == other["account_identity"] {
		t.Fatalf("identities: first=%v duplicate=%v other=%v", first, duplicate, other)
	}
	if seatA["account_identity"] == seatB["account_identity"] {
		t.Fatalf("different seats in one workspace must have different identities: seatA=%v seatB=%v", seatA, seatB)
	}
	for _, rawValue := range []string{"acct-1", "workspace-1", "alice@example.com", "bob@example.com"} {
		if strings.Contains(identity, rawValue) || strings.Contains(seatA["account_identity"].(string), rawValue) || strings.Contains(seatB["account_identity"].(string), rawValue) {
			t.Fatalf("identity must not contain raw value %q: first=%v seatA=%v seatB=%v", rawValue, first, seatA, seatB)
		}
	}
	if len(first) != 1 || len(seatA) != 1 || len(seatB) != 1 {
		t.Fatalf("identity must be the only exposed field: first=%v seatA=%v seatB=%v", first, seatA, seatB)
	}
	if tokenOnly := schedulerCandidateMetadata(&Auth{Provider: "codex", Metadata: map[string]any{"access_token": "secret"}}); tokenOnly != nil {
		t.Fatalf("token-derived identity must not be exposed: %v", tokenOnly)
	}
}
