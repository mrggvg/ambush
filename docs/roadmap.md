# Roadmap

## Done

### Core tunnel
- [x] Exit node connects to gateway over WebSocket (`/exitnode?token=xxx`)
- [x] yamux multiplexes streams over the WebSocket connection
- [x] Exit node accepts yamux streams, reads target address, dials out, relays bidirectionally
- [x] Exit node setup via interactive terminal form (huh) — token saved to `~/.ambush/exitnode.json`
- [x] Exit node reconnects automatically on disconnect (5s retry loop)

### Gateway
- [x] WebSocket upgrade and yamux server session per exit node
- [x] Exit node pool with add/remove lifecycle
- [x] SOCKS5 server on `:1080` with username/password authentication
- [x] DB auth — exit node tokens validated via SHA-256 hash lookup
- [x] DB auth — SOCKS5 credentials validated via bcrypt (`pgcrypto`)
- [x] `is_active` and `expires_at` checks on token validation
- [x] IP tracking — upsert to `exit_node_ips` on each exit node connect
- [x] WebSocket ping/pong keepalive (30s interval, 10s pong timeout)
- [x] Graceful shutdown — drain streams, close sessions, shutdown HTTP server
- [x] Health endpoint `GET /health` — pool size and active streams
- [x] Fallback on dead session — retry with another exit node if stream open fails

### Smart routing
- [x] Domain affinity — same domain routes through same exit node within a window
- [x] Time-based rotation with jitter (5min base ± 20%)
- [x] Request budget per assignment (100 requests max before rotation)
- [x] Cooldown after rotation — rotated exit node excluded from domain for 10min; keyed by public IP so exit nodes sharing the same NAT address are treated as one unit
- [x] Concurrency limit per exit node (max 10 concurrent streams)
- [x] Stream idle timeout — 2min idle closes hung connections
- [x] Per-user affinity — different SOCKS5 users hitting the same domain use different exit nodes (`WithDialAndRequest` + `username:host` affinity key)
- [x] Per-credential rate limiting — cap on concurrent open streams per SOCKS5 username (`MAX_STREAMS_PER_CREDENTIAL`, default 20)

### API
- [x] User management (create, list)
- [x] Exit node token management (create, list, revoke)
- [x] Proxy credential management (create, list, revoke)
- [x] Admin bearer token authentication
- [x] Bruno collection for all endpoints

### Database
- [x] Schema with `users`, `exit_node_tokens`, `exit_node_ips`, `proxy_credentials`
- [x] `user_exit_node_diversity` view
- [x] Hosted on Supabase

### Gateway
- [x] `wsConn.SetDeadline` sets both read and write deadlines — previously only write was set, causing goroutine leaks on stalled connections

### Observability
- [x] Structured logging with `slog` (JSON output, consistent field names) in gateway

### Deployment
- [x] Dockerfile for gateway and API
- [x] Dockerfile for exit node

### Security & correctness
- [x] Rate limit on failed token auth attempts — 10 failures/min → 15-min block per IP (`authRateLimiter`)
- [x] Cooldown only applied on deliberate rotations (budget/expiry) — `session_closed` and `concurrency` no longer block a reconnected node

### Routing improvements
- [x] Exit node health scoring — rolling window of stream-open outcomes per node; degraded nodes deprioritised without being removed (`nodeHealth`)
- [x] Request correlation IDs — short random hex ID generated per SOCKS5 request, carried through all gateway log lines and written into the yamux stream header so exit node logs use the same ID

### Per-node observability
- [x] `ambush_streams_active{exitnode_id}` and `ambush_stream_errors_total{exitnode_id}` — per-node breakdown to identify slow or blocked exit nodes

### Exit node reliability
- [x] Exponential backoff with jitter on reconnect (1s → 60s cap) — prevents thundering herd on gateway restart
- [x] `wsConn.SetDeadline` sets both read and write deadlines in exit node (mirrors gateway fix)

### Testing
- [x] Unit tests for router — affinity, rotation, cooldown, concurrency limit, IP-based grouping
- [x] Unit tests for pool, rate limiter, wsConn, auth rate limiter, node health
- [x] CI runs unit tests with race detector (`go test ./cmd/... -race`) before E2E
- [x] E2E — basic traffic flow through gateway (TLS) → exit node → echo server
- [x] E2E — multiple sequential requests (affinity persistence)
- [x] E2E — exit node reconnect: kill, wait for pool=0, restart, verify requests succeed without cooldown blocking

---

## TODO

### Security & correctness
- [ ] Per-user exit node diversity guarantee

### Observability
- [ ] TLS on the exit node ↔ gateway tunnel — self-signed CA, no domain required (see [docs/tls.md](tls.md))

### Routing improvements
- [ ] Geo-awareness — record exit node country, allow SOCKS5 clients to request a specific region via credentials

### Deployment
- [ ] systemd unit file for exit node (run as a persistent background service)
- [ ] High availability — multiple gateway instances, exit nodes register with all of them

### Testing
- [ ] Load test — measure throughput and latency under concurrent connections

---

## Design principles

Ambush is a **self-contained, integration-agnostic proxy network**. It owns its own database and exposes a clean HTTP API. Any system that needs a pool of diverse outbound IPs — a scraper, a browser automation platform, a multi-account tool, anything — can integrate with it by calling the API to provision credentials and pointing its SOCKS5 client at the gateway.

The network is **protocol-agnostic**. Because traffic is tunnelled as raw TCP streams, anything that runs over TCP works: HTTP, HTTPS, any proprietary protocol. The gateway and exit nodes have no awareness of what is being proxied.

Ambush does not dictate how credentials are issued, how users are onboarded, or what the traffic is used for. That is the responsibility of whatever system integrates with it.

---

## Priority order (suggested)

1. **End-to-end test** — verify the system actually works before building more on top of it
2. **systemd unit file** — so exit nodes can run as persistent background services without Docker
3. **Prometheus metrics** — active exit nodes, requests/s, rotation events; needed once deployed at scale
4. **Per-user diversity guarantee** — today multiple users can land on the same exit node; fix requires tracking assignments across users in `pickSession`
