package tenancy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type credentialValidator struct {
	store       Store
	credentials CredentialResolver
	now         func() time.Time

	mu    sync.Mutex
	users map[string]*credentialValidationState
}

type credentialValidationState struct {
	authIDs      []string
	lastOKByAuth map[string]time.Time
	latestOK     time.Time
	nextDue      time.Time
	cursor       int
	interval     time.Duration
}

// ParseValidationInterval parses the validation interval and reports whether
// real-traffic credential validation is enabled.
func ParseValidationInterval(value string) (time.Duration, bool) {
	interval, errParse := time.ParseDuration(strings.TrimSpace(value))
	if errParse != nil || interval <= 0 {
		return 0, false
	}
	return interval, true
}

func newCredentialValidator(store Store, credentials CredentialResolver) *credentialValidator {
	return &credentialValidator{
		store:       store,
		credentials: credentials,
		now:         time.Now,
		users:       make(map[string]*credentialValidationState),
	}
}

func (v *credentialValidator) preferredAuthIDs(userID string, interval time.Duration) ([]string, error) {
	if v == nil || v.store == nil || v.credentials == nil || interval <= 0 {
		return nil, nil
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, nil
	}

	credentials, errResolve := v.credentials(userID)
	if errResolve != nil {
		return nil, fmt.Errorf("tenancy validation: resolve credentials for user %q: %w", userID, errResolve)
	}
	authIDs := credentialAuthIDs(credentials)
	if len(authIDs) == 0 {
		v.mu.Lock()
		delete(v.users, userID)
		v.mu.Unlock()
		return nil, nil
	}

	v.mu.Lock()
	state := v.users[userID]
	if state != nil && stringSlicesEqual(state.authIDs, authIDs) {
		preferred := v.pickPreferredLocked(state, interval)
		v.mu.Unlock()
		return preferred, nil
	}
	v.mu.Unlock()

	loaded, errLoad := v.loadState(userID, authIDs, interval)
	if errLoad != nil {
		return nil, errLoad
	}

	v.mu.Lock()
	state = v.users[userID]
	if state == nil || !stringSlicesEqual(state.authIDs, authIDs) {
		state = loaded
		v.users[userID] = state
	}
	preferred := v.pickPreferredLocked(state, interval)
	v.mu.Unlock()
	return preferred, nil
}

func (v *credentialValidator) loadState(userID string, authIDs []string, interval time.Duration) (*credentialValidationState, error) {
	state := &credentialValidationState{
		authIDs:      append([]string(nil), authIDs...),
		lastOKByAuth: make(map[string]time.Time, len(authIDs)),
		interval:     interval,
	}
	var latestAttempt time.Time
	for index, authID := range authIDs {
		state.lastOKByAuth[authID] = time.Time{}
		validation, errGet := v.store.GetCredentialValidation(context.Background(), authID)
		if errors.Is(errGet, ErrNotFound) {
			continue
		}
		if errGet != nil {
			return nil, fmt.Errorf("tenancy validation: load auth %q: %w", authID, errGet)
		}
		if validation == nil || strings.TrimSpace(validation.UserID) != userID {
			continue
		}
		if validation.LastOKAt != nil {
			lastOK := validation.LastOKAt.UTC()
			state.lastOKByAuth[authID] = lastOK
			if lastOK.After(state.latestOK) {
				state.latestOK = lastOK
			}
		}
		if validation.LastAttemptAt.After(latestAttempt) {
			latestAttempt = validation.LastAttemptAt
			state.cursor = (index + 1) % len(authIDs)
		}
	}
	if !state.latestOK.IsZero() {
		state.nextDue = state.latestOK.Add(interval)
	}
	return state, nil
}

func (v *credentialValidator) pickPreferredLocked(state *credentialValidationState, interval time.Duration) []string {
	if state == nil || len(state.authIDs) == 0 || interval <= 0 {
		return nil
	}
	if state.interval != interval {
		state.interval = interval
		if state.latestOK.IsZero() {
			state.nextDue = time.Time{}
		} else {
			state.nextDue = state.latestOK.Add(interval)
		}
	}
	now := v.now().UTC()
	if !state.nextDue.IsZero() && now.Before(state.nextDue) {
		return nil
	}
	if state.cursor < 0 || state.cursor >= len(state.authIDs) {
		state.cursor = 0
	}
	authID := state.authIDs[state.cursor]
	state.cursor = (state.cursor + 1) % len(state.authIDs)
	// Reserve the interval while the selected request is in flight so concurrent
	// requests do not all attempt validation at once. A failed outcome restores
	// due state because only successful traffic advances latestOK.
	state.nextDue = now.Add(interval)
	return []string{authID}
}

func (v *credentialValidator) recordOutcome(userID, authID string, failed bool, statusCode int, attemptedAt time.Time) error {
	if v == nil || v.store == nil {
		return nil
	}
	userID = strings.TrimSpace(userID)
	authID = strings.TrimSpace(authID)
	if userID == "" || authID == "" {
		return nil
	}
	if attemptedAt.IsZero() {
		attemptedAt = v.now()
	}
	attemptedAt = attemptedAt.UTC()

	v.mu.Lock()
	state := v.users[userID]
	if state == nil {
		v.mu.Unlock()
		return nil
	}
	if _, owned := state.lastOKByAuth[authID]; !owned {
		v.mu.Unlock()
		return nil
	}
	var lastOKAt *time.Time
	if !failed {
		successAt := attemptedAt
		state.lastOKByAuth[authID] = successAt
		if successAt.After(state.latestOK) {
			state.latestOK = successAt
		}
		lastOKAt = &successAt
	}
	if state.latestOK.IsZero() {
		state.nextDue = time.Time{}
	} else {
		state.nextDue = state.latestOK.Add(state.interval)
	}
	v.mu.Unlock()

	status := "ok"
	if failed {
		status = "failed"
		if statusCode > 0 {
			status = strconv.Itoa(statusCode)
		}
	}
	if errUpsert := v.store.UpsertCredentialValidation(context.Background(), CredentialValidation{
		AuthID:        authID,
		UserID:        userID,
		LastAttemptAt: attemptedAt,
		LastOKAt:      lastOKAt,
		LastStatus:    status,
	}); errUpsert != nil {
		return fmt.Errorf("tenancy validation: record auth %q outcome: %w", authID, errUpsert)
	}
	return nil
}

func credentialAuthIDs(credentials []Credential) []string {
	seen := make(map[string]struct{}, len(credentials))
	authIDs := make([]string, 0, len(credentials))
	for _, credential := range credentials {
		authID := strings.TrimSpace(credential.AuthID)
		if authID == "" {
			continue
		}
		if _, exists := seen[authID]; exists {
			continue
		}
		seen[authID] = struct{}{}
		authIDs = append(authIDs, authID)
	}
	sort.Strings(authIDs)
	return authIDs
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
