package optimizer

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// generationTolerance absorbs reset-time jitter between observations of the
// same window instance (for example reset-after-seconds rounding).
const generationTolerance = 90 * time.Second

// Candidate is one credential offered for the current request.
type Candidate struct {
	ID       string
	Provider string
	// Account is a stable upstream account identity. Duplicate credential files
	// of the same account share it, so their capacity is not double counted.
	Account string
}

// Usage is one completed attempt as reported by the host usage capability.
type Usage struct {
	AuthID      string
	Provider    string
	Model       string
	Generate    bool
	Failed      bool
	Latency     time.Duration
	CompletedAt time.Time
	Tokens      float64
	Headers     http.Header
}

// Window is the durable state of one account window instance.
type Window struct {
	Key               WindowKey `json:"key"`
	Unit              Unit      `json:"unit"`
	Capacity          float64   `json:"capacity"`
	Remaining         float64   `json:"remaining"`
	MaxSeen           float64   `json:"max_seen"`
	ResetAt           time.Time `json:"reset_at"`
	Duration          int64     `json:"duration_ns"`
	Resolution        float64   `json:"resolution"`
	ObservedAt        time.Time `json:"observed_at"`
	Source            string    `json:"source"`
	Generation        uint64    `json:"generation"`
	generationResetAt time.Time `json:"-"`
	// Estimator learns this window's native units per effective token.
	Estimator ConsumptionEstimator `json:"estimator"`
}

// AccountState records where and for which demand classes an account is offered.
type AccountState struct {
	Provider string               `json:"provider"`
	LastSeen time.Time            `json:"last_seen"`
	Classes  map[string]time.Time `json:"classes"`
}

type pendingInterval struct {
	tokens   float64
	requests float64
}

type reservationItem struct {
	key        string
	generation uint64
	amount     float64
}

type reservationGroup struct {
	at    time.Time
	items []reservationItem
}

// PriceSnapshot is the immutable output of one background valuation.
type PriceSnapshot struct {
	ComputedAt time.Time
	// Prices maps a window key to its marginal value per native unit.
	Prices   map[string]float64
	Baseline SimResult
	Steps    int
	Accounts int
}

// ShadowStats compares the legacy and optimizer decisions.
type ShadowStats struct {
	Picks      uint64 `json:"picks"`
	Agreements uint64 `json:"agreements"`
	Fallbacks  uint64 `json:"inadmissible_fallbacks"`
}

// Decision is the optimizer choice for one request.
type Decision struct {
	AuthID     string
	Score      float64
	Admissible bool
}

// Engine owns optimizer state. Picks perform bounded scoring and a reservation
// update under a mutex; rollouts run on copies outside the lock.
type Engine struct {
	cfg Config
	now func() time.Time

	mu               sync.Mutex
	windows          map[string]*Window
	windowIndex      map[string][]string
	pooled           map[string]*ConsumptionEstimator
	classes          map[string]*ClassStats
	demand           map[string]*DemandForecast
	health           map[string]*Health
	authSeen         map[string]time.Time
	identities       map[string]string
	scopedClass      map[string]map[string]time.Time
	accounts         map[string]*AccountState
	pending          map[string]*pendingInterval
	reservations     map[string][]reservationGroup
	reserved         map[string]float64
	lastReserveSweep time.Time
	shadow           ShadowStats
	cursor           uint64

	prices atomic.Pointer[PriceSnapshot]
}

// NewEngine creates an engine. now may be nil to use the wall clock.
func NewEngine(cfg Config, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{
		cfg:          cfg,
		now:          now,
		windows:      make(map[string]*Window),
		windowIndex:  make(map[string][]string),
		pooled:       make(map[string]*ConsumptionEstimator),
		classes:      make(map[string]*ClassStats),
		demand:       make(map[string]*DemandForecast),
		health:       make(map[string]*Health),
		authSeen:     make(map[string]time.Time),
		identities:   make(map[string]string),
		scopedClass:  make(map[string]map[string]time.Time),
		accounts:     make(map[string]*AccountState),
		pending:      make(map[string]*pendingInterval),
		reservations: make(map[string][]reservationGroup),
		reserved:     make(map[string]float64),
	}
}

