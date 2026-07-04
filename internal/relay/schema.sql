CREATE TABLE IF NOT EXISTS agents (
    name        TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    base_domain TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
