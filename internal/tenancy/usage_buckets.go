package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// UsageBuckets groups events in SQL, returning no raw ledger timestamps.
// Bounds apply to usage time; shift is added before grouping release events.
func (s *SQLiteStore) UsageBuckets(ctx context.Context, userID string, since, until time.Time, resolution, shift time.Duration) ([]UsageBucketStat, error) {
	return queryUsageBuckets(ctx, s.db, userID, since, until, resolution, shift)
}

type usageBucketQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryUsageBuckets(ctx context.Context, db usageBucketQuerier, userID string, since, until time.Time, resolution, shift time.Duration) ([]UsageBucketStat, error) {
	if resolution < time.Minute || shift < 0 || !since.Before(until) {
		return nil, fmt.Errorf("tenancy sqlite: invalid bucket interval")
	}
	rows, errQuery := db.QueryContext(ctx, `
 SELECT ((occurred_at + ?) / ?) * ? AS bucket_start,
 COALESCE(SUM(cost_nano_usd), 0), COALESCE(SUM(input_tokens), 0),
 COALESCE(SUM(output_tokens), 0), COUNT(*)
 FROM usage_ledger WHERE user_id = ? AND occurred_at >= ? AND occurred_at < ?
 GROUP BY bucket_start ORDER BY bucket_start`, int64(shift), int64(resolution), int64(resolution), userID, timeValue(since), timeValue(until))
	if errQuery != nil {
		return nil, fmt.Errorf("tenancy sqlite: query usage buckets: %w", errQuery)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Error("tenancy sqlite: close usage buckets")
		}
	}()
	stats := make([]UsageBucketStat, 0)
	for rows.Next() {
		var stat UsageBucketStat
		var start int64
		if errScan := rows.Scan(&start, &stat.CostNanoUSD, &stat.InputTokens, &stat.OutputTokens, &stat.Attempts); errScan != nil {
			return nil, fmt.Errorf("tenancy sqlite: scan usage bucket: %w", errScan)
		}
		stat.Start = timeFromValue(start)
		stats = append(stats, stat)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("tenancy sqlite: read usage buckets: %w", errRows)
	}
	return stats, nil
}

// ReleaseUsageBuckets reads releases and their origin balance in one SQLite
// snapshot. Both exclude usage at or after asOf, including rows appended while
// the request is being served. Rows arriving after the snapshot appear on refresh.
func (s *SQLiteStore) ReleaseUsageBuckets(ctx context.Context, userID string, from, to, asOf time.Time, resolution, window time.Duration) ([]UsageBucketStat, int64, error) {
	if resolution < time.Minute || window <= 0 || from.Before(asOf) || !from.Before(to) {
		return nil, 0, fmt.Errorf("tenancy sqlite: invalid release interval")
	}
	tx, errBegin := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if errBegin != nil {
		return nil, 0, fmt.Errorf("tenancy sqlite: begin release snapshot: %w", errBegin)
	}
	defer func() {
		if errRollback := tx.Rollback(); errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
			log.WithError(errRollback).Error("tenancy sqlite: rollback release snapshot")
		}
	}()
	since, until := from.Add(-window), to.Add(-window)
	if until.After(asOf) {
		until = asOf
	}
	stats := make([]UsageBucketStat, 0)
	if since.Before(until) {
		var errQuery error
		stats, errQuery = queryUsageBuckets(ctx, tx, userID, since, until, resolution, window)
		if errQuery != nil {
			return nil, 0, errQuery
		}
	}
	var used int64
	if errScan := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_nano_usd), 0) FROM usage_ledger
 WHERE user_id = ? AND occurred_at >= ? AND occurred_at < ?`, userID, timeValue(since), timeValue(asOf)).Scan(&used); errScan != nil {
		return nil, 0, fmt.Errorf("tenancy sqlite: read release balance: %w", errScan)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return nil, 0, fmt.Errorf("tenancy sqlite: commit release snapshot: %w", errCommit)
	}
	return stats, used, nil
}
