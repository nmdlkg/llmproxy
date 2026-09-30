package optimizer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// telemetrySchemaVersion is independent from the durable optimizer state
// version. New counters can therefore be added without invalidating learned
// quota state.
const telemetrySchemaVersion = 1

const maxTelemetryHistory = 8

var latencyBoundsMS = []int64{100, 250, 500, 1000, 2000, 5000, 10000, 30000, 60000, 120000, 300000}

// LatencySummary is a bounded, merge-free histogram suitable for persistence.
// Counts has one additional overflow bucket after UpperBoundsMS.
type LatencySummary struct {
	UpperBoundsMS []int64  `json:"upper_bounds_ms"`
	Counts        []uint64 `json:"counts"`
	Samples       uint64   `json:"samples"`
	SumMS         float64  `json:"sum_ms"`
}

func newLatencySummary() LatencySummary {
	bounds := append([]int64(nil), latencyBoundsMS...)
	return LatencySummary{UpperBoundsMS: bounds, Counts: make([]uint64, len(bounds)+1)}
}

func (s *LatencySummary) observe(duration time.Duration) {
	if duration <= 0 {
		return
	}
	ms := float64(duration) / float64(time.Millisecond)
	if len(s.UpperBoundsMS) == 0 || len(s.Counts) != len(s.UpperBoundsMS)+1 {
		*s = newLatencySummary()
	}
	index := len(s.UpperBoundsMS)
	for i, bound := range s.UpperBoundsMS {
		if ms <= float64(bound) {
			index = i
			break
		}
	}
	s.Counts[index]++
	s.Samples++
	s.SumMS += ms
}

func (s *LatencySummary) normalize() {
	valid := len(s.UpperBoundsMS) == len(latencyBoundsMS) && len(s.Counts) == len(latencyBoundsMS)+1
	if valid {
		for i, bound := range latencyBoundsMS {
			valid = valid && s.UpperBoundsMS[i] == bound
		}
	}
	if !valid {
		*s = newLatencySummary()
	}
}

// Telemetry is the active mode-scoped observation epoch. It intentionally
// contains no credential identifiers, headers, request bodies, or tokens.
type Telemetry struct {
	SchemaVersion     int       `json:"schema_version"`
	Mode              string    `json:"mode"`
	EpochID           string    `json:"epoch_id"`
	StartedAt         time.Time `json:"started_at"`
	ConfigFingerprint string    `json:"config_fingerprint"`

	Decisions                 uint64 `json:"decisions"`
	Agreements                uint64 `json:"agreements"`
	InadmissibleDecisions     uint64 `json:"inadmissible_decisions"`
	UnknownCandidateDecisions uint64 `json:"unknown_candidate_decisions"`
	StaleWindowDecisions      uint64 `json:"stale_window_decisions"`
	UnpricedDecisions         uint64 `json:"unpriced_decisions"`
	StalePriceDecisions       uint64 `json:"stale_price_decisions"`

	UsageRecords      uint64 `json:"usage_records"`
	UsageSuccesses    uint64 `json:"usage_successes"`
	UsageFailures     uint64 `json:"usage_failures"`
	Usage429          uint64 `json:"usage_429"`
	UsageWithQuota    uint64 `json:"usage_with_quota"`
	UsageWithoutQuota uint64 `json:"usage_without_quota"`
	BoundaryUsage     uint64 `json:"boundary_usage"`

	ReservationsCreated           uint64 `json:"reservations_created"`
	ReservationsReleased          uint64 `json:"reservations_released"`
	ReservationsExpired           uint64 `json:"reservations_expired"`
	ReservationsInvalidated       uint64 `json:"reservations_invalidated"`
	ReservationsPruned            uint64 `json:"reservations_pruned"`
	UnmatchedUsage                uint64 `json:"unmatched_usage"`
	CarriedReservations           uint64 `json:"carried_reservations"`
	PriorEpochReservationsRemoved uint64 `json:"prior_epoch_reservations_removed"`

	Latency LatencySummary `json:"latency"`
	TTFT    LatencySummary `json:"ttft"`

	// Freshness gauges are sampled at checkpoint time and are not cumulative.
	KnownAccounts        int   `json:"known_accounts"`
	FreshWindows         int   `json:"fresh_windows"`
	StaleWindows         int   `json:"stale_windows"`
	UnlearnedWindows     int   `json:"unlearned_windows"`
	OldestObservationAge int64 `json:"oldest_observation_age_seconds"`
}

func newTelemetry(mode string, now time.Time) Telemetry {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return Telemetry{SchemaVersion: telemetrySchemaVersion, Mode: mode, EpochID: hex.EncodeToString(id[:]), StartedAt: now.UTC(), Latency: newLatencySummary(), TTFT: newLatencySummary()}
}

// StartEpoch isolates telemetry after a restart or a policy reconfiguration.
// In-memory reservations survive hot mode switches; their removals are counted
// separately from reservations created by the new policy.
func (e *Engine) StartEpoch(mode string, acrossPriorities bool, intervals ...time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rotateTelemetryLocked(mode, e.now())
	raw, _ := json.Marshal(struct {
		Config           Config
		AcrossPriorities bool
		Intervals        []time.Duration
	}{e.cfg, acrossPriorities, intervals})
	digest := sha256.Sum256(raw)
	e.telemetry.ConfigFingerprint = hex.EncodeToString(digest[:])
	for _, groups := range e.reservations {
		e.telemetry.CarriedReservations += uint64(len(groups))
	}
}

func cloneTelemetry(t Telemetry) Telemetry {
	t.Latency.UpperBoundsMS = append([]int64(nil), t.Latency.UpperBoundsMS...)
	t.Latency.Counts = append([]uint64(nil), t.Latency.Counts...)
	t.TTFT.UpperBoundsMS = append([]int64(nil), t.TTFT.UpperBoundsMS...)
	t.TTFT.Counts = append([]uint64(nil), t.TTFT.Counts...)
	return t
}

func (e *Engine) refreshFreshnessLocked(now time.Time) {
	t := &e.telemetry
	t.KnownAccounts, t.FreshWindows, t.StaleWindows, t.UnlearnedWindows, t.OldestObservationAge = 0, 0, 0, 0, 0
	known := make(map[string]bool)
	for _, window := range e.windows {
		known[window.Key.Account] = true
		age := now.Sub(window.ObservedAt)
		if window.ObservedAt.IsZero() || age > e.cfg.StaleAfter || (!window.ResetAt.IsZero() && !now.Before(window.ResetAt)) {
			t.StaleWindows++
		} else {
			t.FreshWindows++
		}
		if !window.ObservedAt.IsZero() && int64(age.Seconds()) > t.OldestObservationAge {
			t.OldestObservationAge = int64(age.Seconds())
		}
		if _, learned := window.Estimator.Rate(); !learned && window.Unit != UnitRequests && window.Unit != UnitTokens {
			t.UnlearnedWindows++
		}
	}
	t.KnownAccounts = len(known)
}

func (e *Engine) reservationRemovedLocked(group reservationGroup, counter *uint64) {
	if group.epochID != e.telemetry.EpochID {
		e.telemetry.PriorEpochReservationsRemoved++
		return
	}
	*counter++
}

func (t *Telemetry) normalize() {
	if t.SchemaVersion == 0 {
		t.SchemaVersion = telemetrySchemaVersion
	}
	if t.Latency.UpperBoundsMS == nil {
		t.Latency = newLatencySummary()
	} else {
		t.Latency.normalize()
	}
	if t.TTFT.UpperBoundsMS == nil {
		t.TTFT = newLatencySummary()
	} else {
		t.TTFT.normalize()
	}
}
