package main

import (
	"testing"
)

func TestNodeHealth_NewNodeHasZeroFailureRate(t *testing.T) {
	var h nodeHealth
	if r := h.failureRate(); r != 0 {
		t.Fatalf("expected 0 failure rate for new node, got %f", r)
	}
}

func TestNodeHealth_BelowMinObservationsReturnsZero(t *testing.T) {
	var h nodeHealth
	for i := 0; i < healthMinObservations-1; i++ {
		h.record(false)
	}
	if r := h.failureRate(); r != 0 {
		t.Fatalf("expected 0 before min observations, got %f", r)
	}
}

func TestNodeHealth_AllSuccesses(t *testing.T) {
	var h nodeHealth
	for i := 0; i < healthWindowSize; i++ {
		h.record(true)
	}
	if r := h.failureRate(); r != 0 {
		t.Fatalf("expected 0 failure rate for all successes, got %f", r)
	}
}

func TestNodeHealth_AllFailures(t *testing.T) {
	var h nodeHealth
	for i := 0; i < healthWindowSize; i++ {
		h.record(false)
	}
	if r := h.failureRate(); r != 1.0 {
		t.Fatalf("expected 1.0 failure rate for all failures, got %f", r)
	}
}

func TestNodeHealth_MixedOutcomes(t *testing.T) {
	var h nodeHealth
	for i := 0; i < 10; i++ {
		h.record(true)
	}
	for i := 0; i < 10; i++ {
		h.record(false)
	}
	if r := h.failureRate(); r != 0.5 {
		t.Fatalf("expected 0.5 failure rate, got %f", r)
	}
}

func TestNodeHealth_WindowEvictsOldOutcomes(t *testing.T) {
	var h nodeHealth
	// fill window with failures
	for i := 0; i < healthWindowSize; i++ {
		h.record(false)
	}
	// overwrite entire window with successes
	for i := 0; i < healthWindowSize; i++ {
		h.record(true)
	}
	if r := h.failureRate(); r != 0 {
		t.Fatalf("expected 0 after window reset to successes, got %f", r)
	}
}

func TestNodeHealth_IsDegraded_BelowThreshold(t *testing.T) {
	var h nodeHealth
	for i := 0; i < healthWindowSize; i++ {
		h.record(true)
	}
	if h.isDegraded() {
		t.Fatal("healthy node should not be degraded")
	}
}

func TestNodeHealth_IsDegraded_AtThreshold(t *testing.T) {
	var h nodeHealth
	// exactly 50% failures (threshold is >= 0.5, so this is degraded)
	for i := 0; i < healthWindowSize/2; i++ {
		h.record(true)
	}
	for i := 0; i < healthWindowSize/2; i++ {
		h.record(false)
	}
	if !h.isDegraded() {
		t.Fatal("node at 50% failure rate should be degraded")
	}
}

func TestNodeHealth_RecoverAfterFailures(t *testing.T) {
	var h nodeHealth
	// degrade the node
	for i := 0; i < healthWindowSize; i++ {
		h.record(false)
	}
	if !h.isDegraded() {
		t.Fatal("expected degraded after all failures")
	}
	// recover with successes
	for i := 0; i < healthWindowSize; i++ {
		h.record(true)
	}
	if h.isDegraded() {
		t.Fatal("node should recover after window fills with successes")
	}
}

// --- router integration ---

func TestPickSession_PrefersHealthyOverDegraded(t *testing.T) {
	p, servers := poolWithSessions(t, 2)
	serveStreams(servers[0])
	serveStreams(servers[1])
	r := newTestRouter(p)

	entries := p.snapshot()
	// mark the first node as degraded
	for i := 0; i < healthWindowSize; i++ {
		entries[0].health.record(false)
	}

	// run enough selections to rule out luck
	healthyID := entries[1].id
	for i := 0; i < 20; i++ {
		conn, err := r.DialWithUser(nil, "tcp", "example.com:80", "alice")
		if err != nil {
			t.Fatal(err)
		}
		if connEntryID(conn) != healthyID {
			conn.Close()
			t.Fatalf("expected healthy node %d, got degraded node", healthyID)
		}
		conn.Close()
	}
}

func TestPickSession_FallsBackToDegradedWhenNoHealthyAvailable(t *testing.T) {
	p, servers := poolWithSessions(t, 1)
	serveStreams(servers[0])
	r := newTestRouter(p)

	entry := p.snapshot()[0]
	for i := 0; i < healthWindowSize; i++ {
		entry.health.record(false)
	}

	// should still succeed — degraded nodes are used as last resort
	conn, err := r.DialWithUser(nil, "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatalf("expected fallback to degraded node, got error: %v", err)
	}
	conn.Close()
}

func TestOpenStream_RecordsSuccess(t *testing.T) {
	p, servers := poolWithSessions(t, 1)
	serveStreams(servers[0])
	r := newTestRouter(p)

	conn, err := r.DialWithUser(nil, "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	entry := p.snapshot()[0]
	if entry.health.failureRate() != 0 {
		t.Fatalf("expected 0 failure rate after successful open, got %f", entry.health.failureRate())
	}
}
