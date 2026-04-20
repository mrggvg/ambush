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

// newTestRouter constructs a Router without starting the background cleanup goroutine.
// Uses a permissive limiter (1000 streams/credential) so router logic tests are unaffected.
// metrics is nil — all instrumentation calls are no-ops.
func newTestRouter(p *Pool) *Router {
	return &Router{
		pool:     p,
		limiter:  NewCredentialLimiter(1000),
		affinity: make(map[string]*affinityEntry),
		cooldown: make(map[string]time.Time),
	}
}

// --- isAffinityValid ---

func TestIsAffinityValid_Valid(t *testing.T) {
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	r := newTestRouter(&Pool{})
	aff := &affinityEntry{entry: se, requests: 0, expiresAt: time.Now().Add(time.Minute)}
	if !r.isAffinityValid(aff) {
		t.Fatal("expected valid affinity")
	}
}

func TestIsAffinityValid_ClosedSession(t *testing.T) {
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	client.Close()
	r := newTestRouter(&Pool{})
	aff := &affinityEntry{entry: se, requests: 0, expiresAt: time.Now().Add(time.Minute)}
	if r.isAffinityValid(aff) {
		t.Fatal("expected invalid: session is closed")
	}
}

func TestIsAffinityValid_Expired(t *testing.T) {
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	r := newTestRouter(&Pool{})
	aff := &affinityEntry{entry: se, requests: 0, expiresAt: time.Now().Add(-time.Second)}
	if r.isAffinityValid(aff) {
		t.Fatal("expected invalid: affinity window expired")
	}
}

func TestIsAffinityValid_RequestBudgetExhausted(t *testing.T) {
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	r := newTestRouter(&Pool{})
	aff := &affinityEntry{entry: se, requests: maxRequestsPerNode, expiresAt: time.Now().Add(time.Minute)}
	if r.isAffinityValid(aff) {
		t.Fatal("expected invalid: request budget at limit")
	}
}

func TestIsAffinityValid_StreamLimitReached(t *testing.T) {
	client, _ := yamuxPair(t)
	se := newSessionEntry(client, "10.0.0.1")
	se.activeStreams.Store(maxStreamsPerNode)
	r := newTestRouter(&Pool{})
	aff := &affinityEntry{entry: se, requests: 0, expiresAt: time.Now().Add(time.Minute)}
	if r.isAffinityValid(aff) {
		t.Fatal("expected invalid: stream concurrency limit reached")
	}
}

// --- pickSession ---

func TestPickSession_EmptyPool(t *testing.T) {
	r := newTestRouter(&Pool{})
	if r.pickSession("example.com") != nil {
		t.Fatal("expected nil from empty pool")
	}
}

func TestPickSession_ExcludesStreamLimit(t *testing.T) {
	p, _ := poolWithSessions(t, 1)
	p.snapshot()[0].activeStreams.Store(maxStreamsPerNode)
	r := newTestRouter(p)
	if r.pickSession("example.com") != nil {
		t.Fatal("expected nil: only session is at stream limit")
	}
}

func TestPickSession_ExcludesCooldown(t *testing.T) {
	p, _ := poolWithSessions(t, 1)
	se := p.snapshot()[0]
	r := newTestRouter(p)
	r.setCooldown(se, "example.com")
	if r.pickSession("example.com") != nil {
		t.Fatal("expected nil: only session is in cooldown for this domain")
	}
}

func TestPickSession_CooldownScopedToDomain(t *testing.T) {
	p, _ := poolWithSessions(t, 1)
	se := p.snapshot()[0]
	r := newTestRouter(p)
	r.setCooldown(se, "blocked.com")
	if r.pickSession("other.com") == nil {
		t.Fatal("cooldown for one domain must not block a different domain on the same node")
	}
}

func TestPickSession_CooldownCoversAllSessionsOnSameIP(t *testing.T) {
	// Two sessions with identical public IPs (e.g. two devices behind home NAT).
	// Cooling down one must block the other — rotating between them would not
	// change the IP the target site sees.
	p := &Pool{}
	client1, _ := yamuxPair(t)
	client2, _ := yamuxPair(t)
	se1 := newSessionEntry(client1, "1.2.3.4")
	se2 := newSessionEntry(client2, "1.2.3.4") // same public IP
	p.add(se1)
	p.add(se2)

	r := newTestRouter(p)
	r.setCooldown(se1, "example.com") // cooldown key: "1.2.3.4:example.com"

	// se2 shares the same IP → same cooldown key → must also be excluded
	if r.pickSession("example.com") != nil {
		t.Fatal("expected nil: both sessions share the cooled-down public IP")
	}
}