// Config returns the active configuration.
func (e *Engine) Config() Config { return e.cfg }

func pooledKey(provider, scope, kind string) string { return provider + "|" + scope + "|" + kind }

func accountProviderKey(account, provider string) string { return account + "|" + provider }

func accountOf(c Candidate) string {
	if account := strings.TrimSpace(c.Account); account != "" {
		return account
	}
	return "auth:" + hashAuthID(c.ID)
}

// observeCandidatesLocked records identity, offer time, and class eligibility.
func (e *Engine) observeCandidatesLocked(model string, candidates []Candidate, now time.Time) {
	for _, candidate := range candidates {
		account := accountOf(candidate)
		authKey := hashAuthID(candidate.ID)
		e.identities[authKey] = account
		e.authSeen[authKey] = now
		state := e.accounts[account]
		if state == nil {
			state = &AccountState{Classes: make(map[string]time.Time)}
			e.accounts[account] = state
		}
		state.Provider = strings.ToLower(candidate.Provider)
		state.LastSeen = now
		state.Classes[ClassOf(candidate.Provider, model)] = now
		trimAccountClasses(state, 256)
	}
}

func (e *Engine) accountForAuthLocked(authID string) string {
	if account, ok := e.identities[hashAuthID(authID)]; ok {
		return account
	}
	return "auth:" + hashAuthID(authID)
}

// RecordUsage ingests one completed attempt: it releases the oldest
// reservation of the credential, updates health, demand, and token statistics,
// and applies every quota window found in the response headers.
func (e *Engine) RecordUsage(usage Usage) {
	if strings.TrimSpace(usage.AuthID) == "" {
		return
	}
	now := usage.CompletedAt
	if now.IsZero() {
		now = e.now()
	}
	provider := strings.ToLower(strings.TrimSpace(usage.Provider))
	class := ClassOf(provider, usage.Model)
	tokens := math.Max(1, usage.Tokens)

	e.mu.Lock()
	defer e.mu.Unlock()

	e.releaseLocked(usage.AuthID)
	authKey := hashAuthID(usage.AuthID)
	health := e.health[authKey]
	if health == nil {
		health = &Health{}
		e.health[authKey] = health
	}
	health.Add(usage.Failed, usage.Latency)
	e.authSeen[authKey] = maxTime(e.authSeen[authKey], now)
	if usage.Generate && !usage.Failed {
		forecast := e.demand[class]
		if forecast == nil {
			forecast = &DemandForecast{}
			e.demand[class] = forecast
		}
		forecast.Record(now)
		stats := e.classes[class]
		if stats == nil {
			stats = &ClassStats{}
			e.classes[class] = stats
		}
		stats.Add(tokens)
		stats.LastSeen = now
	}

	account := e.accountForAuthLocked(usage.AuthID)
	for _, key := range e.windowIndex[accountProviderKey(account, provider)] {
		window := e.windows[key]
		if window == nil {
			continue
		}
		pending := e.pending[key]
		if pending == nil {
			pending = &pendingInterval{}
			e.pending[key] = pending
		}
		pending.tokens += tokens
		pending.requests++
	}
	for _, observation := range ParseWindows(provider, usage.Headers, now) {
		key := (WindowKey{Account: account, Provider: observation.Provider, Scope: observation.Scope, Kind: observation.Kind}).String()
		if observation.Scope != "default" && observation.Scope != "tokens" && observation.Scope != "requests" &&
			(scopeMatchesClass(observation.Scope, class) || e.windowDecreasedLocked(key, observation)) {
			classes := e.scopedClass[key]
			if classes == nil {
				classes = make(map[string]time.Time)
				e.scopedClass[key] = classes
			}
			classes[class] = now
		}
		e.applyObservationLocked(account, observation, now)
	}
}

