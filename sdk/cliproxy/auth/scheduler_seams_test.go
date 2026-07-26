package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSchedulerPriorityResolver(t *testing.T) {
	tests := []struct {
		name        string
		resolver    PriorityResolver
		setResolver bool
		want        []string
	}{
		{
			name: "unset preserves attribute priority sequence",
			want: []string{"attribute-high-a", "attribute-high-b", "attribute-high-a", "attribute-high-b"},
		},
		{
			name: "override wins and remains fair inside effective tier",
			resolver: func(auth *Auth) (int, bool) {
				switch auth.ID {
				case "resolved-high-a", "resolved-high-b":
					return 20, true
				default:
					return 0, false
				}
			},
			setResolver: true,
			want:        []string{"resolved-high-a", "resolved-high-b", "resolved-high-a", "resolved-high-b"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			auths := []*Auth{
				{ID: "attribute-high-b", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
				{ID: "attribute-high-a", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
				{ID: "resolved-high-b", Provider: "gemini", Attributes: map[string]string{"priority": "0"}},
				{ID: "resolved-high-a", Provider: "gemini", Attributes: map[string]string{"priority": "0"}},
			}
			for _, auth := range auths {
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatalf("Register(%q) error = %v", auth.ID, errRegister)
				}
			}
			if test.setResolver {
				manager.SetPriorityResolver(test.resolver)
			}

			if _, ok := manager.Selector().(*RoundRobinSelector); !ok {
				t.Fatalf("Selector() type = %T, want *RoundRobinSelector", manager.Selector())
			}
			for index, wantID := range test.want {
				got, errPick := manager.scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
				if errPick != nil {
					t.Fatalf("pickSingle() #%d error = %v", index, errPick)
				}
				if got == nil {
					t.Fatalf("pickSingle() #%d auth = nil", index)
				}
				if got.ID != wantID {
					t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
				}
			}
		})
	}
}

func TestSchedulerPreferredAuthSingleProvider(t *testing.T) {
	tests := []struct {
		name                   string
		preferredValue         any
		preferredModel         string
		preferredModelState    func(string) map[string]*ModelState
		preferredDisabled      bool
		tried                  map[string]struct{}
		wantID                 string
		pinnedID               string
		registerPreferredModel bool
	}{
		{
			name:                   "ready preferred auth wins below highest priority",
			preferredValue:         []string{"preferred"},
			wantID:                 "preferred",
			registerPreferredModel: true,
		},
		{
			name:           "cooling preferred auth falls through",
			preferredValue: "preferred",
			preferredModelState: func(model string) map[string]*ModelState {
				return map[string]*ModelState{
					model: {
						Status:         StatusError,
						Unavailable:    true,
						NextRetryAfter: time.Now().Add(time.Hour),
					},
				}
			},
			wantID:                 "normal",
			registerPreferredModel: true,
		},
		{
			name:                   "model incompatible preferred auth falls through",
			preferredValue:         []byte("preferred"),
			preferredModel:         "other-model",
			wantID:                 "normal",
			registerPreferredModel: true,
		},
		{
			name:                   "tried preferred auth falls through",
			preferredValue:         []any{[]byte("preferred")},
			tried:                  map[string]struct{}{"preferred": {}},
			wantID:                 "normal",
			registerPreferredModel: true,
		},
		{
			name:                   "disabled preferred auth falls through",
			preferredValue:         [][]byte{[]byte("preferred")},
			preferredDisabled:      true,
			wantID:                 "normal",
			registerPreferredModel: true,
		},
		{
			name:                   "pinned auth retains precedence",
			preferredValue:         []string{"preferred"},
			pinnedID:               "normal",
			wantID:                 "normal",
			registerPreferredModel: true,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := fmt.Sprintf("preferred-single-%d", index)
			preferredModel := test.preferredModel
			if preferredModel == "" {
				preferredModel = model
			}
			registerSchedulerModels(t, "gemini", model, "normal")
			if test.registerPreferredModel {
				registerSchedulerModels(t, "gemini", preferredModel, "preferred")
			}

			preferred := &Auth{
				ID:          "preferred",
				Provider:    "gemini",
				Disabled:    test.preferredDisabled,
				Attributes:  map[string]string{"priority": "-10"},
				ModelStates: nil,
			}
			if test.preferredModelState != nil {
				preferred.ModelStates = test.preferredModelState(model)
			}
			scheduler := newSchedulerForTest(
				&RoundRobinSelector{},
				&Auth{ID: "normal", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
				preferred,
			)
			metadata := map[string]any{PreferredAuthIDsMetadataKey: test.preferredValue}
			if test.pinnedID != "" {
				metadata[cliproxyexecutor.PinnedAuthMetadataKey] = test.pinnedID
			}

			got, errPick := scheduler.pickSingle(
				context.Background(),
				"gemini",
				model,
				cliproxyexecutor.Options{Metadata: metadata},
				test.tried,
			)
			if errPick != nil {
				t.Fatalf("pickSingle() error = %v", errPick)
			}
			if got == nil {
				t.Fatal("pickSingle() auth = nil")
			}
			if got.ID != test.wantID {
				t.Fatalf("pickSingle() auth.ID = %q, want %q", got.ID, test.wantID)
			}
		})
	}
}

func TestManagerPreferredCoolingAuthRequestFallsThrough(t *testing.T) {
	const model = "preferred-cooldown-execution"
	registerSchedulerModels(t, "gemini", model, "normal", "preferred")

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "gemini"})
	auths := []*Auth{
		{ID: "normal", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
		{
			ID:         "preferred",
			Provider:   "gemini",
			Attributes: map[string]string{"priority": "-10"},
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(time.Hour),
				},
			},
		},
	}
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%q) error = %v", auth.ID, errRegister)
		}
	}

	selectedID := ""
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		PreferredAuthIDsMetadataKey:                      []string{"preferred"},
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) { selectedID = authID },
	}}
	if _, errExecute := manager.Execute(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{Model: model}, opts); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if selectedID != "normal" {
		t.Fatalf("selected auth ID = %q, want %q", selectedID, "normal")
	}
}