func TestPickSession_ExcludesClosedSession(t *testing.T) {
	p, _ := poolWithSessions(t, 1)
	p.snapshot()[0].session.Close()
	r := newTestRouter(p)
	if r.pickSession("example.com") != nil {
		t.Fatal("expected nil: only session is closed")
	}
}

func TestPickSession_ReturnsFromMultipleCandidates(t *testing.T) {
	p, _ := poolWithSessions(t, 3)
	r := newTestRouter(p)
	if r.pickSession("example.com") == nil {
		t.Fatal("expected a session from pool of 3")
	}
}

// --- assignSession ---

func TestAssignSession_CreatesAffinity(t *testing.T) {
	p, _ := poolWithSessions(t, 1)
	r := newTestRouter(p)
	se, err := r.assignSession("alice:example.com", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if se == nil {
		t.Fatal("expected non-nil session")
	}
	r.mu.Lock()
	_, ok := r.affinity["alice:example.com"]
	r.mu.Unlock()
	if !ok {
		t.Fatal("expected affinity entry to be stored")
	}
}

func TestAssignSession_ReusesSameSession(t *testing.T) {
	p, _ := poolWithSessions(t, 2)
	r := newTestRouter(p)
	se1, _ := r.assignSession("alice:example.com", "example.com")
	se2, _ := r.assignSession("alice:example.com", "example.com")
	if se1.id != se2.id {
		t.Fatalf("expected sticky session (id %d), got id %d on second call", se1.id, se2.id)
	}
}

func TestAssignSession_IncrementsRequestCount(t *testing.T) {
	p, _ := poolWithSessions(t, 1)
	r := newTestRouter(p)
	r.assignSession("alice:example.com", "example.com")
	r.assignSession("alice:example.com", "example.com")
	r.mu.Lock()
	count := r.affinity["alice:example.com"].requests
	r.mu.Unlock()
	if count != 2 {
		t.Fatalf("expected request count 2, got %d", count)
	}
}

func TestAssignSession_RotatesOnRequestBudget(t *testing.T) {
	p, _ := poolWithSessions(t, 2)
	r := newTestRouter(p)
	se1, _ := r.assignSession("alice:example.com", "example.com")
	// exhaust budget directly
	r.mu.Lock()
	r.affinity["alice:example.com"].requests = maxRequestsPerNode
	r.mu.Unlock()
	se2, err := r.assignSession("alice:example.com", "example.com")
	if err != nil {
		t.Fatalf("expected successful rotation: %v", err)
	}
	if se2.id == se1.id {
		t.Fatal("expected different session after request budget exhausted")
	}
}

func TestAssignSession_RotatesOnExpiry(t *testing.T) {
	p, _ := poolWithSessions(t, 2)
	r := newTestRouter(p)
	se1, _ := r.assignSession("alice:example.com", "example.com")
	// expire the affinity window
	r.mu.Lock()
	r.affinity["alice:example.com"].expiresAt = time.Now().Add(-time.Second)
	r.mu.Unlock()
	se2, err := r.assignSession("alice:example.com", "example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if se2.id == se1.id {
		t.Fatal("expected rotation after affinity window expired")
	}
}

func TestAssignSession_SetsCooldownOnRotation(t *testing.T) {
	p, _ := poolWithSessions(t, 2)
	r := newTestRouter(p)
	se1, _ := r.assignSession("alice:example.com", "example.com")
	r.mu.Lock()
	r.affinity["alice:example.com"].requests = maxRequestsPerNode
	r.mu.Unlock()
	r.assignSession("alice:example.com", "example.com")

	r.mu.Lock()
	exp, ok := r.cooldown[se1.ip+":example.com"]
	r.mu.Unlock()

	if !ok {
		t.Fatal("expected cooldown entry for rotated-away session")
	}
	if time.Now().After(exp) {
		t.Fatal("cooldown expiry must be in the future")
	}
}

func TestAssignSession_NoCooldownOnSessionClosed(t *testing.T) {
	// When the affinity entry's session dies, the node should NOT be put in
	// cooldown — it may reconnect with the same IP and should be usable immediately.
	p, servers := poolWithSessions(t, 2)
	serveStreams(servers[0])
	serveStreams(servers[1])
	r := newTestRouter(p)

	se1, _ := r.assignSession("alice:example.com", "example.com")
	// kill the session so isAffinityValid returns false with reason session_closed
	se1.session.Close()

	r.assignSession("alice:example.com", "example.com")

	r.mu.Lock()
	_, ok := r.cooldown[se1.ip+":example.com"]
	r.mu.Unlock()

	if ok {
		t.Fatal("session_closed rotation must not set domain cooldown")
	}
}

func TestAssignSession_NoCooldownOnConcurrency(t *testing.T) {
	// A node that hit its concurrency cap should not be cooled down —
	// it may free up capacity immediately and should be reusable.
	p, servers := poolWithSessions(t, 2)
	serveStreams(servers[0])
	serveStreams(servers[1])
	r := newTestRouter(p)

	se1, _ := r.assignSession("alice:example.com", "example.com")
	// simulate concurrency limit hit on the assigned node
	se1.activeStreams.Store(maxStreamsPerNode)

	r.assignSession("alice:example.com", "example.com")

	r.mu.Lock()
	_, ok := r.cooldown[se1.ip+":example.com"]
	r.mu.Unlock()

	if ok {
		t.Fatal("concurrency rotation must not set domain cooldown")
	}
}

func TestAssignSession_NoSessionsAvailable(t *testing.T) {
	r := newTestRouter(&Pool{})
	_, err := r.assignSession("alice:example.com", "example.com")
	if err == nil {
		t.Fatal("expected error with empty pool")
	}
}

// --- setCooldown / cooldown cleanup ---

func TestSetCooldown_BlocksSessionForDuration(t *testing.T) {
	p, _ := poolWithSessions(t, 1)
	se := p.snapshot()[0]
	r := newTestRouter(p)
	r.setCooldown(se, "example.com")

	r.mu.Lock()
	exp, ok := r.cooldown[se.ip+":example.com"]
	r.mu.Unlock()

	if !ok {
		t.Fatal("expected cooldown entry")
	}
	expectedMin := time.Now().Add(cooldownDuration - time.Second)
	if exp.Before(expectedMin) {
		t.Fatalf("cooldown expiry %v is too soon (want ~%v from now)", exp, cooldownDuration)
	}
}

func TestCooldownCleanup_RemovesExpiredEntries(t *testing.T) {
	r := newTestRouter(&Pool{})
	r.cooldown["expired-key"] = time.Now().Add(-time.Second)
	r.cooldown["active-key"] = time.Now().Add(time.Minute)

	// replicate the cleanup loop body
	now := time.Now()
	r.mu.Lock()
	for k, exp := range r.cooldown {
		if now.After(exp) {
			delete(r.cooldown, k)
		}
	}
	r.mu.Unlock()

	r.mu.Lock()
	_, hasExpired := r.cooldown["expired-key"]
	_, hasActive := r.cooldown["active-key"]
	r.mu.Unlock()

	if hasExpired {
		t.Fatal("expired cooldown should have been removed")
	}
	if !hasActive {
		t.Fatal("active cooldown should be retained")
	}
}

// --- randomExpiry ---

func TestRandomExpiry_WithinJitterRange(t *testing.T) {
	r := newTestRouter(&Pool{})
	minDur := time.Duration(float64(affinityBaseWindow) * (1 - affinityJitter))
	maxDur := time.Duration(float64(affinityBaseWindow) * (1 + affinityJitter))
	for range 100 {
		before := time.Now()
		exp := r.randomExpiry()
		if exp.Before(before.Add(minDur)) || exp.After(before.Add(maxDur)) {
			t.Fatalf("expiry %v outside jitter range [+%v, +%v]", exp, minDur, maxDur)
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

func TestDialWithUser_AffinityStickiness(t *testing.T) {
	p, servers := poolWithSessions(t, 2)
	for _, s := range servers {
		serveStreams(s)
	}
	r := newTestRouter(p)

	conn1, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()

	conn2, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()

	id1 := connEntryID(conn1)
	id2 := connEntryID(conn2)
	if id1 != id2 {
		t.Fatalf("expected affinity: same exit node for both conns, got %d and %d", id1, id2)
	}
}

func TestDialWithUser_SeparateAffinityPerUser(t *testing.T) {
	p, servers := poolWithSessions(t, 1)
	serveStreams(servers[0])
	r := newTestRouter(p)

	conn1, _ := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
	defer conn1.Close()
	conn2, _ := r.DialWithUser(context.Background(), "tcp", "example.com:80", "bob")
	defer conn2.Close()

	r.mu.Lock()
	_, aliceOK := r.affinity["alice:example.com"]
	_, bobOK := r.affinity["bob:example.com"]
	r.mu.Unlock()

	if !aliceOK || !bobOK {
		t.Fatal("each user must have an independent affinity entry")
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

	// fill to concurrency limit
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

	// closing one stream decrements activeStreams below the limit;
	// the existing affinity becomes valid again without triggering rotation
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
	// pin affinity to the session that is about to die
	r.mu.Lock()
	r.affinity["alice:example.com"] = &affinityEntry{
		entry:     entries[0],
		requests:  0,
		expiresAt: time.Now().Add(time.Minute),
	}
	r.mu.Unlock()

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

	// 3 users × 3 requests each = 9 goroutines; well within per-node limits
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
