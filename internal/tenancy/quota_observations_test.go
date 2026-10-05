package tenancy

import (
	"context"
	"testing"
	"time"
)

func TestQuotaObservationCanonicalDeduplication(t *testing.T) {
	store, errOpen := OpenSQLitePath(":memory:")
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer store.Close()
	now := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	item := QuotaObservation{AuthID: "alias-a", Provider: "Claude", QuotaScope: "weekly", WindowKind: "weekly", NativeUnit: "percent", ObservedAt: now, Source: "oauth"}
	if errAppend := store.AppendQuotaObservation(context.Background(), item); errAppend != nil {
		t.Fatal(errAppend)
	}
	item.AuthID = "alias-b"
	item.CanonicalAuthID = CanonicalAuthID("claude", "account-1")
	if errAppend := store.AppendQuotaObservation(context.Background(), item); errAppend != nil {
		t.Fatal(errAppend)
	}
	item.AuthID = "alias-c"
	item.ObservedAt = now.Add(time.Minute)
	if errAppend := store.AppendQuotaObservation(context.Background(), item); errAppend != nil {
		t.Fatal(errAppend)
	}
	rows, errList := store.ListQuotaObservations(context.Background(), CanonicalAuthID("claude", "account-1"), "weekly", 10)
	if errList != nil {
		t.Fatal(errList)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want two time-separated observations", len(rows))
	}
	if CanonicalAuthID("Claude", "account-1") != "claude:account-1" {
		t.Fatal("canonical identity normalization changed")
	}
}
