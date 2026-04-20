# Routing Refactor — Implementation Plan

## Motivation

The current router ties every `(username, domain)` pair to a sticky exit node for up to 5 minutes with a request budget of 100. This made sense for hiding scraper patterns, but it limits throughput: a single SOCKS5 credential can only use one IP at a time per domain, and the affinity window means the same IP is visible to the target for minutes at a stretch.

The new design separates two distinct use cases:

- **Model B (default)** — every connection gets a fresh exit node selected at random from the healthy pool. No affinity, no cooldown. Maximises IP diversity and throughput.
- **Model A (opt-in)** — a named session token in the SOCKS5 username pins all requests to a single exit for the token's TTL. Used when the target needs session continuity (logged-in scraping, multi-step flows).

Clients select behaviour through the SOCKS5 username encoding:
- `myuser` → Model B (fresh exit per connection)
- `myuser-session-abc123` → Model A (sticky to whichever exit node `abc123` resolved to)

This matches the Bright Data / Oxylabs username format that existing SOCKS5 client libraries already support.

---

## Stage 1 — Model B: fresh exit per connection

**Files:** `cmd/gateway/router.go`, `cmd/gateway/router_test.go`

Remove the affinity and cooldown subsystems entirely:
- Delete: `affinityEntry`, `affinity` map, `cooldown` map, `cleanupLoop`
- Delete: `assignSession`, `clearAffinity`, `isAffinityValid`, `rotationReason`, `randomExpiry`, `setCooldown`
- Delete constants: `affinityBaseWindow`, `affinityJitter`, `maxRequestsPerNode`, `cooldownDuration`
- Simplify `dial()`: call `pickSession()` directly (no domain arg needed), retry once with a different node on stream-open failure
- Remove `rotationsTotal` from metrics (no rotations in Model B)

`pickSession()` retains health-tier selection (healthy before degraded) and stream-limit cap. No domain argument needed.

Tests to delete: all `isAffinityValid`, `assignSession`, `setCooldown`, `cooldown cleanup`, `randomExpiry`, and affinity-stickiness tests.

Tests to keep/add: `pickSession` basics, `DialWithUser` flow, retry-on-dead-session, concurrency limit, credential rate limit.

---

## Stage 2 — Username parsing

**New file:** `cmd/gateway/username.go`

Parse the structured SOCKS5 username into a `ParsedUsername` struct:

```
alice                         → {Base: "alice"}
alice-session-tok123          → {Base: "alice", SessionToken: "tok123"}
alice-country-us              → {Base: "alice", Country: "us"}
alice-session-tok123-country-us → {Base: "alice", SessionToken: "tok123", Country: "us"}
```

Format: `base[-key1-val1[-key2-val2...]]` — keys are fixed strings (`session`, `country`).

`DialWithUser` extracts `ParsedUsername` and passes it through to `dial()`:
- DB auth still uses `Base` (credential lookup unchanged)
- Credential rate limiting uses `Base`
- `dial()` uses `SessionToken` to choose Model A vs Model B
- `dial()` uses `Country` for geo-filtering in Stage 4

Unit tests: table-driven, covering all combinations including unknown keys (ignored gracefully).

---

## Stage 3 — Model A: sticky sessions via username token

**New file:** `cmd/gateway/sessions.go`

`SessionStore` maps `token → *sessionEntry` with a TTL (default 30 min). Background goroutine evicts expired entries.

```go
type SessionStore struct {
    mu      sync.Mutex
    entries map[string]*sessionEntry
    ttl     time.Duration
}
func (s *SessionStore) Get(token string) *sessionEntry        // nil if not found/expired
func (s *SessionStore) Set(token string, se *sessionEntry)
func (s *SessionStore) evictLoop(ctx context.Context)
```

`dial()` logic with Model A:
1. If `ParsedUsername.SessionToken != ""`:
   - Look up token in `SessionStore`
   - If found and session alive → `openStream` (no rotation)
   - If not found or session dead → `pickSession()`, store in `SessionStore`, `openStream`
2. Else → existing Model B path

On exit-node disconnect: walk `SessionStore` and evict all entries pointing to the dead session (so the next Model A request picks a fresh node rather than erroring).

Unit tests: get/set/evict, TTL expiry, dead-session eviction on pool remove.

---

## Stage 4 — Exit metadata: country and type

**Files:** `db/schema.sql`, `cmd/exitnode/main.go`, `cmd/gateway/pool.go`

Schema change — add to `exit_node_ips`:
```sql
ALTER TABLE exit_node_ips ADD COLUMN country TEXT;
ALTER TABLE exit_node_ips ADD COLUMN node_type TEXT DEFAULT 'residential';
```

Exit node reads two optional env vars on startup:
```
AMBUSH_COUNTRY=us
AMBUSH_TYPE=residential   # residential | datacenter | mobile
```

Exit node sends these in the WebSocket upgrade query string:
```
/exitnode?token=xxx&country=us&type=residential
```

Gateway: `sessionEntry` gains `Country string` and `NodeType string` fields, populated from query params on connect. `trackExitNodeIP` upserts these columns.

`pickSession()` gains an optional `Constraints` struct:
```go
type Constraints struct {
    Country  string
    NodeType string
}
```
Applied as pre-filters before the healthy/degraded split.

---

## Stage 5 — Gateway read API

**New file:** `cmd/gateway/gatewayapi.go`

Three endpoints, admin-bearer-token protected (same mechanism as `cmd/api`):

```
GET /api/v1/exits
```
Returns live pool snapshot with per-node metadata:
```json
[{"id": "42", "ip": "1.2.3.4", "country": "us", "type": "residential",
  "active_streams": 3, "health": "healthy"}]
```

```
GET /api/v1/stats
```
Aggregate counters:
```json
{"exitnodes": 5, "active_streams": 12, "total_dials": 9843}
```

```
POST /api/v1/feedback
```
Body: `{"session_token": "tok123", "success": false}`
Records outcome into `nodeHealth` for the exit node bound to that session token.
Allows external callers (the scraper) to report target-level failures without going through SOCKS5.

---

## Stage 6 — POST /sessions API

**File:** `cmd/gateway/gatewayapi.go`

```
POST /api/v1/sessions
```
Body: `{"ttl_seconds": 1800, "country": "us", "node_type": "residential"}`

Response:
```json
{
  "token": "abc123",
  "expires_at": "2026-04-20T15:00:00Z",
  "exit": {"ip": "1.2.3.4", "country": "us", "type": "residential"}
}
```

Server picks an eligible exit node (applying Constraints from Stage 4), pre-allocates the session token in `SessionStore`, and returns it. The client embeds the token in subsequent SOCKS5 usernames (`alice-session-abc123`) to get Model A behaviour without the gateway needing to pick a node on the first request.

This is the integration point for a scraper that wants to pre-warm sessions before a crawl run.

---

## Priority order

1. **Stage 1** — unblocks all throughput gains immediately; simplest change
2. **Stage 2** — required by Stage 3; small, self-contained
3. **Stage 3** — Model A; needed for session-continuity use cases
4. **Stage 4** — geo-awareness; DB migration required
5. **Stage 5** — observability API; useful once deployed at scale
6. **Stage 6** — pre-allocated sessions; nice-to-have for high-concurrency scrapers
