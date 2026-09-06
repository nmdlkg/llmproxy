package tenancy

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRegisterAPIKeyHashValidatesAndStoresHashOnly(t *testing.T) {
	store := newTestStore(t)
	user := &User{ID: "hash-user", Email: "hash-user@example.com", Role: RoleUser, Tier: "default"}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	plaintext := "cp_u_browser-only-secret"
	hash := HashAPIKey(plaintext)
	key, errRegister := store.RegisterAPIKeyHash(user.ID, hash, "browser")
	if errRegister != nil {
		t.Fatalf("RegisterAPIKeyHash() error = %v", errRegister)
	}
	if key.KeyHash != hash || key.Label != "browser" {
		t.Fatalf("registered key = %#v, want hash metadata", key)
	}
	var stored string
	if errScan := store.db.QueryRow(`SELECT key_hash FROM user_api_keys WHERE user_id = ?`, user.ID).Scan(&stored); errScan != nil {
		t.Fatalf("load stored key hash: %v", errScan)
	}
	if stored != hash || stored == plaintext {
		t.Fatalf("stored key = %q, want hash only", stored)
	}
	if _, errDuplicate := store.RegisterAPIKeyHash(user.ID, hash, "duplicate"); !errors.Is(errDuplicate, ErrAPIKeyExists) {
		t.Fatalf("duplicate registration error = %v, want ErrAPIKeyExists", errDuplicate)
	}

	for _, invalid := range []string{"", strings.ToUpper(hash), hash[:63], hash + "0", strings.Repeat("g", 64)} {
		if _, errInvalid := store.RegisterAPIKeyHash(user.ID, invalid, "invalid"); !errors.Is(errInvalid, ErrInvalidAPIKeyHash) {
			t.Errorf("RegisterAPIKeyHash(%q) error = %v, want ErrInvalidAPIKeyHash", invalid, errInvalid)
		}
	}
}

func TestRegisterAPIKeyHashCreationRateIsPerActor(t *testing.T) {
	store := newTestStore(t)
	user := &User{ID: "rate-user", Email: "rate-user@example.com", Role: RoleUser, Tier: "default"}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	for index := 0; index < MaxAPIKeyCreationsPerMinute; index++ {
		hash := HashAPIKey(fmt.Sprintf("cp_u_rate-%d", index))
		if _, errRegister := store.RegisterAPIKeyHashForActor("actor-a", user.ID, hash, "rate"); errRegister != nil {
			t.Fatalf("registration %d error = %v", index, errRegister)
		}
	}
	if _, errRegister := store.RegisterAPIKeyHashForActor("actor-a", user.ID, HashAPIKey("cp_u_rate-over"), "rate"); !errors.Is(errRegister, ErrAPIKeyRateLimit) {
		t.Fatalf("sixth registration error = %v, want ErrAPIKeyRateLimit", errRegister)
	}
	// A different actor has its own five-registration budget even when it
	// targets the same user.
	if _, errRegister := store.RegisterAPIKeyHashForActor("actor-b", user.ID, HashAPIKey("cp_u_rate-other-actor"), "rate"); errRegister != nil {
		t.Fatalf("different actor registration error = %v", errRegister)
	}
}

func TestRegisterAPIKeyHashRateCountsRejectedValidHashAttempts(t *testing.T) {
	store := newTestStore(t)
	user := &User{ID: "failed-rate-user", Email: "failed-rate-user@example.com", Role: RoleUser, Tier: "default"}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	hash := HashAPIKey("cp_u_rate-duplicate")
	if _, errRegister := store.RegisterAPIKeyHash(user.ID, hash, "first"); errRegister != nil {
		t.Fatalf("initial registration error = %v", errRegister)
	}
	for index := 1; index < MaxAPIKeyCreationsPerMinute; index++ {
		if _, errDuplicate := store.RegisterAPIKeyHash(user.ID, hash, "duplicate"); !errors.Is(errDuplicate, ErrAPIKeyExists) {
			t.Fatalf("duplicate attempt %d error = %v, want ErrAPIKeyExists", index, errDuplicate)
		}
	}
	if _, errRegister := store.RegisterAPIKeyHash(user.ID, HashAPIKey("cp_u_rate-after-failures"), "over"); !errors.Is(errRegister, ErrAPIKeyRateLimit) {
		t.Fatalf("attempt after rejected registrations = %v, want ErrAPIKeyRateLimit", errRegister)
	}
}

func TestRegisterAPIKeyHashEnforcesActiveCap(t *testing.T) {
	store := newTestStore(t)
	user := &User{ID: "cap-user", Email: "cap-user@example.com", Role: RoleUser, Tier: "default"}
	if errCreate := store.CreateUser(user); errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	now := time.Now().UTC()
	for index := 0; index < MaxActiveAPIKeys; index++ {
		hash := HashAPIKey(fmt.Sprintf("cp_u_cap-%d", index))
		if _, errInsert := store.db.Exec(`
			INSERT INTO user_api_keys (key_hash, user_id, created_by, label, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, hash, user.ID, "seed", "seed", timeValue(now.Add(-time.Duration(index+1)*time.Minute))); errInsert != nil {
			t.Fatalf("seed key %d: %v", index, errInsert)
		}
	}
	if _, errRegister := store.RegisterAPIKeyHash(user.ID, HashAPIKey("cp_u_cap-over"), "cap"); !errors.Is(errRegister, ErrAPIKeyLimit) {
		t.Fatalf("over-cap registration error = %v, want ErrAPIKeyLimit", errRegister)
	}
	if errRevoke := store.RevokeAPIKey(HashAPIKey("cp_u_cap-0")); errRevoke != nil {
		t.Fatalf("RevokeAPIKey() error = %v", errRevoke)
	}
	if _, errRegister := store.RegisterAPIKeyHash(user.ID, HashAPIKey("cp_u_cap-after-revoke"), "cap"); errRegister != nil {
		t.Fatalf("registration after revoke error = %v, want success", errRegister)
	}
}
