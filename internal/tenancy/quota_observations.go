package tenancy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// AppendQuotaObservation persists a normalized provider observation. Empty
// quota scopes remain explicit unknown values and are never inferred.
func (s *SQLiteStore) AppendQuotaObservation(ctx context.Context, observation QuotaObservation) error {
	if s == nil || s.db == nil || strings.TrimSpace(observation.Provider) == "" || strings.TrimSpace(observation.WindowKind) == "" {
		return fmt.Errorf("tenancy sqlite: invalid quota observation")
	}
	at := observation.ObservedAt.UTC()
	if at.IsZero() {
		return fmt.Errorf("tenancy sqlite: quota observation timestamp is required")
	}
	canonical := strings.TrimSpace(observation.CanonicalAuthID)
	if canonical == "" {
		canonical = CanonicalAuthID(observation.Provider, observation.AuthID)
	}
	_, errExec := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO quota_observations
 (canonical_auth_id, auth_id, provider, plan, model_family, quota_scope, window_kind, native_unit,
  used_units, limit_units, remaining_value, observed_at, reset_at, authority, source)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		canonical, strings.TrimSpace(observation.AuthID), strings.ToLower(strings.TrimSpace(observation.Provider)),
		strings.TrimSpace(observation.Plan), strings.TrimSpace(observation.ModelFamily), strings.TrimSpace(observation.QuotaScope),
		strings.TrimSpace(observation.WindowKind), strings.TrimSpace(observation.NativeUnit), observation.UsedUnits, observation.LimitUnits,
		observation.RemainingValue, timeValue(at), nullableTimeValue(observation.ResetAt), strings.TrimSpace(observation.Authority), strings.TrimSpace(observation.Source))
	if errExec != nil {
		return fmt.Errorf("tenancy sqlite: append quota observation: %w", errExec)
	}
	return nil
}

// ListQuotaObservations returns bounded observations for one canonical auth
// and quota scope, newest first. It is intended for forecast adapters.
func (s *SQLiteStore) ListQuotaObservations(ctx context.Context, canonicalAuthID, quotaScope string, limit int) ([]QuotaObservation, error) {
	if s == nil || s.db == nil || strings.TrimSpace(canonicalAuthID) == "" {
		return nil, fmt.Errorf("tenancy sqlite: invalid quota observation query")
	}
	if limit <= 0 || limit > 512 {
		limit = 512
	}
	rows, errQuery := s.db.QueryContext(ctx, `SELECT canonical_auth_id, auth_id, provider, plan, model_family, quota_scope, window_kind, native_unit, used_units, limit_units, remaining_value, observed_at, reset_at, authority, source FROM quota_observations WHERE canonical_auth_id = ? AND quota_scope = ? ORDER BY observed_at DESC LIMIT ?`, canonicalAuthID, quotaScope, limit)
	if errQuery != nil {
		return nil, fmt.Errorf("tenancy sqlite: list quota observations: %w", errQuery)
	}
	defer rows.Close()
	result := make([]QuotaObservation, 0)
	for rows.Next() {
		var item QuotaObservation
		var observed, reset sql.NullInt64
		if errScan := rows.Scan(&item.CanonicalAuthID, &item.AuthID, &item.Provider, &item.Plan, &item.ModelFamily, &item.QuotaScope, &item.WindowKind, &item.NativeUnit, &item.UsedUnits, &item.LimitUnits, &item.RemainingValue, &observed, &reset, &item.Authority, &item.Source); errScan != nil {
			return nil, fmt.Errorf("tenancy sqlite: scan quota observation: %w", errScan)
		}
		item.ObservedAt = timeFromValue(observed.Int64)
		if reset.Valid {
			value := timeFromValue(reset.Int64)
			item.ResetAt = &value
		}
		result = append(result, item)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("tenancy sqlite: read quota observations: %w", errRows)
	}
	return result, nil
}

func nullableTimeValue(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return timeValue(*value)
}
