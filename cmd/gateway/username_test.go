package main

import (
	"context"
	"testing"
	"time"
)

func TestParseUsername(t *testing.T) {
	cases := []struct {
		raw          string
		base         string
		sessionToken string
		country      string
	}{
		// bare credential
		{"alice", "alice", "", ""},
		// base with hyphens, no keys
		{"alice-bob", "alice-bob", "", ""},
		// session key only
		{"alice-session-tok123", "alice", "tok123", ""},
		// country key only
		{"alice-country-us", "alice", "", "us"},
		// both keys — session first
		{"alice-session-tok123-country-us", "alice", "tok123", "us"},
		// both keys — country first
		{"alice-country-gb-session-xyz", "alice", "xyz", "gb"},
		// hyphenated base before keys
		{"my-user-session-abc-country-de", "my-user", "abc", "de"},
		// unknown key before any known key — folded into base
		{"alice-zone-foo", "alice-zone-foo", "", ""},
		// unknown key before first known key — folded into base; known key after is parsed
		{"alice-zone-foo-session-tok1", "alice-zone-foo", "tok1", ""},
		// dangling key with no value — ignored
		{"alice-session", "alice", "", ""},
		// empty string — base is empty
		{"", "", "", ""},
		// only a known key with no value — base is empty
		{"session", "", "", ""},
		// multi-segment token value (only first segment is used — no hyphens in values)
		{"alice-session-tok-extra", "alice", "tok", ""},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got := ParseUsername(tc.raw)
			if got.Base != tc.base {
				t.Errorf("Base: got %q, want %q", got.Base, tc.base)
			}
			if got.SessionToken != tc.sessionToken {
				t.Errorf("SessionToken: got %q, want %q", got.SessionToken, tc.sessionToken)
			}
			if got.Country != tc.country {
				t.Errorf("Country: got %q, want %q", got.Country, tc.country)
			}
		})
	}
}

// TestDialWithUser_RateLimitUsesBase verifies that a structured username
// (alice-session-tok123) shares the same rate-limit slot as the bare name (alice).
func TestDialWithUser_RateLimitUsesBase(t *testing.T) {
	p, servers := poolWithSessions(t, 3)
	for _, s := range servers {
		serveStreams(s)
	}
	r := &Router{
		pool:     p,
		limiter:  NewCredentialLimiter(1),
		sessions: NewSessionStore(context.Background(), time.Hour),
	}

	// structured username consumes alice's single slot
	conn1, err := r.DialWithUser(nil, "tcp", "example.com:80", "alice-session-tok123")
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()

	// bare "alice" must be blocked — same base credential
	_, err = r.DialWithUser(nil, "tcp", "example.com:80", "alice")
	if err == nil {
		t.Fatal("expected rate limit error: alice-session-tok123 and alice share the same slot")
	}
	if err != errCredentialLimitExceeded {
		t.Fatalf("expected errCredentialLimitExceeded, got: %v", err)
	}
}
