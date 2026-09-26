package auth

// Fork seam: effective credential priority. Kept out of selector.go and
// scheduler.go so upstream scheduler edits do not conflict; scheduler.go only
// calls applyPriorityResolverLocked after building scheduled auth metadata.

// PriorityResolver overrides an auth's attribute-based priority when ok is true.
type PriorityResolver func(auth *Auth) (int, bool)

// SetPriorityResolver installs an optional effective-priority resolver.
//
// The fast scheduler resolves priority when an auth is upserted, not on every
// request. Callers must call RefreshSchedulerEntry or RefreshSchedulerAll after
// data used by the resolver changes so existing auths are re-bucketed.
func (m *Manager) SetPriorityResolver(resolver PriorityResolver) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.scheduler != nil {
		m.scheduler.setPriorityResolver(resolver)
	}
	m.mu.Unlock()
	m.RefreshSchedulerAll()
}

// setPriorityResolver updates the resolver used when scheduler entries are upserted.
func (s *authScheduler) setPriorityResolver(resolver PriorityResolver) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.priorityResolver = resolver
}

// applyPriorityResolverLocked replaces the attribute priority of freshly built
// scheduled metadata with the resolver's effective priority when it reports ok.
// An unset resolver or ok == false keeps the attribute priority. Callers hold s.mu.
func (s *authScheduler) applyPriorityResolverLocked(meta *scheduledAuthMeta) {
	if s == nil || meta == nil || meta.auth == nil || s.priorityResolver == nil {
		return
	}
	if priority, ok := s.priorityResolver(meta.auth); ok {
		meta.priority = priority
	}
}
