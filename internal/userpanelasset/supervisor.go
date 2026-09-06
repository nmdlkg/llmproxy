package userpanelasset

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

// Supervisor owns the user-panel update ticker and reacts to live config
// changes. It intentionally contains no cache or verification state; that
// state belongs to Manager.
type Supervisor struct {
	manager  *Manager
	interval time.Duration

	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	started  atomic.Bool
	stopped  atomic.Bool
	configCh chan struct{}
	wg       sync.WaitGroup
}

// NewSupervisor creates a lifecycle companion for a Manager.
func NewSupervisor(manager *Manager) *Supervisor {
	return &Supervisor{
		manager:  manager,
		interval: defaultUpdateInterval,
		configCh: make(chan struct{}, 1),
	}
}

// Start starts the supervisor once. The initial update runs in the supervisor
// goroutine, so server construction and request routing remain local and
// deterministic.
func (s *Supervisor) Start(ctx context.Context) {
	if s == nil || s.manager == nil || !s.started.CompareAndSwap(false, true) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.stopped.Load() {
		s.mu.Unlock()
		return
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	interval := s.interval
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run(interval)
	}()
}

func (s *Supervisor) run(interval time.Duration) {
	if interval <= 0 {
		interval = defaultUpdateInterval
	}
	// A startup check warms the signed cache, while /user itself never performs
	// this work synchronously.
	if errSync := s.manager.Sync(s.context()); errSync != nil {
		log.WithError(errSync).Debug("user panel supervisor: startup update skipped")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.context().Done():
			return
		case <-ticker.C:
			if errSync := s.manager.Sync(s.context()); errSync != nil {
				log.WithError(errSync).Debug("user panel supervisor: scheduled update skipped")
			}
		case <-s.configCh:
			if errSync := s.manager.Sync(s.context()); errSync != nil {
				log.WithError(errSync).Debug("user panel supervisor: config update skipped")
			}
		}
	}
}

func (s *Supervisor) context() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// SetConfig applies a hot-reloaded config and wakes a running supervisor. It
// is safe before Start; the first startup pass will observe the config.
func (s *Supervisor) SetConfig(cfg *config.Config) {
	if s == nil || s.manager == nil {
		return
	}
	s.manager.SetConfig(cfg)
	if s.started.Load() && !s.stopped.Load() {
		select {
		case s.configCh <- struct{}{}:
		default:
		}
	}
}

// Stop stops the ticker and waits for any in-progress scheduled update.
func (s *Supervisor) Stop() {
	if s == nil || !s.started.Load() {
		return
	}
	s.stopped.Store(true)
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Started reports whether Start has been called.
func (s *Supervisor) Started() bool { return s != nil && s.started.Load() && !s.stopped.Load() }
