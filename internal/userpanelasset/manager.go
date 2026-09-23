// Package userpanelasset manages the signed, runtime-updatable tenant user
// panel. It deliberately has no dependency on the HTTP API package so the
// server can own one isolated instance for its whole lifetime.
package userpanelasset

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	userPanelHTMLName      = "user.html"
	userPanelManifestName  = "manifest.json"
	userPanelSignatureName = "manifest.json.sig"
	userPanelFloorName     = "version-floor"

	maxPanelHTMLSize      int64 = 10 << 20
	maxManifestSize       int64 = 256 << 10
	maxSignatureSize      int64 = 16 << 10
	maxReleaseListSize    int64 = 4 << 20
	maxErrorBodySize      int64 = 8 << 10
	maxGitHubTokenSize    int64 = 16 << 10
	manualRefreshFloor          = 30 * time.Second
	defaultUpdateInterval       = 3 * time.Hour
	userPanelUserAgent          = "CLIProxyAPI-user-panel-updater"
)

var (
	// The release public keys are intentionally compiled into the binary. The
	// repository does not provide an in-process key-discovery mechanism.
	// Additional keys can be supplied to tests or a downstream build with
	// WithTrustedPublicKeys without weakening the default verification path.
	compiledTrustedPublicKeys = map[string]ed25519.PublicKey{
		"release-2026": mustPublicKey("780f7f1468778ecd02fa4a4d3044a1073ec1907268cf47c21611e6294bee1480"),
	}

	ErrAutoUpdateDisabled           = errors.New("user panel automatic updates are disabled")
	ErrDevOverride                  = errors.New("user panel development override is active")
	ErrManualRefreshRequiresDevMode = errors.New("user panel manual refresh requires dev-mode")
	ErrRefreshBusy                  = errors.New("user panel refresh is already in progress")
	ErrRefreshThrottled             = errors.New("user panel refresh is throttled")
	ErrNoRelease                    = errors.New("no suitable user panel release found")
)

// Manifest is the signed metadata shipped alongside a panel release.
type Manifest struct {
	SchemaVersion       int      `json:"schema_version"`
	Version             string   `json:"version"`
	KeyID               string   `json:"key_id"`
	Artifact            string   `json:"artifact"`
	Size                int64    `json:"size"`
	SHA256              string   `json:"sha256"`
	CSPHashes           []string `json:"csp_hashes"`
	RuntimeDependencies []string `json:"runtime_dependencies"`
}

// ReleaseInfo is the subset of a GitHub release used by the updater.
type ReleaseInfo struct {
	TagName    string
	Prerelease bool
	Draft      bool
	Assets     []ReleaseAsset
}

// ReleaseAsset describes a downloadable GitHub release asset.
type ReleaseAsset struct {
	Name string
	URL  string
}

// Source identifies which panel asset was served.
type Source string

const (
	SourceUnavailable Source = "unavailable"
	SourceCache       Source = "signed-cache"
	SourceDev         Source = "development"
)

// Status is safe to return from the admin status endpoint.
type Status struct {
	Source                Source    `json:"source"`
	Version               string    `json:"version"`
	CachedVersion         string    `json:"cached_version,omitempty"`
	HighWaterVersion      string    `json:"high_water_version,omitempty"`
	CacheValid            bool      `json:"cache_valid"`
	DevMode               bool      `json:"dev_mode"`
	AutoUpdateDisabled    bool      `json:"auto_update_disabled"`
	RemoteUpdatesDisabled bool      `json:"remote_updates_disabled"`
	LastCheck             time.Time `json:"last_check,omitempty"`
	LastError             string    `json:"last_error,omitempty"`
	RefreshInFlight       bool      `json:"refresh_in_flight"`
	RetryAfterSeconds     int       `json:"retry_after_seconds,omitempty"`
}

// RefreshThrottleError reports the remaining manual refresh floor.
type RefreshThrottleError struct {
	Remaining time.Duration
}

func (e *RefreshThrottleError) Error() string {
	if e == nil {
		return ErrRefreshThrottled.Error()
	}
	return fmt.Sprintf("%s: retry in %s", ErrRefreshThrottled, e.Remaining.Round(time.Second))
}

func (e *RefreshThrottleError) Unwrap() error { return ErrRefreshThrottled }

// RetryAfter returns a useful HTTP Retry-After duration for an updater error.
func RetryAfter(err error) time.Duration {
	var throttle *RefreshThrottleError
	if errors.As(err, &throttle) && throttle != nil {
		return throttle.Remaining
	}
	return 0
}

// Option customises a Manager, primarily for tests and downstream builds that
// ship a different compiled release key set.
type Option func(*Manager)

// WithTrustedPublicKeys replaces the compiled key set for a manager instance.
func WithTrustedPublicKeys(keys map[string]ed25519.PublicKey) Option {
	return func(m *Manager) {
		m.trustedKeys = clonePublicKeys(keys)
	}
}

