package tenancy

import (
	"context"
	"errors"
	"time"
)

const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

var (
	ErrNotFound    = errors.New("tenancy: not found")
	ErrInvalidRole = errors.New("tenancy: invalid role")
)

// User is one tenant that may authenticate to the proxy.
type User struct {
	ID          string
	Email       string
	DisplayName string
	Role        string
	Tier        string
	Disabled    bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// APIKey contains stored metadata for a user API key. KeyHash is the only key
// material retained after issuance.
type APIKey struct {
	KeyHash   string
	UserID    string
	Label     string
	CreatedAt time.Time
	LastUsed  *time.Time
	RevokedAt *time.Time
}

// UsageEntry is one append-only usage ledger row.
type UsageEntry struct {
	ID           int64
	UserID       string
	AuthID       string
	Provider     string
	Model        string
	CostNanoUSD  int64
	InputTokens  int64
	OutputTokens int64
	Failed       bool
	OccurredAt   time.Time
}

// QuotaWindow is the latest provider quota window observed for an auth.
type QuotaWindow struct {
	AuthID      string
	Provider    string
	WindowStart time.Time
	WindowEnd   time.Time
	UsedUnits   int64
	LimitUnits  int64
	Source      string
	UpdatedAt   time.Time
}

// CredentialValidation tracks the most recent real-traffic validation result
// for one user-owned credential.
type CredentialValidation struct {
	AuthID        string
	UserID        string
	LastAttemptAt time.Time
	LastOKAt      *time.Time
	LastStatus    string
}

// Store is the durable persistence surface used by tenancy services.
type Store interface {
	Close() error

	CreateUser(user *User) error
	GetUser(id string) (*User, error)
	UpdateUser(user *User) error
	DeleteUser(id string) error
	ListUsers() ([]User, error)

	IssueAPIKey(userID, label string) (plaintext string, key *APIKey, err error)
	LookupByAPIKey(hash string) (*User, error)
	RevokeAPIKey(hash string) error
	ListAPIKeys(userID string) ([]APIKey, error)

	AppendUsage(ctx context.Context, entries []UsageEntry) error
	UsedUnits(ctx context.Context, userID string, since time.Time) (int64, error)
	OldestUserUsage(ctx context.Context, userID string, since time.Time) (time.Time, bool, error)
	EarliestAuthUsage(ctx context.Context, authID, provider string, since time.Time) (time.Time, bool, error)

	UpsertQuotaWindow(ctx context.Context, window QuotaWindow) error
	ListQuotaWindows(ctx context.Context) ([]QuotaWindow, error)

	UpsertCredentialValidation(ctx context.Context, validation CredentialValidation) error
	GetCredentialValidation(ctx context.Context, authID string) (*CredentialValidation, error)
}
