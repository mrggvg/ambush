# ambush

> ⚠️ Early development — not production ready yet.

A custom residential proxy network written in Go from scratch.

## What is this?

A distributed proxy system where clients connect via SOCKS5 to a central gateway, which routes traffic through a pool of residential exit nodes. Exit nodes are regular machines running behind NAT that maintain a persistent WebSocket connection back to the gateway and perform the actual outbound TCP connections on behalf of clients.

```mermaid
flowchart LR
    C([Client]):::node
    GW([Gateway]):::node
    EN([Exit Node]):::node
    D([Destination]):::node

    C -->|SOCKS5 + auth| GW
    GW <-->|WebSocket tunnel\nresidential IP| EN
    EN -->|TCP| D

    classDef node fill:#1e3a5f,stroke:#2d6a9f,color:#e8f4fd,rx:8
```

## Architecture

- **Gateway** — accepts SOCKS5 connections from clients, authenticates them, and round-robins requests across available exit nodes
- **Exit node** — runs on residential machines, connects to the gateway via WebSocket, and relays traffic to the destination
- **SOCKS5** — implemented from scratch, currently supports IPv4 and domain resolution

## Status

Just started. Core SOCKS5 proxy works. Gateway and exit node tunnel are in progress.

- [ ] SOCKS5 handshake
- [ ] IPv4 relay
- [ ] Domain resolution
- [ ] SOCKS5 auth
- [ ] WebSocket tunnel (gateway ↔ exit node)
- [ ] Exit node pool management
- [ ] Health checks

## License

MIT — free to use, modify, and distribute. See [LICENSE](LICENSE) for details.