// WithHTTPClient supplies the client used for GitHub requests.
func WithHTTPClient(client *http.Client) Option {
	return func(m *Manager) {
		if client != nil {
			m.httpClient = client
			m.customHTTPClient = true
		}
	}
}

// WithReleaseAPIURL overrides the repository-derived releases endpoint. It is
// useful for a controlled mirror and for deterministic package tests.
func WithReleaseAPIURL(endpoint string) Option {
	return func(m *Manager) {
		m.releaseAPIOverride = strings.TrimSpace(endpoint)
		m.allowNonGitHubRemote = true
	}
}

// WithCacheDir overrides the default process static directory.
func WithCacheDir(dir string) Option {
	return func(m *Manager) {
		m.cacheDir = filepath.Clean(strings.TrimSpace(dir))
	}
}

// WithNow injects a clock for deterministic tests.
func WithNow(now func() time.Time) Option {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

type panelConfig struct {
	enabled           bool
	repository        string
	pinnedVersion     string
	disableAutoUpdate bool
	devMode           bool
	proxyURL          string
}

type cachedAsset struct {
	html     []byte
	manifest Manifest
	csp      []string
}

// Manager owns one panel cache and reads the external development panel when
// USER_PANEL_STATIC_PATH is configured.
type Manager struct {
	configFilePath string
	cacheDir       string

	mu                   sync.RWMutex
	cfg                  panelConfig
	trustedKeys          map[string]ed25519.PublicKey
	httpClient           *http.Client
	customHTTPClient     bool
	releaseAPIOverride   string
	allowNonGitHubRemote bool
	now                  func() time.Time

	updateMu          sync.Mutex
	stateMu           sync.RWMutex
	lastCheck         time.Time
	lastError         error
	lastManualRefresh time.Time
	refreshInFlight   bool
}

// NewManager creates an instance-owned user-panel asset manager. It performs
// no network I/O; panel bytes are loaded from the configured development path
// or verified release cache when requests arrive.
func NewManager(configFilePath string, options ...Option) *Manager {
	m := &Manager{
		configFilePath: strings.TrimSpace(configFilePath),
		cacheDir:       defaultCacheDir(configFilePath),
		trustedKeys:    clonePublicKeys(compiledTrustedPublicKeys),
		httpClient:     newHTTPClient(""),
		now:            time.Now,
	}
	for _, option := range options {
		if option != nil {
			option(m)
		}
	}
	return m
}

// CompiledTrustedPublicKeys returns a copy of the built-in release key set.
func CompiledTrustedPublicKeys() map[string]ed25519.PublicKey {
	return clonePublicKeys(compiledTrustedPublicKeys)
}

// SetConfig applies the currently live configuration. It only reads local
// state and never starts a network update.
func (m *Manager) SetConfig(cfg *config.Config) {
	if m == nil {
		return
	}
	snapshot := panelConfig{repository: config.DefaultUserPanelGitHubRepository}
	if cfg != nil {
		snapshot.enabled = cfg.Tenancy.Enabled
		snapshot.repository = strings.TrimSpace(cfg.Tenancy.UserPanel.GitHubRepository)
		snapshot.pinnedVersion = strings.TrimSpace(cfg.Tenancy.UserPanel.PinnedVersion)
		snapshot.disableAutoUpdate = cfg.Tenancy.UserPanel.DisableAutoUpdate
		snapshot.devMode = cfg.Tenancy.UserPanel.DevMode
		snapshot.proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if snapshot.repository == "" {
		snapshot.repository = config.DefaultUserPanelGitHubRepository
	}
	if rawDevMode, configured := os.LookupEnv("USER_PANEL_DEV_MODE"); configured {
		if devMode, errParse := strconv.ParseBool(strings.TrimSpace(rawDevMode)); errParse == nil {
			snapshot.devMode = devMode
		} else {
			log.WithError(errParse).Warn("user panel: ignore invalid USER_PANEL_DEV_MODE")
		}
	}
	m.mu.Lock()
	m.cfg = snapshot
	if !m.customHTTPClient {
		m.httpClient = newHTTPClient(snapshot.proxyURL)
	}
	m.mu.Unlock()
}

// Configured reports the manager's current panel configuration for lifecycle
// and route wiring tests.
func (m *Manager) Configured() bool { return m != nil }

// ServeHTTP serves only local, already-verified data. In particular, this
// method never invokes Sync or any HTTP client.
func (m *Manager) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if m == nil || w == nil {
		return
	}
	body, csp, source, version, ok := m.currentAsset()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", cspHeader(csp))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("X-User-Panel-Source", string(source))
	w.Header().Set("X-User-Panel-Version", version)
	if _, errWrite := w.Write(body); errWrite != nil {
		log.WithError(errWrite).Debug("user panel: write response")
	}
}

// Status reports local state without contacting GitHub.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{}
	}
	cfg := m.configSnapshot()
	cache, cacheErr := m.loadCache()
	dev, devErr := m.loadDevAsset(cfg)
	selectedBody, selectedCSP, source, version, selected := m.currentAssetFrom(cfg, cache, cacheErr, dev, devErr)
	_ = selectedBody
	_ = selectedCSP

	m.stateMu.RLock()
	lastCheck := m.lastCheck
	lastErr := m.lastError
	refreshInFlight := m.refreshInFlight
	m.stateMu.RUnlock()
	status := Status{
		Source:                source,
		Version:               version,
		CacheValid:            cacheErr == nil && cache != nil,
		DevMode:               cfg.devMode,
		AutoUpdateDisabled:    cfg.disableAutoUpdate,
		RemoteUpdatesDisabled: devPath(cfg) != "",
		LastCheck:             lastCheck,
		RefreshInFlight:       refreshInFlight,
	}
	if cache != nil && cacheErr == nil {
		status.CachedVersion = cache.manifest.Version
	}
	if highWater, errHighWater := m.loadVersionFloor(); errHighWater == nil {
		status.HighWaterVersion = highWater
	}
	if lastErr != nil {
		status.LastError = lastErr.Error()
	}
	if retry := m.manualRetryAfter(); retry > 0 {
		status.RetryAfterSeconds = int((retry + time.Second - 1) / time.Second)
	}
	if !selected {
		status.Source = SourceUnavailable
	}
	return status
}

