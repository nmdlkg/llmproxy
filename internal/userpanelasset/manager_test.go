package userpanelasset

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, errGenerate := ed25519.GenerateKey(rand.Reader)
	if errGenerate != nil {
		t.Fatalf("GenerateKey() error = %v", errGenerate)
	}
	return public, private
}

func signedBundle(t *testing.T, private ed25519.PrivateKey, version string, html []byte) ([]byte, []byte, []byte) {
	t.Helper()
	digest := sha256.Sum256(html)
	manifest := Manifest{
		SchemaVersion: 1,
		Version:       version,
		KeyID:         "test",
		Artifact:      userPanelHTMLName,
		Size:          int64(len(html)),
		SHA256:        hex.EncodeToString(digest[:]),
		CSPHashes:     calculateCSPHashes(html),
	}
	manifestRaw, errMarshal := json.Marshal(manifest)
	if errMarshal != nil {
		t.Fatalf("Marshal(manifest) error = %v", errMarshal)
	}
	signature := ed25519.Sign(private, manifestRaw)
	return html, manifestRaw, []byte(base64.StdEncoding.EncodeToString(signature))
}

func TestManagerServesVerifiedCacheAndReturnsUnavailableWithoutAsset(t *testing.T) {
	public, private := testKey(t)
	cacheDir := t.TempDir()
	m := NewManager("config.yaml", WithCacheDir(cacheDir), WithTrustedPublicKeys(map[string]ed25519.PublicKey{"test": public}))
	m.SetConfig(&config.Config{Tenancy: config.TenancyConfig{UserPanel: config.UserPanelConfig{}}})
	html, manifest, signature := signedBundle(t, private, "1.1.0", []byte("<html><style>cache</style><script>cache</script></html>"))
	if errWrite := m.writeCache(html, manifest, signature); errWrite != nil {
		t.Fatalf("writeCache() error = %v", errWrite)
	}
	response := httptest.NewRecorder()
	m.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/user", nil))
	if response.Code != http.StatusOK || response.Body.String() != string(html) {
		t.Fatalf("verified cache response = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("X-User-Panel-Source") != string(SourceCache) {
		t.Fatalf("source = %q, want %q", response.Header().Get("X-User-Panel-Source"), SourceCache)
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "sha256-") {
		t.Fatalf("CSP header missing signed hashes: %q", response.Header().Get("Content-Security-Policy"))
	}
	if response.Header().Get("Referrer-Policy") != "no-referrer" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("security headers missing: %#v", response.Header())
	}
	if errRemove := os.Remove(filepath.Join(cacheDir, userPanelManifestName)); errRemove != nil {
		t.Fatalf("remove manifest: %v", errRemove)
	}
	fallback := httptest.NewRecorder()
	m.ServeHTTP(fallback, httptest.NewRequest(http.MethodGet, "/user", nil))
	if fallback.Code != http.StatusNotFound {
		t.Fatalf("fallback response = %d %q", fallback.Code, fallback.Body.String())
	}
}

