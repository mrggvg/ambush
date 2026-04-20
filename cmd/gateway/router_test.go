package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// --- helpers ---

func yamuxQuiet() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	return cfg
}

// yamuxPair returns a connected (client, server) yamux session pair backed by net.Pipe.
func yamuxPair(t *testing.T) (*yamux.Session, *yamux.Session) {
	t.Helper()
	c1, c2 := net.Pipe()
	client, err := yamux.Client(c1, yamuxQuiet())
	if err != nil {
		t.Fatal(err)
	}
	server, err := yamux.Server(c2, yamuxQuiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// poolWithSessions builds a Pool pre-populated with n live client sessions.
// The returned server sessions must accept streams for DialWithUser to succeed.
func poolWithSessions(t *testing.T, n int) (*Pool, []*yamux.Session) {
	t.Helper()
	p := &Pool{}
	servers := make([]*yamux.Session, n)
	for i := range n {
		client, server := yamuxPair(t)
		servers[i] = server
		p.add(newSessionEntry(client, fmt.Sprintf("10.0.0.%d", i+1)))
	}
	return p, servers
}

// serveStreams accepts yamux streams in background, reads the address line, then idles.
// This is required so openStream's Fprintf doesn't block or error.
func serveStreams(server *yamux.Session) {
	go func() {
		for {
			stream, err := server.Accept()
			if err != nil {
				return
			}
			go func(s net.Conn) {
				buf := make([]byte, 512)
				for {
					n, err := s.Read(buf)
					if err != nil || (n > 0 && buf[n-1] == '\n') {
						return
					}
				}
			}(stream)
		}
	}()
}

// connEntryID unwraps the chain rateLimitedConn → trackedConn to get the exit-node ID.
func connEntryID(conn net.Conn) uint64 {
	return conn.(*rateLimitedConn).Conn.(*trackedConn).entry.id
}

// newTestRouter constructs a Router without any background goroutines.
// Uses a permissive limiter (1000 streams/credential) so router logic tests are unaffected.
// metrics is nil — all instrumentation calls are no-ops.
func newTestRouter(p *Pool) *Router {
	return &Router{
		pool:     p,
		limiter:  NewCredentialLimiter(1000),
		sessions: NewSessionStore(context.Background(), time.Hour),
	}
}

// --- pickSession ---

func TestPickSession_EmptyPool(t *testing.T) {
	r := newTestRouter(&Pool{})
	if r.pickSessionExcluding(nil, Constraints{}) != nil {
		t.Fatal("expected nil from empty pool")
	}
}

func TestPickSession_ExcludesStreamLimit(t *testing.T) {
	p, _ := poolWithSessions(t, 1)
	p.snapshot()[0].activeStreams.Store(maxStreamsPerNode)
	r := newTestRouter(p)
	if r.pickSessionExcluding(nil, Constraints{}) != nil {
		t.Fatal("expected nil: only session is at stream limit")
	}
}

func TestPickSession_ExcludesClosedSession(t *testing.T) {
	p, _ := poolWithSessions(t, 1)
	p.snapshot()[0].session.Close()
	r := newTestRouter(p)
	if r.pickSessionExcluding(nil, Constraints{}) != nil {
		t.Fatal("expected nil: only session is closed")
	}
}

func TestPickSession_ReturnsFromMultipleCandidates(t *testing.T) {
	p, _ := poolWithSessions(t, 3)
	r := newTestRouter(p)
	if r.pickSessionExcluding(nil, Constraints{}) == nil {
		t.Fatal("expected a session from pool of 3")
	}
}

func TestPickSessionExcluding_ExcludesSpecified(t *testing.T) {
	p, _ := poolWithSessions(t, 2)
	r := newTestRouter(p)
	entries := p.snapshot()
	got := r.pickSessionExcluding(entries[0], Constraints{})
	if got == nil {
		t.Fatal("expected a session when excluding one of two")
	}
	if got.id == entries[0].id {
		t.Fatal("expected a different session than the excluded one")
	}
}

func TestPickSessionExcluding_NilExclude(t *testing.T) {
	p, _ := poolWithSessions(t, 1)
	r := newTestRouter(p)
	if r.pickSessionExcluding(nil, Constraints{}) == nil {
		t.Fatal("nil exclude should behave like pickSession")
	}
}

// --- Constraints ---

func TestConstraints_EmptyMatchesAll(t *testing.T) {
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	se.country = "us"
	se.nodeType = "residential"
	if !(Constraints{}).matches(se) {
		t.Fatal("empty constraints must match any node")
	}
}

func TestConstraints_CountryMatch(t *testing.T) {
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	se.country = "us"
	if !(Constraints{Country: "us"}).matches(se) {
		t.Fatal("expected match for same country")
	}
	if (Constraints{Country: "gb"}).matches(se) {
		t.Fatal("expected no match for different country")
	}
}

func TestConstraints_NodeTypeMatch(t *testing.T) {
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	se.nodeType = "datacenter"
	if !(Constraints{NodeType: "datacenter"}).matches(se) {
		t.Fatal("expected match for same node type")
	}
	if (Constraints{NodeType: "residential"}).matches(se) {
		t.Fatal("expected no match for different node type")
	}
}

func TestConstraints_BothFields(t *testing.T) {
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	se.country = "de"
	se.nodeType = "mobile"
	if !(Constraints{Country: "de", NodeType: "mobile"}).matches(se) {
		t.Fatal("expected match when both fields match")
	}
	if (Constraints{Country: "de", NodeType: "residential"}).matches(se) {
		t.Fatal("expected no match when node type differs")
	}
}

func TestPickSession_FiltersByCountry(t *testing.T) {
	p, _ := poolWithSessions(t, 2)
	entries := p.snapshot()
	entries[0].country = "us"
	entries[1].country = "gb"
	r := newTestRouter(p)

	se := r.pickSessionExcluding(nil, Constraints{Country: "us"})
	if se == nil {
		t.Fatal("expected a session for country=us")
	}
	if se.country != "us" {
		t.Fatalf("expected country us, got %q", se.country)
	}
}

func TestPickSession_CountryConstraint_ReturnsNilIfNoMatch(t *testing.T) {
	p, _ := poolWithSessions(t, 2)
	entries := p.snapshot()
	entries[0].country = "us"
	entries[1].country = "us"
	r := newTestRouter(p)

	if r.pickSessionExcluding(nil, Constraints{Country: "gb"}) != nil {
		t.Fatal("expected nil: no nodes match country=gb")
	}
}

func TestPickSession_FiltersByNodeType(t *testing.T) {
	p, _ := poolWithSessions(t, 2)
	entries := p.snapshot()
	entries[0].nodeType = "datacenter"
	entries[1].nodeType = "residential"
	r := newTestRouter(p)

	se := r.pickSessionExcluding(nil, Constraints{NodeType: "datacenter"})
	if se == nil || se.nodeType != "datacenter" {
		t.Fatalf("expected datacenter node, got %v", se)
	}
}

func TestDialWithUser_CountryConstraintInUsername(t *testing.T) {
	p, servers := poolWithSessions(t, 2)
	for _, s := range servers {
		serveStreams(s)
	}
	entries := p.snapshot()
	entries[0].country = "us"
	entries[1].country = "gb"
	r := newTestRouter(p)

	// 20 dials with country=gb — must always land on the gb node
	for i := range 20 {
		conn, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice-country-gb")
		if err != nil {
			t.Fatalf("dial %d: %v", i+1, err)
		}
		id := connEntryID(conn)
		conn.Close()
		if id != entries[1].id {
			t.Fatalf("dial %d: expected gb node (id %d), got id %d", i+1, entries[1].id, id)
		}
	}
}

// --- DialWithUser ---

func TestDialWithUser_BasicDial(t *testing.T) {
	p, servers := poolWithSessions(t, 1)
	serveStreams(servers[0])
	r := newTestRouter(p)

	conn, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestDialWithUser_FreshExitPerConnection(t *testing.T) {
	// With 3 nodes and 30 dials, we expect to see at least 2 distinct exit nodes
	// (Model B — no affinity). This would fail with near-certainty if affinity were
	// still active, since all dials for "alice" would stick to one node.
	p, servers := poolWithSessions(t, 3)
	for _, s := range servers {
		serveStreams(s)
	}
	r := newTestRouter(p)

	seen := map[uint64]bool{}
	for range 30 {
		conn, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
		if err != nil {
			t.Fatal(err)
		}
		seen[connEntryID(conn)] = true
		conn.Close()
	}
	if len(seen) < 2 {
		t.Fatalf("expected multiple distinct exit nodes, got %d — affinity may still be active", len(seen))
	}
}

func TestDialWithUser_ConcurrencyLimitBlocksFurtherDials(t *testing.T) {
	p, servers := poolWithSessions(t, 1)
	serveStreams(servers[0])
	r := newTestRouter(p)

	conns := make([]net.Conn, maxStreamsPerNode)
	for i := range maxStreamsPerNode {
		conn, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
		if err != nil {
			t.Fatalf("stream %d: unexpected error: %v", i+1, err)
		}
		conns[i] = conn
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	_, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
	if err == nil {
		t.Fatal("expected error: at concurrency limit with no other sessions")
	}
}

func TestDialWithUser_ReleasedStreamAllowsNewDial(t *testing.T) {
	p, servers := poolWithSessions(t, 1)
	serveStreams(servers[0])
	r := newTestRouter(p)

	conns := make([]net.Conn, maxStreamsPerNode)
	for i := range maxStreamsPerNode {
		conn, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
		if err != nil {
			t.Fatalf("stream %d: %v", i+1, err)
		}
		conns[i] = conn
	}
	defer func() {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
	}()

	conns[0].Close()
	conns[0] = nil

	conn, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatalf("expected new dial to succeed after releasing a stream: %v", err)
	}
	defer conn.Close()
}

func TestDialWithUser_RetriesOnDeadSession(t *testing.T) {
	p, servers := poolWithSessions(t, 2)
	serveStreams(servers[1]) // only the second server accepts streams
	entries := p.snapshot()
	deadID := entries[0].id

	r := newTestRouter(p)
	entries[0].session.Close()

	conn, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatalf("expected successful retry after dead session: %v", err)
	}
	defer conn.Close()

	if connEntryID(conn) == deadID {
		t.Fatal("expected retry to use a different (alive) session")
	}
}

func TestDialWithUser_Concurrent(t *testing.T) {
	p, servers := poolWithSessions(t, 3)
	for _, s := range servers {
		serveStreams(s)
	}
	r := newTestRouter(p)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var opened []net.Conn

	for i := range 9 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("user%d", i%3)
			conn, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", user)
			if err != nil {
				return
			}
			mu.Lock()
			opened = append(opened, conn)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	for _, c := range opened {
		c.Close()
	}
	if len(opened) == 0 {
		t.Fatal("all concurrent dials failed")
	}
}