func scopeMatchesClass(scope, class string) bool {
	scope = strings.ToLower(strings.TrimSpace(scope))
	return scope != "" && strings.Contains(strings.ToLower(class), scope)
}

func (e *Engine) windowDecreasedLocked(key string, observation Observation) bool {
	window := e.windows[key]
	if window == nil || window.ResetAt.IsZero() || observation.ResetAt.IsZero() ||
		absDuration(window.ResetAt.Sub(observation.ResetAt)) > generationTolerance {
		return false
	}
	return observation.Remaining < window.Remaining
}

func (e *Engine) applyObservationLocked(account string, observation Observation, now time.Time) {
	wk := WindowKey{Account: account, Provider: observation.Provider, Scope: observation.Scope, Kind: observation.Kind}
	key := wk.String()
	window := e.windows[key]
	if window == nil {
		window = &Window{Key: wk}
		e.windows[key] = window
		e.addWindowIndexLocked(key, wk)
	} else if now.Before(window.ObservedAt) {
		// Out-of-order completion: an older snapshot must not overwrite a newer one.
		return
	} else {
		pending := e.pending[key]
		sameInstance := !window.ResetAt.IsZero() && !observation.ResetAt.IsZero() &&
			absDuration(window.ResetAt.Sub(observation.ResetAt)) <= generationTolerance
		switch {
		case sameInstance:
			delta := window.Remaining - observation.Remaining
			if pending != nil && delta >= 0 {
				window.Estimator.Add(delta, pending.tokens, pending.requests, now)
				pooled := e.pooled[pooledKey(wk.Provider, wk.Scope, wk.Kind)]
				if pooled == nil {
					pooled = &ConsumptionEstimator{}
					e.pooled[pooledKey(wk.Provider, wk.Scope, wk.Kind)] = pooled
				}
				pooled.Add(delta, pending.tokens, pending.requests, now)
			}
		default:
			// A new window instance began. Reservations of the old instance
			// must not deplete it.
			if !window.generationResetAt.Equal(window.ResetAt) {
				e.invalidateGenerationReservationsLocked(key, window.Generation)
				window.Generation++
				window.generationResetAt = window.ResetAt
			}
		}
	}
	delete(e.pending, key)
	window.Unit = observation.Unit
	window.Capacity = observation.Capacity
	window.Remaining = observation.Remaining
	window.MaxSeen = math.Max(window.MaxSeen, observation.Remaining)
	window.ResetAt = observation.ResetAt
	window.Duration = int64(observation.Duration)
	window.Resolution = observation.Resolution
	window.ObservedAt = now
	window.Source = observation.Source
	if window.generationResetAt.Equal(window.ResetAt) && now.Before(window.ResetAt) {
		window.generationResetAt = time.Time{}
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func (e *Engine) addWindowIndexLocked(key string, window WindowKey) {
	indexKey := accountProviderKey(window.Account, window.Provider)
	for _, existing := range e.windowIndex[indexKey] {
		if existing == key {
			return
		}
	}
	e.windowIndex[indexKey] = append(e.windowIndex[indexKey], key)
	sort.Strings(e.windowIndex[indexKey])
}

func (e *Engine) removeWindowIndexLocked(key string, window WindowKey) {
	indexKey := accountProviderKey(window.Account, window.Provider)
	keys := e.windowIndex[indexKey]
	for i, existing := range keys {
		if existing == key {
			e.windowIndex[indexKey] = append(keys[:i], keys[i+1:]...)
			if len(e.windowIndex[indexKey]) == 0 {
				delete(e.windowIndex, indexKey)
			}
			return
		}
	}
}

func (e *Engine) releaseLocked(authID string) {
	groups := e.reservations[authID]
	if len(groups) == 0 {
		return
	}
	if len(groups) == 1 {
		e.subtractReservationLocked(groups[0])
		delete(e.reservations, authID)
		return
	}
	e.subtractReservationLocked(groups[0])
	e.reservations[authID] = groups[1:]
}

func (e *Engine) expireReservationsLocked(now time.Time) {
	for authID, groups := range e.reservations {
		for len(groups) > 0 && now.Sub(groups[0].at) >= e.cfg.ReservationTTL {
			e.subtractReservationLocked(groups[0])
			groups = groups[1:]
		}
		if len(groups) == 0 {
			delete(e.reservations, authID)
		} else {
			e.reservations[authID] = groups
		}
	}
}

func (e *Engine) reservedLocked() map[string]float64 {
	if len(e.reservations) == 0 {
		e.reserved = make(map[string]float64)
	}
	return e.reserved
}

func (e *Engine) subtractReservationLocked(group reservationGroup) {
	for _, item := range group.items {
		if window := e.windows[item.key]; window != nil && window.Generation == item.generation {
			e.reserved[item.key] -= item.amount
			if e.reserved[item.key] <= 1e-12 {
				delete(e.reserved, item.key)
			}
		}
	}
}

func (e *Engine) invalidateGenerationReservationsLocked(key string, generation uint64) {
	for authID, groups := range e.reservations {
		for groupIndex := range groups {
			items := groups[groupIndex].items[:0]
			for _, item := range groups[groupIndex].items {
				if item.key == key && item.generation == generation {
					e.reserved[key] -= item.amount
					continue
				}
				items = append(items, item)
			}
			groups[groupIndex].items = items
		}
		kept := groups[:0]
		for _, group := range groups {
			if len(group.items) > 0 {
				kept = append(kept, group)
			}
		}
		if len(kept) == 0 {
			delete(e.reservations, authID)
		} else {
			e.reservations[authID] = kept
		}
	}
	if e.reserved[key] <= 1e-12 {
		delete(e.reserved, key)
	}
}

func (e *Engine) purgeResetReservationsLocked(now time.Time) {
	if !e.lastReserveSweep.IsZero() && now.Sub(e.lastReserveSweep) < time.Second {
		return
	}
	e.lastReserveSweep = now
	for authID, groups := range e.reservations {
		keptGroups := groups[:0]
		for _, group := range groups {
			keptItems := group.items[:0]
			for _, item := range group.items {
				window := e.windows[item.key]
				if window != nil && window.Generation == item.generation && !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
					e.reserved[item.key] -= item.amount
					continue
				}
				keptItems = append(keptItems, item)
			}
			group.items = keptItems
			if len(group.items) > 0 {
				keptGroups = append(keptGroups, group)
			}
		}
		if len(keptGroups) == 0 {
			delete(e.reservations, authID)
		} else {
			e.reservations[authID] = keptGroups
		}
	}
	for key, amount := range e.reserved {
		if amount <= 1e-12 {
			delete(e.reserved, key)
		}
	}
}

// windowView is a window's budget as believed at a decision time.
type windowView struct {
	key       string
	window    *Window
	remaining float64
	capacity  float64
	resetAt   time.Time
	period    time.Duration
	obsSD     float64
	stale     bool
}

func (e *Engine) viewLocked(key string, window *Window, now time.Time) windowView {
	view := windowView{key: key, window: window, remaining: window.Remaining, resetAt: window.ResetAt, period: time.Duration(window.Duration)}
	view.capacity = window.Capacity
	if view.capacity <= 0 {
		view.capacity = window.MaxSeen
	}
	if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
		if !window.generationResetAt.Equal(window.ResetAt) {
			e.invalidateGenerationReservationsLocked(key, window.Generation)
			window.Generation++
			window.generationResetAt = window.ResetAt
		}
		// The observed instance has ended. A fixed reset restores only this
		// window. The next instance may start on first use, so its reset time
		// is unknown; the latest possible time is the conservative assumption.
		view.remaining = view.capacity
		view.resetAt = time.Time{}
		if view.period > 0 {
			view.resetAt = now.Add(view.period)
		}
		view.stale = true
		view.obsSD = window.Resolution / 2
		return view
	}
	age := now.Sub(window.ObservedAt).Hours()
	view.obsSD = window.Resolution/2 + e.cfg.DriftPerHour*view.capacity*math.Max(0, age)
	view.stale = now.Sub(window.ObservedAt) > e.cfg.StaleAfter
	return view
}

