package user

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/authfiles"
	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	providerQuotaCacheTTL       = 30 * time.Second
	providerQuotaParallel       = 4
	providerQuotaMaxCredentials = 32
	providerQuotaBodyLimit      = 1 << 20

	claudeProviderQuotaURL = "https://api.anthropic.com/api/oauth/usage"
	codexProviderQuotaURL  = "https://chatgpt.com/backend-api/wham/usage"
)

var (
	errProviderQuotaUnsupported = errors.New("provider quota is not supported")
	errProviderQuotaNoToken     = errors.New("provider quota credential has no access token")
	errProviderQuotaBusy        = errors.New("provider quota fetch budget is busy")

	// fetchProviderQuota is replaceable in package tests. The production
	// implementation only calls the fixed provider endpoints below.
	fetchProviderQuota = fetchProviderQuotaHTTP
)

// providerQuotaCacheEntry stores only normalized, non-secret data. Keeping the
// cache at the handler level avoids repeating upstream calls during a dashboard
// refresh while preserving ownership checks on every request.
type providerQuotaCacheEntry struct {
	expiresAt time.Time
	account   providerQuotaAccount
}

type providerQuotaAccount struct {
	Label     string                `json:"label"`
	Provider  string                `json:"provider"`
	Status    string                `json:"status"`
	Error     string                `json:"error,omitempty"`
	Windows   []providerQuotaWindow `json:"windows"`
	FetchedAt time.Time             `json:"fetched_at,omitempty"`
}

type providerQuotaWindow struct {
	Name             string     `json:"name"`
	WindowSeconds    int64      `json:"window_seconds,omitempty"`
	UsedPercent      *float64   `json:"used_percent,omitempty"`
	RemainingPercent *float64   `json:"remaining_percent,omitempty"`
	ResetAt          *time.Time `json:"reset_at,omitempty"`
}

// GetProviderQuotas returns live quota windows for the authenticated user's
// owned OAuth credentials. It intentionally has no credential selector: the
// ownership filter is derived from the authenticated user and is applied
// before any provider request is made.
func (h *Handler) GetProviderQuotas(c *gin.Context) {
	user, _ := currentUser(c)
	accounts, truncated := h.providerQuotaAccounts(c.Request.Context(), user.ID)
	c.JSON(http.StatusOK, gin.H{
		"schema_version": 1,
		"accounts":       accounts,
		"truncated":      truncated,
	})
}

func (h *Handler) providerQuotaAccounts(ctx context.Context, userID string) ([]providerQuotaAccount, bool) {
	if h == nil || h.authManager == nil {
		return []providerQuotaAccount{}, false
	}
	auths := make([]*coreauth.Auth, 0)
	for _, auth := range h.authManager.List() {
		if auth == nil || authfiles.OwnerUserID(auth) != userID {
			continue
		}
		auths = append(auths, auth)
	}
	sort.Slice(auths, func(i, j int) bool {
		return strings.ToLower(auths[i].ID) < strings.ToLower(auths[j].ID)
	})
	truncated := len(auths) > providerQuotaMaxCredentials
	if truncated {
		auths = auths[:providerQuotaMaxCredentials]
	}

	accounts := make([]providerQuotaAccount, len(auths))
	var workers sync.WaitGroup
	for index, auth := range auths {
		base := providerQuotaAccountForAuth(auth)
		cacheKey := providerQuotaCacheKey(userID, auth)
		if cached, ok := h.cachedProviderQuota(cacheKey); ok {
			accounts[index] = cached
			continue
		}
		if base.Status != "pending" {
			accounts[index] = base
			h.cacheProviderQuota(cacheKey, base)
			continue
		}

		workers.Add(1)
		go func(index int, auth *coreauth.Auth, base providerQuotaAccount, cacheKey string) {
			defer workers.Done()
			value, errFetch, _ := h.quotaFetches.Do(cacheKey, func() (any, error) {
				slots := h.providerQuotaFetchSlots()
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				default:
					return nil, errProviderQuotaBusy
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				default:
				}
				return fetchProviderQuota(ctx, h.cfg, auth)
			})
			if errFetch != nil {
				base.Status = "unavailable"
				base.Error = providerQuotaErrorCode(errFetch)
				accounts[index] = base
				if !providerQuotaTransientError(errFetch) {
					h.cacheProviderQuota(cacheKey, base)
				}
				return
			}
			windows, ok := value.([]providerQuotaWindow)
			if !ok {
				base.Status = "unavailable"
				base.Error = "upstream_unavailable"
				accounts[index] = base
				h.cacheProviderQuota(cacheKey, base)
				return
			}
			base.Status = "ok"
			base.Windows = windows
			base.FetchedAt = time.Now().UTC()
			accounts[index] = base
			h.cacheProviderQuota(cacheKey, base)
		}(index, auth, base, cacheKey)
	}
	workers.Wait()
	return accounts, truncated
}