func TestHighWaterFloorRejectsOlderSignedCacheAndSecuresDirectory(t *testing.T) {
	public, private := testKey(t)
	cacheDir := t.TempDir()
	m := NewManager("config.yaml", WithCacheDir(cacheDir), WithTrustedPublicKeys(map[string]ed25519.PublicKey{"test": public}))
	m.SetConfig(&config.Config{Tenancy: config.TenancyConfig{UserPanel: config.UserPanelConfig{}}})

	newHTML, newManifest, newSignature := signedBundle(t, private, "2.0.0", []byte("<style>new</style><script>new</script>"))
	if errWrite := m.writeCache(newHTML, newManifest, newSignature); errWrite != nil {
		t.Fatalf("write new cache: %v", errWrite)
	}
	if errFloor := m.writeVersionFloor("2.0.0"); errFloor != nil {
		t.Fatalf("write high-water floor: %v", errFloor)
	}
	oldHTML, oldManifest, oldSignature := signedBundle(t, private, "1.5.0", []byte("<style>old</style><script>old</script>"))
	if errWrite := m.writeCache(oldHTML, oldManifest, oldSignature); errWrite != nil {
		t.Fatalf("write older cache fixture: %v", errWrite)
	}
	if _, errCache := m.loadCache(); errCache == nil || !strings.Contains(errCache.Error(), "high-water floor") {
		t.Fatalf("older cache error = %v, want high-water rejection", errCache)
	}

	dirInfo, errDir := os.Stat(cacheDir)
	if errDir != nil {
		t.Fatalf("stat cache directory: %v", errDir)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory mode = %v, want 0700", dirInfo.Mode().Perm())
	}
	floorInfo, errFloorInfo := os.Stat(filepath.Join(cacheDir, userPanelFloorName))
	if errFloorInfo != nil {
		t.Fatalf("stat high-water floor: %v", errFloorInfo)
	}
	if floorInfo.Mode().Perm() != 0o600 {
		t.Fatalf("floor mode = %v, want 0600", floorInfo.Mode().Perm())
	}
	response := httptest.NewRecorder()
	m.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/user", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("rollback cache was served: status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCompiledPublisherTrustFixture(t *testing.T) {
	html, errHTML := os.ReadFile(filepath.Join("testdata", userPanelHTMLName))
	if errHTML != nil {
		t.Fatalf("read trusted HTML fixture: %v", errHTML)
	}
	manifest, errManifest := os.ReadFile(filepath.Join("testdata", userPanelManifestName))
	if errManifest != nil {
		t.Fatalf("read trusted manifest fixture: %v", errManifest)
	}
	signature, errSignature := os.ReadFile(filepath.Join("testdata", userPanelSignatureName))
	if errSignature != nil {
		t.Fatalf("read trusted signature fixture: %v", errSignature)
	}
	manager := NewManager("config.yaml")
	if _, errVerify := manager.verifyBundle(html, manifest, signature); errVerify != nil {
		t.Fatalf("compiled publisher fixture was rejected: %v", errVerify)
	}

	tampered := append([]byte(nil), html...)
	tampered[len(tampered)-1] ^= 1
	if _, errVerify := manager.verifyBundle(tampered, manifest, signature); errVerify == nil {
		t.Fatal("tampered publisher fixture was accepted")
	}

	_, wrongPrivate := testKey(t)
	wrongSignature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(wrongPrivate, manifest)))
	if _, errVerify := manager.verifyBundle(html, manifest, wrongSignature); errVerify == nil {
		t.Fatal("manifest signed by an untrusted key was accepted")
	}
}

func TestVerifyBundleRejectsIncorrectCSPHashesAndUnknownKeyID(t *testing.T) {
	public, private := testKey(t)
	html := []byte("<html><style>trusted</style><script>trusted</script></html>")
	digest := sha256.Sum256(html)
	manifest := Manifest{
		SchemaVersion: 1,
		Version:       "1.0.0",
		KeyID:         "test",
		Artifact:      userPanelHTMLName,
		Size:          int64(len(html)),
		SHA256:        hex.EncodeToString(digest[:]),
		CSPHashes:     calculateCSPHashes([]byte("<style>different</style><script>different</script>")),
	}
	manifestRaw, errMarshal := json.Marshal(manifest)
	if errMarshal != nil {
		t.Fatalf("marshal incorrect-CSP manifest: %v", errMarshal)
	}
	signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(private, manifestRaw)))
	manager := NewManager("config.yaml", WithTrustedPublicKeys(map[string]ed25519.PublicKey{"test": public}))
	if _, errVerify := manager.verifyBundle(html, manifestRaw, signature); errVerify == nil || !strings.Contains(errVerify.Error(), "CSP hashes") {
		t.Fatalf("incorrect CSP verification error = %v", errVerify)
	}

	manifest.KeyID = "unknown"
	manifest.CSPHashes = calculateCSPHashes(html)
	manifestRaw, errMarshal = json.Marshal(manifest)
	if errMarshal != nil {
		t.Fatalf("marshal unknown-key manifest: %v", errMarshal)
	}
	signature = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(private, manifestRaw)))
	if _, errVerify := manager.verifyBundle(html, manifestRaw, signature); errVerify == nil || !strings.Contains(errVerify.Error(), "unknown trusted key") {
		t.Fatalf("unknown key verification error = %v", errVerify)
	}
}

