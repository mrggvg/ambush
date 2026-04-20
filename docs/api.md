# API Reference

Ambush exposes two separate HTTP APIs:

| Service | Binary | Default port | Purpose |
|---------|--------|-------------|---------|
| **Admin API** | `cmd/api` | `:8081` | User, token, and credential management (writes DB) |
| **Gateway API** | `cmd/gateway` | `:8080` | Live pool state, session management, feedback |

Both require an admin bearer token on every request.

---

## Admin API (`cmd/api`)

### Authentication

```
Authorization: Bearer <ADMIN_TOKEN>
```

`ADMIN_TOKEN` is set via the `ADMIN_TOKEN` environment variable on the API server.

---

### Users

#### `POST /users`

Create a new user.

**Body:**
```json
{ "display_name": "John Doe" }
```

**Response:**
```json
{ "id": "uuid", "display_name": "John Doe" }
```

---

#### `GET /users`

List all users ordered by creation date.

**Response:**
```json
[
  {
    "id": "uuid",
    "display_name": "John Doe",
    "created_at": "2026-04-20T10:00:00Z",
    "is_active": true
  }
]
```

---

### Exit Node Tokens

#### `POST /users/{userID}/tokens`

Generate a new exit node token for a user. The raw token is returned **once only** — it is never stored plain.

**Body:**
```json
{ "label": "vps-fra-1", "expires_at": null }
```

`label` is required and must be unique per user. `expires_at` is an RFC3339 string or `null` for no expiry.

**Response:**
```json
{ "id": "uuid", "label": "vps-fra-1", "token": "a3f9c2d1e8b74f6a..." }
```

The `token` value is what the exit node operator pastes into the setup prompt or sets as `AMBUSH_TOKEN`.

---

#### `GET /users/{userID}/tokens`

List all tokens for a user. Token values are never returned — only metadata.

**Response:**
```json
[
  {
    "id": "uuid",
    "label": "vps-fra-1",
    "created_at": "2026-04-20T10:00:00Z",
    "expires_at": null,
    "is_active": true
  }
]
```

---

#### `DELETE /tokens/{tokenID}`

Revoke a token (sets `is_active = false`). The exit node is rejected on its next reconnect.

**Response:** `204 No Content`

---

### Proxy Credentials

#### `POST /proxy/credentials`

Grant SOCKS5 proxy access. The password is bcrypt-hashed via pgcrypto before storage.

**Body:**
```json
{ "username": "scraper1", "password": "supersecret" }
```

**Response:**
```json
{ "id": "uuid", "username": "scraper1" }
```

---

#### `GET /proxy/credentials`

List all proxy credentials.

**Response:**
```json
[
  {
    "id": "uuid",
    "username": "scraper1",
    "created_at": "2026-04-20T10:00:00Z",
    "is_active": true
  }
]
```

---

#### `DELETE /proxy/credentials/{credID}`

Revoke SOCKS5 access for a credential.

**Response:** `204 No Content`

---

## Gateway API (`cmd/gateway`)

Served on the same port as the WebSocket endpoint (default `:8080`). Protected by `GATEWAY_ADMIN_TOKEN`. If the env var is not set, all `/api/v1/*` endpoints are disabled.

### Authentication

```
Authorization: Bearer <GATEWAY_ADMIN_TOKEN>
```

---

### `GET /api/v1/exits`

Live snapshot of connected exit nodes.

**Response:**
```json
[
  {
    "id": "42",
    "ip": "1.2.3.4",
    "country": "us",
    "node_type": "residential",
    "active_streams": 3,
    "health": "healthy"
  }
]
```

`country` and `node_type` are omitted when not reported by the exit node. `health` is `"healthy"` or `"degraded"` (failure rate > 50% over the last 20 stream outcomes).

---

### `GET /api/v1/stats`

Aggregate pool counters.

**Response:**
```json
{
  "exitnodes": 5,
  "active_streams": 12
}
```

---

### `POST /api/v1/sessions`

Pre-allocate a sticky session token bound to an eligible exit node. The client embeds the returned token in SOCKS5 usernames as `base-session-<token>` to get Model A routing.

**Body:** (all fields optional)
```json
{
  "ttl_seconds": 1800,
  "country": "us",
  "node_type": "residential"
}
```

`ttl_seconds` defaults to 1800 (30 min), capped at 86400 (24h). `country` and `node_type` filter which exit node is selected.

**Response:**
```json
{
  "token": "a3f9c2d1e8b74f6a9d3e2c1b0f8a7e5d",
  "expires_at": "2026-04-20T15:30:00Z",
  "exit": {
    "id": "42",
    "ip": "1.2.3.4",
    "country": "us",
    "node_type": "residential"
  }
}
```

Returns `503 Service Unavailable` if no eligible exit node exists.

---

### `POST /api/v1/feedback`

Record a target-level outcome for a session token's exit node. Updates the node's health score without requiring a new SOCKS5 connection.

**Body:**
```json
{ "session_token": "a3f9c2d1...", "success": false }
```

**Response:** `204 No Content`

Returns `404 Not Found` if the token is not in the session store (expired or never created).

---

## Gateway health endpoint

No authentication required.

### `GET /health`

**Response:**
```json
{
  "status": "ok",
  "exitnodes": 3,
  "active_streams": 7
}
```
