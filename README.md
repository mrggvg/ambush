<p align="center">
  <img src="docs/ambush.png" alt="Ambush" width="180" />
</p>

# Ambush

> ⚠️ Work in progress — not production ready yet.

A self-hosted, integration-agnostic proxy network written in Go. Machines running the exit node binary connect to a central gateway over WebSocket. Any SOCKS5-capable client routes its TCP traffic through the gateway, which tunnels it out through one of the connected exit nodes.

The network is **protocol-agnostic** — anything that runs over TCP works. The gateway and exit nodes have no awareness of what is being proxied.

```mermaid
flowchart LR
    C(["SOCKS5 Client\n(any app)"])
    GW(["Gateway\n:8080 · :1080"])
    EN1(["Exit Node 1"])
    EN2(["Exit Node 2"])
    T(["Target"])

    C -->|"SOCKS5 + auth"| GW
    EN1 -->|"WebSocket + yamux"| GW
    EN2 -->|"WebSocket + yamux"| GW
    GW -->|"TCP stream"| EN1
    GW -->|"TCP stream"| EN2
    EN1 -->|TCP| T
```

## How it works

1. Exit nodes connect **outbound** to the gateway over WebSocket — no inbound ports needed, works from behind NAT
2. The gateway multiplexes TCP streams over each WebSocket connection using [yamux](https://github.com/hashicorp/yamux)
3. SOCKS5 clients connect to the gateway on `:1080` with username/password credentials
4. The gateway's router picks an exit node based on domain affinity, routes the stream through it, and the exit node dials the real target

## Components

| Binary | Port | Role |
|--------|------|------|
| `gateway` | `:8080` (WS), `:1080` (SOCKS5) | Accepts exit node connections, serves SOCKS5 proxy, smart routing |
| `exitnode` | — | Runs on any machine, connects to gateway, relays traffic |
| `api` | `:8081` | Admin HTTP API — manage users, tokens, proxy credentials |

## Smart routing

The router avoids assigning a random exit node per request. Instead:

- Requests to the same domain stick to the same exit node for a time window (5 min ± 20% jitter)
- After the window or 100 requests, the exit node rotates with a 10 minute cooldown
- Each exit node is capped at 10 concurrent streams
- Dead sessions are detected and retried transparently

See [docs/routing.md](docs/routing.md) for the full decision flowchart.

## Running locally

**Gateway:**
```bash
# fill in DATABASE_URL in cmd/gateway/.env first
./cmd/gateway/run.sh
```

**Exit node:**
```bash
# first run triggers interactive setup — paste your token when prompted
./cmd/exitnode/run.sh
```

**API:**
```bash
# fill in DATABASE_URL and ADMIN_TOKEN in cmd/api/.env first
./cmd/api/run.sh
```

**Test the proxy:**
```bash
./scripts/test-socks5.sh
```

## Database

Run `db/schema.sql` against a Postgres instance before starting. Requires the `pgcrypto` extension (first line of the schema handles this).

## Documentation

| Doc | Contents |
|-----|----------|
| [docs/overview.md](docs/overview.md) | System overview, components, design decisions |
| [docs/architecture.md](docs/architecture.md) | Connection flows, internal structure, shutdown sequence |
| [docs/routing.md](docs/routing.md) | Smart routing strategy, affinity, rotation, cooldown |
| [docs/database.md](docs/database.md) | Schema, ER diagram, who reads/writes what |
| [docs/api.md](docs/api.md) | Full API reference with request/response examples |
| [docs/roadmap.md](docs/roadmap.md) | What's done, what's blocked, what's next |

## License

MIT — see [LICENSE](LICENSE) for details.
