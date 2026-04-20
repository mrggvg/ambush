package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCredentialLimiter_AllowsUnderLimit(t *testing.T) {
	l := NewCredentialLimiter(3)
	release, err := l.Acquire("alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer release()
	if l.ActiveStreams("alice") != 1 {
		t.Fatalf("expected 1 active stream, got %d", l.ActiveStreams("alice"))
	}
}

func TestCredentialLimiter_BlocksAtLimit(t *testing.T) {
	l := NewCredentialLimiter(2)
	r1, _ := l.Acquire("alice")
	r2, _ := l.Acquire("alice")
	defer r1()
	defer r2()

	_, err := l.Acquire("alice")
	if err == nil {
		t.Fatal("expected error at limit, got nil")
	}
	if err != errCredentialLimitExceeded {
		t.Fatalf("expected errCredentialLimitExceeded, got: %v", err)
	}
}

func TestCredentialLimiter_ReleaseFreesSlot(t *testing.T) {
	l := NewCredentialLimiter(1)
	release, err := l.Acquire("alice")
	if err != nil {
		t.Fatal(err)
	}
	release()

	if l.ActiveStreams("alice") != 0 {
		t.Fatalf("expected 0 streams after release, got %d", l.ActiveStreams("alice"))
	}

	// should succeed again
	r2, err := l.Acquire("alice")
	if err != nil {
		t.Fatalf("expected success after release: %v", err)
	}
	r2()
}

func TestCredentialLimiter_ReleaseIdempotent(t *testing.T) {
	l := NewCredentialLimiter(2)
	release, _ := l.Acquire("alice")
	release()
	release() // second call must not underflow
	if l.ActiveStreams("alice") < 0 {
		t.Fatal("active streams went negative — release is not idempotent")
	}
}

func TestCredentialLimiter_IndependentPerUser(t *testing.T) {
	l := NewCredentialLimiter(1)
	rA, err := l.Acquire("alice")
	if err != nil {
		t.Fatal(err)
	}
	defer rA()

	// alice is at limit, but bob should be unaffected
	rB, err := l.Acquire("bob")
	if err != nil {
		t.Fatalf("bob should not be limited by alice's usage: %v", err)
	}
	defer rB()

	// alice is still blocked
	_, err = l.Acquire("alice")
	if err == nil {
		t.Fatal("alice should still be at limit")
	}
}

func TestCredentialLimiter_ActiveStreamsUnknownUser(t *testing.T) {
	l := NewCredentialLimiter(5)
	if n := l.ActiveStreams("nobody"); n != 0 {
		t.Fatalf("expected 0 for unknown user, got %d", n)
	}
}

func TestCredentialLimiter_Concurrent(t *testing.T) {
	const limit = 5
	const goroutines = 20
	l := NewCredentialLimiter(limit)

	var wg sync.WaitGroup
	successes := make(chan func(), goroutines)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Acquire("alice")
			if err == nil {
				successes <- release
			}
		}()
	}
	wg.Wait()
	close(successes)

	if int32(len(successes)) > limit {
		t.Fatalf("acquired %d slots, expected at most %d", len(successes), limit)
	}
	if int32(len(successes)) != l.ActiveStreams("alice") {
		t.Fatalf("successes (%d) != ActiveStreams (%d)", len(successes), l.ActiveStreams("alice"))
	}

	for r := range successes {
		r()
	}
	if l.ActiveStreams("alice") != 0 {
		t.Fatalf("expected 0 streams after all releases, got %d", l.ActiveStreams("alice"))
	}
}

// TestDialWithUser_CredentialLimitEnforced verifies that the rate limiter
// is respected end-to-end through DialWithUser.
func TestDialWithUser_CredentialLimitEnforced(t *testing.T) {
	p, servers := poolWithSessions(t, 3) // plenty of exit-node capacity
	for _, s := range servers {
		serveStreams(s)
	}

	r := &Router{
		pool:     p,
		limiter:  NewCredentialLimiter(2), // tight per-credential cap
		affinity: make(map[string]*affinityEntry),
		cooldown: make(map[string]time.Time),
	}

	conn1, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()

	conn2, err := r.DialWithUser(context.Background(), "tcp", "example.com:443", "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()

	// third attempt must be rejected despite available exit nodes
	_, err = r.DialWithUser(context.Background(), "tcp", "example.com:8080", "alice")
	if err == nil {
		t.Fatal("expected rate limit error on third connection")
	}
	if err != errCredentialLimitExceeded {
		t.Fatalf("expected errCredentialLimitExceeded, got: %v", err)
	}

	// bob is unaffected by alice's limit
	conn3, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "bob")
	if err != nil {
		t.Fatalf("bob should not be blocked by alice's limit: %v", err)
	}
	defer conn3.Close()
}

// TestDialWithUser_CredentialReleasedOnClose verifies the limiter slot is freed
// when the connection is closed, allowing the next dial to succeed.
func TestDialWithUser_CredentialReleasedOnClose(t *testing.T) {
	p, servers := poolWithSessions(t, 1)
	serveStreams(servers[0])

	r := &Router{
		pool:     p,
		limiter:  NewCredentialLimiter(1),
		affinity: make(map[string]*affinityEntry),
		cooldown: make(map[string]time.Time),
	}

	conn, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	if r.limiter.ActiveStreams("alice") != 0 {
		t.Fatal("expected slot to be freed after conn.Close()")
	}

	conn2, err := r.DialWithUser(context.Background(), "tcp", "example.com:80", "alice")
	if err != nil {
		t.Fatalf("expected second dial to succeed after close: %v", err)
	}
	defer conn2.Close()
}