func (e *Engine) scopedWindowAppliesLocked(key, class string) bool {
	window := e.windows[key]
	if window == nil || window.Key.Scope == "default" || window.Key.Scope == "tokens" || window.Key.Scope == "requests" {
		return true
	}
	if scopeMatchesClass(window.Key.Scope, class) {
		return true
	}
	_, ok := e.scopedClass[key][class]
	return ok
}

// consumptionLocked returns the mean and standard deviation of the native
// consumption of one class request on a window.
func (e *Engine) consumptionLocked(window *Window, capacity float64, class string) (float64, float64) {
	stats := e.classes[class]
	if window.Unit == UnitRequests {
		return 1, 0
	}
	if window.Unit == UnitTokens {
		if stats != nil && stats.Count > 0 {
			return stats.MeanTokens, stats.SD()
		}
		return 1, 1
	}
	rate, hasRate := window.Estimator.Rate()
	variance, hasVariance := window.Estimator.PerRequestVariance()
	if !hasRate {
		pooled := e.pooled[pooledKey(window.Key.Provider, window.Key.Scope, window.Key.Kind)]
		rate, hasRate = pooled.Rate()
		if !hasVariance {
			variance, hasVariance = pooled.PerRequestVariance()
		}
	}
	if hasRate && stats != nil && stats.Count > 0 {
		mean := rate * stats.MeanTokens
		tokenSD := rate * stats.SD()
		if !hasVariance {
			variance = mean * mean
		}
		return mean, math.Sqrt(variance + tokenSD*tokenSD)
	}
	// Prior before evidence exists: a fixed number of requests per
	// 5-hour-equivalent of window length, with high uncertainty.
	scale := 1.0
	if duration := time.Duration(window.Duration); duration > 0 {
		scale = math.Max(1, float64(duration)/float64(5*time.Hour))
	}
	if capacity <= 0 {
		capacity = math.Max(window.Remaining, 1)
	}
	mean := capacity / (e.cfg.PriorRequestsPerWindow * scale)
	return mean, mean
}

