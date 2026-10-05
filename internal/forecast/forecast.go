// Package forecast contains deterministic, provider-independent usage forecasts.
package forecast

import (
	"math"
	"sort"
	"time"
)

const (
	Bucket          = time.Hour
	Horizon         = 7 * 24 * time.Hour
	Alpha           = 0.3
	minimumTraining = 7 * 24 * time.Hour
)

// Observation is one usage observation. Tokens are the canonical billable
// total for the bucket and must not include retry-only metadata.
type Observation struct {
	At     time.Time
	Tokens int64
}

// Entitlement describes an authoritative token limit, when a provider exposes
// one. Native provider units should be represented by NativeRemaining instead.
type Entitlement struct {
	LimitTokens     int64
	UsedTokens      int64
	RemainingTokens int64
	ResetAt         time.Time
	Authoritative   bool
	NativeRemaining *float64
	NativeUnit      string
}

// Result is stable for identical observations, now, and entitlement inputs.
type Result struct {
	ObservedWeeklyTokens     int64
	ProjectedNextWeekTokens  int64
	ProjectedLowerTokens     int64
	ProjectedUpperTokens     int64
	EstimatedAvailableTokens *int64
	RemainingUntilReset      *time.Duration
	NativeRemaining          *float64
	NativeUnit               string
	Coverage                 float64
	Confidence               string
	Status                   string
	ReasonCodes              []string
	AlgorithmVersion         string
	GeneratedAt              time.Time
}

// Calculate computes a seven-day EWMA forecast. now is supplied by callers so
// tests and repeated API reads can be deterministic.
func Calculate(observations []Observation, entitlement *Entitlement, now time.Time) Result {
	now = now.UTC().Truncate(time.Nanosecond)
	r := Result{Confidence: "unknown", Status: "unknown", AlgorithmVersion: "ewma-v1", GeneratedAt: now, ReasonCodes: []string{}}
	if entitlement != nil {
		if entitlement.NativeRemaining != nil {
			v := *entitlement.NativeRemaining
			r.NativeRemaining, r.NativeUnit = &v, entitlement.NativeUnit
		}
		if entitlement.Authoritative {
			remaining := entitlement.RemainingTokens
			if remaining == 0 && entitlement.LimitTokens > entitlement.UsedTokens {
				remaining = entitlement.LimitTokens - entitlement.UsedTokens
			}
			r.EstimatedAvailableTokens = &remaining
			if !entitlement.ResetAt.IsZero() && entitlement.ResetAt.After(now) {
				d := entitlement.ResetAt.Sub(now)
				r.RemainingUntilReset = &d
			}
		}
	}
	points := append([]Observation(nil), observations...)
	for i := range points {
		points[i].At = points[i].At.UTC().Truncate(Bucket)
		if points[i].Tokens < 0 {
			points[i].Tokens = 0
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i].At.Before(points[j].At) })
	latest := now.Truncate(Bucket).Add(-Bucket)
	first := now.Add(-minimumTraining)
	if len(points) == 0 {
		r.ReasonCodes = append(r.ReasonCodes, "no_observations")
		return r
	}
	// Collapse duplicate buckets deterministically and ignore future/incomplete data.
	buckets := make(map[time.Time]int64)
	for _, p := range points {
		if p.At.After(latest) || p.At.Before(first) {
			continue
		}
		buckets[p.At] += p.Tokens
	}
	if len(buckets) == 0 {
		r.ReasonCodes = append(r.ReasonCodes, "no_complete_buckets")
		return r
	}
	starts := make([]time.Time, 0, len(buckets))
	var observed int64
	for at, tokens := range buckets {
		starts = append(starts, at)
		observed += tokens
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	observedDuration := starts[len(starts)-1].Sub(starts[0]) + Bucket
	if observedDuration < Bucket {
		observedDuration = Bucket
	}
	r.ObservedWeeklyTokens = int64(math.Round(float64(observed) * float64(Horizon) / float64(observedDuration)))
	// The latest complete bucket is the baseline; older buckets only adjust it.
	var ewma = float64(buckets[starts[len(starts)-1]])
	for i := len(starts) - 2; i >= 0; i-- {
		ewma = Alpha*float64(buckets[starts[i]]) + (1-Alpha)*ewma
	}
	projected := int64(math.Ceil(ewma * float64(Horizon/Bucket)))
	r.ProjectedNextWeekTokens = projected
	spread := int64(math.Ceil(float64(projected) * 0.25))
	r.ProjectedLowerTokens = projected - spread
	if r.ProjectedLowerTokens < 0 {
		r.ProjectedLowerTokens = 0
	}
	r.ProjectedUpperTokens = projected + spread
	coverage := now.Sub(starts[0])
	if coverage > minimumTraining {
		coverage = minimumTraining
	}
	r.Coverage = math.Max(0, math.Min(1, coverage.Hours()/minimumTraining.Hours()))
	if len(starts) >= 24*7 || r.Coverage >= 0.99 {
		r.Confidence, r.Status = "medium", "ok"
	} else {
		r.Confidence, r.Status = "low", "ok"
		r.ReasonCodes = append(r.ReasonCodes, "limited_coverage")
	}
	if observed == 0 {
		r.Confidence, r.Status = "low", "zero"
		r.ReasonCodes = append(r.ReasonCodes, "zero_demand")
	}
	return r
}
