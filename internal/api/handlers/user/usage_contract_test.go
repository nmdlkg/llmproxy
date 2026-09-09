package user

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
)

func TestDenseDailyUsageReturnsEveryUTCDateWithExactZeroes(t *testing.T) {
	since := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC)
	sparse := []tenancy.UsageDailyStat{
		{Day: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), CostNanoUSD: 1000000001, Attempts: 1},
		{Day: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), CostNanoUSD: 2, Attempts: 2},
	}
	buckets := denseDailyUsage(since, until, sparse)
	if len(buckets) != 4 {
		t.Fatalf("denseDailyUsage() returned %d buckets, want 4", len(buckets))
	}
	for index, bucket := range buckets {
		wantDay := time.Date(2026, 8, 10+index, 0, 0, 0, 0, time.UTC)
		if !bucket.Day.Equal(wantDay) {
			t.Errorf("bucket %d day = %v, want %v", index, bucket.Day, wantDay)
		}
	}
	if buckets[0].CostNanoUSD != 1000000001 || buckets[1].CostNanoUSD != 0 || buckets[2].CostNanoUSD != 2 || buckets[3].CostNanoUSD != 0 {
		t.Fatalf("dense costs = %#v, want 1000000001,0,2,0", buckets)
	}
	if got := formatNanoUSDDecimal(1000000001); got != "1000000001" {
		t.Fatalf("formatNanoUSDDecimal() = %q, want exact decimal string", got)
	}
}

func TestDenseDailyUsageExcludesUntilMidnight(t *testing.T) {
	since := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	buckets := denseDailyUsage(since, until, nil)
	if len(buckets) != 2 {
		t.Fatalf("denseDailyUsage() returned %d buckets, want 2", len(buckets))
	}
	if !buckets[0].Day.Equal(since) || !buckets[1].Day.Equal(since.Add(24*time.Hour)) {
		t.Fatalf("bucket days = %v, %v", buckets[0].Day, buckets[1].Day)
	}
}
