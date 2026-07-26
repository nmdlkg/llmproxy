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

// HashAPIKey returns the SHA-256 hex digest used for API key persistence.
func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
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
	user.Email = strings.TrimSpace(user.Email)
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
	user.Email = strings.TrimSpace(user.Email)
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
	if _, errUser := s.GetUser(userID); errUser != nil {
		return "", nil, errUser
	}
	randomBytes := make([]byte, 32)
	if _, errRead := rand.Read(randomBytes); errRead != nil {
		return "", nil, fmt.Errorf("tenancy sqlite: generate API key: %w", errRead)
	}
	plaintext := "cp_u_" + base64.RawURLEncoding.EncodeToString(randomBytes)
	key := &APIKey{
		KeyHash:   HashAPIKey(plaintext),
		UserID:    userID,
		Label:     strings.TrimSpace(label),
		CreatedAt: time.Now().UTC(),
	}
	if _, errExec := s.db.Exec(`
		INSERT INTO user_api_keys (key_hash, user_id, label, created_at)
		VALUES (?, ?, ?, ?)
	`, key.KeyHash, key.UserID, key.Label, timeValue(key.CreatedAt)); errExec != nil {
		return "", nil, fmt.Errorf("tenancy sqlite: issue API key: %w", errExec)
	}
	return plaintext, key, nil
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
