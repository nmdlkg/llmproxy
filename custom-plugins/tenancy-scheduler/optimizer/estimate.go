package optimizer

import (
	"math"
	"strings"
	"time"
)

const (
	consumptionHalfLife = 24 * time.Hour
	hoursPerWeek        = 7 * 24
	demandAlpha         = 0.3
	classAlpha          = 0.1
	healthAlpha         = 0.1
)

// ConsumptionEstimator learns native units consumed per effective token of one
// window from successive observations. Deltas are summed over whole intervals,
// so coarse provider rounding telescopes out instead of biasing the rate.
type ConsumptionEstimator struct {
	Units    float64   `json:"units"`
	Tokens   float64   `json:"tokens"`
	Requests float64   `json:"requests"`
	VarSum   float64   `json:"var_sum"`
	Weight   float64   `json:"weight"`
	Updated  time.Time `json:"updated"`
}

func (e *ConsumptionEstimator) decay(now time.Time) {
	if e.Updated.IsZero() || !now.After(e.Updated) {
		e.Updated = maxTime(e.Updated, now)
		return
	}
	factor := math.Exp2(-float64(now.Sub(e.Updated)) / float64(consumptionHalfLife))
	e.Units *= factor
	e.Tokens *= factor
	e.Requests *= factor
	e.VarSum *= factor
	e.Weight *= factor
	e.Updated = now
}

// Add records one observed interval: delta native units consumed by requests
// that together used tokens effective tokens.
func (e *ConsumptionEstimator) Add(delta, tokens, requests float64, now time.Time) {
	if delta < 0 || tokens <= 0 || requests <= 0 {
		return
	}
	e.decay(now)
	rate, hasRate := e.Rate()
	e.Units += delta
	e.Tokens += tokens
	e.Requests += requests
	if hasRate {
		residual := delta - rate*tokens
		e.VarSum += residual * residual / requests
		e.Weight++
	}
}

// Rate returns units per effective token when enough evidence exists.
func (e *ConsumptionEstimator) Rate() (float64, bool) {
	if e == nil || e.Tokens <= 0 || e.Requests < 1 {
		return 0, false
	}
	return e.Units / e.Tokens, true
}

// PerRequestVariance returns the learned per-request variance in squared units.
func (e *ConsumptionEstimator) PerRequestVariance() (float64, bool) {
	if e == nil || e.Weight < 1 {
		return 0, false
	}
	return e.VarSum / e.Weight, true
}

// ClassStats tracks the effective token size of requests in one demand class.
type ClassStats struct {
	MeanTokens float64 `json:"mean_tokens"`
	SqTokens   float64 `json:"sq_tokens"`
	Count      float64 `json:"count"`
}

// Add records one completed request's effective token count.
func (c *ClassStats) Add(tokens float64) {
	if tokens <= 0 {
		return
	}
	if c.Count == 0 {
		c.MeanTokens, c.SqTokens = tokens, tokens*tokens
	} else {
		c.MeanTokens += classAlpha * (tokens - c.MeanTokens)
		c.SqTokens += classAlpha * (tokens*tokens - c.SqTokens)
	}
	c.Count++
}

// SD returns the standard deviation of request token counts.
func (c *ClassStats) SD() float64 {
	if c == nil || c.Count < 2 {
		return 0
	}
	return math.Sqrt(math.Max(0, c.SqTokens-c.MeanTokens*c.MeanTokens))
}

// DemandForecast is a seasonal time-of-week EWMA of hourly request arrivals.
type DemandForecast struct {
	Bins      [hoursPerWeek]float64 `json:"bins"`
	Seen      [hoursPerWeek]bool    `json:"seen"`
	Global    float64               `json:"global"`
	GlobalSet bool                  `json:"global_set"`
	Hour      int64                 `json:"hour"`
	Count     float64               `json:"count"`
}

func unixHour(t time.Time) int64 { return t.Unix() / 3600 }

func weekBin(hour int64) int {
	// Unix epoch started on a Thursday; the offset keeps Monday 00:00 UTC at bin 0.
	return int(((hour+72)%hoursPerWeek + hoursPerWeek) % hoursPerWeek)
}

// Record counts one arrival at now.
func (d *DemandForecast) Record(now time.Time) {
	d.roll(now)
	d.Count++
}

func (d *DemandForecast) roll(now time.Time) {
	hour := unixHour(now)
	if d.Hour == 0 {
		d.Hour = hour
		return
	}
	if hour <= d.Hour {
		return
	}
	steps := hour - d.Hour
	if steps > hoursPerWeek {
		steps = hoursPerWeek
		d.Hour = hour - steps
	}
	for i := int64(0); i < steps; i++ {
		count := 0.0
		if i == 0 {
			count = d.Count
		}
		bin := weekBin(d.Hour + i)
		if d.Seen[bin] {
			d.Bins[bin] += demandAlpha * (count - d.Bins[bin])
		} else {
			d.Bins[bin], d.Seen[bin] = count, true
		}
		if d.GlobalSet {
			d.Global += demandAlpha / 4 * (count - d.Global)
		} else {
			d.Global, d.GlobalSet = count, true
		}
	}
	d.Hour = hour
	d.Count = 0
}

// Rate forecasts arrivals per hour at t. Before any hour completes, the current
// partial count is extrapolated from the observed part of the hour.
func (d *DemandForecast) Rate(t, now time.Time) float64 {
	if d == nil {
		return 0
	}
	bin := weekBin(unixHour(t))
	if d.Seen[bin] {
		return d.Bins[bin]
	}
	if d.GlobalSet {
		return d.Global
	}
	elapsed := now.Sub(time.Unix(d.Hour*3600, 0)).Hours()
	if d.Hour == 0 || elapsed <= 0 {
		return 0
	}
	return d.Count / math.Max(elapsed, 1.0/6)
}

// Health tracks per-credential reliability and latency.
type Health struct {
	Success    float64 `json:"success"`
	LatencySec float64 `json:"latency_sec"`
	Samples    float64 `json:"samples"`
}

// Add records one completed attempt.
func (h *Health) Add(failed bool, latency time.Duration) {
	success := 1.0
	if failed {
		success = 0
	}
	seconds := latency.Seconds()
	if h.Samples == 0 {
		h.Success, h.LatencySec = success, seconds
	} else {
		h.Success += healthAlpha * (success - h.Success)
		h.LatencySec += healthAlpha * (seconds - h.LatencySec)
	}
	h.Samples++
}

// SuccessProbability returns the smoothed success probability.
func (h *Health) SuccessProbability() float64 {
	if h == nil || h.Samples == 0 {
		return 1
	}
	return h.Success
}

// ClassOf maps a request to its demand class: provider plus model family.
// Effort variants and dated suffixes collapse so forecasts pool comparable demand.
func ClassOf(provider, model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if index := strings.IndexByte(model, '('); index >= 0 {
		model = model[:index]
	}
	if index := strings.LastIndexByte(model, '/'); index >= 0 {
		model = model[index+1:]
	}
	return strings.ToLower(strings.TrimSpace(provider)) + "/" + strings.TrimSpace(model)
}

// EffectiveTokens is the token feature used for native-unit consumption.
// Cached input is discounted because providers charge it at a reduced rate.
func EffectiveTokens(input, output, reasoning, cached, total int64) float64 {
	value := float64(total)
	if value <= 0 {
		value = float64(input + output + reasoning)
	}
	value -= 0.9 * float64(cached)
	return math.Max(1, value)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
