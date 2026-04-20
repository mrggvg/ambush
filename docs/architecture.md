# Architecture

## Exit node connection flow

When an exit node starts, it enters a connect-retry loop with exponential backoff (1s → 60s, ×2 per failure, up to 50% jitter). On each attempt it dials the gateway's WebSocket endpoint, authenticates with its bearer token, and establishes a yamux session. The gateway is the yamux server; the exit node is the client. The gateway opens streams into the exit node — not the other way around.

```mermaid
sequenceDiagram
    participant E as Exit Node
    participant G as Gateway
    participant DB as Postgres

    E->>G: WebSocket dial /exitnode?token=xxx&country=us&type=residential
    G->>DB: SELECT id FROM exit_node_tokens WHERE token_hash = sha256(token)
    DB-->>G: token valid, id = uuid
    G->>DB: UPSERT exit_node_ips (token_id, ip, country, node_type)
    G->>E: 101 Switching Protocols
    Note over E,G: yamux session established\nsessionEntry added to Pool with country + nodeType
    loop Every 30s
        G->>E: WebSocket ping
        E-->>G: WebSocket pong
    end
    Note over E,G: Session stays alive until ping fails or either side closes
```

## SOCKS5 request flow

```mermaid
sequenceDiagram
    participant C as SOCKS5 Client
    participant G as Gateway
    participant R as Router
    participant E as Exit Node
    participant T as Target

    C->>G: TCP connect :1080
    C->>G: SOCKS5 handshake + auth (user/pass)
    G->>G: parse username → base credential\nbcrypt verify via pgcrypto
    C->>G: CONNECT example.com:443
    G->>R: DialWithUser("example.com:443", "alice-session-tok-country-us")
    R->>R: ParseUsername → {Base:"alice", SessionToken:"tok", Country:"us"}
    R->>R: limiter.Acquire("alice")
    alt Model B (no session token)
        R->>R: pickSession(Constraints{Country:"us"})
    else Model A (session token present)
        R->>R: SessionStore.Get("tok") → pinned node or nil
        R->>R: pickSession if not found / node dead
        R->>R: SessionStore.Set("tok", node)
    end
    R->>E: yamux stream open
    R->>E: write "reqID example.com:443\n"
    E->>T: net.Dial("tcp", "example.com:443")
    T-->>E: connection established
    Note over C,T: Bidirectional relay with 2min idle timeout
    C-->G: data
    G-->E: data
    E-->T: data
    T-->E: response
    E-->G: response
    G-->C: response
```

## Gateway internal structure

```mermaid
graph TD
    WS["WebSocket Handler\n/exitnode"]
    SOCKS["SOCKS5 Server\n:1080"]
    HEALTH["Health Handler\n/health"]
    GWAPI["Gateway API\n/api/v1/*"]
    POOL["Pool\nsessionEntry[]\n(id, ip, country, nodeType)"]
    ROUTER["Router\nModel A / Model B dispatch"]
    SESSIONS["SessionStore\ntoken → sessionEntry + TTL"]
    LIMITER["CredentialLimiter\nper-base-credential stream cap"]
    HEALTH_SCORE["nodeHealth\nrolling 20-outcome window\nper exit node"]
    DB["DB Pool\npgxpool"]
    YAMUX["yamux sessions"]

    WS -->|add/remove + EvictSession| POOL
    WS -->|auth + IP tracking| DB
    SOCKS -->|"DialWithUser\n(addr + raw username)"| ROUTER
    ROUTER -->|"ParseUsername\n→ Base, SessionToken, Country"| ROUTER
    ROUTER -->|acquire/release| LIMITER
    ROUTER -->|"Model A: get/set token"| SESSIONS
    ROUTER -->|"pickSession\n(Constraints)"| POOL
    ROUTER -->|open stream| YAMUX
    ROUTER -->|record outcome| HEALTH_SCORE
    HEALTH -->|snapshot| POOL
    GWAPI -->|snapshot| POOL
    GWAPI -->|get/set session| SESSIONS
    GWAPI -->|CreateSession| ROUTER
    GWAPI -->|record feedback| HEALTH_SCORE
    YAMUX -->|lives in| POOL
    HEALTH_SCORE -->|lives in| POOL
```

## Exit node internal structure

```mermaid
graph TD
    SETUP["First-run Setup\nhuh form"]
    CONFIG["~/.ambush/exitnode.json"]
    CONNECT["connect loop\nexponential backoff 1s→60s"]
    WS["WebSocket\ngorilla/websocket"]
    YAMUX["yamux client session"]
    ACCEPT["stream accept loop"]
    RELAY["handleStream\nparse reqID+addr header\nidleConn wrapper\n2min idle timeout"]
    TARGET["Target host\nnet.Dial"]

    SETUP -->|saves| CONFIG
    CONFIG -->|loads| CONNECT
    CONNECT -->|"?token=x&country=us&type=residential"| WS
    WS --> YAMUX
    YAMUX --> ACCEPT
    ACCEPT -->|goroutine per stream| RELAY
    RELAY --> TARGET
```

## Stream header protocol

When the gateway opens a yamux stream, it writes a single header line before relaying data:

```
<req_id> <addr>\n
```

- `req_id` — 12-character random hex correlation ID, shared across all gateway log lines for this request
- `addr` — `host:port` of the target

The exit node parses this line, logs `req=<req_id> relaying to <addr>`, then dials the target. This ties gateway and exit node log lines together with a single searchable ID.

## WebSocket as net.Conn

yamux requires a `net.Conn`. gorilla WebSocket is not one — it frames messages. The `wsConn` adapter bridges this:

- **Read**: calls `conn.NextReader()` per message, drains each frame, loops to next on EOF
- **Write**: wraps each write as a single binary WebSocket message, protected by a mutex
- **ping**: uses `WriteControl` through the same mutex to avoid concurrent write panics
- **SetDeadline**: sets both read and write deadlines — yamux calls this to enforce its own keepalive and stream timeouts, so both must be applied or stalled connections leak goroutines

## Metrics

The gateway exposes Prometheus metrics at `GET /metrics` (same port as the WebSocket endpoint, default `:8080`).

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `ambush_exitnodes_active` | Gauge | — | Connected exit nodes |
| `ambush_streams_active` | GaugeVec | `exitnode_id` | Currently open proxy streams, per exit node |
| `ambush_dials_total` | CounterVec | `result` | Dial attempts — `success`, `error`, `rate_limited` |
| `ambush_stream_errors_total` | CounterVec | `exitnode_id` | `yamux.Open()` failures per exit node |
| `ambush_credential_limit_exceeded_total` | Counter | — | Credentials that hit `MAX_STREAMS_PER_CREDENTIAL` |

Minimal Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: ambush_gateway
    static_configs:
      - targets: ['gateway:8080']
```

## Graceful shutdown sequence

```mermaid
flowchart LR
    A[SIGTERM received] --> B[Cancel root context\nstops SessionStore + auth rate limiter goroutines]
    B --> C[Close SOCKS5 listener\nno new proxy connections]
    C --> D[Wait up to 30s\nfor active streams to drain]
    D --> E[Close all yamux sessions\ndisconnect exit nodes]
    E --> F[HTTP server Shutdown\n10s timeout]
    F --> G[Exit]
```