func providerQuotaTransientError(err error) bool {
	return errors.Is(err, errProviderQuotaBusy) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (h *Handler) providerQuotaFetchSlots() chan struct{} {
	if h == nil {
		return make(chan struct{}, providerQuotaParallel)
	}
	h.quotaSlotMu.Lock()
	defer h.quotaSlotMu.Unlock()
	if h.quotaFetchSlots == nil {
		h.quotaFetchSlots = make(chan struct{}, providerQuotaParallel)
	}
	return h.quotaFetchSlots
}

func providerQuotaAccountForAuth(auth *coreauth.Auth) providerQuotaAccount {
	account := providerQuotaAccount{
		Label:    providerQuotaLabel(auth),
		Provider: strings.ToLower(strings.TrimSpace(auth.Provider)),
		Status:   "unsupported",
		Windows:  []providerQuotaWindow{},
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		account.Status = "disabled"
		account.Error = "credential_disabled"
		return account
	}
	if account.Provider != "claude" && account.Provider != "codex" {
		account.Error = "provider_not_supported"
		return account
	}
	if auth.AuthKind() != coreauth.AuthKindOAuth {
		account.Error = "oauth_credential_required"
		return account
	}
	if providerQuotaAccessToken(auth) == "" {
		account.Status = "unavailable"
		account.Error = "missing_access_token"
		return account
	}
	account.Status = "pending"
	return account
}

func providerQuotaCacheKey(userID string, auth *coreauth.Auth) string {
	if auth == nil {
		return strings.TrimSpace(userID)
	}
	tokenHash := sha256.Sum256([]byte(providerQuotaAccessToken(auth)))
	return strings.Join([]string{
		strings.TrimSpace(userID),
		strings.TrimSpace(auth.ID),
		strings.ToLower(strings.TrimSpace(auth.Provider)),
		fmt.Sprintf("%x", tokenHash[:]),
	}, "\x00")
}

func (h *Handler) cachedProviderQuota(cacheKey string) (providerQuotaAccount, bool) {
	if h == nil {
		return providerQuotaAccount{}, false
	}
	h.quotaMu.Lock()
	defer h.quotaMu.Unlock()
	h.pruneProviderQuotaCacheLocked(time.Now())
	entry, ok := h.quotaCache[strings.TrimSpace(cacheKey)]
	if !ok || !time.Now().Before(entry.expiresAt) {
		if ok {
			delete(h.quotaCache, strings.TrimSpace(cacheKey))
		}
		return providerQuotaAccount{}, false
	}
	return cloneProviderQuotaAccount(entry.account), true
}

func (h *Handler) cacheProviderQuota(cacheKey string, account providerQuotaAccount) {
	if h == nil || strings.TrimSpace(cacheKey) == "" {
		return
	}
	h.quotaMu.Lock()
	defer h.quotaMu.Unlock()
	h.pruneProviderQuotaCacheLocked(time.Now())
	if h.quotaCache == nil {
		h.quotaCache = make(map[string]providerQuotaCacheEntry)
	}
	h.quotaCache[strings.TrimSpace(cacheKey)] = providerQuotaCacheEntry{
		expiresAt: time.Now().Add(providerQuotaCacheTTL),
		account:   cloneProviderQuotaAccount(account),
	}
}

func (h *Handler) pruneProviderQuotaCacheLocked(now time.Time) {
	if h == nil || len(h.quotaCache) == 0 {
		return
	}
	for key, entry := range h.quotaCache {
		if !now.Before(entry.expiresAt) {
			delete(h.quotaCache, key)
		}
	}
}

func cloneProviderQuotaAccount(account providerQuotaAccount) providerQuotaAccount {
	clone := account
	clone.Windows = append([]providerQuotaWindow(nil), account.Windows...)
	return clone
}

func providerQuotaErrorCode(err error) string {
	switch {
	case errors.Is(err, errProviderQuotaUnsupported):
		return "provider_not_supported"
	case errors.Is(err, errProviderQuotaNoToken):
		return "missing_access_token"
	case errors.Is(err, errProviderQuotaBusy):
		return "busy"
	case errors.Is(err, context.Canceled):
		return "request_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "request_deadline_exceeded"
	default:
		return "upstream_unavailable"
	}
}

func providerQuotaLabel(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if email, ok := auth.Metadata["email"].(string); ok {
			if email = strings.TrimSpace(email); email != "" {
				return email
			}
		}
	}
	if auth.FileName != "" {
		return auth.FileName
	}
	return auth.Provider
}

