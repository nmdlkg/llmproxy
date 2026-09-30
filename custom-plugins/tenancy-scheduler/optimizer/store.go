package optimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StateVersion is the persisted schema version. Incompatible files are ignored.
const StateVersion = 1

// ErrCorruptState marks malformed or incompatible optimizer state.
var ErrCorruptState = errors.New("optimizer state is corrupt")

// persistedState is the plugin-owned durable state. It stores no tokens,
// client API keys, or response headers; reservations are intentionally not
// persisted because their attempts do not survive a reload.
type persistedState struct {
	Version     int                              `json:"version"`
	SavedAt     time.Time                        `json:"saved_at"`
	Windows     []*Window                        `json:"windows"`
	Pooled      map[string]*ConsumptionEstimator `json:"pooled"`
	Classes     map[string]*ClassStats           `json:"classes"`
	Demand      map[string]*DemandForecast       `json:"demand"`
	Health      map[string]*Health               `json:"health"`
	AuthSeen    map[string]time.Time             `json:"auth_seen"`
	Identities  map[string]string                `json:"identities"`
	ScopedClass map[string]map[string]time.Time  `json:"scoped_classes,omitempty"`
	Accounts    map[string]*AccountState         `json:"accounts"`
	Shadow      ShadowStats                      `json:"shadow"`
	// Diagnostics is informational and ignored on load.
	Diagnostics *Diagnostics `json:"diagnostics,omitempty"`
}

// Diagnostics summarizes optimizer health for operators.
type Diagnostics struct {
	Mode           string    `json:"mode"`
	PricesAt       time.Time `json:"prices_at,omitempty"`
	PricedWindows  int       `json:"priced_windows"`
	RolloutSteps   int       `json:"rollout_steps"`
	RolloutValue   float64   `json:"rollout_value"`
	RolloutUnserve float64   `json:"rollout_unserved"`
	Windows        int       `json:"windows"`
	Accounts       int       `json:"accounts"`
	Reservations   int       `json:"reservations"`
}

// Save writes the state atomically to path.
func (e *Engine) Save(path, mode string) error {
	if path == "" {
		return nil
	}
	e.mu.Lock()
	state := persistedState{
		Version:     StateVersion,
		SavedAt:     e.now().UTC(),
		Pooled:      e.pooled,
		Classes:     e.classes,
		Demand:      e.demand,
		Health:      e.health,
		AuthSeen:    e.authSeen,
		Identities:  e.identities,
		ScopedClass: e.scopedClass,
		Accounts:    e.accounts,
		Shadow:      e.shadow,
	}
	for _, window := range e.windows {
		state.Windows = append(state.Windows, window)
	}
	diagnostics := &Diagnostics{Mode: mode, Windows: len(e.windows), Accounts: len(e.accounts)}
	for _, groups := range e.reservations {
		diagnostics.Reservations += len(groups)
	}
	if snapshot := e.prices.Load(); snapshot != nil {
		diagnostics.PricesAt = snapshot.ComputedAt
		diagnostics.PricedWindows = len(snapshot.Prices)
		diagnostics.RolloutSteps = snapshot.Steps
		diagnostics.RolloutValue = snapshot.Baseline.Value
		diagnostics.RolloutUnserve = snapshot.Baseline.Unserved
	}
	state.Diagnostics = diagnostics
	raw, errMarshal := json.Marshal(state)
	e.mu.Unlock()
	if errMarshal != nil {
		return fmt.Errorf("optimizer state: marshal: %w", errMarshal)
	}
	return writeFileAtomic(path, raw)
}

func writeFileAtomic(path string, raw []byte) (err error) {
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("optimizer state: create dir: %w", errMkdir)
	}
	tmp, errCreate := os.CreateTemp(dir, ".tenancy-scheduler-*.tmp")
	if errCreate != nil {
		return fmt.Errorf("optimizer state: create temp: %w", errCreate)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, errWrite := tmp.Write(raw); errWrite != nil {
		_ = tmp.Close()
		return fmt.Errorf("optimizer state: write: %w", errWrite)
	}
	if errSync := tmp.Sync(); errSync != nil {
		_ = tmp.Close()
		return fmt.Errorf("optimizer state: sync: %w", errSync)
	}
	if errClose := tmp.Close(); errClose != nil {
		return fmt.Errorf("optimizer state: close: %w", errClose)
	}
	if errRename := os.Rename(tmp.Name(), path); errRename != nil {
		return fmt.Errorf("optimizer state: rename: %w", errRename)
	}
	return nil
}