// Refresh performs a manually requested update. It is intentionally gated by
// the live dev-mode setting and shares a single update slot with scheduled
// updates.
func (m *Manager) Refresh(ctx context.Context) error {
	if m == nil {
		return errors.New("user panel manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := m.configSnapshot()
	if !cfg.devMode {
		return ErrManualRefreshRequiresDevMode
	}
	if path := devPath(cfg); path != "" {
		return ErrDevOverride
	}
	if !m.updateMu.TryLock() {
		return ErrRefreshBusy
	}
	defer m.updateMu.Unlock()
	now := m.now()
	m.stateMu.Lock()
	if !m.lastManualRefresh.IsZero() {
		elapsed := now.Sub(m.lastManualRefresh)
		if elapsed < manualRefreshFloor {
			remaining := manualRefreshFloor - elapsed
			m.stateMu.Unlock()
			return &RefreshThrottleError{Remaining: remaining}
		}
	}
	m.lastManualRefresh = now
	m.refreshInFlight = true
	m.stateMu.Unlock()
	defer func() {
		m.stateMu.Lock()
		m.refreshInFlight = false
		m.stateMu.Unlock()
	}()
	return m.syncLocked(ctx, true)
}

// Sync performs one automatic update attempt. It is safe to call from a
// supervisor and blocks other automatic attempts so there is one updater per
// server instance.
func (m *Manager) Sync(ctx context.Context) error {
	if m == nil {
		return errors.New("user panel manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	return m.syncLocked(ctx, false)
}

func (m *Manager) syncLocked(ctx context.Context, manual bool) error {
	cfg := m.configSnapshot()
	if !cfg.enabled {
		m.recordResult(nil)
		return ErrAutoUpdateDisabled
	}
	if !manual && cfg.disableAutoUpdate {
		m.recordResult(nil)
		return ErrAutoUpdateDisabled
	}
	if path := devPath(cfg); path != "" {
		m.recordResult(nil)
		return ErrDevOverride
	}
	if strings.TrimSpace(cfg.repository) == "" {
		m.recordResult(fmt.Errorf("%w: repository is empty", ErrNoRelease))
		return fmt.Errorf("%w: repository is empty", ErrNoRelease)
	}
	if errContext := ctx.Err(); errContext != nil {
		m.recordResult(errContext)
		return errContext
	}

	cache, cacheErr := m.loadCache()
	floor := ""
	highWater, errHighWater := m.loadVersionFloor()
	if errHighWater != nil && !errors.Is(errHighWater, os.ErrNotExist) {
		m.recordResult(errHighWater)
		return errHighWater
	}
	if errHighWater == nil {
		floor = highWater
	}
	if cacheErr == nil && cache != nil {
		if floor == "" || compareSemverString(cache.manifest.Version, floor) > 0 {
			floor = cache.manifest.Version
		}
		if highWater == "" || compareSemverString(floor, highWater) > 0 {
			if errFloor := m.writeVersionFloor(floor); errFloor != nil {
				m.recordResult(errFloor)
				return errFloor
			}
		}
	}

	releases, errReleases := m.fetchReleases(ctx, cfg)
	if errReleases != nil {
		m.recordResult(errReleases)
		return errReleases
	}
	release, errSelect := selectRelease(releases, cfg.pinnedVersion)
	if errSelect != nil {
		m.recordResult(errSelect)
		return errSelect
	}
	if compareSemverString(release.TagName, floor) <= 0 {
		m.recordResult(nil)
		return nil
	}

	assets := make(map[string]ReleaseAsset, len(release.Assets))
	for _, asset := range release.Assets {
		assets[strings.ToLower(strings.TrimSpace(asset.Name))] = asset
	}
	var missing []string
	for _, name := range []string{userPanelHTMLName, userPanelManifestName, userPanelSignatureName} {
		if strings.TrimSpace(assets[name].URL) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		errMissing := fmt.Errorf("%w: release %s missing assets %s", ErrNoRelease, release.TagName, strings.Join(missing, ", "))
		m.recordResult(errMissing)
		return errMissing
	}
	html, errHTML := m.fetchAsset(ctx, assets[userPanelHTMLName].URL, maxPanelHTMLSize)
	if errHTML != nil {
		m.recordResult(errHTML)
		return errHTML
	}
	manifestRaw, errManifest := m.fetchAsset(ctx, assets[userPanelManifestName].URL, maxManifestSize)
	if errManifest != nil {
		m.recordResult(errManifest)
		return errManifest
	}
	signatureRaw, errSignature := m.fetchAsset(ctx, assets[userPanelSignatureName].URL, maxSignatureSize)
	if errSignature != nil {
		m.recordResult(errSignature)
		return errSignature
	}
	verified, errVerify := m.verifyBundle(html, manifestRaw, signatureRaw)
	if errVerify != nil {
		m.recordResult(errVerify)
		return errVerify
	}
	if compareSemverString(verified.manifest.Version, release.TagName) != 0 {
		errVersion := fmt.Errorf("release tag %q does not match manifest version %q", release.TagName, verified.manifest.Version)
		m.recordResult(errVersion)
		return errVersion
	}
	if compareSemverString(verified.manifest.Version, floor) <= 0 {
		m.recordResult(nil)
		return nil
	}
	if errWrite := m.writeCache(html, manifestRaw, signatureRaw); errWrite != nil {
		m.recordResult(errWrite)
		return errWrite
	}
	if errFloor := m.writeVersionFloor(verified.manifest.Version); errFloor != nil {
		m.recordResult(errFloor)
		return errFloor
	}
	m.recordResult(nil)
	return nil
}

func (m *Manager) recordResult(err error) {
	m.stateMu.Lock()
	m.lastCheck = m.now()
	m.lastError = err
	m.stateMu.Unlock()
}

func (m *Manager) configSnapshot() panelConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Manager) manualRetryAfter() time.Duration {
	m.stateMu.RLock()
	last := m.lastManualRefresh
	m.stateMu.RUnlock()
	if last.IsZero() {
		return 0
	}
	remaining := manualRefreshFloor - m.now().Sub(last)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func (m *Manager) currentAsset() ([]byte, []string, Source, string, bool) {
	cfg := m.configSnapshot()
	cache, cacheErr := m.loadCache()
	dev, devErr := m.loadDevAsset(cfg)
	return m.currentAssetFrom(cfg, cache, cacheErr, dev, devErr)
}

func (m *Manager) currentAssetFrom(cfg panelConfig, cache *cachedAsset, cacheErr error, dev *cachedAsset, devErr error) ([]byte, []string, Source, string, bool) {
	if devErr == nil && dev != nil {
		return append([]byte(nil), dev.html...), append([]string(nil), dev.csp...), SourceDev, "dev", true
	}
	if cacheErr == nil && cache != nil {
		return append([]byte(nil), cache.html...), append([]string(nil), cache.csp...), SourceCache, cache.manifest.Version, true
	}
	return nil, nil, SourceUnavailable, "", false
}

func (m *Manager) loadDevAsset(cfg panelConfig) (*cachedAsset, error) {
	path := devPath(cfg)
	if path == "" {
		return nil, os.ErrNotExist
	}
	data, errRead := readLimitedFile(path, maxPanelHTMLSize)
	if errRead != nil {
		return nil, fmt.Errorf("read development panel: %w", errRead)
	}
	return &cachedAsset{html: data, csp: calculateCSPHashes(data)}, nil
}

func (m *Manager) loadCache() (*cachedAsset, error) {
	paths := m.cachePaths()
	if paths.dir == "" {
		return nil, os.ErrNotExist
	}
	manifestRaw, errManifest := readLimitedFile(paths.manifest, maxManifestSize)
	if errManifest != nil {
		return nil, errManifest
	}
	signatureRaw, errSignature := readLimitedFile(paths.signature, maxSignatureSize)
	if errSignature != nil {
		return nil, errSignature
	}
	html, errHTML := readLimitedFile(paths.html, maxPanelHTMLSize)
	if errHTML != nil {
		return nil, errHTML
	}
	asset, errVerify := m.verifyBundle(html, manifestRaw, signatureRaw)
	if errVerify != nil {
		return nil, errVerify
	}
	if highWater, errHighWater := m.loadVersionFloor(); errHighWater == nil && compareSemverString(asset.manifest.Version, highWater) < 0 {
		return nil, fmt.Errorf("cached panel version %q is below high-water floor %q", asset.manifest.Version, highWater)
	} else if errHighWater != nil && !errors.Is(errHighWater, os.ErrNotExist) {
		return nil, errHighWater
	}
	return asset, nil
}

func (m *Manager) verifyBundle(html, manifestRaw, signatureRaw []byte) (*cachedAsset, error) {
	manifest, errDecode := decodeManifest(manifestRaw)
	if errDecode != nil {
		return nil, errDecode
	}
	signature, errSignature := decodeSignature(signatureRaw)
	if errSignature != nil {
		return nil, errSignature
	}
	key, ok := m.trustedKeys[manifest.KeyID]
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("manifest uses unknown trusted key id %q", manifest.KeyID)
	}
	if !ed25519.Verify(key, manifestRaw, signature) {
		return nil, errors.New("manifest signature verification failed")
	}
	if manifest.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported manifest schema version %d", manifest.SchemaVersion)
	}
	if manifest.Artifact != userPanelHTMLName {
		return nil, fmt.Errorf("manifest artifact %q is not %q", manifest.Artifact, userPanelHTMLName)
	}
	if len(manifest.RuntimeDependencies) != 0 {
		return nil, errors.New("user panel manifest declares external runtime dependencies")
	}
	version, errVersion := parseSemver(manifest.Version)
	if errVersion != nil {
		return nil, fmt.Errorf("manifest version: %w", errVersion)
	}
	if version == nil {
		return nil, errors.New("manifest version is empty")
	}
	if manifest.Size <= 0 || manifest.Size > maxPanelHTMLSize || manifest.Size != int64(len(html)) {
		return nil, fmt.Errorf("manifest size %d does not match panel size %d", manifest.Size, len(html))
	}
	digest, errDigest := decodeSHA256(manifest.SHA256)
	if errDigest != nil {
		return nil, errDigest
	}
	actual := sha256.Sum256(html)
	if !equalBytes(digest, actual[:]) {
		return nil, errors.New("panel SHA-256 digest mismatch")
	}
	csp, errCSP := normalizeCSPHashes(manifest.CSPHashes)
	if errCSP != nil {
		return nil, errCSP
	}
	if !sameStringSet(csp, calculateCSPHashes(html)) {
		return nil, errors.New("manifest CSP hashes do not match inline panel assets")
	}
	return &cachedAsset{html: append([]byte(nil), html...), manifest: manifest, csp: csp}, nil
}

type cachePaths struct {
	dir, html, manifest, signature, floor string
}

func (m *Manager) cachePaths() cachePaths {
	m.mu.RLock()
	dir := m.cacheDir
	m.mu.RUnlock()
	if dir == "" {
		return cachePaths{}
	}
	return cachePaths{
		dir:       dir,
		html:      filepath.Join(dir, userPanelHTMLName),
		manifest:  filepath.Join(dir, userPanelManifestName),
		signature: filepath.Join(dir, userPanelSignatureName),
		floor:     filepath.Join(dir, userPanelFloorName),
	}
}

func (m *Manager) writeCache(html, manifest, signature []byte) error {
	paths := m.cachePaths()
	if paths.dir == "" {
		return errors.New("user panel cache directory is empty")
	}
	if errMkdir := os.MkdirAll(paths.dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create user panel cache directory: %w", errMkdir)
	}
	if errChmod := os.Chmod(paths.dir, 0o700); errChmod != nil {
		return fmt.Errorf("secure user panel cache directory: %w", errChmod)
	}
	// Each file is replaced atomically. A reader that observes the short window
	// between replacements rejects the mixed bundle during signature/hash checks.
	if errWrite := atomicWrite(paths.html, html, "user-panel-*.html", 0o600); errWrite != nil {
		return fmt.Errorf("write user panel HTML cache: %w", errWrite)
	}
	if errWrite := atomicWrite(paths.manifest, manifest, "user-panel-*.json", 0o600); errWrite != nil {
		return fmt.Errorf("write user panel manifest cache: %w", errWrite)
	}
	if errWrite := atomicWrite(paths.signature, signature, "user-panel-*.sig", 0o600); errWrite != nil {
		return fmt.Errorf("write user panel signature cache: %w", errWrite)
	}
	return nil
}

func (m *Manager) loadVersionFloor() (string, error) {
	paths := m.cachePaths()
	if paths.floor == "" {
		return "", os.ErrNotExist
	}
	raw, errRead := readLimitedFile(paths.floor, 128)
	if errRead != nil {
		return "", errRead
	}
	version := strings.TrimSpace(string(raw))
	if _, errVersion := parseSemver(version); errVersion != nil {
		return "", fmt.Errorf("invalid user panel high-water version: %w", errVersion)
	}
	return version, nil
}

func (m *Manager) writeVersionFloor(version string) error {
	paths := m.cachePaths()
	if paths.dir == "" || paths.floor == "" {
		return errors.New("user panel cache directory is empty")
	}
	if _, errVersion := parseSemver(version); errVersion != nil {
		return fmt.Errorf("write user panel high-water version: %w", errVersion)
	}
	if errMkdir := os.MkdirAll(paths.dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create user panel cache directory: %w", errMkdir)
	}
	if errChmod := os.Chmod(paths.dir, 0o700); errChmod != nil {
		return fmt.Errorf("secure user panel cache directory: %w", errChmod)
	}
	if errWrite := atomicWrite(paths.floor, []byte(strings.TrimSpace(version)+"\n"), "user-panel-floor-*", 0o600); errWrite != nil {
		return fmt.Errorf("write user panel high-water version: %w", errWrite)
	}
	return nil
}

func (m *Manager) fetchReleases(ctx context.Context, cfg panelConfig) ([]ReleaseInfo, error) {
	endpoint := strings.TrimSpace(m.releaseAPIOverride)
	if endpoint == "" {
		var errEndpoint error
		endpoint, errEndpoint = githubReleasesEndpoint(cfg.repository)
		if errEndpoint != nil {
			return nil, errEndpoint
		}
	}
	data, errFetch := m.fetchBytesWithAccept(ctx, endpoint, maxReleaseListSize, "application/vnd.github+json")
	if errFetch != nil {
		return nil, fmt.Errorf("fetch user panel releases: %w", errFetch)
	}
	var raw []struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
		Assets     []struct {
			Name       string `json:"name"`
			APIURL     string `json:"url"`
			BrowserURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if errDecode := json.Unmarshal(data, &raw); errDecode != nil {
		return nil, fmt.Errorf("decode user panel releases: %w", errDecode)
	}
	releases := make([]ReleaseInfo, 0, len(raw))
	for _, item := range raw {
		assets := make([]ReleaseAsset, 0, len(item.Assets))
		for _, asset := range item.Assets {
			assetURL := strings.TrimSpace(asset.APIURL)
			if assetURL == "" {
				assetURL = strings.TrimSpace(asset.BrowserURL)
			}
			assets = append(assets, ReleaseAsset{Name: asset.Name, URL: assetURL})
		}
		releases = append(releases, ReleaseInfo{TagName: item.TagName, Prerelease: item.Prerelease, Draft: item.Draft, Assets: assets})
	}
	return releases, nil
}

func (m *Manager) fetchAsset(ctx context.Context, assetURL string, limit int64) ([]byte, error) {
	return m.fetchBytesWithAccept(ctx, assetURL, limit, "application/octet-stream")
}

func (m *Manager) fetchBytes(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	return m.fetchBytesWithAccept(ctx, rawURL, limit, "application/vnd.github+json")
}

func (m *Manager) fetchBytesWithAccept(ctx context.Context, rawURL string, limit int64, accept string) ([]byte, error) {
	parsed, errParse := url.Parse(strings.TrimSpace(rawURL))
	if errParse != nil || parsed.Host == "" || parsed.Scheme != "https" {
		if !m.allowNonGitHubRemote {
			return nil, fmt.Errorf("reject unsafe user panel URL %q", rawURL)
		}
	}
	if !m.allowNonGitHubRemote && !allowedGitHubHost(parsed.Hostname()) {
		return nil, fmt.Errorf("reject non-GitHub user panel URL %q", rawURL)
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create user panel request: %w", errRequest)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userPanelUserAgent)
	if allowedGitHubHost(parsed.Hostname()) {
		token, errToken := githubToken()
		if errToken != nil {
			return nil, errToken
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		}
	}
	m.mu.RLock()
	client := m.httpClient
	m.mu.RUnlock()
	if client == nil {
		client = newHTTPClient("")
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request user panel URL: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("user panel: close response body")
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, fmt.Errorf("user panel URL returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if errRead != nil {
		return nil, fmt.Errorf("read user panel response: %w", errRead)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("user panel response exceeds %d bytes", limit)
	}
	return data, nil
}

func githubToken() (string, error) {
	if path := strings.TrimSpace(os.Getenv("USER_PANEL_GITHUB_TOKEN_FILE")); path != "" {
		token, errRead := readLimitedFile(filepath.Clean(path), maxGitHubTokenSize)
		if errRead != nil {
			return "", fmt.Errorf("read user panel GitHub credential: %w", errRead)
		}
		return strings.TrimSpace(string(token)), nil
	}
	return strings.TrimSpace(os.Getenv("USER_PANEL_GITHUB_TOKEN")), nil
}

func newHTTPClient(proxyURL string) *http.Client {
	client := &http.Client{}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many user panel redirects")
		}
		if req.URL == nil || (!isHTTPS(req.URL) && !isLocalTestURL(req.URL)) {
			return errors.New("reject insecure user panel redirect")
		}
		if !isLocalTestURL(req.URL) && !allowedGitHubHost(req.URL.Hostname()) {
			return errors.New("reject off-site user panel redirect")
		}
		return nil
	}
	if strings.TrimSpace(proxyURL) != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: strings.TrimSpace(proxyURL)}
		util.SetProxy(sdkCfg, client)
	}
	return client
}

func defaultCacheDir(configFilePath string) string {
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, "static")
	}
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		return ""
	}
	base := filepath.Dir(configFilePath)
	if info, errStat := os.Stat(configFilePath); errStat == nil && info.IsDir() {
		base = configFilePath
	}
	return filepath.Join(base, "static")
}

