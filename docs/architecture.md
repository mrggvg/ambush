# Architecture

## Exit node connection flow

When an exit node starts, it enters a connect-retry loop. On each attempt it dials the gateway's WebSocket endpoint, authenticates with its bearer token, and establishes a yamux session. The gateway is the yamux server; the exit node is the client. The gateway opens streams into the exit node — not the other way around.

```mermaid
sequenceDiagram
    participant E as Exit Node
    participant G as Gateway
    participant DB as Postgres

    E->>G: WebSocket dial /exitnode?token=xxx
    G->>DB: SELECT id FROM exit_node_tokens WHERE token_hash = sha256(token)
    DB-->>G: token valid, id = uuid
    G->>DB: UPSERT exit_node_ips (token_id, ip)
    G->>E: 101 Switching Protocols
    Note over E,G: yamux session established
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
    G->>G: bcrypt verify via pgcrypto
    C->>G: CONNECT example.com:443
    G->>R: DialWithRequest("example.com:443", username)
    R->>R: assignSession("username:example.com")
    R->>E: yamux stream open
    R->>E: write "example.com:443\n"
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
    POOL["Pool\nsessionEntry[] + public IP"]
    ROUTER["Router\naffinity map\ncooldown map (IP:domain)"]
    LIMITER["CredentialLimiter\nper-username stream cap"]
    DB["DB Pool\npgxpool"]
    YAMUX["yamux sessions"]

    WS -->|add/remove| POOL
    WS -->|auth| DB
    SOCKS -->|"DialWithRequest\n(addr + username)"| LIMITER
    LIMITER -->|acquire/release slot| ROUTER
    ROUTER -->|snapshot| POOL
    ROUTER -->|open stream| YAMUX
    HEALTH -->|snapshot| POOL
    YAMUX -->|lives in| POOL
```

## Exit node internal structure

```mermaid
graph TD
    SETUP["First-run Setup\nhuh form"]
    CONFIG["~/.ambush/exitnode.json"]
    CONNECT["connect loop\n5s retry"]
    WS["WebSocket\ngorilla/websocket"]
    YAMUX["yamux client session"]
    ACCEPT["stream accept loop"]
    RELAY["handleStream\nidleConn wrapper\n2min idle timeout"]
    TARGET["Target host\nnet.Dial"]

    SETUP -->|saves| CONFIG
    CONFIG -->|loads| CONNECT
    CONNECT --> WS
    WS --> YAMUX
    YAMUX --> ACCEPT
    ACCEPT -->|goroutine per stream| RELAY
    RELAY --> TARGET
```

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
| `ambush_dials_total` | Counter | `result` | Dial attempts — `success`, `error`, `rate_limited` |
| `ambush_rotations_total` | Counter | `reason` | Affinity rotations — `budget`, `expiry`, `session_closed`, `concurrency` |
| `ambush_stream_errors_total` | CounterVec | `exitnode_id` | `yamux.Open()` failures per exit node (session died mid-routing) |
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
    A[SIGTERM received] --> B[Close SOCKS5 listener\nno new proxy connections]
    B --> C[Wait up to 30s\nfor active streams to drain]
    C --> D[Close all yamux sessions\ndisconnect exit nodes]
    D --> E[HTTP server Shutdown\n10s timeout]
    E --> F[Exit]
```
