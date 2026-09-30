package user

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/tenancy"
)

func TestUsagePlanningIsolationCacheAndValidation(t *testing.T) {
	h := newUserTestHarness(t, true)
	now := time.Now().UTC()
	h.seedUsage(t, "user-a", tenancy.UsageEntry{CostNanoUSD: 123, OccurredAt: now.Add(-time.Hour)})
	h.seedUsage(t, "user-b", tenancy.UsageEntry{CostNanoUSD: 999, OccurredAt: now.Add(-time.Hour)})
	for _, path := range []string{"/usage/timeline?days=7", "/usage/releases?hours=48"} {
		response := h.request(t, "user-a", http.MethodGet, "/v0/user"+path, nil)
		if response.Code != 200 {
			t.Fatalf("%s: %s", path, response.Body.String())
		}
		payload := decodeBody(t, response.Body.Bytes())
		var total int
		for _, raw := range payload["buckets"].([]any) {
			row := raw.(map[string]any)
			if row["cost_nano_usd"] == "123" {
				total++
			} else if row["cost_nano_usd"] != "0" {
				t.Fatalf("unexpected amount: %v", row)
			}
		}
		if total != 1 {
			t.Fatalf("missing usage: %v", payload)
		}
		cached := h.request(t, "user-a", http.MethodGet, "/v0/user"+path, nil)
		if response.Body.String() != cached.Body.String() {
			t.Fatal("snapshot was not cached")
		}
	}
	for _, query := range []string{"resolution=1m", "days=8", "days=0", "resolution=15m", "from=bad&to=bad"} {
		response := h.request(t, "user-a", http.MethodGet, "/v0/user/usage/timeline?"+query, nil)
		if response.Code != 400 {
			t.Fatalf("query %s status %d", query, response.Code)
		}
	}
	response := h.request(t, "user-a", http.MethodGet, "/v0/user/usage/timeline?resolution=15m&days=1", nil)
	if response.Code != 200 {
		t.Fatalf("15m detail: %s", response.Body.String())
	}
}

func TestEmptyQuotaHasNoReset(t *testing.T) {
	h := newUserTestHarness(t, true)
	response := h.request(t, "user-a", http.MethodGet, "/v0/user/me", nil)
	payload := decodeBody(t, response.Body.Bytes())
	quota := payload["quota"].(map[string]any)
	if quota["reset_at"] != nil || quota["next_release_at"] != nil || quota["mode"] != "rolling" {
		t.Fatalf("empty quota: %v", quota)
	}
}

func TestReleaseForecastExcludesExpiredAndBeyondHorizon(t *testing.T) {
	h := newUserTestHarness(t, true)
	now := time.Now().UTC()
	// The test configuration uses a 24-hour rolling window.
	h.seedUsage(t, "user-a",
		tenancy.UsageEntry{CostNanoUSD: 10, OccurredAt: now.Add(-25 * time.Hour)},
		tenancy.UsageEntry{CostNanoUSD: 20, OccurredAt: now.Add(-23 * time.Hour)},
		tenancy.UsageEntry{CostNanoUSD: 30, OccurredAt: now.Add(-time.Hour)},
	)
	response := h.request(t, "user-a", http.MethodGet, "/v0/user/usage/releases?hours=2", nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	payload := decodeBody(t, response.Body.Bytes())
	if payload["used_nano_usd"] != "50" {
		t.Fatalf("forecast origin used = %v", payload["used_nano_usd"])
	}
	found := 0
	for _, raw := range payload["buckets"].([]any) {
		row := raw.(map[string]any)
		if row["cost_nano_usd"] == "20" {
			found++
		} else if row["cost_nano_usd"] != "0" {
			t.Fatalf("unexpected release: %v", row)
		}
	}
	if found != 1 {
		t.Fatalf("release amount missing: %v", payload)
	}
	h.seedUsage(t, "user-a", tenancy.UsageEntry{CostNanoUSD: 100, OccurredAt: now})
	cached := h.request(t, "user-a", http.MethodGet, "/v0/user/usage/releases?hours=2", nil)
	if cached.Body.String() != response.Body.String() {
		t.Fatal("cached balance and releases must remain one snapshot")
	}
}

func TestPlanningRangeSupportsAgingChartDrilldown(t *testing.T) {
	snapshot := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		releases      bool
		from, to, now time.Time
	}{
		{"oldest history bucket", false, snapshot.Add(-7 * 24 * time.Hour), snapshot.Add(-7*24*time.Hour + 30*time.Minute), snapshot.Add(20 * time.Second)},
		{"partially elapsed release bucket", true, snapshot, snapshot.Add(30 * time.Minute), snapshot.Add(2 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			query := url.Values{"resolution": {"15m"}, "from": {tc.from.Format(time.RFC3339Nano)}, "to": {tc.to.Format(time.RFC3339Nano)}}
			c.Request = httptest.NewRequest(http.MethodGet, "/?"+query.Encode(), nil)
			from, to, _, errRange := planningRange(c, tc.now, tc.releases)
			if errRange != nil {
				t.Fatal(errRange)
			}
			if tc.releases && from.Before(tc.now) {
				t.Fatal("elapsed releases must be clipped")
			}
			if !to.Equal(tc.to) {
				t.Fatal("interval end changed")
			}
		})
	}
}

