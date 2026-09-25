package config

// Fork-owned multi-tenancy configuration. Kept out of config_types.go so
// upstream merges do not conflict with fork declarations.

// DefaultUserPanelGitHubRepository is the release channel for the tenant user panel.
const DefaultUserPanelGitHubRepository = "https://github.com/nmdlkg/CLIProxyAPI-User-Panel"

// TenancyConfig configures multi-user credential ownership, per-user quota accounting,
// and quota-window-aware credential balancing.
//
// Credential ownership itself is not stored here: it lives in the auth file JSON as the
// top-level keys "owner_user_id", "shared", and "contribution_tier", which every auth
// store backend round-trips unchanged.
type TenancyConfig struct {
	// Enabled turns on multi-user mode. When false, behavior is unchanged.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// DBPath overrides the tenancy database location. Empty means <auth-dir>/../tenancy.db.
	DBPath string `yaml:"db-path,omitempty" json:"db-path,omitempty"`

	// ValidationInterval is how often a user's own credential is preferred for their
	// request so their OAuth token is validated by real traffic. Duration string, e.g. "1h".
	ValidationInterval string `yaml:"validation-interval,omitempty" json:"validation-interval,omitempty"`

	// Quota configures per-user quota limits and USD cost accounting.
	Quota TenancyQuota `yaml:"quota" json:"quota"`

	// Balancing configures reset-window-aware credential preference.
	Balancing TenancyBalancing `yaml:"balancing" json:"balancing"`

	// UserPanel controls the tenant-facing web panel asset.
	UserPanel UserPanelConfig `yaml:"user-panel" json:"user-panel"`

	// Pricing is populated during config sanitization so the tenancy service can
	// honor OpenRouter cost-basis settings without changing its server-owned
	// constructor. It is runtime-only; OpenRouter remains the human-facing key.
	Pricing OpenRouterConfig `yaml:"-" json:"-"`
}

// TenancyQuota configures per-user quota limits derived from contributed credentials.
type TenancyQuota struct {
	// Enforce controls whether quota is actually enforced.
	//
	// Default false = observe-only: usage is still attributed and written to the
	// ledger, but no request is ever denied and Quota.Check is not consulted at
	// all. This is deliberately a separate switch from a very large limit: the
	// quota check fails CLOSED on store errors, so a "huge number" would still
	// return 429 if the ledger were unreadable. Observe-only must have zero user
	// impact, so it skips the check entirely.
	//
	// Flip to true once real usage data justifies concrete limits.
	Enforce bool `yaml:"enforce" json:"enforce"`

	// Window is the rolling quota window. Duration string. Defaults to "168h"
	// (weekly). Note Go's duration parser has no day unit, so express multi-day
	// windows in hours ("168h", not "7d").
	Window string `yaml:"window,omitempty" json:"window,omitempty"`

	// BaseUSD maps a user tier label to its USD allowance per rolling window.
	// Human config uses decimal USD; values are nano-USD integers after decode.
	// The key "default" applies to users without a matching tier.
	BaseUSD map[string]USDLimit `yaml:"base-usd,omitempty" json:"base-usd,omitempty"`

	// ContributionUSD maps provider -> plan tier -> additional USD per
	// contributed credential. Human config uses decimal USD; values are
	// nano-USD integers after decode. "default" applies when the plan is unknown.
	ContributionUSD map[string]map[string]USDLimit `yaml:"contribution-usd,omitempty" json:"contribution-usd,omitempty"`

	// ModelPriceOverrides maps a local model name to prices in $ per 1M tokens.
	// Values are stored internally as nano-USD per token. The "*" entry is the
	// last-resort floor for models with no exact or OpenRouter price.
	ModelPriceOverrides map[string]ModelPriceOverride `yaml:"model-price-overrides,omitempty" json:"model-price-overrides,omitempty"`

	// ProviderWindows maps provider -> assumed quota window length, used when the
	// upstream supplies no rate-limit reset header. Duration strings, e.g. "5h".
	ProviderWindows map[string]string `yaml:"provider-windows,omitempty" json:"provider-windows,omitempty"`
}

// ModelPriceOverride is a local model's token pricing in conventional $/1M
// units. Pointer fields preserve the distinction between an absent price and an
// explicitly configured free price of zero.
type ModelPriceOverride struct {
	Prompt     *USDPerMillionTokens `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Completion *USDPerMillionTokens `yaml:"completion,omitempty" json:"completion,omitempty"`
	Reasoning  *USDPerMillionTokens `yaml:"reasoning,omitempty" json:"reasoning,omitempty"`
	CacheRead  *USDPerMillionTokens `yaml:"cache-read,omitempty" json:"cache-read,omitempty"`
	CacheWrite *USDPerMillionTokens `yaml:"cache-write,omitempty" json:"cache-write,omitempty"`
}

// TenancyBalancing configures preference for credentials whose quota window resets soon
// and that still have unused budget.
type TenancyBalancing struct {
	// UrgencyHorizon is how close a window reset must be to earn a priority bonus.
	// Duration string, e.g. "30m".
	UrgencyHorizon string `yaml:"urgency-horizon,omitempty" json:"urgency-horizon,omitempty"`

	// HighWater is the used/limit ratio above which no bonus is granted (0 < v <= 1).
	HighWater float64 `yaml:"high-water,omitempty" json:"high-water,omitempty"`

	// UrgencyBonus is the discrete priority bonus added to qualifying credentials.
	UrgencyBonus int `yaml:"urgency-bonus,omitempty" json:"urgency-bonus,omitempty"`
}

// UserPanelConfig controls the externally maintained tenant user-panel asset
// and its verified update channel.
type UserPanelConfig struct {
	// GitHubRepository is the GitHub repository containing signed panel releases.
	GitHubRepository string `yaml:"github-repository" json:"github-repository"`
	// PinnedVersion selects one exact semantic-versioned release when set.
	PinnedVersion string `yaml:"pinned-version,omitempty" json:"pinned-version,omitempty"`
	// DisableAutoUpdate disables scheduled and hot-reload-triggered updates.
	DisableAutoUpdate bool `yaml:"disable-auto-update" json:"disable-auto-update"`
	// DevMode enables development-only controls, including the manual refresh API.
	DevMode bool `yaml:"dev-mode" json:"dev-mode"`
}
