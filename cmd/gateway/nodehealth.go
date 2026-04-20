package main

import "sync"

const (
	healthWindowSize       = 20  // rolling window of stream-open outcomes
	healthMinObservations  = 5   // minimum outcomes before a node can be deprioritised
	healthFailureThreshold = 0.5 // failure rate at or above which a node is degraded
)

// nodeHealth tracks a rolling window of stream-open outcomes for a single exit node.
// It is embedded as a value in sessionEntry — no heap allocation.
type nodeHealth struct {
	mu       sync.Mutex
	outcomes [healthWindowSize]bool // true = success; ring buffer
	pos      int                    // next write position
	count    int                    // total outcomes recorded, capped at healthWindowSize
}

// record adds an outcome to the rolling window.
func (h *nodeHealth) record(success bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outcomes[h.pos] = success
	h.pos = (h.pos + 1) % healthWindowSize
	if h.count < healthWindowSize {
		h.count++
	}
}

// failureRate returns the fraction of failures in the current window.
// Returns 0 if fewer than healthMinObservations have been recorded so that
// a new node is never penalised before it has a chance to prove itself.
func (h *nodeHealth) failureRate() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count < healthMinObservations {
		return 0
	}
	failures := 0
	for i := 0; i < h.count; i++ {
		if !h.outcomes[i] {
			failures++
		}
	}
	return float64(failures) / float64(h.count)
}

// isDegraded reports whether this node should be deprioritised in routing.
func (h *nodeHealth) isDegraded() bool {
	return h.failureRate() >= healthFailureThreshold
}
