<p align="center">
  <img src="docs/ambush.png" alt="Ambush" width="200" />
</p>

<h1 align="center">Ambush</h1>

<p align="center">
  Self-hosted proxy network — exit nodes connect outbound, SOCKS5 clients route through them
</p>

<p align="center">
  <img src="https://img.shields.io/badge/go-1.26-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go version" />
  <img src="https://img.shields.io/badge/license-MIT-blue?style=flat-square" alt="License" />
  <img src="https://github.com/mrggvg/ambush/actions/workflows/e2e.yml/badge.svg" alt="E2E" />
</p>

---

Ambush is a self-hosted TCP proxy network. You run a central **gateway** and a lightweight **API**. Volunteer machines run the **exit node** binary — they connect outbound to the gateway over WebSocket, no inbound port required. Your SOCKS5-capable applications route traffic through the gateway, which tunnels it out through a connected exit node.

Because traffic is forwarded as raw TCP streams, **anything that runs over TCP works** — HTTP, HTTPS, any protocol.

**Designed for:** distributed web scraping, residential IP diversity, multi-account tooling, or any scenario where you need a pool of varied outbound IPs you control.

---

## How it works

```mermaid
flowchart LR
    app["Your app\n(scraper, browser…)"]

    subgraph gw["Gateway"]
        ws[":8080 WebSocket"]
        socks[":1080 SOCKS5"]
    end

    subgraph nodes["Exit Nodes"]
        e1["Exit Node\n(your VPS)"]
        e2["Exit Node\n(friend's machine)"]
    end

    target["target.com"]

    app -->|"SOCKS5 + auth"| socks
    e1 -->|"WebSocket + yamux"| ws
    e2 -->|"WebSocket + yamux"| ws
    ws -->|"stream"| e1
    ws -->|"stream"| e2
    e1 -->|TCP| target
    e2 -->|TCP| target
```

