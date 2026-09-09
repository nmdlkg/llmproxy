package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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
