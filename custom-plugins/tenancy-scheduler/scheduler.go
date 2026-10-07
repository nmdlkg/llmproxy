package main

import (
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cliproxy-tenancy-scheduler/optimizer"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var sharedScheduler scheduler

// scheduler applies the sharing policy, then selects with the configured mode.
// A single legacy cursor avoids retaining unbounded state for client models.
type scheduler struct {
	cursor atomic.Uint64

	lifecycleMu sync.Mutex
	saveMu      sync.Mutex
	mu          sync.RWMutex
	cfg         pluginConfig
	engine      *optimizer.Engine
	worker      *optimizer.Worker
	saveEnabled bool
}

func shareable(candidate pluginapi.SchedulerAuthCandidate) bool {
	if schedulerWeight(candidate) <= 0 {
		return false
	}
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

func schedulerWeight(candidate pluginapi.SchedulerAuthCandidate) float64 {
	if raw, ok := candidate.Attributes["weight"]; ok && strings.TrimSpace(raw) != "" {
		weight, errParse := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if errParse != nil {
			return 0
		}
		return float64(weight)
	}
	if raw, ok := candidate.Metadata["weight"]; ok {
		switch value := raw.(type) {
		case int:
			return float64(value)
		case int64:
			return float64(value)
		case float64:
			if value == float64(int64(value)) {
				return value
			}
		case string:
			weight, errParse := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if errParse == nil {
				return float64(weight)
			}
		}
		return 0
	}
	return 1
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
	var total uint64
	for _, candidate := range sorted {
		weight := schedulerWeight(candidate)
		if weight > 0 {
			total += uint64(weight)
		}
	}
	if total == 0 {
		return ""
	}
	position := (s.cursor.Add(1) - 1) % total
	for _, candidate := range sorted {
		weight := schedulerWeight(candidate)
		if weight <= 0 {
			continue
		}
		w := uint64(weight)
		if position < w {
			return candidate.ID
		}
		position -= w
	}
	return sorted[len(sorted)-1].ID
}

func optimizerCandidates(eligible []pluginapi.SchedulerAuthCandidate) []optimizer.Candidate {
	out := make([]optimizer.Candidate, 0, len(eligible))
	for _, candidate := range eligible {
		account, _ := candidate.Metadata["account_identity"].(string)
		out = append(out, optimizer.Candidate{ID: candidate.ID, Provider: candidate.Provider, Account: account, Weight: schedulerWeight(candidate)})
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
	defer s.mu.RUnlock()
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
	if mode == modeClaudeSimple {
		if len(legacyPool) == 0 {
			return pluginapi.SchedulerPickResponse{Handled: false}
		}
		claudePool := make([]pluginapi.SchedulerAuthCandidate, 0, len(legacyPool))
		for _, candidate := range legacyPool {
			if strings.EqualFold(candidate.Provider, "claude") {
				claudePool = append(claudePool, candidate)
			}
		}
		if len(claudePool) == 0 {
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: s.rotate(legacyPool)}
		}
		decision := engine.Pick(req.Model, optimizerCandidates(claudePool), "")
		if decision.AuthID == "" {
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: s.rotate(legacyPool)}
		}
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: decision.AuthID}
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
		// Shadow retains optimizer-vs-legacy telemetry. Claude requests use the
		// optimizer's weekly-surplus decision for execution, while all other
		// providers continue executing the legacy rotation choice.
		if len(optimizerPool) > 0 {
			hasClaude := false
			for _, candidate := range legacyPool {
				hasClaude = hasClaude || strings.EqualFold(candidate.Provider, "claude")
			}
			if hasClaude {
				claudePool := make([]pluginapi.SchedulerAuthCandidate, 0, len(optimizerPool))
				for _, candidate := range optimizerPool {
					if strings.EqualFold(candidate.Provider, "claude") {
						claudePool = append(claudePool, candidate)
					}
				}
				if len(claudePool) > 0 {
					actual := engine.PickShadow(req.Model, optimizerCandidates(claudePool), legacyID, "")
					if actual.AuthID != "" {
						return pluginapi.SchedulerPickResponse{Handled: true, AuthID: actual.AuthID}
					}
				}
			}
			engine.Pick(req.Model, optimizerCandidates(optimizerPool), legacyID)
		}
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
	defer s.mu.RUnlock()
	engine := s.engine
	if engine == nil {
		return
	}
	observedAt := record.RequestedAt
	if !observedAt.IsZero() && record.TTFT > 0 {
		observedAt = observedAt.Add(record.TTFT)
	}
	model := record.Alias
	if model == "" {
		model = record.Model
	}
	engine.RecordUsage(optimizer.Usage{
		AuthID:      record.AuthID,
		Provider:    record.Provider,
		Model:       model,
		Generate:    record.Generate,
		Failed:      record.Failed,
		Latency:     record.Latency,
		TTFT:        record.TTFT,
		StatusCode:  record.Failure.StatusCode,
		RequestedAt: record.RequestedAt,
		CompletedAt: observedAt,
		Tokens: optimizer.EffectiveTokens(record.Detail.InputTokens, record.Detail.OutputTokens,
			record.Detail.ReasoningTokens, record.Detail.CachedTokens, record.Detail.TotalTokens),
		Headers: record.ResponseHeaders,
	})
}

// configure applies a (re)registration. The engine keeps its learned state
// across reconfiguration unless its settings change; legacy mode stops it.
func (s *scheduler) configure(cfg pluginConfig) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.mu.Lock()
	previous := s.cfg
	if cfg.Mode != modeLegacy && s.engine != nil && s.worker != nil && s.saveEnabled &&
		previous.Optimizer == cfg.Optimizer && previous.StatePath == cfg.StatePath &&
		previous.RecomputeEvery == cfg.RecomputeEvery && previous.CheckpointEvery == cfg.CheckpointEvery {
		if previous.Mode != cfg.Mode || previous.AcrossPriorities != cfg.AcrossPriorities {
			s.engine.StartEpoch(cfg.Mode, cfg.AcrossPriorities, cfg.RecomputeEvery, cfg.CheckpointEvery)
		}
		s.cfg = cfg
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	engine, worker, save := s.detach()
	worker.Stop()
	if engine != nil && save {
		_ = engine.Save(previous.StatePath, previous.Mode)
	}
	if cfg.Mode == modeLegacy {
		s.mu.Lock()
		s.cfg = cfg
		s.mu.Unlock()
		return
	}

	engine = optimizer.NewEngine(cfg.Optimizer, nil)
	save = true
	if errLoad := engine.Load(cfg.StatePath); errLoad != nil {
		// Preserve inaccessible state; only invalid content may be quarantined.
		save = errors.Is(errLoad, optimizer.ErrCorruptState) && quarantineState(cfg.StatePath) == nil
	}
	engine.StartEpoch(cfg.Mode, cfg.AcrossPriorities, cfg.RecomputeEvery, cfg.CheckpointEvery)
	s.mu.Lock()
	s.cfg = cfg
	s.engine = engine
	s.saveEnabled = save
	s.worker = optimizer.StartWorker(engine, cfg.RecomputeEvery, cfg.CheckpointEvery, func() {
		_ = s.checkpointEngine(engine)
	})
	s.mu.Unlock()
}

func quarantineState(path string) error {
	if path == "" {
		return nil
	}
	return os.Rename(path, path+".corrupt-"+time.Now().UTC().Format("20060102T150405Z"))
}

// checkpoint flushes optimizer state without stopping it.
func (s *scheduler) checkpoint() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.checkpointEngine(nil)
}

