package tenancy

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestBalancingRuntimePublishesSnapshotAndKeepsMissingWindowBasePriority(t *testing.T) {
	store := newTestStore(t)
	manager := coreauth.NewManager(nil, nil, nil)
	registerTestAuth(t, manager, "urgent-auth", "3")
	registerTestAuth(t, manager, "no-window-auth", "8")

	now := time.Now().UTC()
	if errUpsert := store.UpsertQuotaWindow(context.Background(), QuotaWindow{
		AuthID:      "urgent-auth",
		Provider:    "gemini",
		WindowStart: now.Add(-time.Hour),
		WindowEnd:   now.Add(10 * time.Minute),
		UsedUnits:   20,
		LimitUnits:  100,
		Source:      "test",
		UpdatedAt:   now,
	}); errUpsert != nil {
		t.Fatalf("UpsertQuotaWindow() error = %v", errUpsert)
	}

	runtime := startBalancingRuntime(
		store,
		Balancing{UrgencyHorizon: 30 * time.Minute, HighWater: 0.9, UrgencyBonus: 2},
		manager,
		time.Hour,
	)
	t.Cleanup(runtime.stopAndUninstall)
	requestBalancingRecompute(t, runtime)

	if priority, ok := runtime.resolvePriority(&coreauth.Auth{ID: "urgent-auth"}); !ok || priority != 5 {
		t.Fatalf("urgent priority = (%d, %v), want (5, true)", priority, ok)
	}
	if priority, ok := runtime.resolvePriority(&coreauth.Auth{ID: "no-window-auth"}); !ok || priority != 8 {
		t.Fatalf("missing-window priority = (%d, %v), want configured base (8, true)", priority, ok)
	}
	if priority, ok := runtime.resolvePriority(&coreauth.Auth{ID: "unknown-auth"}); ok || priority != 0 {
		t.Fatalf("unknown priority = (%d, %v), want (0, false)", priority, ok)
	}

	noWindow, ok := manager.GetByID("no-window-auth")
	if !ok {
		t.Fatal("missing-window auth disappeared")
	}
	noWindow.Attributes = map[string]string{"priority": "9"}
	if _, errUpdate := manager.Update(context.Background(), noWindow); errUpdate != nil {
		t.Fatalf("Manager.Update(no-window-auth) error = %v", errUpdate)
	}
	requestBalancingRecompute(t, runtime)
	if priority, ok := runtime.resolvePriority(&coreauth.Auth{ID: "no-window-auth"}); !ok || priority != 9 {
		t.Fatalf("updated missing-window priority = (%d, %v), want configured base (9, true)", priority, ok)
	}

	updated, ok := manager.GetByID("urgent-auth")
	if !ok {
		t.Fatal("urgent auth disappeared")
	}
	updated.Attributes = map[string]string{"priority": "4"}
	if _, errUpdate := manager.Update(context.Background(), updated); errUpdate != nil {
		t.Fatalf("Manager.Update() error = %v", errUpdate)
	}
	requestBalancingRecompute(t, runtime)
	if priority, ok := runtime.resolvePriority(&coreauth.Auth{ID: "urgent-auth"}); !ok || priority != 6 {
		t.Fatalf("updated urgent priority = (%d, %v), want base 4 + bonus 2", priority, ok)
	}
}

type failingQuotaWindowStore struct {
	Store
	fail atomic.Bool
}

func (s *failingQuotaWindowStore) ListQuotaWindows(ctx context.Context) ([]QuotaWindow, error) {
	if s.fail.Load() {
		return nil, errors.New("quota window store unavailable")
	}
	return s.Store.ListQuotaWindows(ctx)
}

