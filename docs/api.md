# API Reference

The API binary (`cmd/api`) runs on `:8081` and is the only writer to the database. All routes require an admin bearer token.

## Authentication

Every request must include:

```
Authorization: Bearer <ADMIN_TOKEN>
```

Where `ADMIN_TOKEN` is set in `cmd/api/.env`.

## Users

### `POST /users`

Create a new user.

**Body:**
```json
{
  "display_name": "John Doe"
}
```

**Response:**
```json
{
  "id": "uuid",
  "display_name": "John Doe"
}
```

---

### `GET /users`

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

## Exit Node Tokens

### `POST /users/{userID}/tokens`

Generate a new exit node token for a user. The raw token is returned **once only** — it is never stored plain.

**Body:**
```json
{
  "label": "vps-fra-1",
  "expires_at": null
}
```
`label` is required and must be unique per user. `expires_at` is an RFC3339 string or `null` for no expiry.

**Response:**
```json
{
  "id": "uuid",
  "label": "vps-fra-1",
  "token": "a3f9c2d1e8b74f6a..."
}
```

The `token` value is what the exit node operator pastes into the setup prompt.

---

### `GET /users/{userID}/tokens`

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

### `DELETE /tokens/{tokenID}`

Revoke a token (sets `is_active = false`). The exit node will be rejected on its next reconnect attempt.

**Response:** `204 No Content`

---

## Proxy Credentials

### `POST /proxy/credentials`

Grant SOCKS5 proxy access. The password is bcrypt-hashed via pgcrypto before storage.

**Body:**
```json
{
  "username": "scraper1",
  "password": "supersecret"
}
```

**Response:**
```json
{
  "id": "uuid",
  "username": "scraper1"
}
```

---

### `GET /proxy/credentials`

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

### `DELETE /proxy/credentials/{credID}`

Revoke SOCKS5 access for a credential.

**Response:** `204 No Content`

---

## Gateway Health

This endpoint lives on the **gateway** (`:8080`), not the API.

### `GET /health`

Returns live pool state. No authentication required.

**Response:**
```json
{
  "status": "ok",
  "exitnodes": 3,
  "active_streams": 7
}
```