func providerQuotaAccessToken(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		for _, key := range []string{"access_token", "accessToken"} {
			if token, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(token) != "" {
				return strings.TrimSpace(token)
			}
		}
		if nested, ok := auth.Metadata["token"].(map[string]any); ok {
			for _, key := range []string{"access_token", "accessToken"} {
				if token, ok := nested[key].(string); ok && strings.TrimSpace(token) != "" {
					return strings.TrimSpace(token)
				}
			}
		}
		if nested, ok := auth.Metadata["token"].(map[string]string); ok {
			for _, key := range []string{"access_token", "accessToken"} {
				if token := strings.TrimSpace(nested[key]); token != "" {
					return token
				}
			}
		}
	}
	return ""
}

func fetchProviderQuotaHTTP(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]providerQuotaWindow, error) {
	if auth == nil {
		return nil, errProviderQuotaUnsupported
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	var endpoint string
	switch provider {
	case "claude":
		endpoint = claudeProviderQuotaURL
	case "codex":
		endpoint = codexProviderQuotaURL
	default:
		return nil, errProviderQuotaUnsupported
	}
	token := providerQuotaAccessToken(auth)
	if token == "" {
		return nil, errProviderQuotaNoToken
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if provider == "claude" {
		request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	} else {
		request.Header.Set("Originator", "codex_cli_rs")
		request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
		if accountID := providerQuotaAccountID(auth); accountID != "" {
			request.Header.Set("Chatgpt-Account-Id", accountID)
		}
	}
	client := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	// Do not send the bearer token to a redirected host or an open redirect.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, errDo := client.Do(request)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("provider quota upstream status %d", response.StatusCode)
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, providerQuotaBodyLimit+1))
	if errRead != nil {
		return nil, errRead
	}
	if len(body) > providerQuotaBodyLimit {
		return nil, errors.New("provider quota response too large")
	}
	if provider == "claude" {
		return parseClaudeProviderQuota(body)
	}
	return parseCodexProviderQuotaAt(body, time.Now().UTC())
}

func providerQuotaAccountID(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
		return strings.TrimSpace(accountID)
	}
	if token, ok := auth.Metadata["id_token"].(string); ok && strings.TrimSpace(token) != "" {
		claims, errParse := codexauth.ParseJWTToken(strings.TrimSpace(token))
		if errParse == nil && claims != nil {
			return strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID)
		}
	}
	return ""
}

func parseClaudeProviderQuota(body []byte) ([]providerQuotaWindow, error) {
	var root map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	windows := make([]providerQuotaWindow, 0, 2)
	for _, candidate := range []struct {
		key     string
		name    string
		seconds int64
	}{
		{key: "five_hour", name: "5h", seconds: int64(5 * time.Hour / time.Second)},
		{key: "seven_day", name: "weekly", seconds: int64(7 * 24 * time.Hour / time.Second)},
	} {
		raw, ok := root[candidate.key]
		if !ok || string(raw) == "null" {
			continue
		}
		var values map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(raw, &values); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		window := providerQuotaWindow{Name: candidate.name, WindowSeconds: candidate.seconds}
		if value, ok := quotaPercent(values["utilization"]); ok {
			window.UsedPercent = float64Pointer(value)
			remaining := 100 - value
			window.RemainingPercent = float64Pointer(remaining)
		}
		if reset, ok := quotaTime(values["resets_at"]); ok {
			window.ResetAt = timePointer(reset)
		}
		windows = append(windows, window)
	}
	if len(windows) == 0 {
		return nil, errors.New("provider quota response has no supported windows")
	}
	return windows, nil
}