// Load restores state from path. A missing file is not an error.
func (e *Engine) Load(path string) error {
	if path == "" {
		return nil
	}
	raw, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		return fmt.Errorf("optimizer state: read: %w", errRead)
	}
	var state persistedState
	if errUnmarshal := json.Unmarshal(raw, &state); errUnmarshal != nil {
		return fmt.Errorf("%w: decode: %v", ErrCorruptState, errUnmarshal)
	}
	if state.Version != StateVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrCorruptState, state.Version)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.windows = make(map[string]*Window, len(state.Windows))
	e.windowIndex = make(map[string][]string)
	for _, window := range state.Windows {
		if window != nil {
			key := window.Key.String()
			e.windows[key] = window
			e.addWindowIndexLocked(key, window.Key)
		}
	}
	e.pooled = nonNilEstimators(state.Pooled)
	e.classes = nonNilClassStats(state.Classes)
	e.demand = nonNilForecasts(state.Demand)
	e.health = nonNilHealth(state.Health)
	e.authSeen = normalizeAuthTimes(state.AuthSeen)
	e.identities = normalizeIdentities(state.Identities)
	e.scopedClass = nonNilScopedClasses(state.ScopedClass)
	e.accounts = nonNilAccounts(state.Accounts)
	for _, account := range e.accounts {
		if account.Classes == nil {
			account.Classes = make(map[string]time.Time)
		}
	}
	e.shadow = state.Shadow
	e.pending = make(map[string]*pendingInterval)
	e.reservations = make(map[string][]reservationGroup)
	e.reserved = make(map[string]float64)
	e.lastReserveSweep = time.Time{}
	return nil
}

func hashAuthID(authID string) string {
	digest := sha256.Sum256([]byte(authID))
	return "h:" + hex.EncodeToString(digest[:])[:32]
}

func isHashedAuthKey(key string) bool {
	if len(key) != 34 || !strings.HasPrefix(key, "h:") {
		return false
	}
	_, errDecode := hex.DecodeString(key[2:])
	return errDecode == nil
}

func normalizeAuthKey(key string) string {
	if isHashedAuthKey(key) {
		return key
	}
	return hashAuthID(key)
}

func nonNilEstimators(input map[string]*ConsumptionEstimator) map[string]*ConsumptionEstimator {
	output := make(map[string]*ConsumptionEstimator, len(input))
	for key, value := range input {
		if value != nil {
			output[key] = value
		}
	}
	return output
}

func nonNilClassStats(input map[string]*ClassStats) map[string]*ClassStats {
	output := make(map[string]*ClassStats, len(input))
	for key, value := range input {
		if value != nil {
			output[key] = value
		}
	}
	return output
}

func nonNilForecasts(input map[string]*DemandForecast) map[string]*DemandForecast {
	output := make(map[string]*DemandForecast, len(input))
	for key, value := range input {
		if value != nil {
			output[key] = value
		}
	}
	return output
}

func nonNilHealth(input map[string]*Health) map[string]*Health {
	output := make(map[string]*Health, len(input))
	for key, value := range input {
		if value != nil {
			output[normalizeAuthKey(key)] = value
		}
	}
	return output
}

func normalizeAuthTimes(input map[string]time.Time) map[string]time.Time {
	output := make(map[string]time.Time, len(input))
	for key, value := range input {
		output[normalizeAuthKey(key)] = value
	}
	return output
}

func normalizeIdentities(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		normalized := normalizeAuthKey(key)
		if !isHashedAuthKey(key) && value == "auth:"+key {
			value = "auth:" + normalized
		}
		output[normalized] = value
	}
	return output
}

func nonNilScopedClasses(input map[string]map[string]time.Time) map[string]map[string]time.Time {
	output := make(map[string]map[string]time.Time, len(input))
	for key, classes := range input {
		if classes == nil {
			continue
		}
		copyClasses := make(map[string]time.Time, len(classes))
		for class, seen := range classes {
			copyClasses[class] = seen
		}
		output[key] = copyClasses
	}
	return output
}

func nonNilAccounts(input map[string]*AccountState) map[string]*AccountState {
	output := make(map[string]*AccountState, len(input))
	for key, value := range input {
		if value != nil {
			output[key] = value
		}
	}
	return output
}
