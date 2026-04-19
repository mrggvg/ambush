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

func (r *Router) Dial(ctx context.Context, _, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	se, err := r.assignSession(host)
	if err != nil {
		return nil, err
	}
	return r.openStream(se, addr)
}

func (r *Router) assignSession(host string) (*sessionEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if aff := r.affinity[host]; aff != nil {
		if r.isAffinityValid(aff) {
			aff.requests++
			return aff.entry, nil
		}
		// budget exhausted or session died — rotate
		r.setCooldown(aff.entry, host)
		delete(r.affinity, host)
	}

	se := r.pickSession(host)
	if se == nil {
		return nil, errors.New("no exitnodes available")
	}

	r.affinity[host] = &affinityEntry{
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
