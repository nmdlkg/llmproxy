package tenancy

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// MaxActiveAPIKeys is the maximum number of non-revoked keys a user may
	// retain. Revoked rows remain in the database and do not consume this cap.
	MaxActiveAPIKeys = 25
	// MaxAPIKeyCreationsPerMinute limits key creation attempts for one
	// authenticated actor. The limit is enforced transactionally in SQLite.
	MaxAPIKeyCreationsPerMinute = 5
	apiKeyCreationWindow        = time.Minute
)

// HashAPIKey returns the SHA-256 hex digest used for API key persistence.
func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ValidateAPIKeyHash accepts only the lowercase SHA-256 wire representation
// used by browser clients. The plaintext key must never be sent to the server
// for browser registration.
func ValidateAPIKeyHash(keyHash string) error {
	if len(keyHash) != sha256.Size*2 || keyHash != strings.ToLower(keyHash) {
		return fmt.Errorf("%w: expected lowercase 64-character hex", ErrInvalidAPIKeyHash)
	}
	if _, errDecode := hex.DecodeString(keyHash); errDecode != nil {
		return fmt.Errorf("%w: expected lowercase 64-character hex: %v", ErrInvalidAPIKeyHash, errDecode)
	}
	return nil
}

// normalizeEmail canonicalizes a user email to trimmed lower case.
//
// The users.email UNIQUE constraint is case-sensitive in SQLite, so without this
// two rows differing only in case can coexist. That matters beyond tidiness:
// the email is the ledger and quota key and the join key every dashboard and the
// monthly usage report group by, so a case-duplicate silently splits one person's
// usage across two accounts and grants them quota twice. Normalizing here rather
// than in each caller keeps the CLI bootstrap and the HTTP admin API in
// agreement, since only this layer is common to both.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// CreateUser inserts a user, generating an ID when one is not supplied.
func (s *SQLiteStore) CreateUser(user *User) error {
	if user == nil {
		return fmt.Errorf("tenancy sqlite: user is nil")
	}
	role, errRole := normalizeRole(user.Role)
	if errRole != nil {
		return errRole
	}
	if strings.TrimSpace(user.ID) == "" {
		id, errID := randomIdentifier("u_", 16)
		if errID != nil {
			return fmt.Errorf("tenancy sqlite: generate user ID: %w", errID)
		}
		user.ID = id
	}
	user.Email = normalizeEmail(user.Email)
	if user.Email == "" {
		return fmt.Errorf("tenancy sqlite: user email is empty")
	}
	user.DisplayName = strings.TrimSpace(user.DisplayName)
	user.Role = role
	user.Tier = strings.ToLower(strings.TrimSpace(user.Tier))
	if user.Tier == "" {
		user.Tier = "default"
	}
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	if user.UpdatedAt.IsZero() {
		user.UpdatedAt = user.CreatedAt
	}

	_, errExec := s.db.Exec(`
		INSERT INTO users (
			id, email, display_name, role, tier, disabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		user.ID,
		user.Email,
		user.DisplayName,
		user.Role,
		user.Tier,
		boolInt(user.Disabled),
		timeValue(user.CreatedAt),
		timeValue(user.UpdatedAt),
	)
	if errExec != nil {
		return fmt.Errorf("tenancy sqlite: create user: %w", errExec)
	}
	return nil
}

// GetUser loads a user by ID.
func (s *SQLiteStore) GetUser(id string) (*User, error) {
	return scanUser(s.db.QueryRow(`
		SELECT id, email, display_name, role, tier, disabled, created_at, updated_at
		FROM users
		WHERE id = ?
	`, id))
}

// UpdateUser replaces the mutable fields of an existing user.
func (s *SQLiteStore) UpdateUser(user *User) error {
	if user == nil {
		return fmt.Errorf("tenancy sqlite: user is nil")
	}
	role, errRole := normalizeRole(user.Role)
	if errRole != nil {
		return errRole
	}
	user.Email = normalizeEmail(user.Email)
	if user.Email == "" {
		return fmt.Errorf("tenancy sqlite: user email is empty")
	}
	user.DisplayName = strings.TrimSpace(user.DisplayName)
	user.Role = role
	user.Tier = strings.ToLower(strings.TrimSpace(user.Tier))
	if user.Tier == "" {
		user.Tier = "default"
	}
	user.UpdatedAt = time.Now().UTC()
	result, errExec := s.db.Exec(`
		UPDATE users
		SET email = ?, display_name = ?, role = ?, tier = ?, disabled = ?, updated_at = ?
		WHERE id = ?
	`,
		user.Email,
		user.DisplayName,
		user.Role,
		user.Tier,
		boolInt(user.Disabled),
		timeValue(user.UpdatedAt),
		user.ID,
	)
	if errExec != nil {
		return fmt.Errorf("tenancy sqlite: update user: %w", errExec)
	}
	return requireAffected(result, "user", user.ID)
}

// DeleteUser deletes a user and their API keys.
func (s *SQLiteStore) DeleteUser(id string) error {
	result, errExec := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if errExec != nil {
		return fmt.Errorf("tenancy sqlite: delete user: %w", errExec)
	}
	return requireAffected(result, "user", id)
}

// ListUsers lists users by creation time and ID.
func (s *SQLiteStore) ListUsers() ([]User, error) {
	rows, errQuery := s.db.Query(`
		SELECT id, email, display_name, role, tier, disabled, created_at, updated_at
		FROM users
		ORDER BY created_at, id
	`)
	if errQuery != nil {
		return nil, fmt.Errorf("tenancy sqlite: list users: %w", errQuery)
	}
	defer func() {
		_ = rows.Close()
	}()

	users := make([]User, 0)
	for rows.Next() {
		user, errScan := scanUser(rows)
		if errScan != nil {
			return nil, errScan
		}
		users = append(users, *user)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("tenancy sqlite: iterate users: %w", errRows)
	}
	return users, nil
}

// IssueAPIKey creates a user key and returns its plaintext exactly once.
func (s *SQLiteStore) IssueAPIKey(userID, label string) (string, *APIKey, error) {
	randomBytes := make([]byte, 32)
	if _, errRead := rand.Read(randomBytes); errRead != nil {
		return "", nil, fmt.Errorf("tenancy sqlite: generate API key: %w", errRead)
	}
	plaintext := "cp_u_" + base64.RawURLEncoding.EncodeToString(randomBytes)
	key, errRegister := s.RegisterAPIKeyHash(userID, HashAPIKey(plaintext), label)
	if errRegister != nil {
		return "", nil, fmt.Errorf("tenancy sqlite: issue API key: %w", errRegister)
	}
	return plaintext, key, nil
}

// RegisterAPIKeyHash registers browser-generated key metadata. It accepts only
// a SHA-256 hash; plaintext cp_u_ material is intentionally not an argument.
func (s *SQLiteStore) RegisterAPIKeyHash(userID, keyHash, label string) (*APIKey, error) {
	return s.RegisterAPIKeyHashForActor(userID, userID, keyHash, label)
}

// RegisterAPIKeyHashForActor registers a key on behalf of an authenticated
// actor. Admin workflows use this form so the creation rate limit is attached
// to the actor rather than the target user.
func (s *SQLiteStore) RegisterAPIKeyHashForActor(actorID, userID, keyHash, label string) (*APIKey, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("tenancy sqlite: not initialized")
	}
	if errValidate := ValidateAPIKeyHash(keyHash); errValidate != nil {
		return nil, errValidate
	}
	actorID = strings.TrimSpace(actorID)
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("tenancy sqlite: user ID is empty")
	}
	if actorID == "" {
		actorID = userID
	}
	if _, errUser := s.GetUser(userID); errUser != nil {
		return nil, errUser
	}

	now := time.Now().UTC()
	if errAttempt := s.consumeAPIKeyRegistrationAttempt(actorID, now); errAttempt != nil {
		return nil, errAttempt
	}
	tx, errBegin := s.db.Begin()
	if errBegin != nil {
		return nil, fmt.Errorf("tenancy sqlite: begin API key registration: %w", errBegin)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var exists int
	if errScan := tx.QueryRow(`SELECT COUNT(1) FROM user_api_keys WHERE key_hash = ?`, keyHash).Scan(&exists); errScan != nil {
		return nil, fmt.Errorf("tenancy sqlite: inspect API key registration: %w", errScan)
	}
	if exists != 0 {
		return nil, ErrAPIKeyExists
	}

	var active int
	if errScan := tx.QueryRow(`
		SELECT COUNT(1)
		FROM user_api_keys
		WHERE user_id = ? AND revoked_at IS NULL
	`, userID).Scan(&active); errScan != nil {
		return nil, fmt.Errorf("tenancy sqlite: count active API keys: %w", errScan)
	}
	if active >= MaxActiveAPIKeys {
		return nil, ErrAPIKeyLimit
	}

	key := &APIKey{
		KeyHash:   keyHash,
		UserID:    userID,
		Label:     strings.TrimSpace(label),
		CreatedAt: now,
	}
	if _, errExec := tx.Exec(`
		INSERT INTO user_api_keys (key_hash, user_id, created_by, label, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, key.KeyHash, key.UserID, actorID, key.Label, timeValue(key.CreatedAt)); errExec != nil {
		return nil, fmt.Errorf("tenancy sqlite: register API key hash: %w", errExec)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return nil, fmt.Errorf("tenancy sqlite: commit API key registration: %w", errCommit)
	}
	return key, nil
}

// consumeAPIKeyRegistrationAttempt reserves one actor-scoped attempt before
// key registration begins. It commits independently so duplicate, over-cap,
// and other rejected valid-hash attempts cannot bypass the limit by rolling
// back the registration transaction.
func (s *SQLiteStore) consumeAPIKeyRegistrationAttempt(actorID string, now time.Time) error {
	tx, errBegin := s.db.Begin()
	if errBegin != nil {
		return fmt.Errorf("tenancy sqlite: begin API key attempt: %w", errBegin)
	}
	defer func() { _ = tx.Rollback() }()

	cutoff := timeValue(now.Add(-apiKeyCreationWindow))
	if _, errDelete := tx.Exec(`DELETE FROM api_key_registration_attempts WHERE attempted_at < ?`, cutoff); errDelete != nil {
		return fmt.Errorf("tenancy sqlite: prune API key attempts: %w", errDelete)
	}
	var recent int
	if errScan := tx.QueryRow(`
		SELECT COUNT(1)
		FROM api_key_registration_attempts
		WHERE actor_id = ? AND attempted_at >= ?
	`, actorID, cutoff).Scan(&recent); errScan != nil {
		return fmt.Errorf("tenancy sqlite: count API key attempts: %w", errScan)
	}
	if recent >= MaxAPIKeyCreationsPerMinute {
		return ErrAPIKeyRateLimit
	}
	if _, errInsert := tx.Exec(`
		INSERT INTO api_key_registration_attempts (actor_id, attempted_at)
		VALUES (?, ?)
	`, actorID, timeValue(now)); errInsert != nil {
		return fmt.Errorf("tenancy sqlite: record API key attempt: %w", errInsert)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("tenancy sqlite: commit API key attempt: %w", errCommit)
	}
	return nil
}

// LookupByAPIKey resolves an active key hash and records its last use.
func (s *SQLiteStore) LookupByAPIKey(hash string) (*User, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	user, errScan := scanUser(s.db.QueryRow(`
		SELECT u.id, u.email, u.display_name, u.role, u.tier,
		       u.disabled, u.created_at, u.updated_at
		FROM user_api_keys AS k
		JOIN users AS u ON u.id = k.user_id
		WHERE k.key_hash = ? AND k.revoked_at IS NULL
	`, hash))
	if errScan != nil {
		return nil, errScan
	}
	if _, errExec := s.db.Exec(
		`UPDATE user_api_keys SET last_used_at = ? WHERE key_hash = ?`,
		timeValue(time.Now().UTC()),
		hash,
	); errExec != nil {
		return nil, fmt.Errorf("tenancy sqlite: update API key last use: %w", errExec)
	}
	return user, nil
}

// RevokeAPIKey permanently revokes a key hash.
func (s *SQLiteStore) RevokeAPIKey(hash string) error {
	hash = strings.ToLower(strings.TrimSpace(hash))
	result, errExec := s.db.Exec(`
		UPDATE user_api_keys
		SET revoked_at = ?
		WHERE key_hash = ? AND revoked_at IS NULL
	`, timeValue(time.Now().UTC()), hash)
	if errExec != nil {
		return fmt.Errorf("tenancy sqlite: revoke API key: %w", errExec)
	}
	return requireAffected(result, "API key", hash)
}

// ListAPIKeys lists key metadata without plaintext key material.
func (s *SQLiteStore) ListAPIKeys(userID string) ([]APIKey, error) {
	rows, errQuery := s.db.Query(`
		SELECT key_hash, user_id, label, created_at, last_used_at, revoked_at
		FROM user_api_keys
		WHERE user_id = ?
		ORDER BY created_at, key_hash
	`, userID)
	if errQuery != nil {
		return nil, fmt.Errorf("tenancy sqlite: list API keys: %w", errQuery)
	}
	defer func() {
		_ = rows.Close()
	}()

	keys := make([]APIKey, 0)
	for rows.Next() {
		var key APIKey
		var createdAt int64
		var lastUsedAt, revokedAt sql.NullInt64
		if errScan := rows.Scan(
			&key.KeyHash,
			&key.UserID,
			&key.Label,
			&createdAt,
			&lastUsedAt,
			&revokedAt,
		); errScan != nil {
			return nil, fmt.Errorf("tenancy sqlite: scan API key: %w", errScan)
		}
		key.CreatedAt = timeFromValue(createdAt)
		if lastUsedAt.Valid {
			value := timeFromValue(lastUsedAt.Int64)
			key.LastUsed = &value
		}
		if revokedAt.Valid {
			value := timeFromValue(revokedAt.Int64)
			key.RevokedAt = &value
		}
		keys = append(keys, key)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("tenancy sqlite: iterate API keys: %w", errRows)
	}
	return keys, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(scanner rowScanner) (*User, error) {
	var user User
	var disabled int
	var createdAt, updatedAt int64
	errScan := scanner.Scan(
		&user.ID,
		&user.Email,
		&user.DisplayName,
		&user.Role,
		&user.Tier,
		&disabled,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(errScan, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: user", ErrNotFound)
	}
	if errScan != nil {
		return nil, fmt.Errorf("tenancy sqlite: scan user: %w", errScan)
	}
	user.Disabled = disabled != 0
	user.CreatedAt = timeFromValue(createdAt)
	user.UpdatedAt = timeFromValue(updatedAt)
	return &user, nil
}

func normalizeRole(role string) (string, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		role = RoleUser
	}
	if role != RoleAdmin && role != RoleUser {
		return "", fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
	return role, nil
}

func randomIdentifier(prefix string, byteCount int) (string, error) {
	value := make([]byte, byteCount)
	if _, errRead := rand.Read(value); errRead != nil {
		return "", errRead
	}
	return prefix + hex.EncodeToString(value), nil
}

func requireAffected(result sql.Result, kind, id string) error {
	affected, errRows := result.RowsAffected()
	if errRows != nil {
		return fmt.Errorf("tenancy sqlite: inspect %s mutation: %w", kind, errRows)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s %q", ErrNotFound, kind, id)
	}
	return nil
}
