package main

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
)

var errCredentialLimitExceeded = errors.New("rate limit: too many concurrent streams for this credential")

type credentialState struct {
	active atomic.Int32
}

// CredentialLimiter caps the number of concurrent open streams per SOCKS5 username,
// preventing a single credential from monopolising all exit nodes.
type CredentialLimiter struct {
	mu        sync.Mutex
	users     map[string]*credentialState
	maxActive int32
}

func NewCredentialLimiter(maxActive int32) *CredentialLimiter {
	return &CredentialLimiter{
		users:     make(map[string]*credentialState),
		maxActive: maxActive,
	}
}

// Acquire reserves one stream slot for username. On success it returns a release
// func that must be called exactly once when the stream closes. If the limit is
// already reached it returns errCredentialLimitExceeded.
func (l *CredentialLimiter) Acquire(username string) (func(), error) {
	l.mu.Lock()
	s, ok := l.users[username]
	if !ok {
		s = &credentialState{}
		l.users[username] = s
	}
	l.mu.Unlock()

	for {
		cur := s.active.Load()
		if cur >= l.maxActive {
			return nil, errCredentialLimitExceeded
		}
		if s.active.CompareAndSwap(cur, cur+1) {
			var once sync.Once
			return func() { once.Do(func() { s.active.Add(-1) }) }, nil
		}
	}
}

// ActiveStreams returns the current open stream count for username.
func (l *CredentialLimiter) ActiveStreams(username string) int32 {
	l.mu.Lock()
	s := l.users[username]
	l.mu.Unlock()
	if s == nil {
		return 0
	}
	return s.active.Load()
}

// rateLimitedConn releases the credential slot when the connection is closed.
type rateLimitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *rateLimitedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
