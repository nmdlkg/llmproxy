package main

import (
	"sort"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// maxSmoothWeightedEntries bounds retained credits, mirroring the host bound.
const maxSmoothWeightedEntries = 1024

// smoothWeighted is the host's smooth weighted round-robin (nginx style): each
// pick adds every candidate's weight to its credit, selects the largest credit,
// and charges the selection the total weight. It interleaves selections
// (3:1 yields a,a,b,a) instead of serving a weight as one burst. The zero value
// is ready to use.
type smoothWeighted struct {
	mu      sync.Mutex
	current map[string]int64
	weights map[string]int64
}

func (w *smoothWeighted) pick(pool []pluginapi.SchedulerAuthCandidate) string {
	sorted := append([]pluginapi.SchedulerAuthCandidate(nil), pool...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current == nil {
		w.current = make(map[string]int64)
		w.weights = make(map[string]int64)
	}
	present := make(map[string]struct{}, len(sorted))
	for _, candidate := range sorted {
		weight := schedulerWeight(candidate)
		if weight <= 0 {
			continue
		}
		present[candidate.ID] = struct{}{}
		if previous, ok := w.weights[candidate.ID]; ok && previous != weight {
			// A configuration change restarts the sequence, as the host does.
			clear(w.current)
		}
		w.weights[candidate.ID] = weight
	}
	if len(w.current) > maxSmoothWeightedEntries || len(w.weights) > maxSmoothWeightedEntries {
		for id := range w.weights {
			if _, ok := present[id]; !ok {
				delete(w.weights, id)
				delete(w.current, id)
			}
		}
	}

	picked := ""
	var pickedCurrent, total int64
	for _, candidate := range sorted {
		weight := schedulerWeight(candidate)
		if weight <= 0 {
			continue
		}
		w.current[candidate.ID] += weight
		total += weight
		if picked == "" || w.current[candidate.ID] > pickedCurrent {
			picked, pickedCurrent = candidate.ID, w.current[candidate.ID]
		}
	}
	if picked != "" {
		w.current[picked] -= total
	}
	return picked
}
