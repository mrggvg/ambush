package main

import (
	"context"
	"sync"
	"time"
)

// authRateLimiter tracks failed authentication attempts per IP and blocks
// IPs that exceed the threshold within a sliding window.
type authRateLimiter struct {
	mu      sync.Mutex
	entries map[string]*authEntry
	max     int
	window  time.Duration
	block   time.Duration
}

type authEntry struct {
	failures     int
	windowEnd    time.Time
	blockedUntil time.Time
}

func newAuthRateLimiter(ctx context.Context, max int, window, block time.Duration) *authRateLimiter {
	l := &authRateLimiter{
		entries: make(map[string]*authEntry),
		max:     max,
		window:  window,
		block:   block,
	}
	go l.cleanupLoop(ctx, block)
	return l
}

// cleanupLoop periodically removes entries whose block period and window have
// both expired. It exits when ctx is cancelled.
func (l *authRateLimiter) cleanupLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.mu.Lock()
			now := time.Now()
			for ip, e := range l.entries {
				if now.After(e.blockedUntil) && now.After(e.windowEnd) {
					delete(l.entries, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}

// allow reports whether ip may attempt authentication.
// Returns false if the IP is currently blocked due to too many failures.
func (l *authRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok {
		return true
	}
	return time.Now().After(e.blockedUntil)
}

// recordFailure records a failed auth attempt for ip.
// If failures within the current window reach the threshold, the IP is blocked.
func (l *authRateLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e, ok := l.entries[ip]
	if !ok {
		e = &authEntry{windowEnd: now.Add(l.window)}
		l.entries[ip] = e
	}
	if now.After(e.windowEnd) {
		e.failures = 0
		e.windowEnd = now.Add(l.window)
	}
	e.failures++
	if e.failures >= l.max {
		e.blockedUntil = now.Add(l.block)
	}
}

// recordSuccess clears the failure state for ip on a successful auth.
func (l *authRateLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

// blocked reports whether ip is currently in a block period.
// Useful for logging — call after recordFailure.
func (l *authRateLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok {
		return false
	}
	return time.Now().Before(e.blockedUntil)
}
