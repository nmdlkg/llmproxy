package tenancy

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseValidationInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value   string
		want    time.Duration
		enabled bool
	}{
		{value: "1h", want: time.Hour, enabled: true},
		{value: " 30m ", want: 30 * time.Minute, enabled: true},
		{value: "", enabled: false},
		{value: "not-a-duration", enabled: false},
		{value: "0s", enabled: false},
		{value: "-1s", enabled: false},
	}
	for _, test := range tests {
		got, enabled := ParseValidationInterval(test.value)
		if enabled != test.enabled || got != test.want {
			t.Errorf("ParseValidationInterval(%q) = (%v, %t), want (%v, %t)", test.value, got, enabled, test.want, test.enabled)
		}
	}
}

func TestCredentialValidatorDueBoundaryUsesLatestSuccess(t *testing.T) {
	store := newTestStore(t)
	userID := "user-1"
	base := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	oldSuccess := base.Add(-2 * time.Hour)
	freshSuccess := base
	for _, validation := range []CredentialValidation{
		{
			AuthID:        "auth-a",
			UserID:        userID,
			LastAttemptAt: oldSuccess,
			LastOKAt:      &oldSuccess,
			LastStatus:    "ok",
		},
		{
			AuthID:        "auth-b",
			UserID:        userID,
			LastAttemptAt: freshSuccess,
			LastOKAt:      &freshSuccess,
			LastStatus:    "ok",
		},
	} {
		if errUpsert := store.UpsertCredentialValidation(context.Background(), validation); errUpsert != nil {
			t.Fatalf("UpsertCredentialValidation() error = %v", errUpsert)
		}
	}

	validator := newCredentialValidator(store, func(string) ([]Credential, error) {
		return []Credential{{AuthID: "auth-b"}, {AuthID: "auth-a"}}, nil
	})
	now := base.Add(time.Hour - time.Nanosecond)
	validator.now = func() time.Time { return now }

	got, errPreferred := validator.preferredAuthIDs(userID, time.Hour)
	if errPreferred != nil {
		t.Fatalf("preferredAuthIDs() error = %v", errPreferred)
	}
	if len(got) != 0 {
		t.Fatalf("preferredAuthIDs() before boundary = %#v, want none", got)
	}

	now = base.Add(time.Hour)
	got, errPreferred = validator.preferredAuthIDs(userID, time.Hour)
	if errPreferred != nil {
		t.Fatalf("preferredAuthIDs() at boundary error = %v", errPreferred)
	}
	if want := []string{"auth-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("preferredAuthIDs() at boundary = %#v, want %#v", got, want)
	}
}

func TestCredentialValidatorRoundRobinsCredentials(t *testing.T) {
	store := newTestStore(t)
	validator := newCredentialValidator(store, func(string) ([]Credential, error) {
		return []Credential{
			{AuthID: "auth-b"},
			{AuthID: "auth-a"},
			{AuthID: "auth-a"},
		}, nil
	})
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	validator.now = func() time.Time { return now }

	for index, want := range [][]string{{"auth-a"}, {"auth-b"}, {"auth-a"}} {
		got, errPreferred := validator.preferredAuthIDs("user-1", time.Hour)
		if errPreferred != nil {
			t.Fatalf("preferredAuthIDs() call %d error = %v", index, errPreferred)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("preferredAuthIDs() call %d = %#v, want %#v", index, got, want)
		}
		now = now.Add(time.Hour)
	}
}

func TestCredentialValidatorZeroCredentialsIsNeverDue(t *testing.T) {
	store := &countingValidationStore{Store: newTestStore(t)}
	validator := newCredentialValidator(store, func(string) ([]Credential, error) {
		return nil, nil
	})

	got, errPreferred := validator.preferredAuthIDs("user-1", time.Hour)
	if errPreferred != nil {
		t.Fatalf("preferredAuthIDs() error = %v", errPreferred)
	}
	if len(got) != 0 {
		t.Fatalf("preferredAuthIDs() = %#v, want none", got)
	}
	if calls := store.getCalls.Load(); calls != 0 {
		t.Fatalf("GetCredentialValidation() calls = %d, want 0", calls)
	}
}

func TestCredentialValidatorConsultsStoreOnlyWhenCredentialSetChanges(t *testing.T) {
	store := &countingValidationStore{Store: newTestStore(t)}
	credentials := []Credential{{AuthID: "auth-a"}}
	validator := newCredentialValidator(store, func(string) ([]Credential, error) {
		return append([]Credential(nil), credentials...), nil
	})
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	validator.now = func() time.Time { return now }

	if _, errPreferred := validator.preferredAuthIDs("user-1", time.Hour); errPreferred != nil {
		t.Fatalf("first preferredAuthIDs() error = %v", errPreferred)
	}
	if _, errPreferred := validator.preferredAuthIDs("user-1", time.Hour); errPreferred != nil {
		t.Fatalf("second preferredAuthIDs() error = %v", errPreferred)
	}
	if calls := store.getCalls.Load(); calls != 1 {
		t.Fatalf("GetCredentialValidation() calls after stable credentials = %d, want 1", calls)
	}

	credentials = append(credentials, Credential{AuthID: "auth-b"})
	if _, errPreferred := validator.preferredAuthIDs("user-1", time.Hour); errPreferred != nil {
		t.Fatalf("preferredAuthIDs() after credential change error = %v", errPreferred)
	}
	if calls := store.getCalls.Load(); calls != 3 {
		t.Fatalf("GetCredentialValidation() calls after credential change = %d, want 3", calls)
	}
}

func TestCredentialValidatorConcurrentAccess(t *testing.T) {
	store := newTestStore(t)
	validator := newCredentialValidator(store, func(string) ([]Credential, error) {
		return []Credential{{AuthID: "auth-a"}, {AuthID: "auth-b"}}, nil
	})
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	validator.now = func() time.Time { return now }
	if _, errPreferred := validator.preferredAuthIDs("user-1", time.Hour); errPreferred != nil {
		t.Fatalf("initial preferredAuthIDs() error = %v", errPreferred)
	}

	const goroutines = 32
	errorsCh := make(chan error, goroutines)
	var waitGroup sync.WaitGroup
	for index := 0; index < goroutines; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			if index%2 == 0 {
				_, errPreferred := validator.preferredAuthIDs("user-1", time.Hour)
				errorsCh <- errPreferred
				return
			}
			authID := "auth-a"
			if index%4 == 3 {
				authID = "auth-b"
			}
			errorsCh <- validator.recordOutcome("user-1", authID, index%3 == 0, 401, now)
		}(index)
	}
	waitGroup.Wait()
	close(errorsCh)
	for errValidation := range errorsCh {
		if errValidation != nil {
			t.Fatalf("concurrent validation error = %v", errValidation)
		}
	}
}

type countingValidationStore struct {
	Store
	getCalls atomic.Int32
}

func (s *countingValidationStore) GetCredentialValidation(ctx context.Context, authID string) (*CredentialValidation, error) {
	s.getCalls.Add(1)
	return s.Store.GetCredentialValidation(ctx, authID)
}
