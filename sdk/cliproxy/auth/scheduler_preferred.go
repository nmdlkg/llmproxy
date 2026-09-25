package auth

import (
	"context"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Fork seam: soft preferred-auth first pass. Kept out of scheduler.go so
// upstream scheduler edits do not conflict; pickSingleWithStrategy and
// pickMixedWithStrategy call pickPreferredSingle / pickPreferredMixed first.
//
// The first pass reuses the upstream selection itself: every scheduled auth
// that is not preferred is treated as already tried, so the ordinary
// predicate (eligibility, cooldown, disabled, model support, weights, cursors)
// selects among the ready preferred credentials only. When none is ready the
// request falls through to normal selection without the preferred list.

// PreferredAuthIDsMetadataKey carries auth IDs that should be tried softly on the first credential pick before normal priority selection.
const PreferredAuthIDsMetadataKey = "preferred_auth_ids"

// pickPreferredSingle runs the preferred first pass for a single provider. It
// always strips the preferred list from opts so the caller's normal pass and
// any nested pick do not repeat it.
func (s *authScheduler) pickPreferredSingle(ctx context.Context, provider, model string, opts *cliproxyexecutor.Options, tried map[string]struct{}, strategy schedulerStrategy) (*Auth, bool) {
	excluded, ok := s.preparePreferredPass(opts, tried)
	if !ok {
		return nil, false
	}
	picked, errPick := s.pickSingleWithStrategy(ctx, provider, model, *opts, excluded, strategy)
	if errPick != nil || picked == nil {
		return nil, false
	}
	return picked, true
}

// pickPreferredMixed runs the preferred first pass for a mixed-provider pick.
func (s *authScheduler) pickPreferredMixed(ctx context.Context, providers []string, model string, opts *cliproxyexecutor.Options, tried map[string]struct{}, strategy schedulerStrategy) (*Auth, string, bool) {
	excluded, ok := s.preparePreferredPass(opts, tried)
	if !ok {
		return nil, "", false
	}
	picked, providerKey, errPick := s.pickMixedWithStrategy(ctx, providers, model, *opts, excluded, strategy)
	if errPick != nil || picked == nil {
		return nil, "", false
	}
	return picked, providerKey, true
}

// preparePreferredPass strips the preferred list from opts and, when a first
// pass applies (first attempt, not pinned, non-empty preferred list), returns
// the set of scheduled auth IDs to exclude.
func (s *authScheduler) preparePreferredPass(opts *cliproxyexecutor.Options, tried map[string]struct{}) (map[string]struct{}, bool) {
	if s == nil || opts == nil {
		return nil, false
	}
	preferred := preferredAuthIDsFromMetadata(opts.Metadata)
	if len(preferred) == 0 {
		return nil, false
	}
	opts.Metadata = withoutPreferredAuthIDs(opts.Metadata)
	if len(tried) > 0 || pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return nil, false
	}
	s.mu.Lock()
	excluded := make(map[string]struct{}, len(s.authProviders))
	for authID := range s.authProviders {
		if _, isPreferred := preferred[authID]; !isPreferred {
			excluded[authID] = struct{}{}
		}
	}
	s.mu.Unlock()
	return excluded, true
}

func withoutPreferredAuthIDs(meta map[string]any) map[string]any {
	out := make(map[string]any, len(meta))
	for key, value := range meta {
		if key != PreferredAuthIDsMetadataKey {
			out[key] = value
		}
	}
	return out
}

func preferredAuthIDsFromMetadata(meta map[string]any) map[string]struct{} {
	if len(meta) == 0 {
		return nil
	}
	raw, ok := meta[PreferredAuthIDsMetadataKey]
	if !ok || raw == nil {
		return nil
	}
	preferred := make(map[string]struct{})
	add := func(value any) {
		var authID string
		switch val := value.(type) {
		case string:
			authID = strings.TrimSpace(val)
		case []byte:
			authID = strings.TrimSpace(string(val))
		}
		if authID != "" {
			preferred[authID] = struct{}{}
		}
	}
	switch val := raw.(type) {
	case string, []byte:
		add(val)
	case []string:
		for _, authID := range val {
			add(authID)
		}
	case [][]byte:
		for _, authID := range val {
			add(authID)
		}
	case []any:
		for _, authID := range val {
			add(authID)
		}
	}
	if len(preferred) == 0 {
		return nil
	}
	return preferred
}