1. Exit nodes connect **outbound** to the gateway — works from behind NAT, no port forwarding needed
2. The gateway multiplexes TCP streams over each WebSocket connection using [yamux](https://github.com/hashicorp/yamux)
3. SOCKS5 clients authenticate with a username/password credential you provision
4. The router picks an exit node, opens a stream, and the exit node dials the real target

---

## Features

**Smart routing** — the router keeps traffic looking natural to anti-bot systems:
- **Domain affinity** — a given domain sticks to the same exit node for 5 min ± 20% jitter, then rotates
- **IP-aware cooldown** — after rotation, that public IP is excluded from that domain for 10 minutes; exit nodes sharing the same NAT address (e.g. two machines behind a home router) are grouped and cooled down together
- **Per-credential isolation** — each SOCKS5 username has its own affinity map, so different users hitting the same domain get different exit nodes
- **Per-credential rate limiting** — configurable cap on concurrent streams per credential so no single user monopolises the pool
- **Concurrency cap** — each exit node handles at most 10 simultaneous streams
- **Automatic failover** — if a session dies between selection and stream open, the router retries transparently

**Security:**
- TLS on the exit node ↔ gateway tunnel using a self-signed CA — no public domain required
- Exit node tokens are SHA-256 hashed in the database; never stored plain
- SOCKS5 passwords are bcrypt-hashed via pgcrypto

**Operations:**
- Prometheus metrics at `GET /metrics` — active exit nodes, open streams, dial rate, rotation events, stream errors
- `GET /health` — live pool state, no auth required
- Graceful shutdown — drains active streams before closing

---

## What you need to deploy

Three things: a **Postgres database**, a **gateway server**, and **exit nodes** running on machines whose IPs you want to use.

### 1 — Database

Run [`db/schema.sql`](db/schema.sql) against any Postgres 14+ instance. The schema requires the `pgcrypto` extension (available by default on Supabase, RDS, and most managed providers).

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;
\i db/schema.sql
```

Keep the connection string — you'll need it for the gateway and API.

---

### 2 — Gateway + API

Both run as single Docker containers. The gateway needs two ports open: `8080` (exit node WebSocket connections) and `1080` (SOCKS5 proxy).

**Generate TLS certificates first** (one time, on the gateway machine):

```bash
./cmd/gencerts/run.sh ./certs
# outputs: ca.crt  ca.key  gateway.crt  gateway.key
# keep ca.key secret — you need ca.crt when onboarding exit nodes
```

**Run the gateway:**

```bash
docker run -d \
  --name ambush-gateway \
  --restart unless-stopped \
  -p 8080:8080 \
  -p 1080:1080 \
  -v /path/to/certs:/certs:ro \
  -e DATABASE_URL="postgres://..." \
  -e TLS_CERT=/certs/gateway.crt \
  -e TLS_KEY=/certs/gateway.key \
  ghcr.io/yourname/ambush-gateway:latest
```

**Run the API:**

```bash
docker run -d \
  --name ambush-api \
  --restart unless-stopped \
  -p 8081:8081 \
  -e DATABASE_URL="postgres://..." \
  -e ADMIN_TOKEN="your-secret-admin-token" \
  ghcr.io/yourname/ambush-api:latest
```

**Verify:**

```bash
curl https://your-server:8080/health
# {"status":"ok","exitnodes":0,"active_streams":0}
```

> For local development without TLS, omit `TLS_CERT`/`TLS_KEY` and use `http://` / `ws://`.
> See [`cmd/gateway/RUNNING.md`](cmd/gateway/RUNNING.md) for full options.

---

### 3 — Exit nodes

Exit nodes are distributed machines whose IPs become your proxy pool. Each one needs a **token** — generated via the API — and a one-line Docker command.

**Create a user and token for each exit node operator:**

```bash
# create a user
curl -s -X POST https://your-server:8081/users \
  -H "Authorization: Bearer your-admin-token" \
  -H "Content-Type: application/json" \
  -d '{"display_name": "alice"}' | jq

# generate a token (replace USER_ID)
curl -s -X POST https://your-server:8081/users/USER_ID/tokens \
  -H "Authorization: Bearer your-admin-token" \
  -H "Content-Type: application/json" \
  -d '{"label": "home-pc"}' | jq
# → { "token": "a3f9c2..." }  — shown once only, save it
```

**Send the operator this command with their token pre-filled:**

```bash
docker run -d \
  --name ambush-exitnode \
  --restart unless-stopped \
  -e AMBUSH_GATEWAY_URL=wss://your-server:8080 \
  -e AMBUSH_TOKEN=TOKEN_HERE \
  -e AMBUSH_CA_CERT="$(cat ca.crt)" \
  ghcr.io/yourname/ambush-exitnode:latest
```

They paste it, hit enter — the container connects, auto-restarts on crash and reboot. The gateway logs `exitnode connected` and `/health` shows `"exitnodes": 1`.

To revoke access at any time:

```bash
curl -s -X DELETE https://your-server:8081/tokens/TOKEN_ID \
  -H "Authorization: Bearer your-admin-token"
# exit node is rejected on its next reconnect attempt
```

> See [`cmd/exitnode/RUNNING.md`](cmd/exitnode/RUNNING.md) for distribution instructions and Windows notes.

---

## Integrating your application

### Provision SOCKS5 credentials

Each application or user that should route traffic through the network needs a credential:

```bash
curl -s -X POST https://your-server:8081/proxy/credentials \
  -H "Authorization: Bearer your-admin-token" \
  -H "Content-Type: application/json" \
  -d '{"username": "scraper-job-1", "password": "hunter2"}'
```

Different usernames get **independent affinity state** — `scraper-job-1` and `scraper-job-2` hitting `example.com` will be routed through different exit nodes when possible.

### Point your application at the proxy

```
host:  your-gateway-ip
port:  1080
auth:  username / password (SOCKS5 username/password)
```

Python (requests + PySocks):
```python
proxies = {
    "http":  "socks5://scraper-job-1:hunter2@your-gateway:1080",
    "https": "socks5://scraper-job-1:hunter2@your-gateway:1080",
}
response = requests.get("https://example.com", proxies=proxies)
```

curl:
```bash
curl --proxy socks5h://scraper-job-1:hunter2@your-gateway:1080 https://example.com
```

Playwright / Puppeteer:
```js
const browser = await chromium.launch({
  proxy: { server: "socks5://your-gateway:1080", username: "scraper-job-1", password: "hunter2" }
});
```

---

## Configuration reference

### Gateway

| Env var | Default | Description |
|---------|---------|-------------|
| `GATEWAY_ADDR` | `:8080` | WebSocket and HTTP listen address |
| `SOCKS5_ADDR` | `:1080` | SOCKS5 proxy listen address |
| `DATABASE_URL` | required | Postgres connection string |
| `TLS_CERT` | — | Path to gateway TLS certificate (leave blank for dev) |
| `TLS_KEY` | — | Path to gateway TLS private key |
| `MAX_STREAMS_PER_CREDENTIAL` | `20` | Max concurrent proxy streams per SOCKS5 username |

### API

| Env var | Default | Description |
|---------|---------|-------------|
| `API_ADDR` | `:8081` | Listen address |
| `DATABASE_URL` | required | Postgres connection string |
| `ADMIN_TOKEN` | required | Bearer token required on all API requests |

### Exit node

| Env var | Description |
|---------|-------------|
| `AMBUSH_GATEWAY_URL` | Gateway WebSocket URL (`wss://` in production, `ws://` in dev) |
| `AMBUSH_TOKEN` | Exit node bearer token from the API |
| `AMBUSH_CA_CERT` | Full PEM content of `ca.crt` (for TLS verification) |

---

## Observability

**Health check** (no auth):
```bash
curl https://your-gateway:8080/health
# {"status":"ok","exitnodes":4,"active_streams":12}
```

**Prometheus metrics** at `https://your-gateway:8080/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `ambush_exitnodes_active` | Gauge | Connected exit nodes |
| `ambush_streams_active{exitnode_id}` | GaugeVec | Currently open proxy streams, per exit node |
| `ambush_dials_total{result}` | Counter | Dial attempts — `success`, `error`, `rate_limited` |
| `ambush_rotations_total{reason}` | Counter | Affinity rotations — `budget`, `expiry`, `session_closed`, `concurrency` |
| `ambush_stream_errors_total{exitnode_id}` | CounterVec | Stream open failures per exit node (session died mid-routing) |
| `ambush_credential_limit_exceeded_total` | Counter | Credentials that hit their concurrent stream cap |

Scrape config:
```yaml
scrape_configs:
  - job_name: ambush_gateway
    static_configs:
      - targets: ['your-gateway:8080']
    scheme: https
    tls_config:
      ca_file: /path/to/ca.crt
```

---

## Documentation

| Doc | Contents |
|-----|----------|
| [architecture.md](docs/architecture.md) | Connection flows, internal structure, metrics, shutdown sequence |
| [routing.md](docs/routing.md) | Affinity, rotation, cooldown, per-user isolation, rate limiting |
| [tls.md](docs/tls.md) | Cert generation, distribution, renewal |
| [api.md](docs/api.md) | Full API reference with request/response examples |
| [database.md](docs/database.md) | Schema, ER diagram, who reads/writes what |
| [roadmap.md](docs/roadmap.md) | What's done, what's next |

---

## License

MIT — see [LICENSE](LICENSE) for details.
