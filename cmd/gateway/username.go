package main

import "strings"

// ParsedUsername holds the structured fields from a SOCKS5 username.
//
// Format: base[-key1-val1[-key2-val2...]]
// Known keys: session, country
// Unknown keys and their values are silently ignored.
// Values must not contain hyphens.
//
// Examples:
//
//	alice                        → Base: "alice"
//	alice-session-tok123         → Base: "alice", SessionToken: "tok123"
//	alice-country-us             → Base: "alice", Country: "us"
//	alice-session-tok123-country-us → Base: "alice", SessionToken: "tok123", Country: "us"
type ParsedUsername struct {
	Base         string // credential used for DB auth and rate limiting
	SessionToken string // non-empty → Model A (sticky session)
	Country      string // non-empty → geo-filter exit nodes (Stage 4)
}

var knownKeys = map[string]bool{"session": true, "country": true}

// ParseUsername splits a raw SOCKS5 username into its structured fields.
// If the username contains no known key segments, the entire string is the Base.
func ParseUsername(raw string) ParsedUsername {
	parts := strings.Split(raw, "-")

	// find the index of the first known key
	firstKeyIdx := len(parts)
	for i, p := range parts {
		if knownKeys[p] {
			firstKeyIdx = i
			break
		}
	}

	result := ParsedUsername{
		Base: strings.Join(parts[:firstKeyIdx], "-"),
	}

	// parse key-value pairs from firstKeyIdx onward; skip dangling keys
	rest := parts[firstKeyIdx:]
	for i := 0; i+1 < len(rest); i += 2 {
		switch rest[i] {
		case "session":
			result.SessionToken = rest[i+1]
		case "country":
			result.Country = rest[i+1]
		}
	}
	return result
}