func parseCodexProviderQuotaAt(body []byte, now time.Time) ([]providerQuotaWindow, error) {
	var root map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	rateLimit := root
	if raw, ok := root["rate_limit"]; ok {
		if errUnmarshal := json.Unmarshal(raw, &rateLimit); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	windows := make([]providerQuotaWindow, 0, 2)
	for _, key := range []string{"primary_window", "secondary_window"} {
		raw, ok := rateLimit[key]
		if !ok || string(raw) == "null" {
			continue
		}
		var values map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(raw, &values); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		seconds := quotaInt64(values["limit_window_seconds"])
		if seconds <= 0 {
			seconds = quotaInt64(values["window_seconds"])
		}
		if seconds <= 0 {
			seconds = quotaInt64(values["window_minutes"]) * 60
		}
		name := codexQuotaWindowName(key, seconds)
		window := providerQuotaWindow{Name: name, WindowSeconds: seconds}
		if value, ok := quotaPercent(values["used_percent"]); ok {
			window.UsedPercent = float64Pointer(value)
			window.RemainingPercent = float64Pointer(100 - value)
		}
		if reset, ok := quotaTime(values["reset_at"]); ok {
			window.ResetAt = timePointer(reset)
		} else if after := quotaInt64(values["reset_after_seconds"]); after > 0 {
			reset := now.Add(time.Duration(after) * time.Second).UTC()
			window.ResetAt = timePointer(reset)
		}
		windows = append(windows, window)
	}
	if len(windows) == 0 {
		return nil, errors.New("provider quota response has no supported windows")
	}
	return windows, nil
}

func codexQuotaWindowName(key string, seconds int64) string {
	switch {
	case seconds >= int64(5*24*60*60):
		return "weekly"
	case seconds >= int64(4*60*60) && seconds <= int64(6*60*60):
		return "5h"
	case key == "primary_window":
		return "5h"
	default:
		return "weekly"
	}
}

func quotaPercent(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var value float64
	if json.Unmarshal(raw, &value) == nil {
		return validQuotaPercent(value)
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return 0, false
	}
	text = strings.TrimSpace(text)
	if strings.HasSuffix(text, "%") {
		text = strings.TrimSpace(strings.TrimSuffix(text, "%"))
	}
	if text == "" {
		return 0, false
	}
	parsed, errParse := strconv.ParseFloat(text, 64)
	if errParse != nil {
		return 0, false
	}
	return validQuotaPercent(parsed)
}

func validQuotaPercent(value float64) (float64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
		return 0, false
	}
	return value, true
}

func quotaInt64(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var value int64
	if json.Unmarshal(raw, &value) == nil && value > 0 {
		return value
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, errParse := strconv.ParseInt(strings.TrimSpace(text), 10, 64); errParse == nil && parsed > 0 {
			return parsed
		}
	}
	var floatValue float64
	if json.Unmarshal(raw, &floatValue) == nil && floatValue > 0 && floatValue <= math.MaxInt64 {
		return int64(floatValue)
	}
	return 0
}

func quotaTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return time.Time{}, false
		}
		if parsed, errParse := time.Parse(time.RFC3339Nano, text); errParse == nil {
			return validQuotaTime(parsed.UTC())
		}
		if seconds, ok := parseQuotaUnix(text); ok {
			return seconds, true
		}
		return time.Time{}, false
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil && number > 0 && !math.IsNaN(number) && !math.IsInf(number, 0) {
		return quotaUnixTime(number)
	}
	return time.Time{}, false
}

func parseQuotaUnix(value string) (time.Time, bool) {
	number, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if errParse != nil || number <= 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return time.Time{}, false
	}
	return quotaUnixTime(number)
}

func quotaUnixTime(number float64) (time.Time, bool) {
	if number > 1e12 {
		number /= 1000
	}
	minimum := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	maximum := time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)
	minimumUnix := float64(minimum.Unix())
	maximumUnix := float64(maximum.Unix())
	if number < minimumUnix || number >= maximumUnix {
		return time.Time{}, false
	}
	return validQuotaTime(time.Unix(int64(number), 0).UTC())
}

func validQuotaTime(value time.Time) (time.Time, bool) {
	minimum := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	maximum := time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)
	if value.Before(minimum) || !value.Before(maximum) {
		return time.Time{}, false
	}
	return value.UTC(), true
}

func float64Pointer(value float64) *float64 { return &value }

func timePointer(value time.Time) *time.Time { return &value }