func TestSchedulerPreferredAuthMixedProvider(t *testing.T) {
	const model = "preferred-mixed"
	registerSchedulerModels(t, "gemini", model, "gemini-normal")
	registerSchedulerModels(t, "claude", model, "claude-preferred")

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "gemini-normal", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
		&Auth{ID: "claude-preferred", Provider: "claude", Attributes: map[string]string{"priority": "-10"}},
	)
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		PreferredAuthIDsMetadataKey: []string{"claude-preferred"},
	}}

	got, provider, errPick := scheduler.pickMixed(context.Background(), []string{"gemini", "claude"}, model, opts, nil)
	if errPick != nil {
		t.Fatalf("pickMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickMixed() auth = nil")
	}
	if provider != "claude" {
		t.Fatalf("pickMixed() provider = %q, want %q", provider, "claude")
	}
	if got.ID != "claude-preferred" {
		t.Fatalf("pickMixed() auth.ID = %q, want %q", got.ID, "claude-preferred")
	}
}

func TestPreferredAuthIDsFromMetadata(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  []string
	}{
		{name: "string", value: " auth-a ", want: []string{"auth-a"}},
		{name: "bytes", value: []byte(" auth-a "), want: []string{"auth-a"}},
		{name: "string list", value: []string{"auth-a", " ", "auth-b"}, want: []string{"auth-a", "auth-b"}},
		{name: "byte list", value: [][]byte{[]byte("auth-a"), []byte("auth-b")}, want: []string{"auth-a", "auth-b"}},
		{name: "interface list", value: []any{"auth-a", []byte("auth-b"), 42}, want: []string{"auth-a", "auth-b"}},
		{name: "unsupported", value: 42},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := preferredAuthIDsFromMetadata(map[string]any{PreferredAuthIDsMetadataKey: test.value})
			if len(got) != len(test.want) {
				t.Fatalf("preferredAuthIDsFromMetadata() size = %d, want %d", len(got), len(test.want))
			}
			for _, authID := range test.want {
				if _, ok := got[authID]; !ok {
					t.Fatalf("preferredAuthIDsFromMetadata() missing %q", authID)
				}
			}
		})
	}
}
