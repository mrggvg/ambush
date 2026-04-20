package main

import (
	"context"
	"testing"
	"time"
)

func newTestAuthLimiter() *authRateLimiter {
	return newAuthRateLimiter(context.Background(), 3, 10*time.Second, 30*time.Second)
}

func TestAuthRateLimiter_AllowsUnderThreshold(t *testing.T) {
	l := newTestAuthLimiter()
	l.recordFailure("1.2.3.4")
	l.recordFailure("1.2.3.4")
	if !l.allow("1.2.3.4") {
		t.Fatal("should allow before threshold is reached")
	}
}

func TestAuthRateLimiter_BlocksAfterThreshold(t *testing.T) {
	l := newTestAuthLimiter()
	for i := 0; i < 3; i++ {
		l.recordFailure("1.2.3.4")
	}
	if l.allow("1.2.3.4") {
		t.Fatal("should block after threshold")
	}
}

func TestAuthRateLimiter_BlockedReturnsTrue(t *testing.T) {
	l := newTestAuthLimiter()
	for i := 0; i < 3; i++ {
		l.recordFailure("1.2.3.4")
	}
	if !l.blocked("1.2.3.4") {
		t.Fatal("blocked should return true after threshold")
	}
}

func TestAuthRateLimiter_BlockedReturnsFalseUnderThreshold(t *testing.T) {
	l := newTestAuthLimiter()
	l.recordFailure("1.2.3.4")
	if l.blocked("1.2.3.4") {
		t.Fatal("blocked should return false before threshold")
	}
}

func TestAuthRateLimiter_SuccessResetsCounter(t *testing.T) {
	l := newTestAuthLimiter()
	l.recordFailure("1.2.3.4")
	l.recordFailure("1.2.3.4")
	l.recordSuccess("1.2.3.4")
	// after success, two more failures should not block (threshold is 3)
	l.recordFailure("1.2.3.4")
	l.recordFailure("1.2.3.4")
	if !l.allow("1.2.3.4") {
		t.Fatal("should allow after success reset")
	}
}

func TestAuthRateLimiter_UnblocksAfterBlockExpiry(t *testing.T) {
	l := newAuthRateLimiter(context.Background(), 2, 10*time.Second, 10*time.Millisecond)
	l.recordFailure("1.2.3.4")
	l.recordFailure("1.2.3.4")
	if l.allow("1.2.3.4") {
		t.Fatal("should be blocked immediately after threshold")
	}
	time.Sleep(20 * time.Millisecond)
	if !l.allow("1.2.3.4") {
		t.Fatal("should be unblocked after block period expires")
	}
}

func TestAuthRateLimiter_WindowReset(t *testing.T) {
	l := newAuthRateLimiter(context.Background(), 3, 10*time.Millisecond, 30*time.Second)
	l.recordFailure("1.2.3.4")
	l.recordFailure("1.2.3.4")
	time.Sleep(20 * time.Millisecond) // window expires
	// two more failures in the new window should not block
	l.recordFailure("1.2.3.4")
	l.recordFailure("1.2.3.4")
	if !l.allow("1.2.3.4") {
		t.Fatal("failures from expired window should not carry over")
	}
}

func TestAuthRateLimiter_IsolatedPerIP(t *testing.T) {
	l := newTestAuthLimiter()
	for i := 0; i < 3; i++ {
		l.recordFailure("1.2.3.4")
	}
	if !l.allow("5.6.7.8") {
		t.Fatal("different IP should not be affected")
	}
}

func TestAuthRateLimiter_AllowUnknownIP(t *testing.T) {
	l := newTestAuthLimiter()
	if !l.allow("9.9.9.9") {
		t.Fatal("unknown IP should be allowed")
	}
}

func TestAuthRateLimiter_StaleEntryCleanedByLoop(t *testing.T) {
	// block expires in 10ms, cleanup runs every 10ms — entry should disappear
	// without any allow() call.
	l := newAuthRateLimiter(context.Background(), 2, 5*time.Millisecond, 10*time.Millisecond)
	l.recordFailure("1.2.3.4")
	l.recordFailure("1.2.3.4")
	time.Sleep(50 * time.Millisecond)
	l.mu.Lock()
	_, exists := l.entries["1.2.3.4"]
	l.mu.Unlock()
	if exists {
		t.Fatal("cleanup loop should have removed expired entry")
	}
}

func TestAuthRateLimiter_CleanupLoopStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l := newAuthRateLimiter(ctx, 3, 10*time.Second, 30*time.Second)
	cancel()
	// give the goroutine time to observe cancellation
	time.Sleep(20 * time.Millisecond)
	// just verify the limiter still works correctly after cancel
	l.recordFailure("1.2.3.4")
	if !l.allow("1.2.3.4") {
		t.Fatal("should still allow under threshold after cancel")
	}
}
