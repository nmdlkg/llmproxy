package user

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/tenancy"
	log "github.com/sirupsen/logrus"
)

type planningCacheEntry struct {
	generated time.Time
	payload   gin.H
}

func (h *Handler) GetUsageTimeline(c *gin.Context) { h.getUsagePlanning(c, false) }
func (h *Handler) GetUsageReleases(c *gin.Context) { h.getUsagePlanning(c, true) }

func planningRange(c *gin.Context, now time.Time, releases bool) (time.Time, time.Time, time.Duration, error) {
	resolution := time.Hour
	switch c.DefaultQuery("resolution", "1h") {
	case "1h":
	case "15m":
		resolution = 15 * time.Minute
	default:
		return time.Time{}, time.Time{}, 0, fmt.Errorf("resolution must be 1h or 15m")
	}
	name, fallback, maximum, unit := "days", "7", 7, 24*time.Hour
	if releases {
		name, fallback, maximum, unit = "hours", "48", 168, time.Hour
	}
	count, errParse := strconv.Atoi(c.DefaultQuery(name, fallback))
	if errParse != nil || count < 1 || count > maximum {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("%s must be between 1 and %d", name, maximum)
	}
	from, to := now.Add(-time.Duration(count)*unit), now
	if releases {
		from, to = now, now.Add(time.Duration(count)*unit)
	}
	if c.Query("from") != "" || c.Query("to") != "" {
		var errFrom, errTo error
		from, errFrom = time.Parse(time.RFC3339Nano, c.Query("from"))
		to, errTo = time.Parse(time.RFC3339Nano, c.Query("to"))
		if errFrom != nil || errTo != nil {
			return from, to, resolution, fmt.Errorf("from and to must both be RFC3339 timestamps")
		}
	}
	maxSpan := 7 * 24 * time.Hour
	if resolution == 15*time.Minute {
		maxSpan = 24 * time.Hour
	}
	if !from.Before(to) || to.Sub(from) > maxSpan || (!releases && to.After(now)) || (releases && to.After(now.Add(7*24*time.Hour))) {
		return from, to, resolution, fmt.Errorf("invalid range: hourly ranges allow 7 days; 15m detail allows 24 hours")
	}
	// A chart may remain open while part of a release interval elapses.
	if releases && from.Before(now) {
		from = now
	}
	if !from.Before(to) {
		return from, to, resolution, fmt.Errorf("range has already elapsed")
	}
	return from.UTC(), to.UTC(), resolution, nil
}

func (h *Handler) getUsagePlanning(c *gin.Context, releases bool) {
	user, _ := currentUser(c)
	now := time.Now().UTC()
	from, to, resolution, errRange := planningRange(c, now, releases)
	if errRange != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errRange.Error()})
		return
	}
	window := quotaWindow(h.cfg)
	// Key only validated semantics; query ordering, aliases and unrelated
	// parameters must not defeat caching or concurrent request coalescing.
	interval := to.Sub(from).String()
	if c.Query("from") != "" {
		requestedFrom, _ := time.Parse(time.RFC3339Nano, c.Query("from"))
		requestedTo, _ := time.Parse(time.RFC3339Nano, c.Query("to"))
		interval = requestedFrom.UTC().Format(time.RFC3339Nano) + "|" + requestedTo.UTC().Format(time.RFC3339Nano)
	}
	key := fmt.Sprintf("%q|%t|%d|%d|%s", user.ID, releases, window, resolution, interval)
	c.Header("Cache-Control", "no-store")
	requestCtx := c.Request.Context()
	value, errLoad := h.loadPlanning(requestCtx, key, func(fetchCtx context.Context) (any, error) {
		h.planningMu.Lock()
		entry, ok := h.planningCache[key]
		h.planningMu.Unlock()
		if ok && now.Sub(entry.generated) < 30*time.Second {
			return entry.payload, nil
		}
		var stats []tenancy.UsageBucketStat
		var used int64
		var errQuery error
		if releases {
			stats, used, errQuery = h.store().ReleaseUsageBuckets(fetchCtx, user.ID, from, to, now, resolution, window)
		} else {
			stats, errQuery = h.store().UsageBuckets(fetchCtx, user.ID, from, to, resolution, 0)
		}
		if errQuery != nil {
			return nil, errQuery
		}
		byStart := make(map[int64]tenancy.UsageBucketStat, len(stats))
		for _, stat := range stats {
			byStart[stat.Start.UnixNano()] = stat
		}
		buckets := make([]gin.H, 0)
		for start := from.Truncate(resolution); start.Before(to); start = start.Add(resolution) {
			stat := byStart[start.UnixNano()]
			end, clippedStart := start.Add(resolution), start
			if clippedStart.Before(from) {
				clippedStart = from
			}
			if end.After(to) {
				end = to
			}
			buckets = append(buckets, gin.H{"start": clippedStart, "end": end,
				"cost_nano_usd": formatNanoUSDDecimal(stat.CostNanoUSD), "input_tokens": stat.InputTokens,
				"output_tokens": stat.OutputTokens, "attempts": stat.Attempts})
		}
		payload := gin.H{"schema_version": 1, "currency": "USD", "mode": "rolling", "window": window.String(),
			"generated_at": now, "cache_ttl_seconds": 30, "resolution_seconds": int64(resolution / time.Second),
			"range": gin.H{"start": from, "end": to, "timezone": "UTC"}, "buckets": buckets}
		if releases {
			// Balance and releases came from one bounded database snapshot.
			limit, errLimit := h.service.Quota().Limit(user.ID)
			if errLimit != nil {
				return nil, errLimit
			}
			payload["used_nano_usd"] = formatNanoUSDDecimal(used)
			payload["limit_nano_usd"] = formatNanoUSDDecimal(limit)
		}
		h.planningMu.Lock()
		if h.planningCache == nil || len(h.planningCache) >= 512 {
			h.planningCache = make(map[string]planningCacheEntry)
		}
		h.planningCache[key] = planningCacheEntry{generated: now, payload: payload}
		h.planningMu.Unlock()
		return payload, nil
	})
	if requestCtx.Err() != nil {
		return
	}
	if errLoad != nil {
		log.WithError(errLoad).WithField("user_id", user.ID).Error("user planning: load buckets")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load quota planning"})
		return
	}
	c.JSON(http.StatusOK, value)
}

// loadPlanning lets each waiter leave on its own cancellation. If a different
// caller led the shared query and disconnected, surviving callers retry under
// their own contexts. No detached database work or new timeout is needed.
func (h *Handler) loadPlanning(ctx context.Context, key string, fetch func(context.Context) (any, error)) (any, error) {
	for {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		led := false
		pending := h.planningFetches.DoChan(key, func() (any, error) {
			led = true
			value, errFetch := fetch(ctx)
			// Normalize driver interruption errors that may not wrap context errors.
			if errFetch != nil && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return value, errFetch
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-pending:
			if errContext := ctx.Err(); errContext != nil {
				return nil, errContext
			}
			if result.Shared && !led && (errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, context.DeadlineExceeded)) {
				continue
			}
			return result.Val, result.Err
		}
	}
}
