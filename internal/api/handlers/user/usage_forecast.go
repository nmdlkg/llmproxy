package user

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/forecast"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/openrouter"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/tenancy"
)

const forecastMaxRows = 256

// GetUsageForecast returns advisory, tenant-scoped weekly usage planning data.
// It never feeds the request selector or changes routing eligibility.
func (h *Handler) GetUsageForecast(c *gin.Context) {
	user, _ := currentUser(c)
	now := time.Now().UTC()
	from := now.Add(-7 * 24 * time.Hour)
	store := h.store()
	// Hourly buckets provide a fixed input shape for the pure forecast engine.
	buckets, errBuckets := store.UsageBuckets(c.Request.Context(), user.ID, from, now, time.Hour, 0)
	if errBuckets != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load usage forecast"})
		return
	}
	observations := make([]forecast.Observation, 0, len(buckets))
	for _, bucket := range buckets {
		observations = append(observations, forecast.Observation{At: bucket.Start, Tokens: bucket.InputTokens + bucket.OutputTokens})
	}
	result := forecast.Calculate(observations, nil, now)
	rows := make([]tenancy.UsageForecastStat, 0)
	truncated := false
	if projection, ok := store.(tenancy.ForecastStore); ok {
		var errRows error
		rows, errRows = projection.UsageForecast(c.Request.Context(), user.ID, from, now, forecastMaxRows+1)
		if errRows != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load usage forecast"})
			return
		}
		if len(rows) > forecastMaxRows {
			rows, truncated = rows[:forecastMaxRows], true
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].AuthID < rows[j].AuthID
	})
	items := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		pricing := tenancy.ModelPricingFor(row.Model, h.cfg.Tenancy)
		version := pricingVersion(pricing)
		status := "ok"
		if !pricing.PromptNanoUSDPerToken.Known || !pricing.CompletionNanoUSDPerToken.Known {
			status = "partial"
		}
		items = append(items, gin.H{
			"provider": row.Provider, "plan": nil, "credential": redactedCredentialHandle(user.ID, row.AuthID), "model_family": modelFamily(row.Model),
			"model": row.Model, "input_tokens": row.InputTokens, "output_tokens": row.OutputTokens, "reasoning_tokens": row.ReasoningTokens, "cache_read_tokens": row.CacheReadTokens, "cache_creation_tokens": row.CacheCreationTokens, "attempts": row.Attempts, "failed_attempts": row.FailedAttempts,
			"api_price_equivalent_nano_usd": apiEquivalent(row, pricing), "price_status": status, "price_catalog_version": version, "price_effective_at": now,
		})
	}
	payload := gin.H{
		"schema_version": 1, "advisory": true, "algorithm_version": result.AlgorithmVersion, "generated_at": result.GeneratedAt,
		"source": "usage_ledger", "freshness": "current", "confidence": result.Confidence, "coverage": result.Coverage,
		"status": result.Status, "reason_codes": result.ReasonCodes, "window": gin.H{"start": from, "end": now, "timezone": "UTC"},
		"observed_weekly_tokens": result.ObservedWeeklyTokens, "projected_next_week_tokens": result.ProjectedNextWeekTokens,
		"projected_next_week_tokens_lower": result.ProjectedLowerTokens, "projected_next_week_tokens_upper": result.ProjectedUpperTokens,
		"estimated_available_tokens": result.EstimatedAvailableTokens, "remaining_until_reset_seconds": durationSeconds(result.RemainingUntilReset),
		"rows": items, "truncated": truncated, "max_rows": forecastMaxRows,
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, payload)
}

func durationSeconds(value *time.Duration) any {
	if value == nil {
		return nil
	}
	return int64(value.Seconds())
}

func redactedCredentialHandle(userID, authID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(userID) + "\x00" + strings.TrimSpace(authID)))
	return "cred_" + hex.EncodeToString(sum[:8])
}

func modelFamily(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "unknown"
	}
	if slash := strings.LastIndex(model, "/"); slash >= 0 && slash+1 < len(model) {
		model = model[slash+1:]
	}
	if colon := strings.IndexByte(model, ':'); colon > 0 {
		model = model[:colon]
	}
	return model
}

func pricingVersion(pricing any) string {
	data, errMarshal := json.Marshal(pricing)
	if errMarshal != nil {
		return "unknown"
	}
	sum := sha256.Sum256(data)
	return "catalog-" + hex.EncodeToString(sum[:8])
}

func apiEquivalent(row tenancy.UsageForecastStat, pricing openrouter.ModelPricing) any {
	// Kept as a small adapter so the response always uses decimal nano-USD and
	// never confuses an unknown price with a measured zero.
	reasoningPrice := pricing.InternalReasoningNanoUSDPerToken
	if !reasoningPrice.Known {
		reasoningPrice = pricing.CompletionNanoUSDPerToken
	}
	if !pricing.PromptNanoUSDPerToken.Known || !pricing.CompletionNanoUSDPerToken.Known || (row.ReasoningTokens > 0 && !reasoningPrice.Known) || (row.CacheReadTokens > 0 && !pricing.InputCacheReadNanoUSDPerToken.Known) || (row.CacheCreationTokens > 0 && !pricing.InputCacheWriteNanoUSDPerToken.Known) {
		return nil
	}
	uncached := row.InputTokens - row.CacheReadTokens - row.CacheCreationTokens
	if uncached < 0 {
		uncached = 0
	}
	value := uncached*pricing.PromptNanoUSDPerToken.NanoUSD + row.OutputTokens*pricing.CompletionNanoUSDPerToken.NanoUSD + row.ReasoningTokens*reasoningPrice.NanoUSD + row.CacheReadTokens*pricing.InputCacheReadNanoUSDPerToken.NanoUSD + row.CacheCreationTokens*pricing.InputCacheWriteNanoUSDPerToken.NanoUSD
	return fmt.Sprintf("%d", value)
}
