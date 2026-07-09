CREATE TABLE IF NOT EXISTS agents (
    name        TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    base_domain TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS agents_base_domain_unique
    ON agents(base_domain);

CREATE TABLE IF NOT EXISTS accounts (
    id          TEXT PRIMARY KEY,
    github_id   TEXT NOT NULL UNIQUE,
    username    TEXT NOT NULL UNIQUE,
    disabled    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS account_creds (
    token_hash  TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS hostnames (
    hostname    TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    app         TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    UNIQUE(account_id, app)
);