func (e *Engine) accountWindowsLocked(account, provider string, now time.Time) []windowView {
	keys := e.windowIndex[accountProviderKey(account, provider)]
	views := make([]windowView, 0, len(keys))
	for _, key := range keys {
		if window := e.windows[key]; window != nil {
			views = append(views, e.viewLocked(key, window, now))
		}
	}
	return views
}

// zScore returns the one-sided normal quantile for 1-epsilon.
func zScore(epsilon float64) float64 {
	return math.Sqrt2 * math.Erfinv(1-2*epsilon)
}

type scored struct {
	candidate  Candidate
	score      float64
	margin     float64
	admissible bool
	amounts    []reservationItem
}

// Pick records the offered candidates, scores them, reserves estimated
// consumption on the executed credential, and returns the optimizer decision.
// executed selects which credential is reserved: the optimizer choice when
// empty, otherwise the given legacy choice (shadow mode).
func (e *Engine) Pick(model string, candidates []Candidate, executed string) Decision {
	if len(candidates) == 0 {
		return Decision{}
	}
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observeCandidatesLocked(model, candidates, now)
	e.expireReservationsLocked(now)
	e.purgeResetReservationsLocked(now)

	snapshot := e.prices.Load()
	reserved := e.reservedLocked()
	z := zScore(e.cfg.Epsilon)
	results := make([]scored, 0, len(candidates))
	maxKnownCost := 0.0
	unknown := make([]int, 0)
	for _, candidate := range candidates {
		account := accountOf(candidate)
		provider := strings.ToLower(candidate.Provider)
		class := ClassOf(provider, model)
		health := e.health[hashAuthID(candidate.ID)]
		success := health.SuccessProbability()
		latency := 0.0
		if health != nil {
			latency = health.LatencySec
		}
		result := scored{candidate: candidate, admissible: true, margin: math.Inf(1)}
		result.score = e.cfg.RequestValue*success - e.cfg.LambdaLatency*latency - e.cfg.LambdaFailure*(1-success)
		views := e.accountWindowsLocked(account, provider, now)
		cost := 0.0
		for _, view := range views {
			if !e.scopedWindowAppliesLocked(view.key, class) {
				continue
			}
			mean, sd := e.consumptionLocked(view.window, view.capacity, class)
			available := view.remaining - reserved[view.key]
			if view.stale && view.window.ResetAt.After(now) {
				// Stale but live: keep the observation, widen its uncertainty.
				available -= view.obsSD
			}
			margin := available - mean - z*math.Hypot(sd, view.obsSD)
			scale := math.Max(view.capacity, 1)
			result.margin = math.Min(result.margin, margin/scale)
			if margin < 0 {
				result.admissible = false
			}
			if snapshot != nil {
				cost += snapshot.Prices[view.key] * mean
			}
			result.amounts = append(result.amounts, reservationItem{key: view.key, generation: view.window.Generation, amount: mean})
		}
		if len(views) == 0 {
			unknown = append(unknown, len(results))
		} else {
			maxKnownCost = math.Max(maxKnownCost, cost)
		}
		result.score -= cost
		results = append(results, result)
	}
	// Unobserved accounts are admitted (host cooldowns stay authoritative) but
	// valued at the highest known opportunity cost: neither free nor excluded.
	for _, index := range unknown {
		results[index].score -= maxKnownCost
	}

	best := e.selectLocked(results)
	decision := Decision{AuthID: results[best].candidate.ID, Score: results[best].score, Admissible: results[best].admissible}
	if !decision.Admissible {
		e.shadow.Fallbacks++
	}
	reserveIndex := best
	if executed != "" {
		e.shadow.Picks++
		if executed == decision.AuthID {
			e.shadow.Agreements++
		}
		reserveIndex = -1
		for i := range results {
			if results[i].candidate.ID == executed {
				reserveIndex = i
				break
			}
		}
	}
	if reserveIndex >= 0 {
		id := results[reserveIndex].candidate.ID
		group := reservationGroup{at: now, items: results[reserveIndex].amounts}
		e.reservations[id] = append(e.reservations[id], group)
		for _, item := range group.items {
			e.reserved[item.key] += item.amount
		}
	}
	return decision
}

