package auth

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// defaultCodexPlanModelUnsupportedCooldown is the tier-wide cooldown applied when
// Codex rejects a model for a ChatGPT plan. New models usually roll out to the pro
// tier first, so lower tiers are re-probed once per period until the rollout lands.
const defaultCodexPlanModelUnsupportedCooldown = 24 * time.Hour

var codexPlanModelUnsupportedCooldownSeconds atomic.Int64

// SetCodexPlanModelUnsupportedCooldownSeconds configures the plan-tier-wide cooldown
// for Codex "model is not supported when using Codex with a ChatGPT account" errors.
// 0 keeps the default (24h); negative values disable tier-wide propagation and fall
// back to the per-credential model-support cooldown.
func SetCodexPlanModelUnsupportedCooldownSeconds(seconds int) {
	codexPlanModelUnsupportedCooldownSeconds.Store(int64(seconds))
}

// codexPlanModelUnsupportedCooldown returns the configured period and whether the
// tier-wide cooldown is enabled.
func codexPlanModelUnsupportedCooldown() (time.Duration, bool) {
	seconds := codexPlanModelUnsupportedCooldownSeconds.Load()
	if seconds < 0 {
		return 0, false
	}
	if seconds == 0 {
		return defaultCodexPlanModelUnsupportedCooldown, true
	}
	return time.Duration(seconds) * time.Second, true
}

// isCodexPlanModelUnsupportedError reports the Codex backend rejection for a model
// that the credential's ChatGPT plan cannot use yet. Upstream answers with HTTP 400
// and a FastAPI-style body without error.code, e.g.
// {"detail":"The 'gpt-6-luna' model is not supported when using Codex with a ChatGPT account."}
func isCodexPlanModelUnsupportedError(provider string, err *Error) bool {
	if err == nil || !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return false
	}
	switch err.StatusCode() {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
	default:
		return false
	}
	return strings.Contains(strings.ToLower(err.Message), "not supported when using codex with a chatgpt account")
}

// codexPlanTier returns the normalized ChatGPT plan tier of a Codex credential.
func codexPlanTier(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(auth.Attributes["plan_type"]))
}

// codexPlanTierPeersLocked returns other active Codex credentials on the same plan tier.
// Callers must hold m.mu.
func (m *Manager) codexPlanTierPeersLocked(origin *Auth) []*Auth {
	tier := codexPlanTier(origin)
	if m == nil || origin == nil || tier == "" {
		return nil
	}
	peers := make([]*Auth, 0)
	for _, peer := range m.auths {
		if peer == nil || peer.ID == origin.ID || peer.Disabled || peer.Status == StatusDisabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(peer.Provider), "codex") || codexPlanTier(peer) != tier {
			continue
		}
		peers = append(peers, peer)
	}
	return peers
}

// applyCodexPlanModelCooldownLocked cools modelKey on every Codex credential sharing the
// origin's plan tier and returns snapshots of the changed peers. Callers must hold m.mu.
func (m *Manager) applyCodexPlanModelCooldownLocked(ctx context.Context, origin *Auth, modelKey string, resultErr *Error, now time.Time) []*Auth {
	period, enabled := codexPlanModelUnsupportedCooldown()
	if !enabled || modelKey == "" {
		return nil
	}
	next := now.Add(period)
	var snapshots []*Auth
	for _, peer := range m.codexPlanTierPeersLocked(origin) {
		if m.cooldownDisabledForAuth(peer) {
			continue
		}
		state := ensureModelState(peer, modelKey)
		if state == nil {
			continue
		}
		state.Unavailable = true
		state.Status = StatusError
		state.UpdatedAt = now
		state.LastError = cloneError(resultErr)
		if resultErr != nil {
			state.StatusMessage = resultErr.Message
		}
		// Never shorten a longer still-live deadline.
		if !state.NextRetryAfter.After(next) {
			state.NextRetryAfter = next
		}
		peer.Status = StatusError
		updateAggregatedAvailability(peer, now)
		peer.Generation++
		peer.UpdatedAt = now
		_ = m.persist(ctx, peer)
		snapshots = append(snapshots, peer.Clone())
	}
	return snapshots
}

// clearCodexPlanModelCooldownLocked lifts a tier-wide plan cooldown for modelKey once any
// credential on the tier succeeds, so a completed rollout is picked up immediately.
// Callers must hold m.mu.
func (m *Manager) clearCodexPlanModelCooldownLocked(ctx context.Context, origin *Auth, modelKey string, now time.Time) []*Auth {
	if modelKey == "" {
		return nil
	}
	var snapshots []*Auth
	for _, peer := range m.codexPlanTierPeersLocked(origin) {
		state := existingModelState(peer, modelKey)
		if state == nil || !state.NextRetryAfter.After(now) || !isCodexPlanModelUnsupportedError(peer.Provider, state.LastError) {
			continue
		}
		resetModelState(state, now)
		updateAggregatedAvailability(peer, now)
		if !hasModelError(peer, now) {
			peer.LastError = nil
			peer.StatusMessage = ""
			peer.Status = StatusActive
		}
		peer.Generation++
		peer.UpdatedAt = now
		_ = m.persist(ctx, peer)
		snapshots = append(snapshots, peer.Clone())
	}
	return snapshots
}

// applyAuthModelProjections refreshes the registry projections for one auth snapshot.
func (m *Manager) applyAuthModelProjections(authID string, snapshot *Auth, now time.Time) {
	if snapshot == nil {
		return
	}
	reg := registry.GetGlobalRegistry()
	supportedModels, regEpoch := reg.GetModelsAndEpochForClient(authID)
	projections := make([]registry.ClientModelProjection, 0, len(supportedModels))
	for _, sm := range supportedModels {
		if sm == nil || strings.TrimSpace(sm.ID) == "" {
			continue
		}
		projections = append(projections, m.clientModelProjectionForAuth(snapshot, sm.ID, now))
	}
	if len(projections) > 0 {
		reg.ApplyClientModelProjections(authID, regEpoch, snapshot.Generation, projections)
	}
}
