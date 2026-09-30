package tenancy

import (
	"context"
	"testing"
	"time"
)

func TestUsageBucketsBoundariesAndReleaseShift(t *testing.T) {
	store := newTestStore(t)
	for _, id := range []string{"a", "b"} {
		if errCreate := store.CreateUser(&User{ID: id, Email: id + "@test.local", Role: RoleUser}); errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	entries := []UsageEntry{
		{UserID: "a", CostNanoUSD: 1, OccurredAt: start.Add(-time.Nanosecond)},
		{UserID: "a", CostNanoUSD: 2, InputTokens: 3, OccurredAt: start},
		{UserID: "a", CostNanoUSD: 4, OutputTokens: 5, OccurredAt: start.Add(45 * time.Minute)},
		{UserID: "a", CostNanoUSD: 8, OccurredAt: start.Add(time.Hour)},
		{UserID: "a", CostNanoUSD: 16, OccurredAt: start.Add(2 * time.Hour)},
		{UserID: "b", CostNanoUSD: 32, OccurredAt: start},
	}
	if errAppend := store.AppendUsage(context.Background(), entries); errAppend != nil {
		t.Fatal(errAppend)
	}
	rows, errQuery := store.UsageBuckets(context.Background(), "a", start, start.Add(2*time.Hour), time.Hour, 0)
	if errQuery != nil {
		t.Fatal(errQuery)
	}
	if len(rows) != 2 || rows[0].CostNanoUSD != 6 || rows[1].CostNanoUSD != 8 || rows[0].Attempts != 2 || rows[0].InputTokens != 3 || rows[0].OutputTokens != 5 {
		t.Fatalf("history: %+v", rows)
	}
	// A non-hour-aligned quota window must shift before grouping, not after.
	rows, errQuery = store.UsageBuckets(context.Background(), "a", start, start.Add(2*time.Hour), time.Hour, 168*time.Hour+30*time.Minute)
	if errQuery != nil {
		t.Fatal(errQuery)
	}
	if len(rows) != 2 || rows[0].CostNanoUSD != 2 || rows[1].CostNanoUSD != 12 || !rows[0].Start.Equal(start.Add(168*time.Hour)) {
		t.Fatalf("releases: %+v", rows)
	}
}

func TestReleaseUsageBucketsOriginAndConcurrentWrites(t *testing.T) {
	store := newTestStore(t)
	if errCreate := store.CreateUser(&User{ID: "a", Email: "a@test.local", Role: RoleUser}); errCreate != nil {
		t.Fatal(errCreate)
	}
	ctx := context.Background()
	asOf := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if errAppend := store.AppendUsage(ctx, []UsageEntry{
		{UserID: "a", CostNanoUSD: 7, OccurredAt: asOf.Add(-time.Hour)},
		{UserID: "a", CostNanoUSD: 1000, OccurredAt: asOf},
		{UserID: "a", CostNanoUSD: 2000, OccurredAt: asOf.Add(time.Hour)},
		{UserID: "a", CostNanoUSD: 4000, OccurredAt: asOf.Add(-25 * time.Hour)},
	}); errAppend != nil {
		t.Fatal(errAppend)
	}
	rows, used, errQuery := store.ReleaseUsageBuckets(ctx, "a", asOf, asOf.Add(24*time.Hour), asOf, time.Hour, 24*time.Hour)
	if errQuery != nil {
		t.Fatal(errQuery)
	}
	if used != 7 || len(rows) != 1 || rows[0].CostNanoUSD != 7 {
		t.Fatalf("origin includes out-of-range rows: used=%d rows=%+v", used, rows)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			if errAppend := store.AppendUsage(ctx, []UsageEntry{{UserID: "a", CostNanoUSD: 1, OccurredAt: asOf.Add(-time.Hour)}}); errAppend != nil {
				done <- errAppend
				return
			}
		}
		done <- nil
	}()
	// Every inserted row releases within the requested full window. A response's
	// origin balance must equal its sum even when ledger batches arrive between reads.
	for i := 0; i < 100; i++ {
		rows, used, errQuery = store.ReleaseUsageBuckets(ctx, "a", asOf, asOf.Add(24*time.Hour), asOf, time.Hour, 24*time.Hour)
		if errQuery != nil {
			t.Error(errQuery)
			break
		}
		var released int64
		for _, row := range rows {
			released += row.CostNanoUSD
		}
		if released != used {
			t.Errorf("mixed snapshots: releases=%d used=%d", released, used)
			break
		}
	}
	if errAppend := <-done; errAppend != nil {
		t.Fatal(errAppend)
	}
}