const scoreTieTolerance = 1e-9

// selectLocked returns the admissible argmax score; exact ties rotate for
// fairness. Without an admissible candidate, it returns the smallest violation.
func (e *Engine) selectLocked(results []scored) int {
	anyAdmissible := false
	for _, result := range results {
		if result.admissible {
			anyAdmissible = true
			break
		}
	}
	better := func(a, b scored) int {
		if anyAdmissible {
			if a.admissible != b.admissible {
				if a.admissible {
					return 1
				}
				return -1
			}
			if math.Abs(a.score-b.score) > scoreTieTolerance {
				if a.score > b.score {
					return 1
				}
				return -1
			}
			return 0
		}
		if math.Abs(a.margin-b.margin) > scoreTieTolerance {
			if a.margin > b.margin {
				return 1
			}
			return -1
		}
		return 0
	}
	ties := []int{0}
	for i := 1; i < len(results); i++ {
		switch better(results[i], results[ties[0]]) {
		case 1:
			ties = ties[:0]
			ties = append(ties, i)
		case 0:
			ties = append(ties, i)
		}
	}
	sort.Slice(ties, func(i, j int) bool { return results[ties[i]].candidate.ID < results[ties[j]].candidate.ID })
	choice := ties[e.cursor%uint64(len(ties))]
	e.cursor++
	return choice
}

