package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

// SQLiteStore persists tenancy state in a pure-Go SQLite database.
type SQLiteStore struct {
	db   *sql.DB
	path string
}

var _ Store = (*SQLiteStore)(nil)

// ResolveDBPath applies the tenancy database path contract.
//
// Both inputs may start with "~": auth-dir conventionally does (the shipped
// default is "~/.cli-proxy-api") and callers pass cfg.AuthDir unexpanded, so the
// tilde must be resolved here. Leaving it literal would create a directory
// actually named "~" under the process working directory, putting the database
// somewhere other than documented and, worse, making it depend on the working
// directory: a restart from elsewhere would silently open a different database
// and appear to lose every user and usage row.
func ResolveDBPath(cfg config.TenancyConfig, authDir string) string {
	if path := strings.TrimSpace(cfg.DBPath); path != "" {
		return filepath.Clean(expandHomePath(path))
	}
	// ResolveAuthDir owns the auth-dir contract, including the tilde form and the
	// empty-means-default case.
	resolved, errResolve := util.ResolveAuthDir(authDir)
	if errResolve != nil {
		log.WithError(errResolve).
			WithField("auth_dir", authDir).
			Warn("tenancy sqlite: cannot resolve auth dir; deriving database path from the raw value")
		resolved = authDir
	}
	return filepath.Clean(filepath.Join(resolved, "..", "tenancy.db"))
}

// expandHomePath resolves a leading "~" in an explicitly configured path. Unlike
// util.ResolveAuthDir it applies no default, because an empty database path is a
// caller error rather than a request for the default auth directory.
func expandHomePath(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		log.WithError(errHome).Warn("tenancy sqlite: cannot resolve home directory; using the configured path verbatim")
		return path
	}
	remainder := strings.TrimLeft(strings.TrimPrefix(path, "~"), "/\\")
	if remainder == "" {
		return home
	}
	return filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(remainder, "\\", "/")))
}

// OpenSQLite opens the configured tenancy database and creates its schema.
func OpenSQLite(cfg config.TenancyConfig, authDir string) (*SQLiteStore, error) {
	return OpenSQLitePath(ResolveDBPath(cfg, authDir))
}

// OpenSQLitePath opens a tenancy database at an explicit path.
func OpenSQLitePath(path string) (*SQLiteStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("tenancy sqlite: database path is empty")
	}
	if path != ":memory:" {
		parent := filepath.Dir(path)
		if errMkdir := os.MkdirAll(parent, 0o700); errMkdir != nil {
			return nil, fmt.Errorf("tenancy sqlite: create database directory: %w", errMkdir)
		}
	}

	db, errOpen := sql.Open("sqlite", path)
	if errOpen != nil {
		return nil, fmt.Errorf("tenancy sqlite: open database: %w", errOpen)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &SQLiteStore{db: db, path: path}
	if errPing := db.PingContext(context.Background()); errPing != nil {
		_ = db.Close()
		return nil, fmt.Errorf("tenancy sqlite: ping database: %w", errPing)
	}
	if errSchema := store.EnsureSchema(context.Background()); errSchema != nil {
		_ = db.Close()
		return nil, errSchema
	}
	return store, nil
}

// Path returns the database path used by the store.
func (s *SQLiteStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Close releases the database connection.
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if errClose := s.db.Close(); errClose != nil {
		return fmt.Errorf("tenancy sqlite: close database: %w", errClose)
	}
	return nil
}