func TestPlanningCacheUsesValidatedQuerySemantics(t *testing.T) {
	h := newUserTestHarness(t, true)
	first := h.request(t, "user-a", http.MethodGet, "/v0/user/usage/timeline?days=7", nil)
	equivalent := h.request(t, "user-a", http.MethodGet, "/v0/user/usage/timeline?unused=cache-buster&resolution=1h&days=07", nil)
	if first.Code != http.StatusOK || equivalent.Code != http.StatusOK || first.Body.String() != equivalent.Body.String() {
		t.Fatal("equivalent queries missed the planning cache")
	}
}

func TestPlanningDoesNotRetryIndependentCancellation(t *testing.T) {
	h := &Handler{}
	calls := 0
	_, errLoad := h.loadPlanning(context.Background(), "same-user", func(context.Context) (any, error) { calls++; return nil, context.Canceled })
	if !errors.Is(errLoad, context.Canceled) || calls != 1 {
		t.Fatalf("independent error loop: err=%v calls=%d", errLoad, calls)
	}
}

// Done is evaluated by loadPlanning's select after joining the shared call.
// This lets cancellation tests synchronize without wall-clock sleeps.
type planningWaitContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *planningWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func TestPlanningRetriesOnlySurvivingWaiter(t *testing.T) {
	h := &Handler{}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	started := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, errLoad := h.loadPlanning(leaderCtx, "same-user", func(ctx context.Context) (any, error) {
			close(started)
			<-ctx.Done()
			return nil, errors.New("driver interrupted without wrapping context.Canceled")
		})
		leaderDone <- errLoad
	}()
	<-started
	followerCtx := &planningWaitContext{Context: context.Background(), joined: make(chan struct{})}
	followerDone := make(chan error, 1)
	go func() {
		value, errLoad := h.loadPlanning(followerCtx, "same-user", func(context.Context) (any, error) { return "snapshot", nil })
		if errLoad == nil && value != "snapshot" {
			errLoad = fmt.Errorf("unexpected snapshot %v", value)
		}
		followerDone <- errLoad
	}()
	<-followerCtx.joined
	cancelLeader()
	if errLoad := <-leaderDone; !errors.Is(errLoad, context.Canceled) {
		t.Errorf("leader=%v", errLoad)
	}
	if errLoad := <-followerDone; errLoad != nil {
		t.Fatalf("follower=%v", errLoad)
	}
}

func TestPlanningCanceledWaiterDoesNotWaitForLeader(t *testing.T) {
	h := &Handler{}
	started, release := make(chan struct{}), make(chan struct{})
	leader := h.planningFetches.DoChan("same-user", func() (any, error) { close(started); <-release; return "snapshot", nil })
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, errLoad := h.loadPlanning(ctx, "same-user", func(context.Context) (any, error) { return nil, errors.New("unexpected replacement leader") })
		waiterDone <- errLoad
	}()
	cancel()
	errLoad := <-waiterDone
	close(release)
	result := <-leader
	if !errors.Is(errLoad, context.Canceled) || result.Err != nil || result.Val != "snapshot" {
		t.Fatalf("waiter=%v leader=%+v", errLoad, result)
	}
}