func devPath(cfg panelConfig) string {
	if !cfg.devMode {
		return ""
	}
	value := strings.TrimSpace(os.Getenv("USER_PANEL_STATIC_PATH"))
	if value == "" {
		return ""
	}
	cleaned := filepath.Clean(value)
	if strings.EqualFold(filepath.Base(cleaned), userPanelHTMLName) {
		return cleaned
	}
	return filepath.Join(cleaned, userPanelHTMLName)
}

func githubReleasesEndpoint(repository string) (string, error) {
	parsed, errParse := url.Parse(strings.TrimSpace(repository))
	if errParse != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", fmt.Errorf("user panel repository must be an HTTPS github.com URL")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid user panel GitHub repository URL")
	}
	owner := url.PathEscape(parts[0])
	repo := url.PathEscape(strings.TrimSuffix(parts[1], ".git"))
	return "https://api.github.com/repos/" + owner + "/" + repo + "/releases?per_page=100", nil
}

func allowedGitHubHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "github.com" || host == "api.github.com" || host == "objects.githubusercontent.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

func isHTTPS(parsed *url.URL) bool { return parsed != nil && strings.EqualFold(parsed.Scheme, "https") }

func isLocalTestURL(parsed *url.URL) bool {
	if parsed == nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && (host == "127.0.0.1" || host == "localhost" || host == "::1")
}

