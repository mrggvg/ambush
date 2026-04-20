package main

import (
	"context"
	"sync"
	"time"
)

const defaultSessionTTL = 30 * time.Minute

type sessionRecord struct {
	entry     *sessionEntry
	expiresAt time.Time
}

// SessionStore maps a client-supplied session token to a pinned exit node (Model A).
// All methods are safe for concurrent use.
type SessionStore struct {
	mu      sync.Mutex
	records map[string]*sessionRecord
	ttl     time.Duration
}

func NewSessionStore(ctx context.Context, ttl time.Duration) *SessionStore {
	s := &SessionStore{
		records: make(map[string]*sessionRecord),
		ttl:     ttl,
	}
	go s.evictLoop(ctx)
	return s
}

// Get returns the pinned sessionEntry for token, or nil if not found or expired.
func (s *SessionStore) Get(token string) *sessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[token]
	if !ok {
		return nil
	}
	if time.Now().After(rec.expiresAt) {
		delete(s.records, token)
		return nil
	}
	return rec.entry
}

// Set stores or refreshes the binding using the store's default TTL.
func (s *SessionStore) Set(token string, se *sessionEntry) {
	s.SetWithTTL(token, se, s.ttl)
}

// SetWithTTL stores or refreshes the binding with an explicit TTL.
func (s *SessionStore) SetWithTTL(token string, se *sessionEntry, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[token] = &sessionRecord{
		entry:     se,
		expiresAt: time.Now().Add(ttl),
	}
}

// Delete removes the token binding explicitly.
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, token)
}

// EvictForSession removes all bindings that point to se.
// Called when an exit node disconnects so the next Model A request re-picks.
func (s *SessionStore) EvictForSession(se *sessionEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, rec := range s.records {
		if rec.entry.id == se.id {
			delete(s.records, token)
		}
	}
}

func (s *SessionStore) evictLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			s.mu.Lock()
			for token, rec := range s.records {
				if now.After(rec.expiresAt) {
					delete(s.records, token)
				}
			}
			s.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}
