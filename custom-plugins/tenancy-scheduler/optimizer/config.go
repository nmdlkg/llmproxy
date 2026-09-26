package optimizer

import (
	"fmt"
	"time"
)

// Config tunes the optimizer. Zero values are replaced by Defaults.
type Config struct {
	// Epsilon is the tolerated probability that a request overruns a known window.
	Epsilon float64
	// Scenarios is the number of rollout demand scenarios.
	Scenarios int
	// Step is the rollout time resolution.
	Step time.Duration
	// MaxHorizon caps the rollout lookahead.
	MaxHorizon time.Duration
	// StaleAfter marks an observation as stale for admission and valuation.
	StaleAfter time.Duration
	// ReservationTTL expires reservations whose attempt never reports usage.
	ReservationTTL time.Duration
	// ActiveAccountWindow limits rollout capacity to recently offered accounts.
	ActiveAccountWindow time.Duration
	// AccountTTL removes accounts, windows, and health not seen for this long.
	AccountTTL time.Duration
	// RequestValue is the reward of one successful request.
	RequestValue float64
	// LambdaLatency is the cost per second of expected latency.
	LambdaLatency float64
	// LambdaFailure is the cost of one expected failed attempt.
	LambdaFailure float64
	// PerturbRequests sizes the finite difference used for shadow prices.
	PerturbRequests float64
	// PriorRequestsPerWindow sets the consumption prior of an unlearned
	// 5-hour-equivalent window before observations exist.
	PriorRequestsPerWindow float64
	// DemandCV and ConsumptionCV are rollout scenario uncertainty.
	DemandCV      float64
	ConsumptionCV float64
	// DriftPerHour is the assumed external consumption uncertainty as a
	// fraction of capacity per hour since the last observation.
	DriftPerHour float64
	// TerminalValue values capacity beyond the horizon per request-equivalent.
	TerminalValue float64
	// MaxWork bounds rollout work per recompute (windows*scenarios*steps*accounts*classes).
	MaxWork float64
	// Seed makes rollouts reproducible.
	Seed uint64
}

// Defaults returns the documented default configuration.
func Defaults() Config {
	return Config{
		Epsilon:                0.05,
		Scenarios:              16,
		Step:                   30 * time.Minute,
		MaxHorizon:             8 * 24 * time.Hour,
		StaleAfter:             30 * time.Minute,
		ReservationTTL:         30 * time.Minute,
		ActiveAccountWindow:    24 * time.Hour,
		AccountTTL:             7 * 24 * time.Hour,
		RequestValue:           1,
		LambdaLatency:          0,
		LambdaFailure:          0.5,
		PerturbRequests:        5,
		PriorRequestsPerWindow: 500,
		DemandCV:               0.3,
		ConsumptionCV:          0.2,
		DriftPerHour:           0.005,
		TerminalValue:          0.5,
		MaxWork:                5e7,
		Seed:                   1,
	}
}

// Normalize fills zero values with defaults and validates ranges.
func (c Config) Normalize() (Config, error) {
	d := Defaults()
	if c.Epsilon == 0 {
		c.Epsilon = d.Epsilon
	}
	if c.Epsilon <= 0 || c.Epsilon >= 0.5 {
		return c, fmt.Errorf("epsilon must be in (0, 0.5)")
	}
	setInt := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	setDur := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	setFloat := func(v *float64, def float64) {
		if *v <= 0 {
			*v = def
		}
	}
	setInt(&c.Scenarios, d.Scenarios)
	setDur(&c.Step, d.Step)
	setDur(&c.MaxHorizon, d.MaxHorizon)
	setDur(&c.StaleAfter, d.StaleAfter)
	setDur(&c.ReservationTTL, d.ReservationTTL)
	setDur(&c.ActiveAccountWindow, d.ActiveAccountWindow)
	setDur(&c.AccountTTL, d.AccountTTL)
	setFloat(&c.RequestValue, d.RequestValue)
	setFloat(&c.PerturbRequests, d.PerturbRequests)
	setFloat(&c.PriorRequestsPerWindow, d.PriorRequestsPerWindow)
	setFloat(&c.MaxWork, d.MaxWork)
	if c.Seed == 0 {
		c.Seed = d.Seed
	}
	if c.LambdaLatency < 0 || c.LambdaFailure < 0 || c.DemandCV < 0 || c.ConsumptionCV < 0 || c.DriftPerHour < 0 || c.TerminalValue < 0 {
		return c, fmt.Errorf("costs and uncertainty parameters must not be negative")
	}
	if c.Step > c.MaxHorizon {
		return c, fmt.Errorf("step must not exceed max-horizon")
	}
	return c, nil
}