// Prices returns the latest published valuation, or nil before the first one.
func (e *Engine) Prices() *PriceSnapshot { return e.prices.Load() }

// Shadow returns shadow-mode comparison counters.
func (e *Engine) Shadow() ShadowStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shadow
}

// Prune drops accounts, windows, and health that have not been seen recently,
// so removed or rotated credentials do not keep actionable phantom capacity.
func (e *Engine) Prune() {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for account, state := range e.accounts {
		if now.Sub(state.LastSeen) > e.cfg.AccountTTL {
			delete(e.accounts, account)
		}
	}
	for key, window := range e.windows {
		if _, ok := e.accounts[window.Key.Account]; !ok && now.Sub(window.ObservedAt) > e.cfg.AccountTTL {
			delete(e.windows, key)
			delete(e.pending, key)
			delete(e.scopedClass, key)
			e.removeWindowIndexLocked(key, window.Key)
			delete(e.reserved, key)
		}
	}
	for key, classes := range e.scopedClass {
		for class, seen := range classes {
			if now.Sub(seen) > e.cfg.AccountTTL {
				delete(classes, class)
			}
		}
		if len(classes) == 0 {
			delete(e.scopedClass, key)
		}
	}
	for authID, seen := range e.authSeen {
		if now.Sub(seen) > e.cfg.AccountTTL {
			delete(e.authSeen, authID)
			delete(e.health, authID)
			delete(e.identities, authID)
			for reservationAuthID, groups := range e.reservations {
				if hashAuthID(reservationAuthID) != authID {
					continue
				}
				for _, group := range groups {
					e.subtractReservationLocked(group)
				}
				delete(e.reservations, reservationAuthID)
			}
		}
	}
	e.pruneClassesLocked(now)
}

func trimAccountClasses(state *AccountState, limit int) {
	for len(state.Classes) > limit {
		var oldest string
		var oldestAt time.Time
		for class, seen := range state.Classes {
			if oldest == "" || seen.Before(oldestAt) {
				oldest, oldestAt = class, seen
			}
		}
		delete(state.Classes, oldest)
	}
}

func (e *Engine) pruneClassesLocked(now time.Time) {
	ttl := e.cfg.AccountTTL
	lastSeen := make(map[string]time.Time, len(e.demand)+len(e.classes))
	for class, forecast := range e.demand {
		if forecast != nil {
			lastSeen[class] = maxTime(lastSeen[class], forecast.LastSeen)
		}
	}
	for class, stats := range e.classes {
		if stats != nil {
			lastSeen[class] = maxTime(lastSeen[class], stats.LastSeen)
		}
	}
	for class, seen := range lastSeen {
		if !seen.IsZero() && now.Sub(seen) > ttl {
			delete(e.demand, class)
			delete(e.classes, class)
		}
	}
	for account, state := range e.accounts {
		for class, seen := range state.Classes {
			if now.Sub(seen) > ttl {
				delete(state.Classes, class)
			}
		}
		if len(state.Classes) == 0 && now.Sub(state.LastSeen) > ttl {
			delete(e.accounts, account)
		}
	}
	if len(lastSeen) <= 256 {
		return
	}
	classes := make([]string, 0, len(lastSeen))
	for class := range lastSeen {
		classes = append(classes, class)
	}
	sort.Slice(classes, func(i, j int) bool {
		if lastSeen[classes[i]].Equal(lastSeen[classes[j]]) {
			return classes[i] < classes[j]
		}
		return lastSeen[classes[i]].After(lastSeen[classes[j]])
	})
	for _, class := range classes[256:] {
		delete(e.demand, class)
		delete(e.classes, class)
		for _, state := range e.accounts {
			delete(state.Classes, class)
		}
	}
}
