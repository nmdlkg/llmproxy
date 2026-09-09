package main

import (
	"sort"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var sharedScheduler scheduler

// A single cursor avoids retaining unbounded state for client-supplied models.
type scheduler struct {
	cursor atomic.Uint64
}

func shareable(candidate pluginapi.SchedulerAuthCandidate) bool {
	if value, exists := candidate.Metadata["shared"]; exists {
		switch shared := value.(type) {
		case bool:
			return shared
		case string:
			return strings.EqualFold(strings.TrimSpace(shared), "true")
		default:
			return false
		}
	}
	owner, _ := candidate.Metadata["owner_user_id"].(string)
	return strings.TrimSpace(owner) == ""
}

func (s *scheduler) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	eligible := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidate.ID != "" && shareable(candidate) {
			eligible = append(eligible, candidate)
		}
	}
	if len(eligible) == 0 {
		// This is a best-effort sharing policy. Preserve the host fallback.
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })
	index := (s.cursor.Add(1) - 1) % uint64(len(eligible))
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: eligible[index].ID}
}
