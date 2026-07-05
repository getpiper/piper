CREATE TABLE IF NOT EXISTS apps (
    name           TEXT PRIMARY KEY,
    port           INTEGER NOT NULL,
    repo           TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS deployments (
    id           TEXT PRIMARY KEY,
    app          TEXT NOT NULL REFERENCES apps(name),
    image_id     TEXT NOT NULL,
    container_id TEXT NOT NULL,
    host_port    INTEGER NOT NULL,
    status       TEXT NOT NULL,
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deployments_app ON deployments(app, created_at);
CREATE TABLE IF NOT EXISTS github_app (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    app_id         INTEGER NOT NULL,
    private_key    TEXT NOT NULL,
    webhook_secret TEXT NOT NULL
);
