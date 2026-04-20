# Ambush — Overview

Ambush is a self-hosted, managed proxy network. Volunteer machines (exit nodes) connect to a central gateway over WebSocket. SOCKS5 clients route their traffic through the gateway, which tunnels it through one of the connected exit nodes to the target.

The network is protocol-agnostic — any TCP traffic can be proxied, not just HTTP/S.

## Use cases

- Distributed web scraping with residential IP diversity
- Bypassing geo-restrictions
- Multi-account management
- Any scenario requiring a pool of diverse outbound IPs

## System overview

```mermaid
graph TB
    subgraph Clients
        C1["Scraper / Browser"]
        C2["curl / any app"]
    end

    subgraph Gateway [:8080 WS · :1080 SOCKS5]
        G["Gateway\n(router + pool)"]
    end

    DB[(Postgres)]

    subgraph API [:8081]
        A["API\n(token + credential mgmt)"]
    end

    subgraph Exit Nodes
        E1["Exit Node 1\nresidential IP"]
        E2["Exit Node 2\nresidential IP"]
        E3["Exit Node N\n..."]
    end

    subgraph Targets
        T["example.com\nshopify.com\n..."]
    end

    C1 -->|SOCKS5| G
    C2 -->|SOCKS5| G
    E1 -->|WebSocket + yamux| G
    E2 -->|WebSocket + yamux| G
    E3 -->|WebSocket + yamux| G
    G -->|TCP stream| E1
    G -->|TCP stream| E2
    G -->|auth queries| DB
    A -->|writes| DB
```

## Components

| Component | Binary | Purpose |
|-----------|--------|---------|
| **Gateway** | `gateway` | Accepts exit node connections, serves SOCKS5 proxy |
| **Exit Node** | `exitnode` | Runs on volunteer machines, dials out to targets |
| **API** | `api` | Admin HTTP API for managing users, tokens, credentials |

## Key design decisions

- **yamux over WebSocket** — exit nodes connect outbound, no inbound port needed. Works from behind NAT and firewalls.
- **Bearer token auth** — each exit node instance has its own token, hashed with SHA-256 in the DB.
- **Domain affinity routing** — requests to the same domain go through the same exit node for a window, then rotate. Looks natural to anti-bot systems.
- **Postgres** — owns its own database for token management, credential storage, and IP diversity tracking. Exposes a clean HTTP API so any external system can integrate without touching the DB directly.
