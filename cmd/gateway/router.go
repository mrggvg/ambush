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

const maxStreamsPerNode = 10

// Router handles routing: per-exitnode concurrency limits, per-credential
// rate limiting, and Model A (sticky) / Model B (fresh) session selection.
type Router struct {
	mu       sync.Mutex
	pool     *Pool
	limiter  *CredentialLimiter
	metrics  *Metrics
	sessions *SessionStore
}

func NewRouter(pool *Pool, limiter *CredentialLimiter, metrics *Metrics, sessions *SessionStore) *Router {
	return &Router{
		pool:     pool,
		limiter:  limiter,
		metrics:  metrics,
		sessions: sessions,
	}
}

// CreateSession pre-allocates a session token bound to an eligible exit node.
// The caller embeds the returned token in SOCKS5 usernames as "base-session-<token>"
// to get Model A (sticky) routing for ttl. Returns an error if no eligible node exists.
func (r *Router) CreateSession(c Constraints, ttl time.Duration) (token string, expiresAt time.Time, se *sessionEntry, err error) {
	se = r.pickSessionExcluding(nil, c)
	if se == nil {
		return "", time.Time{}, nil, errors.New("no eligible exit nodes")
	}
	token = newSessionToken()
	expiresAt = time.Now().Add(ttl)
	r.sessions.SetWithTTL(token, se, ttl)
	slog.Info("session pre-allocated", "token", token, "exitnode_id", se.id, "ttl", ttl)
	return token, expiresAt, se, nil
}

// EvictSession removes all session-token bindings pointing to se.
// Call when an exit node disconnects.
func (r *Router) EvictSession(se *sessionEntry) {
	r.sessions.EvictForSession(se)
}

// DialWithUser is the core dial function.
// username is parsed so that structured variants share the same rate-limit slot.
func (r *Router) DialWithUser(ctx context.Context, _, addr, username string) (net.Conn, error) {
	reqID := requestIDFromCtx(ctx)
	parsed := ParseUsername(username)

	release, err := r.limiter.Acquire(parsed.Base)
	if err != nil {
		slog.Warn("credential rate limit exceeded", "req_id", reqID, "username", parsed.Base, "active_streams", r.limiter.ActiveStreams(parsed.Base))
		r.metrics.incCredLimitExceeded()
		r.metrics.incDials("rate_limited")
		return nil, err
	}

	conn, err := r.dial(addr, parsed, reqID)
	if err != nil {
		r.metrics.incDials("error")
		release()
		return nil, err
	}
	r.metrics.incDials("success")
	return &rateLimitedConn{Conn: conn, release: release}, nil
}

// Constraints optionally filters exit node selection by metadata.
// Empty fields are unconstrained.
type Constraints struct {
	Country  string // ISO-3166-1 alpha-2, e.g. "us"
	NodeType string // "residential" | "datacenter" | "mobile"
}

func (c Constraints) matches(se *sessionEntry) bool {
	if c.Country != "" && se.country != c.Country {
		return false
	}
	if c.NodeType != "" && se.nodeType != c.NodeType {
		return false
	}
	return true
}

// dial dispatches to Model A (sticky) or Model B (fresh) based on SessionToken.
func (r *Router) dial(addr string, parsed ParsedUsername, reqID string) (net.Conn, error) {
	c := Constraints{Country: parsed.Country}
	if parsed.SessionToken != "" {
		return r.dialWithSession(addr, parsed, reqID, c)
	}
	return r.dialFresh(addr, parsed.Base, reqID, c)
}

// dialFresh picks a new exit node on every call (Model B).
func (r *Router) dialFresh(addr, username, reqID string, c Constraints) (net.Conn, error) {
	se := r.pickSessionExcluding(nil, c)
	if se == nil {
		slog.Warn("no eligible exit nodes", "req_id", reqID, "addr", addr, "pool_size", len(r.pool.snapshot()))
		return nil, errors.New("no exitnodes available")
	}

	conn, err := r.openStream(se, addr, username, reqID)
	if err != nil {
		se.health.record(false)
		r.metrics.incStreamErrors(strconv.FormatUint(se.id, 10))
		slog.Warn("stream open failed, retrying with new session", "req_id", reqID, "exitnode_id", se.id, "addr", addr, "error", err)

		se2 := r.pickSessionExcluding(se, c)
		if se2 == nil {
			return nil, errors.New("no exitnodes available after retry")
		}
		conn, err = r.openStream(se2, addr, username, reqID)
		if err != nil {
			se2.health.record(false)
			r.metrics.incStreamErrors(strconv.FormatUint(se2.id, 10))
			return nil, err
		}
		return conn, nil
	}
	return conn, nil
}

// dialWithSession pins all requests for a token to the same exit node (Model A).
// If the pinned node is dead or the token is new, a fresh node is selected and stored.
func (r *Router) dialWithSession(addr string, parsed ParsedUsername, reqID string, c Constraints) (net.Conn, error) {
	token := parsed.SessionToken

	if se := r.sessions.Get(token); se != nil && !se.session.IsClosed() {
		conn, err := r.openStream(se, addr, parsed.Base, reqID)
		if err == nil {
			slog.Info("session reused", "req_id", reqID, "token", token, "exitnode_id", se.id)
			return conn, nil
		}
		se.health.record(false)
		r.metrics.incStreamErrors(strconv.FormatUint(se.id, 10))
		slog.Warn("session stream failed, re-assigning", "req_id", reqID, "token", token, "exitnode_id", se.id, "error", err)
		r.sessions.Delete(token)
	}

	se := r.pickSessionExcluding(nil, c)
	if se == nil {
		slog.Warn("no eligible exit nodes", "req_id", reqID, "addr", addr, "pool_size", len(r.pool.snapshot()))
		return nil, errors.New("no exitnodes available")
	}
	r.sessions.Set(token, se)

	conn, err := r.openStream(se, addr, parsed.Base, reqID)
	if err != nil {
		se.health.record(false)
		r.metrics.incStreamErrors(strconv.FormatUint(se.id, 10))
		r.sessions.Delete(token)
		return nil, err
	}
	slog.Info("session bound to exit node", "req_id", reqID, "token", token, "exitnode_id", se.id)
	return conn, nil
}

// pickSessionExcluding selects a random eligible exit node, skipping exclude (may be nil)
// and applying c. Healthy nodes are preferred; degraded nodes used as last resort.
func (r *Router) pickSessionExcluding(exclude *sessionEntry, c Constraints) *sessionEntry {
	candidates := r.pool.snapshot()
	var healthy, degraded []*sessionEntry
	for _, se := range candidates {
		if exclude != nil && se.id == exclude.id {
			continue
		}
		if se.session.IsClosed() {
			continue
		}
		if se.activeStreams.Load() >= maxStreamsPerNode {
			continue
		}
		if !c.matches(se) {
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
		slog.Warn("all eligible exit nodes are degraded, using fallback", "degraded_count", len(degraded))
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
