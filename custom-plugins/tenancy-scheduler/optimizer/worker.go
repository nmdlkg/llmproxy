package optimizer

import (
	"sync"
	"time"
)

// Worker periodically recomputes prices, prunes stale state, and checkpoints.
type Worker struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// StartWorker launches background valuation. checkpoint may be nil.
func StartWorker(engine *Engine, recomputeEvery, checkpointEvery time.Duration, checkpoint func()) *Worker {
	worker := &Worker{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(worker.done)
		engine.Recompute()
		recompute := time.NewTicker(recomputeEvery)
		defer recompute.Stop()
		save := time.NewTicker(checkpointEvery)
		defer save.Stop()
		for {
			select {
			case <-recompute.C:
				engine.Prune()
				engine.Recompute()
			case <-save.C:
				if checkpoint != nil {
					checkpoint()
				}
			case <-worker.stop:
				return
			}
		}
	}()
	return worker
}

// Stop stops the worker and waits for the current iteration to finish.
func (w *Worker) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
}