func TestBalancingRuntimeRecomputeFailureRetainsPublishedSnapshot(t *testing.T) {
	sqliteStore := newTestStore(t)
	store := &failingQuotaWindowStore{Store: sqliteStore}
	manager := coreauth.NewManager(nil, nil, nil)
	registerTestAuth(t, manager, "failure-auth", "2")

	now := time.Now().UTC()
	if errUpsert := store.UpsertQuotaWindow(context.Background(), QuotaWindow{
		AuthID:      "failure-auth",
		Provider:    "gemini",
		WindowStart: now.Add(-time.Hour),
		WindowEnd:   now.Add(10 * time.Minute),
		UsedUnits:   20,
		LimitUnits:  100,
		Source:      "test",
		UpdatedAt:   now,
	}); errUpsert != nil {
		t.Fatalf("UpsertQuotaWindow() error = %v", errUpsert)
	}

	runtime := startBalancingRuntime(
		store,
		Balancing{UrgencyHorizon: 30 * time.Minute, HighWater: 0.9, UrgencyBonus: 1},
		manager,
		time.Hour,
	)
	t.Cleanup(runtime.stopAndUninstall)
	requestBalancingRecompute(t, runtime)
	if priority, ok := runtime.resolvePriority(&coreauth.Auth{ID: "failure-auth"}); !ok || priority != 3 {
		t.Fatalf("initial priority = (%d, %v), want (3, true)", priority, ok)
	}

	updated, ok := manager.GetByID("failure-auth")
	if !ok {
		t.Fatal("failure auth disappeared")
	}
	updated.Attributes = map[string]string{"priority": "50"}
	if _, errUpdate := manager.Update(context.Background(), updated); errUpdate != nil {
		t.Fatalf("Manager.Update() error = %v", errUpdate)
	}
	store.fail.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if errRecompute := runtime.requestRecompute(ctx); errRecompute == nil {
		t.Fatal("requestRecompute() error = nil, want store failure")
	}
	if priority, ok := runtime.resolvePriority(&coreauth.Auth{ID: "failure-auth"}); !ok || priority != 3 {
		t.Fatalf("priority after failed recompute = (%d, %v), want retained (3, true)", priority, ok)
	}
}

func TestServicePublishesPriorityBeforeSchedulerRefreshAndCloseUninstalls(t *testing.T) {
	const (
		model          = "tenancy-balancing-close-model"
		urgentAuthID   = "tenancy-balancing-urgent"
		steadyAuthID   = "tenancy-balancing-steady"
		urgentBonus    = 2
		steadyBase     = 1
		requestTimeout = 2 * time.Second
	)

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	registerTestAuth(t, manager, urgentAuthID, "0")
	registerTestAuth(t, manager, steadyAuthID, strconv.Itoa(steadyBase))
	registerTestModels(t, manager, model, urgentAuthID, steadyAuthID)

	service, errNew := NewService(config.TenancyConfig{
		Enabled: true,
		DBPath:  t.TempDir() + "/tenancy.db",
		Balancing: config.TenancyBalancing{
			UrgencyHorizon: "30m",
			HighWater:      0.9,
			UrgencyBonus:   urgentBonus,
		},
	}, t.TempDir(), manager)
	if errNew != nil {
		t.Fatalf("NewService() error = %v", errNew)
	}
	t.Cleanup(func() {
		if errClose := service.Close(); errClose != nil {
			t.Errorf("Service.Close() error = %v", errClose)
		}
	})

	requestBalancingRecompute(t, service.balancing)
	now := time.Now().UTC()
	if errUpsert := service.Store().UpsertQuotaWindow(context.Background(), QuotaWindow{
		AuthID:      urgentAuthID,
		Provider:    "gemini",
		WindowStart: now.Add(-time.Hour),
		WindowEnd:   now.Add(10 * time.Minute),
		UsedUnits:   20,
		LimitUnits:  100,
		Source:      "test",
		UpdatedAt:   now,
	}); errUpsert != nil {
		t.Fatalf("UpsertQuotaWindow() error = %v", errUpsert)
	}
	service.TriggerBalancing()
	waitForCondition(t, requestTimeout, func() bool {
		priority, ok := service.balancing.resolvePriority(&coreauth.Auth{ID: urgentAuthID})
		return ok && priority == urgentBonus
	})
	requestBalancingRecompute(t, service.balancing)

	selectedID := executeSelectedAuth(t, manager, model)
	if selectedID != urgentAuthID {
		t.Fatalf("selected auth with balancing = %q, want %q", selectedID, urgentAuthID)
	}

	done := service.balancing.done
	if errClose := service.Close(); errClose != nil {
		t.Fatalf("Service.Close() error = %v", errClose)
	}
	select {
	case <-done:
	default:
		t.Fatal("balancing goroutine still running after Service.Close()")
	}

	selectedID = executeSelectedAuth(t, manager, model)
	if selectedID != steadyAuthID {
		t.Fatalf("selected auth after Close = %q, want configured-priority auth %q", selectedID, steadyAuthID)
	}
}

func TestNewServiceWithNilAuthManagerDoesNotStartBalancing(t *testing.T) {
	service, errNew := NewService(config.TenancyConfig{
		Enabled: true,
		DBPath:  t.TempDir() + "/tenancy.db",
	}, t.TempDir(), nil)
	if errNew != nil {
		t.Fatalf("NewService() error = %v", errNew)
	}
	if service == nil {
		t.Fatal("NewService() service = nil for enabled tenancy")
	}
	if service.balancing != nil {
		t.Fatalf("balancing runtime = %#v, want nil without auth manager", service.balancing)
	}
	if errClose := service.Close(); errClose != nil {
		t.Fatalf("Service.Close() error = %v", errClose)
	}
}

