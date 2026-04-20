package main

import (
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

var sessionIDCounter atomic.Uint64

type sessionEntry struct {
	id           uint64
	ip           string // public IP of the exit node as seen by the gateway
	session      *yamux.Session
	activeStreams atomic.Int32
}

func newSessionEntry(s *yamux.Session, ip string) *sessionEntry {
	return &sessionEntry{
		id:      sessionIDCounter.Add(1),
		ip:      ip,
		session: s,
	}
}

type Pool struct {
	mu      sync.Mutex
	entries []*sessionEntry
	streams sync.WaitGroup
	metrics *Metrics
}

func (p *Pool) add(e *sessionEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, e)
	slog.Info("pool: exitnode added", "exitnode_id", e.id, "total", len(p.entries))
	p.metrics.incExitnodes()
}

func (p *Pool) remove(e *sessionEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, entry := range p.entries {
		if entry.id == e.id {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			break
		}
	}
	slog.Info("pool: exitnode removed", "exitnode_id", e.id, "total", len(p.entries))
	p.metrics.decExitnodes()
}

func (p *Pool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		_ = e.session.Close()
	}
}

func (p *Pool) waitStreams(timeout time.Duration) {
	done := make(chan struct{})
	go func() { p.streams.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("shutdown: timed out waiting for active streams")
	}
}

// byIP returns all current entries (open or closed) sharing the given public IP.
func (p *Pool) byIP(ip string) []*sessionEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*sessionEntry
	for _, e := range p.entries {
		if e.ip == ip {
			out = append(out, e)
		}
	}
	return out
}

// snapshot returns alive entries without holding p.mu across the call.
func (p *Pool) snapshot() []*sessionEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	alive := make([]*sessionEntry, 0, len(p.entries))
	for _, e := range p.entries {
		if !e.session.IsClosed() {
			alive = append(alive, e)
		}
	}
	return alive
}

// trackedConn wraps a stream and signals the pool when closed.
type trackedConn struct {
	net.Conn
	once    sync.Once
	entry   *sessionEntry
	pool    *Pool
	metrics *Metrics
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.entry.activeStreams.Add(-1)
		c.pool.streams.Done()
		c.metrics.decStreams()
	})
	return c.Conn.Close()
}