func readLimitedFile(path string, limit int64) ([]byte, error) {
	info, errStat := os.Lstat(path)
	if errStat != nil {
		return nil, errStat
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("refuse user panel symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("user panel cache entry is not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("user panel file exceeds %d bytes", limit)
	}
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return nil, errOpen
	}
	defer func() { _ = file.Close() }()
	data, errRead := io.ReadAll(io.LimitReader(file, limit+1))
	if errRead != nil {
		return nil, errRead
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("user panel file exceeds %d bytes", limit)
	}
	return data, nil
}

func atomicWrite(path string, data []byte, pattern string, mode os.FileMode) error {
	tmp, errCreate := os.CreateTemp(filepath.Dir(path), pattern)
	if errCreate != nil {
		return errCreate
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if errClose := tmp.Close(); errClose != nil {
			log.WithError(errClose).Debug("user panel: close temporary cache file")
		}
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if errChmod := tmp.Chmod(mode); errChmod != nil {
		return errChmod
	}
	if _, errWrite := tmp.Write(data); errWrite != nil {
		return errWrite
	}
	if errSync := tmp.Sync(); errSync != nil {
		return errSync
	}
	if errClose := tmp.Close(); errClose != nil {
		return errClose
	}
	if errRename := os.Rename(tmpName, path); errRename != nil {
		return errRename
	}
	directory, errOpenDir := os.Open(filepath.Dir(path))
	if errOpenDir != nil {
		return errOpenDir
	}
	if errSyncDir := directory.Sync(); errSyncDir != nil {
		_ = directory.Close()
		return errSyncDir
	}
	if errCloseDir := directory.Close(); errCloseDir != nil {
		return errCloseDir
	}
	cleanup = false
	return nil
}

func mustPublicKey(encoded string) ed25519.PublicKey {
	key, errDecode := hex.DecodeString(encoded)
	if errDecode != nil || len(key) != ed25519.PublicKeySize {
		panic("invalid compiled user panel public key")
	}
	return ed25519.PublicKey(key)
}

func clonePublicKeys(keys map[string]ed25519.PublicKey) map[string]ed25519.PublicKey {
	cloned := make(map[string]ed25519.PublicKey, len(keys))
	for id, key := range keys {
		if len(key) != ed25519.PublicKeySize {
			continue
		}
		cloned[strings.TrimSpace(id)] = append(ed25519.PublicKey(nil), key...)
	}
	return cloned
}

func decodeManifest(raw []byte) (Manifest, error) {
	var value map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if errDecode := decoder.Decode(&value); errDecode != nil || value == nil {
		if errDecode == nil {
			errDecode = errors.New("manifest must be a JSON object")
		}
		return Manifest{}, fmt.Errorf("decode manifest: %w", errDecode)
	}
	var extra any
	if errTrailing := decoder.Decode(&extra); !errors.Is(errTrailing, io.EOF) {
		return Manifest{}, errors.New("manifest has trailing JSON data")
	}
	getString := func(names ...string) string {
		for _, name := range names {
			if data, ok := value[name]; ok {
				var text string
				if json.Unmarshal(data, &text) == nil {
					return strings.TrimSpace(text)
				}
			}
		}
		return ""
	}
	var schemaVersion int
	if data, ok := value["schema_version"]; ok {
		if errSchema := json.Unmarshal(data, &schemaVersion); errSchema != nil {
			return Manifest{}, errors.New("manifest schema_version is not an integer")
		}
	}
	var size int64
	if data, ok := value["size"]; ok {
		var number json.Number
		if errNumber := json.Unmarshal(data, &number); errNumber != nil {
			return Manifest{}, errors.New("manifest size is not an integer")
		}
		parsed, errParse := strconv.ParseInt(string(number), 10, 64)
		if errParse != nil {
			return Manifest{}, errors.New("manifest size is not an integer")
		}
		size = parsed
	}
	var csp []string
	if data, ok := value["csp_hashes"]; ok {
		if errCSP := json.Unmarshal(data, &csp); errCSP != nil {
			return Manifest{}, fmt.Errorf("manifest csp_hashes: %w", errCSP)
		}
	} else if data, ok := value["csp-hashes"]; ok {
		if errCSP := json.Unmarshal(data, &csp); errCSP != nil {
			return Manifest{}, fmt.Errorf("manifest csp-hashes: %w", errCSP)
		}
	}
	var runtimeDependencies []string
	if data, ok := value["runtime_dependencies"]; ok {
		if errDependencies := json.Unmarshal(data, &runtimeDependencies); errDependencies != nil {
			return Manifest{}, fmt.Errorf("manifest runtime_dependencies: %w", errDependencies)
		}
	}
	return Manifest{
		SchemaVersion:       schemaVersion,
		Version:             strings.TrimSpace(getString("version")),
		KeyID:               strings.TrimSpace(getString("key_id", "key-id", "keyId")),
		Artifact:            strings.TrimSpace(getString("artifact")),
		Size:                size,
		SHA256:              strings.TrimSpace(getString("sha256", "sha-256")),
		CSPHashes:           csp,
		RuntimeDependencies: runtimeDependencies,
	}, nil
}

func decodeSignature(raw []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(raw))
	if len(raw) == ed25519.SignatureSize {
		return append([]byte(nil), raw...), nil
	}
	for _, decoder := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, errDecode := decoder.DecodeString(trimmed)
		if errDecode == nil && len(decoded) == ed25519.SignatureSize {
			return decoded, nil
		}
	}
	if decoded, errDecode := hex.DecodeString(trimmed); errDecode == nil && len(decoded) == ed25519.SignatureSize {
		return decoded, nil
	}
	return nil, errors.New("manifest signature is not a valid Ed25519 signature")
}

