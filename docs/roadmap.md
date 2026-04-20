# Roadmap

## Done

### Core tunnel
- [x] Exit node connects to gateway over WebSocket (`/exitnode?token=xxx`)
- [x] yamux multiplexes streams over the WebSocket connection
- [x] Exit node accepts yamux streams, reads `reqID addr` header, dials out, relays bidirectionally
- [x] Exit node setup via interactive terminal form (huh) — token saved to `~/.ambush/exitnode.json`
- [x] Exponential backoff with jitter on reconnect (1s → 60s cap) — prevents thundering herd on gateway restart
- [x] `wsConn.SetDeadline` sets both read and write deadlines in exit node and gateway — prevents goroutine leaks on stalled connections

### Gateway
- [x] WebSocket upgrade and yamux server session per exit node
- [x] Exit node pool with add/remove lifecycle
- [x] SOCKS5 server on `:1080` with username/password authentication
- [x] DB auth — exit node tokens validated via SHA-256 hash lookup
- [x] DB auth — SOCKS5 credentials validated via bcrypt (`pgcrypto`); base credential extracted from structured username
- [x] `is_active` and `expires_at` checks on token validation
- [x] IP tracking — upsert to `exit_node_ips` (with `country` and `node_type`) on each exit node connect
- [x] WebSocket ping/pong keepalive (30s interval, 10s pong timeout)
- [x] Graceful shutdown — drain streams, close sessions, shutdown HTTP server
- [x] Health endpoint `GET /health` — pool size and active streams
- [x] Fallback on dead session — retry with a different exit node (excluding failed) if stream open fails
- [x] Rate limit on failed token auth attempts — 10 failures/min → 15-min block per IP (`authRateLimiter`)

### Routing
- [x] Model B — fresh exit per connection (default); maximises IP diversity and throughput
- [x] Model A — sticky sessions via `alice-session-<token>` SOCKS5 username; `SessionStore` with TTL and dead-node eviction
- [x] SOCKS5 username parsing — `base[-key-val...]` format; `session` and `country` keys; base used for auth and rate limiting
- [x] Geo-filtering — exit nodes self-report `AMBUSH_COUNTRY` / `AMBUSH_TYPE`; `Constraints` filter applied in `pickSession`; `alice-country-us` routes to US nodes
- [x] Concurrency limit per exit node (max 10 concurrent streams)
- [x] Per-credential rate limiting — cap on concurrent streams per SOCKS5 base credential (`MAX_STREAMS_PER_CREDENTIAL`, default 20)
- [x] Stream idle timeout — 2min idle closes hung connections
- [x] Request correlation IDs — 12-char hex ID per SOCKS5 request, carried through all gateway log lines and written into the yamux stream header so exit node logs use the same ID

### Health & observability
- [x] Structured logging with `slog` (JSON output, consistent field names)
- [x] Exit node health scoring — rolling 20-outcome window per node; degraded nodes deprioritised but not removed (`nodeHealth`)
- [x] `ambush_streams_active{exitnode_id}` and `ambush_stream_errors_total{exitnode_id}` — per-node Prometheus metrics

### Gateway API (`/api/v1/*`)
- [x] `GET /api/v1/exits` — live pool snapshot with per-node metadata and health
- [x] `GET /api/v1/stats` — aggregate exitnode count and active stream count
- [x] `POST /api/v1/sessions` — pre-allocate a sticky session token with optional TTL and Constraints; returns token + exit info
- [x] `POST /api/v1/feedback` — record target-level outcome into node health without a new SOCKS5 connection
- [x] Bearer-token protection via `GATEWAY_ADMIN_TOKEN`; disabled (no routes mounted) if env var is unset

### Admin API (`cmd/api`)
- [x] User management (create, list)
- [x] Exit node token management (create, list, revoke)
- [x] Proxy credential management (create, list, revoke)
- [x] Admin bearer token authentication
- [x] Bruno collection for all endpoints

### Database
- [x] Schema with `users`, `exit_node_tokens`, `exit_node_ips` (+ `country`, `node_type`), `proxy_credentials`
- [x] `user_exit_node_diversity` view
- [x] Hosted on Supabase

### Deployment
- [x] Dockerfile for gateway and API
- [x] Dockerfile for exit node

### Testing
- [x] Unit tests for router (Model A/B, Constraints, concurrency limit, health scoring, retry)
- [x] Unit tests for SessionStore, username parser, credential limiter, pool, wsConn, auth rate limiter, node health, gateway API
- [x] CI — unit tests with race detector (`go test ./cmd/... -race`) before E2E
- [x] E2E — basic traffic flow through gateway (TLS) → exit node → echo server
- [x] E2E — multiple sequential requests
- [x] E2E — exit node reconnect: kill → pool=0 → restart → requests succeed

---

## TODO

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
