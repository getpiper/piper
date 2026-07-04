CREATE TABLE IF NOT EXISTS apps (
    name       TEXT PRIMARY KEY,
    port       INTEGER NOT NULL,
    created_at TEXT NOT NULL
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
