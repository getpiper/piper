CREATE TABLE IF NOT EXISTS agents (
    name        TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    base_domain TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS agents_base_domain_unique
    ON agents(base_domain);

CREATE TABLE IF NOT EXISTS accounts (
    id           TEXT PRIMARY KEY,
    github_id    TEXT UNIQUE,
    github_login TEXT,
    username     TEXT NOT NULL UNIQUE,
    type         TEXT NOT NULL DEFAULT 'user',
    disabled     INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL
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

CREATE TABLE IF NOT EXISTS org_members (
    org_id     TEXT NOT NULL REFERENCES accounts(id),
    account_id TEXT NOT NULL REFERENCES accounts(id),
    role       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (org_id, account_id)
);

CREATE TABLE IF NOT EXISTS org_invites (
    org_id       TEXT NOT NULL REFERENCES accounts(id),
    github_login TEXT NOT NULL,
    invited_by   TEXT NOT NULL REFERENCES accounts(id),
    created_at   TEXT NOT NULL,
    PRIMARY KEY (org_id, github_login)
);

CREATE TABLE IF NOT EXISTS custom_domains (
    domain      TEXT PRIMARY KEY,
    agent_base  TEXT NOT NULL,
    status      TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS custom_domains_agent_base ON custom_domains(agent_base);
