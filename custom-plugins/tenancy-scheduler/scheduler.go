package main

import (
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cliproxy-tenancy-scheduler/optimizer"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var sharedScheduler scheduler

// scheduler applies the sharing policy, then selects with the configured mode.
// A single legacy cursor avoids retaining unbounded state for client models.
type scheduler struct {
	cursor atomic.Uint64

	mu     sync.RWMutex
	cfg    pluginConfig
	engine *optimizer.Engine
	worker *optimizer.Worker
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

// highestTier restricts eligible candidates to the highest priority among all
// offered candidates. With default host semantics every candidate is already in
// one tier; with across-priorities the legacy decision keeps tier semantics.
func highestTier(all, eligible []pluginapi.SchedulerAuthCandidate) []pluginapi.SchedulerAuthCandidate {
	if len(all) == 0 {
		return nil
	}
	top := all[0].Priority
	for _, candidate := range all[1:] {
		top = max(top, candidate.Priority)
	}
	out := eligible[:0:0]
	for _, candidate := range eligible {
		if candidate.Priority == top {
			out = append(out, candidate)
		}
	}
	return out
}

func (s *scheduler) rotate(eligible []pluginapi.SchedulerAuthCandidate) string {
	if len(eligible) == 0 {
		return ""
	}
	sorted := append([]pluginapi.SchedulerAuthCandidate(nil), eligible...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	index := (s.cursor.Add(1) - 1) % uint64(len(sorted))
	return sorted[index].ID
}

func optimizerCandidates(eligible []pluginapi.SchedulerAuthCandidate) []optimizer.Candidate {
	out := make([]optimizer.Candidate, 0, len(eligible))
	for _, candidate := range eligible {
		account, _ := candidate.Metadata["account_identity"].(string)
		out = append(out, optimizer.Candidate{ID: candidate.ID, Provider: candidate.Provider, Account: account})
	}
	return out
}

func (s *scheduler) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	eligible := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidate.ID != "" && shareable(candidate) {
			eligible = append(eligible, candidate)
		}
	}
	s.mu.RLock()
	mode, engine, across := s.cfg.Mode, s.engine, s.cfg.AcrossPriorities
	s.mu.RUnlock()
	if mode == "" {
		mode = modeLegacy
	}

	legacyPool := highestTier(req.Candidates, eligible)
	if engine == nil || mode == modeLegacy {
		if len(legacyPool) == 0 {
			// Best-effort sharing policy: preserve the host fallback.
			return pluginapi.SchedulerPickResponse{Handled: false}
		}
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: s.rotate(legacyPool)}
	}

	optimizerPool := legacyPool
	if across {
		optimizerPool = eligible
	}
	if mode == modeShadow {
		if len(legacyPool) == 0 {
			return pluginapi.SchedulerPickResponse{Handled: false}
		}
		legacyID := s.rotate(legacyPool)
		engine.Pick(req.Model, optimizerCandidates(optimizerPool), legacyID)
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: legacyID}
	}
	if len(optimizerPool) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
	decision := engine.Pick(req.Model, optimizerCandidates(optimizerPool), "")
	if decision.AuthID == "" {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: decision.AuthID}
}

// handleUsage feeds completed attempts to the optimizer. Only the fields used
// for estimation are copied; client API keys and bodies are dropped here.
func (s *scheduler) handleUsage(record pluginapi.UsageRecord) {
	s.mu.RLock()
	engine := s.engine
	s.mu.RUnlock()
	if engine == nil {
		return
	}
	completedAt := record.RequestedAt
	if !completedAt.IsZero() {
		completedAt = completedAt.Add(record.Latency)
	}
	engine.RecordUsage(optimizer.Usage{
		AuthID:      record.AuthID,
		Provider:    record.Provider,
		Model:       record.Model,
		Generate:    record.Generate,
		Failed:      record.Failed,
		Latency:     record.Latency,
		CompletedAt: completedAt,
		Tokens: optimizer.EffectiveTokens(record.Detail.InputTokens, record.Detail.OutputTokens,
			record.Detail.ReasoningTokens, record.Detail.CachedTokens, record.Detail.TotalTokens),
		Headers: record.ResponseHeaders,
	})
}

// configure applies a (re)registration. The engine keeps its learned state
// across reconfiguration unless its settings change; legacy mode stops it.
func (s *scheduler) configure(cfg pluginConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.cfg
	s.cfg = cfg
	if cfg.Mode == modeLegacy {
		s.stopLocked(previous)
		return
	}
	if s.engine != nil && previous.Optimizer == cfg.Optimizer && previous.StatePath == cfg.StatePath &&
		previous.RecomputeEvery == cfg.RecomputeEvery && previous.CheckpointEvery == cfg.CheckpointEvery {
		return
	}
	s.stopLocked(previous)
	engine := optimizer.NewEngine(cfg.Optimizer, nil)
	if errLoad := engine.Load(cfg.StatePath); errLoad != nil {
		// Keep the unreadable file for inspection and start from an empty belief.
		quarantineState(cfg.StatePath)
		engine = optimizer.NewEngine(cfg.Optimizer, nil)
	}
	s.engine = engine
	statePath, mode := cfg.StatePath, cfg.Mode
	s.worker = optimizer.StartWorker(engine, cfg.RecomputeEvery, cfg.CheckpointEvery, func() {
		_ = engine.Save(statePath, mode)
	})
}

func quarantineState(path string) {
	if path == "" {
		return
	}
	_ = os.Rename(path, path+".corrupt-"+time.Now().UTC().Format("20060102T150405Z"))
}

// checkpoint flushes optimizer state without stopping it.
func (s *scheduler) checkpoint() error {
	s.mu.RLock()
	engine, path, mode := s.engine, s.cfg.StatePath, s.cfg.Mode
	s.mu.RUnlock()
	if engine == nil {
		return nil
	}
	return engine.Save(path, mode)
}

func (s *scheduler) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked(s.cfg)
}

// stopLocked stops background work and checkpoints with the settings the
// engine was started with.
func (s *scheduler) stopLocked(cfg pluginConfig) {
	if s.worker != nil {
		s.worker.Stop()
		s.worker = nil
	}
	if s.engine != nil {
		_ = s.engine.Save(cfg.StatePath, cfg.Mode)
		s.engine = nil
	}
}
