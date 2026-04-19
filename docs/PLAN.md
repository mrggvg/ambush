1. DB auth — gateway still uses hardcoded token and SOCKS5 credentials. Needs to validate exit node tokens via SHA-256 hash lookup and
   SOCKS5 credentials via pgcrypto bcrypt check, both with is_active + expires_at checks
2. IP tracking — on each exitnode connect, upsert into exit_node_ips (first/last seen). It's in the schema but nothing writes to it
3. WebSocket ping/pong keepalive — without it, silently dead connections stay in the pool and requests fail. gorilla supports it, just
   needs a ticker
4. Graceful shutdown — catch SIGTERM, stop accepting new SOCKS5 connections, wait for active streams to finish
5. TLS — both the exitnode WebSocket endpoint and SOCKS5 should be behind TLS in prod. Can be terminated at a reverse proxy
   (nginx/caddy) but needs a plan

Important but not day-one:

6. Fallback on dead session — if pool.Dial opens a stream on a session that just died, it should retry on another session rather than
   failing the request
7. Health endpoint — GET /health returning pool size, for load balancers and monitoring
8. Structured logging — replace log.Printf with slog, add request IDs
9. Metrics — active exitnodes, requests/sec, stream errors — even a simple Prometheus endpoint

Nice to have:

10. Rate limiting on SOCKS5 per credential
11. Max streams per session to avoid one client saturating one exitnode
12. Connection timeout on streams so hung connections don't pile up