func TestSelectReleasePrefersStableAndSupportsPinnedPrerelease(t *testing.T) {
	releases := []ReleaseInfo{
		{TagName: "v2.0.0-rc.1", Prerelease: true},
		{TagName: "v1.5.0", Prerelease: false},
		{TagName: "v1.4.0", Prerelease: false},
	}
	selected, errSelect := selectRelease(releases, "")
	if errSelect != nil || selected.TagName != "v1.5.0" {
		t.Fatalf("selected = %#v, error = %v", selected, errSelect)
	}
	selected, errSelect = selectRelease(releases, "2.0.0-rc.1")
	if errSelect != nil || selected.TagName != "v2.0.0-rc.1" {
		t.Fatalf("pinned selected = %#v, error = %v", selected, errSelect)
	}
}

func TestSelectReleaseAcceptsTwoComponentTagAsPatchZero(t *testing.T) {
	releases := []ReleaseInfo{{TagName: "v0.1"}, {TagName: "v0.0.9"}}
	selected, errSelect := selectRelease(releases, "")
	if errSelect != nil || selected.TagName != "v0.1" {
		t.Fatalf("selected = %#v, error = %v, want v0.1", selected, errSelect)
	}
	if compareSemverString("0.1", "0.1.0") != 0 {
		t.Fatal("two-component version was not normalized to patch zero")
	}
}

func TestManagerManualRefreshRequiresDevModeAndHasFloor(t *testing.T) {
	public, _ := testKey(t)
	now := time.Now()
	clock := now
	// The refresh gate is tested without making a network request; a live
	// development config is enough to exercise the policy and clock floor.
	m := NewManager("config.yaml", WithTrustedPublicKeys(map[string]ed25519.PublicKey{"test": public}), WithNow(func() time.Time { return clock }))
	m.SetConfig(&config.Config{Tenancy: config.TenancyConfig{UserPanel: config.UserPanelConfig{GitHubRepository: "bad", DevMode: false}}})
	if errRefresh := m.Refresh(nil); errRefresh != ErrManualRefreshRequiresDevMode {
		t.Fatalf("Refresh() error = %v, want dev-mode gate", errRefresh)
	}
	m.SetConfig(&config.Config{Tenancy: config.TenancyConfig{UserPanel: config.UserPanelConfig{GitHubRepository: "bad", DevMode: true}}})
	// A first attempt records the manual floor even when the remote endpoint is
	// unavailable, so an immediate second attempt is throttled.
	_ = m.Refresh(nil)
	clock = now.Add(time.Second)
	if errRefresh := m.Refresh(nil); !errors.Is(errRefresh, ErrRefreshThrottled) {
		t.Fatalf("second Refresh() error = %v, want throttle", errRefresh)
	}
}

func TestEnvironmentCanEnableLocalDevelopmentPanel(t *testing.T) {
	panelPath := filepath.Join(t.TempDir(), userPanelHTMLName)
	if errWrite := os.WriteFile(panelPath, []byte("<style>dev</style><script>dev</script>"), 0o600); errWrite != nil {
		t.Fatalf("write development panel: %v", errWrite)
	}
	t.Setenv("USER_PANEL_DEV_MODE", "true")
	t.Setenv("USER_PANEL_STATIC_PATH", panelPath)
	m := NewManager("config.yaml", WithCacheDir(t.TempDir()))
	m.SetConfig(&config.Config{Tenancy: config.TenancyConfig{Enabled: true}})
	response := httptest.NewRecorder()
	m.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/user", nil))
	if response.Body.String() != "<style>dev</style><script>dev</script>" || response.Header().Get("X-User-Panel-Source") != string(SourceDev) {
		t.Fatalf("environment dev response = %q source=%q", response.Body.String(), response.Header().Get("X-User-Panel-Source"))
	}
}

func TestCSPHashesAreDerivedFromSeparateInlineAssets(t *testing.T) {
	html := []byte("<style>one</style><script>two</script>")
	hashes := calculateCSPHashes(html)
	if len(hashes) != 2 {
		t.Fatalf("CSP hashes = %#v, want style and script hashes", hashes)
	}
	if bytes.Equal([]byte(hashes[0]), []byte(hashes[1])) {
		t.Fatal("style and script hashes unexpectedly equal")
	}
}
