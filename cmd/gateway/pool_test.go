package main

import (
	"testing"
)

// --- Pool ---

func TestPool_AddIncreasesSnapshot(t *testing.T) {
	p := &Pool{}
	client, _ := yamuxPair(t)
	p.add(newSessionEntry(client, "1.2.3.4"))
	if n := len(p.snapshot()); n != 1 {
		t.Fatalf("expected 1 entry after add, got %d", n)
	}
}

func TestPool_RemoveDecreasesSnapshot(t *testing.T) {
	p := &Pool{}
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "1.2.3.4")
	p.add(se)
	p.remove(se)
	if n := len(p.snapshot()); n != 0 {
		t.Fatalf("expected 0 entries after remove, got %d", n)
	}
}

func TestPool_RemoveUnknownEntryIsNoop(t *testing.T) {
	p := &Pool{}
	client1, _ := yamuxPair(t)
	client2, _ := yamuxPair(t)
	se1 := newSessionEntry(client1, "1.2.3.4")
	se2 := newSessionEntry(client2, "1.2.3.5")
	p.add(se1)
	p.remove(se2) // se2 was never added
	if n := len(p.snapshot()); n != 1 {
		t.Fatalf("expected pool unchanged after removing unknown entry, got %d entries", n)
	}
}

func TestPool_SnapshotExcludesClosedSessions(t *testing.T) {
	p := &Pool{}
	clientAlive, _ := yamuxPair(t)
	clientDead, _ := yamuxPair(t)

	p.add(newSessionEntry(clientAlive, "1.2.3.4"))
	p.add(newSessionEntry(clientDead, "1.2.3.5"))
	clientDead.Close()

	entries := p.snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected 1 alive entry in snapshot, got %d", len(entries))
	}
	if entries[0].ip != "1.2.3.4" {
		t.Fatalf("expected alive entry (1.2.3.4), got %s", entries[0].ip)
	}
}

func TestPool_ByIP_ReturnsMatchingEntries(t *testing.T) {
	p := &Pool{}
	c1, _ := yamuxPair(t)
	c2, _ := yamuxPair(t)
	c3, _ := yamuxPair(t)
	p.add(newSessionEntry(c1, "1.2.3.4"))
	p.add(newSessionEntry(c2, "1.2.3.4"))
	p.add(newSessionEntry(c3, "9.9.9.9"))

	matches := p.byIP("1.2.3.4")
	if len(matches) != 2 {
		t.Fatalf("expected 2 entries for 1.2.3.4, got %d", len(matches))
	}
	for _, e := range matches {
		if e.ip != "1.2.3.4" {
			t.Fatalf("byIP returned entry with wrong ip: %s", e.ip)
		}
	}
}

func TestPool_ByIP_ReturnsEmptyForUnknownIP(t *testing.T) {
	p := &Pool{}
	c, _ := yamuxPair(t)
	p.add(newSessionEntry(c, "1.2.3.4"))
	if matches := p.byIP("9.9.9.9"); len(matches) != 0 {
		t.Fatalf("expected 0 matches for unknown IP, got %d", len(matches))
	}
}

func TestPool_ByIP_IncludesClosedSessions(t *testing.T) {
	// byIP is used for the duplicate-IP warning at registration time,
	// so it must count all entries including closed ones.
	p := &Pool{}
	c1, _ := yamuxPair(t)
	c2, _ := yamuxPair(t)
	se1 := newSessionEntry(c1, "1.2.3.4")
	se2 := newSessionEntry(c2, "1.2.3.4")
	p.add(se1)
	p.add(se2)
	se1.session.Close()

	if n := len(p.byIP("1.2.3.4")); n != 2 {
		t.Fatalf("byIP should include closed sessions, expected 2, got %d", n)
	}
}

// --- trackedConn ---

func TestTrackedConn_CloseDecrementsActiveStreams(t *testing.T) {
	p, servers := poolWithSessions(t, 1)
	serveStreams(servers[0])
	r := newTestRouter(p)

	conn, err := r.DialWithUser(nil, "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatal(err)
	}

	se := p.snapshot()[0]
	if se.activeStreams.Load() != 1 {
		t.Fatalf("expected 1 active stream before close, got %d", se.activeStreams.Load())
	}

	conn.Close()

	if se.activeStreams.Load() != 0 {
		t.Fatalf("expected 0 active streams after close, got %d", se.activeStreams.Load())
	}
}

func TestTrackedConn_CloseIsIdempotent(t *testing.T) {
	p, servers := poolWithSessions(t, 1)
	serveStreams(servers[0])
	r := newTestRouter(p)

	conn, err := r.DialWithUser(nil, "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatal(err)
	}

	conn.Close()
	conn.Close() // second close must not underflow activeStreams

	se := p.snapshot()[0]
	if se.activeStreams.Load() < 0 {
		t.Fatal("activeStreams went negative — trackedConn.Close is not idempotent")
	}
}
