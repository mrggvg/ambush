package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	affinityBaseWindow = 5 * time.Minute
	affinityJitter     = 0.20 // ±20% of base window
	maxRequestsPerNode = 100
	cooldownDuration   = 10 * time.Minute
	maxStreamsPerNode   = 10
)

type affinityEntry struct {
	entry     *sessionEntry
	requests  int
	expiresAt time.Time
}

// Router handles smart routing: domain affinity, rotation with cooldown,
// per-exitnode concurrency limits, and per-credential rate limiting.
type Router struct {
	mu       sync.Mutex
	pool     *Pool
	limiter  *CredentialLimiter
	metrics  *Metrics
	affinity map[string]*affinityEntry // domain -> active assignment
	cooldown map[string]time.Time      // "ip:domain" -> expires
}

func NewRouter(pool *Pool, limiter *CredentialLimiter, metrics *Metrics) *Router {
	r := &Router{
		pool:     pool,
		limiter:  limiter,
		metrics:  metrics,
		affinity: make(map[string]*affinityEntry),
		cooldown: make(map[string]time.Time),
	}
	go r.cleanupLoop()
	return r
}

func (r *Router) cleanupLoop() {
	ticker := time.NewTicker(cooldownDuration)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		r.mu.Lock()
		for key, exp := range r.cooldown {
			if now.After(exp) {
				delete(r.cooldown, key)
			}
		}
		r.mu.Unlock()
	}
}

// DialWithUser is the core dial function. username scopes affinity so two
// different SOCKS5 users hitting the same domain use different exit nodes.
func (r *Router) DialWithUser(ctx context.Context, _, addr, username string) (net.Conn, error) {
	reqID := requestIDFromCtx(ctx)

	release, err := r.limiter.Acquire(username)
	if err != nil {
		slog.Warn("credential rate limit exceeded", "req_id", reqID, "username", username, "active_streams", r.limiter.ActiveStreams(username))
		r.metrics.incCredLimitExceeded()
		r.metrics.incDials("rate_limited")
		return nil, err
	}

	conn, err := r.dial(addr, username, reqID)
	if err != nil {
		r.metrics.incDials("error")
		release()
		return nil, err
	}
	r.metrics.incDials("success")
	return &rateLimitedConn{Conn: conn, release: release}, nil
}

func (r *Router) dial(addr, username, reqID string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	affinityKey := username + ":" + host

	se, err := r.assignSession(affinityKey, host)
	if err != nil {
		return nil, err
	}

	conn, err := r.openStream(se, addr, username, reqID)
	if err != nil {
		// session died between selection and open — record the failure, clear affinity, retry once
		se.health.record(false)
		r.metrics.incStreamErrors(strconv.FormatUint(se.id, 10))
		slog.Warn("stream open failed, retrying with new session", "req_id", reqID, "exitnode_id", se.id, "addr", addr, "error", err)
		r.clearAffinity(affinityKey)
		se, err = r.assignSession(affinityKey, host)
		if err != nil {
			return nil, err
		}
		conn, err = r.openStream(se, addr, username, reqID)
		if err != nil {
			se.health.record(false)
			r.metrics.incStreamErrors(strconv.FormatUint(se.id, 10))
			return nil, err
		}
		return conn, nil
	}
	return conn, nil
}

func (r *Router) clearAffinity(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.affinity, key)
}

func (r *Router) assignSession(affinityKey, domain string) (*sessionEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if aff := r.affinity[affinityKey]; aff != nil {
		if r.isAffinityValid(aff) {
			aff.requests++
			return aff.entry, nil
		}
		// rotate — only apply domain cooldown for deliberate anti-bot rotations
		// (budget/expiry). session_closed and concurrency are infrastructure events;
		// cooling down the IP would block a reconnected node or a node that just
		// freed up capacity.
		reason := r.rotationReason(aff)
		r.metrics.incRotations(reason)
		if reason == "budget" || reason == "expiry" {
			r.setCooldown(aff.entry, domain)
		}
		delete(r.affinity, affinityKey)
	}

	se := r.pickSession(domain)
	if se == nil {
		slog.Warn("no eligible exit nodes", "domain", domain, "pool_size", len(r.pool.snapshot()))
		return nil, errors.New("no exitnodes available")
	}

	r.affinity[affinityKey] = &affinityEntry{
		entry:     se,
		requests:  1,
		expiresAt: r.randomExpiry(),
	}
	return se, nil
}

func (r *Router) isAffinityValid(aff *affinityEntry) bool {
	return !aff.entry.session.IsClosed() &&
		time.Now().Before(aff.expiresAt) &&
		aff.requests < maxRequestsPerNode &&
		aff.entry.activeStreams.Load() < maxStreamsPerNode
}

// pickSession selects an eligible session for the given domain.
// Excludes sessions in cooldown or at their concurrency limit.
// Healthy nodes (failure rate below threshold) are preferred; degraded nodes
// are used only when no healthy nodes are available.
func (r *Router) pickSession(domain string) *sessionEntry {
	candidates := r.pool.snapshot()
	var healthy, degraded []*sessionEntry
	for _, se := range candidates {
		if se.activeStreams.Load() >= maxStreamsPerNode {
			continue
		}
		if exp, ok := r.cooldown[se.ip+":"+domain]; ok && time.Now().Before(exp) {
			continue
		}
		if se.health.isDegraded() {
			degraded = append(degraded, se)
		} else {
			healthy = append(healthy, se)
		}
	}
	if len(healthy) > 0 {
		return healthy[rand.IntN(len(healthy))]
	}
	if len(degraded) > 0 {
		slog.Warn("all eligible exit nodes are degraded, using fallback",
			"domain", domain, "degraded_count", len(degraded))
		return degraded[rand.IntN(len(degraded))]
	}
	return nil
}

func (r *Router) openStream(se *sessionEntry, addr, username, reqID string) (net.Conn, error) {
	stream, err := se.session.Open()
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(stream, "%s %s\n", reqID, addr); err != nil {
		_ = stream.Close()
		return nil, err
	}
	se.health.record(true)
	se.activeStreams.Add(1)
	r.pool.streams.Add(1)
	r.metrics.incStreams(strconv.FormatUint(se.id, 10))
	slog.Info("stream opened", "req_id", reqID, "exitnode_id", se.id, "addr", addr, "username", username)
	return &trackedConn{Conn: stream, entry: se, pool: r.pool, metrics: r.metrics}, nil
}

func (r *Router) setCooldown(se *sessionEntry, domain string) {
	r.cooldown[se.ip+":"+domain] = time.Now().Add(cooldownDuration)
}

// rotationReason returns a Prometheus label describing why an affinity entry became invalid.
// Conditions are checked in priority order: a closed session is the root cause even if the
// budget is also exhausted.
func (r *Router) rotationReason(aff *affinityEntry) string {
	if aff.entry.session.IsClosed() {
		return "session_closed"
	}
	if !time.Now().Before(aff.expiresAt) {
		return "expiry"
	}
	if aff.requests >= maxRequestsPerNode {
		return "budget"
	}
	return "concurrency"
}

func (r *Router) randomExpiry() time.Time {
	jitter := float64(affinityBaseWindow) * (1 + (rand.Float64()*2-1)*affinityJitter)
	return time.Now().Add(time.Duration(jitter))
}
