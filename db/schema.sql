CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE users (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    display_name TEXT       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    is_active   BOOLEAN     NOT NULL DEFAULT true
);

-- Bearer tokens for exit nodes, scoped to a user.
-- The token value is stored as a SHA-256 hex digest — show the raw token to the
-- user exactly once at generation time, never store it plain.
CREATE TABLE exit_node_tokens (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  TEXT        NOT NULL UNIQUE,                 -- SHA-256 hex of the bearer token
    label       TEXT        NOT NULL,                        -- required, unique name for this instance e.g. "vps-fra-1"
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,                                 -- NULL means no expiry
    is_active   BOOLEAN     NOT NULL DEFAULT true,
    UNIQUE (user_id, label)
);

CREATE INDEX idx_exit_node_tokens_token_hash ON exit_node_tokens(token_hash);

-- Tracks unique public IPs seen per token (basis for IP diversity metrics).
CREATE TABLE exit_node_ips (
    token_id      UUID        NOT NULL REFERENCES exit_node_tokens(id) ON DELETE CASCADE,
    ip            INET        NOT NULL,
    country       TEXT,                                    -- self-reported ISO-3166-1 alpha-2, NULL if not set
    node_type     TEXT,                                    -- "residential" | "datacenter" | "mobile", NULL if not set
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (token_id, ip)
);

-- SOCKS5 proxy credentials, admin-managed.
CREATE TABLE proxy_credentials (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    username      TEXT        NOT NULL UNIQUE,
    password_hash TEXT        NOT NULL,                      -- bcrypt
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    is_active     BOOLEAN     NOT NULL DEFAULT true
);

-- Convenience view: how many unique IPs each user's tokens have seen in total.
CREATE VIEW user_exit_node_diversity AS
    SELECT
        u.id         AS user_id,
        u.display_name,
        COUNT(DISTINCT ei.ip) AS unique_ips
    FROM users u
    JOIN exit_node_tokens et ON et.user_id = u.id
    JOIN exit_node_ips ei    ON ei.token_id = et.id
    GROUP BY u.id, u.display_name;