func TestBalancingResolverDoesNotDeadlockConcurrentUpsertsAndRecomputes(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	const updaterCount = 4
	for index := 0; index < updaterCount; index++ {
		registerTestAuth(t, manager, "deadlock-auth-"+strconv.Itoa(index), strconv.Itoa(index))
	}

	service, errNew := NewService(config.TenancyConfig{
		Enabled: true,
		DBPath:  t.TempDir() + "/tenancy.db",
		Balancing: config.TenancyBalancing{
			UrgencyHorizon: "30m",
			HighWater:      0.9,
			UrgencyBonus:   1,
		},
	}, t.TempDir(), manager)
	if errNew != nil {
		t.Fatalf("NewService() error = %v", errNew)
	}

	requestBalancingRecompute(t, service.balancing)
	stopWork := make(chan struct{})
	var workers sync.WaitGroup
	for index := 0; index < updaterCount; index++ {
		authID := "deadlock-auth-" + strconv.Itoa(index)
		workers.Add(1)
		go func() {
			defer workers.Done()
			priority := 0
			for {
				select {
				case <-stopWork:
					return
				default:
				}
				auth, ok := manager.GetByID(authID)
				if !ok {
					return
				}
				auth.Attributes = map[string]string{"priority": strconv.Itoa(priority % 7)}
				if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
					return
				}
				priority++
			}
		}()
	}

	recomputeDone := make(chan error, 1)
	go func() {
		for index := 0; index < 25; index++ {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			errRecompute := service.balancing.requestRecompute(ctx)
			cancel()
			if errRecompute != nil {
				recomputeDone <- errRecompute
				return
			}
		}
		recomputeDone <- nil
	}()

	select {
	case errRecompute := <-recomputeDone:
		if errRecompute != nil {
			close(stopWork)
			t.Fatalf("concurrent recompute error = %v", errRecompute)
		}
	case <-time.After(3 * time.Second):
		close(stopWork)
		t.Fatal("concurrent scheduler upserts and recomputes deadlocked")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- service.Close()
	}()
	select {
	case errClose := <-closeDone:
		if errClose != nil {
			close(stopWork)
			t.Fatalf("Service.Close() error = %v", errClose)
		}
	case <-time.After(3 * time.Second):
		close(stopWork)
		t.Fatal("Service.Close() deadlocked while scheduler upserts were active")
	}
	close(stopWork)

	workersDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler upsert workers did not stop")
	}
}

func requestBalancingRecompute(t *testing.T, runtime *balancingRuntime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if errRecompute := runtime.requestRecompute(ctx); errRecompute != nil {
		t.Fatalf("requestRecompute() error = %v", errRecompute)
	}
}

func registerTestAuth(t *testing.T, manager *coreauth.Manager, authID, priority string) {
	t.Helper()
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:         authID,
		Provider:   "gemini",
		Attributes: map[string]string{"priority": priority},
	}); errRegister != nil {
		t.Fatalf("Register(%q) error = %v", authID, errRegister)
	}
}

func registerTestModels(t *testing.T, manager *coreauth.Manager, model string, authIDs ...string) {
	t.Helper()
	manager.RegisterExecutor(balancingTestExecutor{})
	modelRegistry := registry.GetGlobalRegistry()
	for _, authID := range authIDs {
		modelRegistry.RegisterClient(authID, "gemini", []*registry.ModelInfo{{ID: model}})
		manager.RefreshSchedulerEntry(authID)
	}
	t.Cleanup(func() {
		for _, authID := range authIDs {
			modelRegistry.UnregisterClient(authID)
		}
	})
}

func executeSelectedAuth(t *testing.T, manager *coreauth.Manager, model string) string {
	t.Helper()
	selectedID := ""
	_, errExecute := manager.Execute(
		context.Background(),
		[]string{"gemini"},
		cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
				selectedID = authID
			},
		}},
	)
	if errExecute != nil {
		t.Fatalf("Manager.Execute() error = %v", errExecute)
	}
	return selectedID
}

type balancingTestExecutor struct{}

func (balancingTestExecutor) Identifier() string {
	return "gemini"
}

func (balancingTestExecutor) Execute(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (balancingTestExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (balancingTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (balancingTestExecutor) CountTokens(
	context.Context,
	*coreauth.Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (balancingTestExecutor) HttpRequest(
	context.Context,
	*coreauth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, nil
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