// EnsureSchema creates all tenancy tables idempotently.
func (s *SQLiteStore) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("tenancy sqlite: not initialized")
	}
	if _, errPragma := s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); errPragma != nil {
		return fmt.Errorf("tenancy sqlite: enable foreign keys: %w", errPragma)
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			role TEXT NOT NULL CHECK (role IN ('admin', 'user')),
			tier TEXT NOT NULL,
			disabled INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS user_api_keys (
			key_hash TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			label TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_used_at INTEGER,
			revoked_at INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS usage_ledger (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL,
			auth_id TEXT NOT NULL,
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			cost_nano_usd INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			failed INTEGER NOT NULL,
			occurred_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS quota_windows (
			auth_id TEXT NOT NULL,
			provider TEXT NOT NULL,
			window_start INTEGER NOT NULL,
			window_end INTEGER NOT NULL,
			used_units INTEGER NOT NULL,
			limit_units INTEGER NOT NULL,
			source TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (auth_id, provider)
		)`,
		`CREATE TABLE IF NOT EXISTS credential_validation (
			auth_id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			last_attempt_at INTEGER NOT NULL,
			last_ok_at INTEGER,
			last_status TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_ledger_user_time
			ON usage_ledger(user_id, occurred_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_ledger_auth_provider_time
			ON usage_ledger(auth_id, provider, occurred_at)`,
		`CREATE INDEX IF NOT EXISTS idx_user_api_keys_user
			ON user_api_keys(user_id)`,
	}

	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("tenancy sqlite: begin schema transaction: %w", errBegin)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	for _, statement := range statements {
		if _, errExec := tx.ExecContext(ctx, statement); errExec != nil {
			return fmt.Errorf("tenancy sqlite: ensure schema: %w", errExec)
		}
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("tenancy sqlite: commit schema transaction: %w", errCommit)
	}
	return nil
}

// AppendUsage appends a batch of usage rows in one transaction.
func (s *SQLiteStore) AppendUsage(ctx context.Context, entries []UsageEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("tenancy sqlite: begin usage append: %w", errBegin)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	statement, errPrepare := tx.PrepareContext(ctx, `
		INSERT INTO usage_ledger (
			user_id, auth_id, provider, model, cost_nano_usd,
			input_tokens, output_tokens, failed, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if errPrepare != nil {
		return fmt.Errorf("tenancy sqlite: prepare usage append: %w", errPrepare)
	}
	defer func() {
		_ = statement.Close()
	}()
	for _, entry := range entries {
		occurredAt := entry.OccurredAt
		if occurredAt.IsZero() {
			occurredAt = time.Now().UTC()
		}
		if _, errExec := statement.ExecContext(
			ctx,
			entry.UserID,
			entry.AuthID,
			strings.ToLower(strings.TrimSpace(entry.Provider)),
			entry.Model,
			entry.CostNanoUSD,
			entry.InputTokens,
			entry.OutputTokens,
			boolInt(entry.Failed),
			timeValue(occurredAt),
		); errExec != nil {
			return fmt.Errorf("tenancy sqlite: append usage row: %w", errExec)
		}
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("tenancy sqlite: commit usage append: %w", errCommit)
	}
	return nil
}

// UsedUnits sums exact nano-USD usage in the rolling interval.
func (s *SQLiteStore) UsedUnits(ctx context.Context, userID string, since time.Time) (int64, error) {
	var used int64
	errScan := s.db.QueryRowContext(
		ctx,
		`SELECT COALESCE(SUM(cost_nano_usd), 0)
		 FROM usage_ledger
		 WHERE user_id = ? AND occurred_at >= ?`,
		userID,
		timeValue(since),
	).Scan(&used)
	if errScan != nil {
		return 0, fmt.Errorf("tenancy sqlite: sum used units: %w", errScan)
	}
	return used, nil
}

// OldestUserUsage returns the oldest user ledger row within the interval.
func (s *SQLiteStore) OldestUserUsage(ctx context.Context, userID string, since time.Time) (time.Time, bool, error) {
	return s.minimumUsageTime(
		ctx,
		`SELECT MIN(occurred_at) FROM usage_ledger WHERE user_id = ? AND occurred_at >= ?`,
		userID,
		timeValue(since),
	)
}

// EarliestAuthUsage returns the earliest auth/provider ledger row within the interval.
func (s *SQLiteStore) EarliestAuthUsage(ctx context.Context, authID, provider string, since time.Time) (time.Time, bool, error) {
	return s.minimumUsageTime(
		ctx,
		`SELECT MIN(occurred_at)
		 FROM usage_ledger
		 WHERE auth_id = ? AND provider = ? AND occurred_at >= ?`,
		authID,
		strings.ToLower(strings.TrimSpace(provider)),
		timeValue(since),
	)
}

func (s *SQLiteStore) minimumUsageTime(ctx context.Context, query string, args ...any) (time.Time, bool, error) {
	var value sql.NullInt64
	if errScan := s.db.QueryRowContext(ctx, query, args...).Scan(&value); errScan != nil {
		return time.Time{}, false, fmt.Errorf("tenancy sqlite: query earliest usage: %w", errScan)
	}
	if !value.Valid {
		return time.Time{}, false, nil
	}
	return timeFromValue(value.Int64), true, nil
}

// UpsertQuotaWindow records the latest quota window for an auth/provider pair.
func (s *SQLiteStore) UpsertQuotaWindow(ctx context.Context, window QuotaWindow) error {
	_, errExec := s.db.ExecContext(ctx, `
		INSERT INTO quota_windows (
			auth_id, provider, window_start, window_end,
			used_units, limit_units, source, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_id, provider) DO UPDATE SET
			window_start = excluded.window_start,
			window_end = excluded.window_end,
			used_units = CASE
				WHEN excluded.limit_units > 0 THEN excluded.used_units
				ELSE quota_windows.used_units
			END,
			limit_units = CASE
				WHEN excluded.limit_units > 0 THEN excluded.limit_units
				ELSE quota_windows.limit_units
			END,
			source = excluded.source,
			updated_at = excluded.updated_at
	`,
		window.AuthID,
		strings.ToLower(strings.TrimSpace(window.Provider)),
		timeValue(window.WindowStart),
		timeValue(window.WindowEnd),
		window.UsedUnits,
		window.LimitUnits,
		window.Source,
		timeValue(window.UpdatedAt),
	)
	if errExec != nil {
		return fmt.Errorf("tenancy sqlite: upsert quota window: %w", errExec)
	}
	return nil
}

// ListQuotaWindows returns all current quota windows.
func (s *SQLiteStore) ListQuotaWindows(ctx context.Context) ([]QuotaWindow, error) {
	rows, errQuery := s.db.QueryContext(ctx, `
		SELECT auth_id, provider, window_start, window_end,
		       used_units, limit_units, source, updated_at
		FROM quota_windows
		ORDER BY auth_id, provider
	`)
	if errQuery != nil {
		return nil, fmt.Errorf("tenancy sqlite: list quota windows: %w", errQuery)
	}
	defer func() {
		_ = rows.Close()
	}()

	windows := make([]QuotaWindow, 0)
	for rows.Next() {
		var window QuotaWindow
		var windowStart, windowEnd, updatedAt int64
		if errScan := rows.Scan(
			&window.AuthID,
			&window.Provider,
			&windowStart,
			&windowEnd,
			&window.UsedUnits,
			&window.LimitUnits,
			&window.Source,
			&updatedAt,
		); errScan != nil {
			return nil, fmt.Errorf("tenancy sqlite: scan quota window: %w", errScan)
		}
		window.WindowStart = timeFromValue(windowStart)
		window.WindowEnd = timeFromValue(windowEnd)
		window.UpdatedAt = timeFromValue(updatedAt)
		windows = append(windows, window)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("tenancy sqlite: iterate quota windows: %w", errRows)
	}
	return windows, nil
}

// UpsertCredentialValidation stores the latest credential validation state.
func (s *SQLiteStore) UpsertCredentialValidation(ctx context.Context, validation CredentialValidation) error {
	var lastOK any
	if validation.LastOKAt != nil {
		lastOK = timeValue(*validation.LastOKAt)
	}
	_, errExec := s.db.ExecContext(ctx, `
		INSERT INTO credential_validation (
			auth_id, user_id, last_attempt_at, last_ok_at, last_status
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(auth_id) DO UPDATE SET
			user_id = excluded.user_id,
			last_attempt_at = excluded.last_attempt_at,
			last_ok_at = CASE
				WHEN excluded.last_ok_at IS NOT NULL THEN excluded.last_ok_at
				WHEN credential_validation.user_id = excluded.user_id THEN credential_validation.last_ok_at
				ELSE NULL
			END,
			last_status = excluded.last_status
	`,
		validation.AuthID,
		validation.UserID,
		timeValue(validation.LastAttemptAt),
		lastOK,
		validation.LastStatus,
	)
	if errExec != nil {
		return fmt.Errorf("tenancy sqlite: upsert credential validation: %w", errExec)
	}
	return nil
}

// GetCredentialValidation loads the latest validation state for one auth.
func (s *SQLiteStore) GetCredentialValidation(ctx context.Context, authID string) (*CredentialValidation, error) {
	var validation CredentialValidation
	var lastAttemptAt int64
	var lastOKAt sql.NullInt64
	errScan := s.db.QueryRowContext(ctx, `
		SELECT auth_id, user_id, last_attempt_at, last_ok_at, last_status
		FROM credential_validation
		WHERE auth_id = ?
	`, authID).Scan(
		&validation.AuthID,
		&validation.UserID,
		&lastAttemptAt,
		&lastOKAt,
		&validation.LastStatus,
	)
	if errors.Is(errScan, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: credential validation %q", ErrNotFound, authID)
	}
	if errScan != nil {
		return nil, fmt.Errorf("tenancy sqlite: get credential validation: %w", errScan)
	}
	validation.LastAttemptAt = timeFromValue(lastAttemptAt)
	if lastOKAt.Valid {
		value := timeFromValue(lastOKAt.Int64)
		validation.LastOKAt = &value
	}
	return &validation, nil
}

func timeValue(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

func timeFromValue(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