func (s *scheduler) checkpointEngine(expected *optimizer.Engine) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	engine, path, mode, enabled := s.engine, s.cfg.StatePath, s.cfg.Mode, s.saveEnabled
	s.mu.RUnlock()
	if engine == nil || !enabled || (expected != nil && engine != expected) {
		return nil
	}
	return engine.Save(path, mode)
}

// quiesce makes the current instance stop writing its state file before a
// replacement instance can load it.
func (s *scheduler) quiesce() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.saveMu.Lock()
	s.mu.RLock()
	engine, path, mode, enabled := s.engine, s.cfg.StatePath, s.cfg.Mode, s.saveEnabled
	s.mu.RUnlock()
	if engine != nil && enabled {
		if errSave := engine.Save(path, mode); errSave != nil {
			s.saveMu.Unlock()
			return errSave
		}
	}
	s.mu.Lock()
	s.saveEnabled = false
	worker := s.worker
	s.worker = nil
	s.mu.Unlock()
	s.saveMu.Unlock()
	worker.Stop()
	return nil
}

func (s *scheduler) shutdown() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	engine, worker, save := s.detach()
	worker.Stop()
	if engine != nil && save {
		_ = engine.Save(cfg.StatePath, cfg.Mode)
	}
}

// detach waits for any in-flight checkpoint, then removes the active engine
// without holding the pick lock during worker shutdown or disk I/O.
func (s *scheduler) detach() (*optimizer.Engine, *optimizer.Worker, bool) {
	s.saveMu.Lock()
	s.mu.Lock()
	engine, worker, save := s.engine, s.worker, s.saveEnabled
	s.engine, s.worker, s.saveEnabled = nil, nil, false
	s.mu.Unlock()
	s.saveMu.Unlock()
	return engine, worker, save
}
