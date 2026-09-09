package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSharingPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		want     bool
	}{
		{"administrator", nil, true},
		{"administrator explicitly off", map[string]any{"shared": false}, false},
		{"owned private", map[string]any{"owner_user_id": "u1", "shared": false}, false},
		{"owned shared", map[string]any{"owner_user_id": "u1", "shared": true}, true},
		{"owned missing flag", map[string]any{"owner_user_id": "u1"}, false},
		{"legacy string true", map[string]any{"shared": "true"}, true},
		{"legacy string false", map[string]any{"shared": "false"}, false},
		{"malformed flag", map[string]any{"shared": 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shareable(pluginapi.SchedulerAuthCandidate{Metadata: tc.metadata}); got != tc.want {
				t.Fatalf("shareable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPickFiltersAndRotatesAcrossProviders(t *testing.T) {
	var s scheduler
	req := pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "private", Provider: "codex", Metadata: map[string]any{"shared": false}},
		{ID: "b", Provider: "claude", Metadata: map[string]any{"shared": true}},
		{ID: "a", Provider: "codex"},
	}}
	for _, want := range []string{"a", "b", "a", "b"} {
		got := s.pick(req)
		if !got.Handled || got.AuthID != want || got.DelegateBuiltin != "" {
			t.Fatalf("pick = %#v, want handled %s", got, want)
		}
	}
	// A hot-reloaded sharing toggle must apply on the very next pick.
	req.Candidates[1].Metadata["shared"] = false
	for i := 0; i < 3; i++ {
		if got := s.pick(req); got.AuthID != "a" {
			t.Fatalf("pick after toggle = %#v", got)
		}
	}
}

func TestPickPreservesFallback(t *testing.T) {
	var s scheduler
	for _, candidates := range [][]pluginapi.SchedulerAuthCandidate{
		nil,
		{{ID: "private", Metadata: map[string]any{"shared": false}}},
	} {
		if got := s.pick(pluginapi.SchedulerPickRequest{Candidates: candidates}); got.Handled || got.AuthID != "" {
			t.Fatalf("pick = %#v, want unhandled fallback", got)
		}
	}
}

func TestSchedulerJSONContract(t *testing.T) {
	raw, errCall := handleMethod(pluginabi.MethodSchedulerPick, []byte(`{"Candidates":[{"ID":"private","Metadata":{"shared":false}},{"ID":"shared","Metadata":{"shared":true}}]}`))
	if errCall != nil {
		t.Fatal(errCall)
	}
	var env envelope
	if errDecode := json.Unmarshal(raw, &env); errDecode != nil {
		t.Fatal(errDecode)
	}
	var result pluginapi.SchedulerPickResponse
	if errDecode := json.Unmarshal(env.Result, &result); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !env.OK || !result.Handled || result.AuthID != "shared" {
		t.Fatalf("response = %s", raw)
	}
}