func decodeSHA256(value string) ([]byte, error) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(value), "sha256:"))
	decoded, errDecode := hex.DecodeString(value)
	if errDecode != nil || len(decoded) != sha256.Size {
		return nil, errors.New("manifest SHA-256 is invalid")
	}
	return decoded, nil
}

func normalizeCSPHashes(hashes []string) ([]string, error) {
	if len(hashes) == 0 {
		return nil, errors.New("manifest csp_hashes is empty")
	}
	out := make([]string, 0, len(hashes))
	seen := make(map[string]struct{}, len(hashes))
	for _, raw := range hashes {
		hash := strings.Trim(strings.TrimSpace(raw), "'")
		if !strings.HasPrefix(hash, "sha256-") {
			return nil, fmt.Errorf("manifest CSP hash %q is not sha256", raw)
		}
		decoded, errDecode := base64.StdEncoding.DecodeString(strings.TrimPrefix(hash, "sha256-"))
		if errDecode != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("manifest CSP hash %q is invalid", raw)
		}
		if _, exists := seen[hash]; exists {
			continue
		}
		seen[hash] = struct{}{}
		out = append(out, hash)
	}
	if len(out) == 0 {
		return nil, errors.New("manifest csp_hashes is empty")
	}
	return out, nil
}

func calculateCSPHashes(html []byte) []string {
	text := string(html)
	var hashes []string
	for _, tag := range []string{"style", "script"} {
		lower := strings.ToLower(text)
		start := 0
		for {
			open := strings.Index(lower[start:], "<"+tag)
			if open < 0 {
				break
			}
			open += start
			closeOpen := strings.Index(lower[open:], ">")
			if closeOpen < 0 {
				break
			}
			contentStart := open + closeOpen + 1
			close := strings.Index(lower[contentStart:], "</"+tag+">")
			if close < 0 {
				break
			}
			contentEnd := contentStart + close
			sum := sha256.Sum256([]byte(text[contentStart:contentEnd]))
			hashes = append(hashes, "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
			start = contentEnd + len(tag) + 3
		}
	}
	return deduplicateStrings(hashes)
}

func deduplicateStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	values := make(map[string]struct{}, len(left))
	for _, value := range left {
		values[value] = struct{}{}
	}
	for _, value := range right {
		if _, ok := values[value]; !ok {
			return false
		}
	}
	return true
}

func cspHeader(hashes []string) string {
	quoted := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		if strings.HasPrefix(hash, "sha256-") {
			quoted = append(quoted, "'"+hash+"'")
		}
	}
	if len(quoted) == 0 {
		quoted = []string{"'none'"}
	}
	return "default-src 'none'; base-uri 'none'; connect-src 'self'; frame-ancestors 'none'; img-src 'self' data:; form-action 'none'; script-src " + strings.Join(quoted, " ") + "; style-src " + strings.Join(quoted, " ")
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var value byte
	for i := range left {
		value |= left[i] ^ right[i]
	}
	return value == 0
}
