package user

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) GetMe(c *gin.Context) {
	user, _ := currentUser(c)
	quota, errQuota := h.quotaResponse(c.Request.Context(), user.ID)
	if errQuota != nil {
		log.WithError(errQuota).WithField("user_id", user.ID).Error("user profile: load quota")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load quota"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":           user.ID,
		"email":        user.Email,
		"display_name": user.DisplayName,
		"role":         user.Role,
		"tier":         user.Tier,
		"quota":        quota,
	})
}

func (h *Handler) GetUsage(c *gin.Context) {
	user, _ := currentUser(c)
	ctx := c.Request.Context()
	quota, errQuota := h.quotaResponse(ctx, user.ID)
	if errQuota != nil {
		log.WithError(errQuota).WithField("user_id", user.ID).Error("user usage: load rollup")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load usage"})
		return
	}
	window := quotaWindow(h.cfg)
	until := time.Now().UTC()
	since := until.Add(-window)
	models, errModels := h.store().UsageByModel(ctx, user.ID, since, until)
	if errModels != nil {
		log.WithError(errModels).WithField("user_id", user.ID).Error("user usage: load model breakdown")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load usage"})
		return
	}
	days, errDays := h.store().UsageByDay(ctx, user.ID, since, until)
	if errDays != nil {
		log.WithError(errDays).WithField("user_id", user.ID).Error("user usage: load daily trend")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load usage"})
		return
	}
	modelItems := make([]gin.H, 0, len(models))
	var attempts, failedAttempts int64
	for _, stat := range models {
		attempts += stat.Attempts
		failedAttempts += stat.FailedAttempts
		modelItems = append(modelItems, gin.H{
			"provider":        stat.Provider,
			"model":           stat.Model,
			"cost":            tenancy.FormatNanoUSD(stat.CostNanoUSD),
			"input_tokens":    stat.InputTokens,
			"output_tokens":   stat.OutputTokens,
			"attempts":        stat.Attempts,
			"failed_attempts": stat.FailedAttempts,
		})
	}
	dailyItems := make([]gin.H, 0, len(days))
	for _, stat := range days {
		dailyItems = append(dailyItems, gin.H{
			"day":             stat.Day.Format("2006-01-02"),
			"cost":            tenancy.FormatNanoUSD(stat.CostNanoUSD),
			"input_tokens":    stat.InputTokens,
			"output_tokens":   stat.OutputTokens,
			"attempts":        stat.Attempts,
			"failed_attempts": stat.FailedAttempts,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"usage":  quota,
		"models": modelItems,
		"daily":  dailyItems,
		"totals": gin.H{
			"attempts":        attempts,
			"failed_attempts": failedAttempts,
		},
	})
}

func (h *Handler) ListAPIKeys(c *gin.Context) {
	user, _ := currentUser(c)
	keys, errList := h.store().ListAPIKeys(user.ID)
	if errList != nil {
		log.WithError(errList).WithField("user_id", user.ID).Error("user api keys: list failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list API keys"})
		return
	}
	items := make([]gin.H, 0, len(keys))
	for _, key := range keys {
		items = append(items, apiKeyResponse(key))
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": items})
}

func (h *Handler) IssueAPIKey(c *gin.Context) {
	user, _ := currentUser(c)
	var request struct {
		Label string `json:"label"`
	}
	if c.Request.ContentLength != 0 {
		if errBind := c.ShouldBindJSON(&request); errBind != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}
	plaintext, key, errIssue := h.store().IssueAPIKey(user.ID, request.Label)
	if errIssue != nil {
		log.WithError(errIssue).WithField("user_id", user.ID).Error("user api keys: issue failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue API key"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"api_key": plaintext,
		"key":     apiKeyResponse(*key),
	})
}

func (h *Handler) DeleteAPIKey(c *gin.Context) {
	user, _ := currentUser(c)
	hash := strings.TrimSpace(c.Query("hash"))
	if hash == "" {
		var request struct {
			Hash string `json:"hash"`
		}
		if errBind := c.ShouldBindJSON(&request); errBind == nil {
			hash = strings.TrimSpace(request.Hash)
		}
	}
	if hash == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hash is required"})
		return
	}
	keys, errList := h.store().ListAPIKeys(user.ID)
	if errList != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to inspect API key"})
		return
	}
	owned := false
	for _, key := range keys {
		if key.KeyHash == hash && key.RevokedAt == nil {
			owned = true
			break
		}
	}
	if !owned {
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}
	if errRevoke := h.store().RevokeAPIKey(hash); errRevoke != nil {
		if errors.Is(errRevoke, tenancy.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}
		log.WithError(errRevoke).WithField("user_id", user.ID).Error("user api keys: revoke failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke API key"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) quotaResponse(ctx context.Context, userID string) (gin.H, error) {
	if h.service == nil || h.service.Quota() == nil || h.store() == nil {
		return nil, errors.New("quota service unavailable")
	}
	limit, errLimit := h.service.Quota().Limit(userID)
	if errLimit != nil {
		return nil, errLimit
	}
	used, errUsed := h.service.Quota().Used(userID)
	if errUsed != nil {
		return nil, errUsed
	}
	window := quotaWindow(h.cfg)
	now := time.Now().UTC()
	since := now.Add(-window)
	oldest, found, errOldest := h.store().OldestUserUsage(ctx, userID, since)
	if errOldest != nil {
		return nil, errOldest
	}
	resetAt := now.Add(window)
	if found {
		resetAt = oldest.Add(window)
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	response := gin.H{
		"limit":        tenancy.FormatNanoUSD(limit),
		"used":         tenancy.FormatNanoUSD(used),
		"remaining":    tenancy.FormatNanoUSD(remaining),
		"window":       window.String(),
		"window_start": since,
		"reset_at":     resetAt,
	}
	if composition, errComposition := h.service.Quota().Composition(userID); errComposition == nil {
		response["composition"] = gin.H{
			"base":                     tenancy.FormatNanoUSD(composition.BaseNanoUSD),
			"contribution":             tenancy.FormatNanoUSD(composition.ContributionNanoUSD),
			"contributing_credentials": composition.ContributingCredentials,
		}
	} else {
		log.WithError(errComposition).WithField("user_id", userID).
			Warn("user quota: limit composition unavailable")
	}
	return response, nil
}

func quotaWindow(cfg *config.Config) time.Duration {
	windowText := config.DefaultTenancyQuotaWindow
	if cfg != nil && strings.TrimSpace(cfg.Tenancy.Quota.Window) != "" {
		windowText = strings.TrimSpace(cfg.Tenancy.Quota.Window)
	}
	window, errParse := time.ParseDuration(windowText)
	if errParse != nil || window <= 0 {
		window, _ = time.ParseDuration(config.DefaultTenancyQuotaWindow)
	}
	return window
}

func apiKeyResponse(key tenancy.APIKey) gin.H {
	response := gin.H{
		"hash":       key.KeyHash,
		"label":      key.Label,
		"created_at": key.CreatedAt,
		"revoked":    key.RevokedAt != nil,
	}
	if key.LastUsed != nil {
		response["last_used_at"] = key.LastUsed
	}
	if key.RevokedAt != nil {
		response["revoked_at"] = key.RevokedAt
	}
	return response
}
