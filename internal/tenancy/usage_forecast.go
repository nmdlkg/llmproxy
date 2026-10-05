package tenancy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UsageForecast returns a stable provider/auth/model projection for one user.
// The user predicate is part of the SQL query, so callers cannot accidentally
// widen the ownership boundary. auth IDs are internal and must be redacted by
// API handlers before returning them.
func (s *SQLiteStore) UsageForecast(ctx context.Context, userID string, since, until time.Time, limit int) ([]UsageForecastStat, error) {
	if s == nil || s.db == nil || strings.TrimSpace(userID) == "" || !since.Before(until) {
		return nil, fmt.Errorf("tenancy sqlite: invalid forecast range")
	}
	if limit <= 0 || limit > 512 {
		limit = 512
	}
	rows, errQuery := s.db.QueryContext(ctx, `
	SELECT provider, auth_id, model, plan, model_family, quota_scope,
	       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
	       COALESCE(SUM(reasoning_tokens), 0), COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0),
        COUNT(*), COALESCE(SUM(failed), 0)
 FROM usage_ledger
 WHERE user_id = ? AND occurred_at >= ? AND occurred_at < ?
 GROUP BY provider, auth_id, model
 ORDER BY provider ASC, auth_id ASC, model ASC
 LIMIT ?`, userID, timeValue(since), timeValue(until), limit)
	if errQuery != nil {
		return nil, fmt.Errorf("tenancy sqlite: usage forecast: %w", errQuery)
	}
	defer rows.Close()
	stats := make([]UsageForecastStat, 0)
	for rows.Next() {
		var stat UsageForecastStat
		if errScan := rows.Scan(&stat.Provider, &stat.AuthID, &stat.Model, &stat.Plan, &stat.ModelFamily, &stat.QuotaScope, &stat.InputTokens, &stat.OutputTokens, &stat.ReasoningTokens, &stat.CacheReadTokens, &stat.CacheCreationTokens, &stat.Attempts, &stat.FailedAttempts); errScan != nil {
			return nil, fmt.Errorf("tenancy sqlite: scan usage forecast: %w", errScan)
		}
		stats = append(stats, stat)
	}
	if errRows := rows.Err(); errRows != nil && errRows != sql.ErrNoRows {
		return nil, fmt.Errorf("tenancy sqlite: read usage forecast: %w", errRows)
	}
	return stats, nil
}
