package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
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
	affinity map[string]*affinityEntry // domain -> active assignment
	cooldown map[string]time.Time      // "sessionID:domain" -> expires
}

func NewRouter(pool *Pool, limiter *CredentialLimiter) *Router {
	r := &Router{
		pool:     pool,
		limiter:  limiter,
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
	release, err := r.limiter.Acquire(username)
	if err != nil {
		slog.Warn("credential rate limit exceeded", "username", username, "active_streams", r.limiter.ActiveStreams(username))
		return nil, err
	}

	conn, err := r.dial(ctx, addr, username)
	if err != nil {
		release()
		return nil, err
	}
	return &rateLimitedConn{Conn: conn, release: release}, nil
}

func (r *Router) dial(ctx context.Context, addr, username string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	affinityKey := username + ":" + host

	se, err := r.assignSession(affinityKey, host)
	if err != nil {
		return nil, err
	}

	conn, err := r.openStream(se, addr, username)
	if err != nil {
		// session died between selection and open — clear affinity and retry once
		slog.Warn("stream open failed, retrying with new session", "exitnode_id", se.id, "addr", addr, "error", err)
		r.clearAffinity(affinityKey)
		se, err = r.assignSession(affinityKey, host)
		if err != nil {
			return nil, err
		}
		return r.openStream(se, addr, username)
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
		// budget exhausted or session died — rotate
		r.setCooldown(aff.entry, domain)
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

// pickSession selects an eligible session for the given domain,
// excluding sessions in cooldown or at their concurrency limit.
// Cooldown is keyed by public IP so sessions sharing the same NAT address
// are treated as one unit — rotating between them would not change the IP
// the target site sees.
func (r *Router) pickSession(domain string) *sessionEntry {
	candidates := r.pool.snapshot()
	eligible := make([]*sessionEntry, 0, len(candidates))
	for _, se := range candidates {
		if se.activeStreams.Load() >= maxStreamsPerNode {
			continue
		}
		if exp, ok := r.cooldown[se.ip+":"+domain]; ok && time.Now().Before(exp) {
			continue
		}
		eligible = append(eligible, se)
	}
	if len(eligible) == 0 {
		return nil
	}
	return eligible[rand.IntN(len(eligible))]
}

func (r *Router) openStream(se *sessionEntry, addr, username string) (net.Conn, error) {
	stream, err := se.session.Open()
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(stream, "%s\n", addr); err != nil {
		_ = stream.Close()
		return nil, err
	}
	se.activeStreams.Add(1)
	r.pool.streams.Add(1)
	slog.Info("stream opened", "exitnode_id", se.id, "addr", addr, "username", username)
	return &trackedConn{Conn: stream, entry: se, pool: r.pool}, nil
}

func (r *Router) setCooldown(se *sessionEntry, domain string) {
	r.cooldown[se.ip+":"+domain] = time.Now().Add(cooldownDuration)
}

func (r *Router) randomExpiry() time.Time {
	jitter := float64(affinityBaseWindow) * (1 + (rand.Float64()*2-1)*affinityJitter)
	return time.Now().Add(time.Duration(jitter))
}
