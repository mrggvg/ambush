package main

import (
	"context"
	"errors"
	"fmt"
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
// and per-exitnode concurrency limits.
type Router struct {
	mu       sync.Mutex
	pool     *Pool
	affinity map[string]*affinityEntry // domain -> active assignment
	cooldown map[string]time.Time      // "sessionID:domain" -> expires
}

func NewRouter(pool *Pool) *Router {
	return &Router{
		pool:     pool,
		affinity: make(map[string]*affinityEntry),
		cooldown: make(map[string]time.Time),
	}
}

// DialWithUser is the core dial function. username scopes affinity so two
// different SOCKS5 users hitting the same domain use different exit nodes.
func (r *Router) DialWithUser(ctx context.Context, _, addr, username string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	affinityKey := username + ":" + host

	se, err := r.assignSession(affinityKey, host)
	if err != nil {
		return nil, err
	}

	conn, err := r.openStream(se, addr)
	if err != nil {
		// session died between selection and open — clear affinity and retry once
		r.clearAffinity(affinityKey)
		se, err = r.assignSession(affinityKey, host)
		if err != nil {
			return nil, err
		}
		return r.openStream(se, addr)
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
func (r *Router) pickSession(domain string) *sessionEntry {
	candidates := r.pool.snapshot()
	eligible := make([]*sessionEntry, 0, len(candidates))
	for _, se := range candidates {
		if se.activeStreams.Load() >= maxStreamsPerNode {
			continue
		}
		key := fmt.Sprintf("%d:%s", se.id, domain)
		if exp, ok := r.cooldown[key]; ok && time.Now().Before(exp) {
			continue
		}
		eligible = append(eligible, se)
	}
	if len(eligible) == 0 {
		return nil
	}
	return eligible[rand.IntN(len(eligible))]
}

func (r *Router) openStream(se *sessionEntry, addr string) (net.Conn, error) {
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
	return &trackedConn{Conn: stream, entry: se, pool: r.pool}, nil
}

func (r *Router) setCooldown(se *sessionEntry, domain string) {
	key := fmt.Sprintf("%d:%s", se.id, domain)
	r.cooldown[key] = time.Now().Add(cooldownDuration)
}

func (r *Router) randomExpiry() time.Time {
	jitter := float64(affinityBaseWindow) * (1 + (rand.Float64()*2-1)*affinityJitter)
	return time.Now().Add(time.Duration(jitter))
}
