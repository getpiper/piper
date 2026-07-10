# Deployment history + build/deploy logs on the control API — design

Implements [#101](https://github.com/getpiper/piper/issues/101) (Phase 2
dependency of the dashboard roadmap, getpiper/dashboard #76).

Today `DockerRuntime.Build` drains the Docker build stream to `io.Discard`,
so when a build fails a remote caller sees `failed` and nothing else. This
spec captures build + deploy output per deployment and exposes deployment
history and per-deployment logs over the authenticated control API.

## Scope decisions

- **Capture** = build log always, plus the container's stdout/stderr when the
  run or health-check fails. Container output on *successful* deploys is a
  separate "app logs" feature, not deploy history.
- **Storage** = a `logs` column on the existing `deployments` table. One
  state store, retention is an `UPDATE`, backup stays one file.
- **Retention** = logs kept for the last 20 deployments per app. Deployment
  *rows* live forever — they are the history and they're tiny; only log
  blobs are pruned.
- **No streaming/follow** — the issue defers it; a complete log after the
  fact is the v1.
- **No CLI subcommands** — the issue is `[agent]`-scoped and every
  acceptance criterion is control-API. A `piper deployments` / log surface
  is a follow-up.

## 1. Store

Schema: `deployments` gains `logs TEXT NOT NULL DEFAULT ''`. Migration is a
guarded `ALTER TABLE` in `migrate` (skipped when the column already exists),
alongside the schema.sql baseline for fresh databases.

- `CreateDeployment` and `CreatePreviewDeployment` grow a `logs` parameter
  (all call sites live in `deploy`).
- `ListDeployments(app string) ([]Deployment, error)` — all rows for an
  app, newest first (rides the existing `idx_deployments_app` index).
- `DeploymentLogs(app, id string) (string, error)` — the log for one
  deployment, scoped by app so an id from another app is `ErrNotFound`.

After each insert the store prunes: set `logs = ''` on every row for that
app outside its 20 newest by `created_at`.

## 2. Capture (runtime + deploy)

### Runtime

`BuildResult` gains `Log string`, populated **even when Build returns an
error**. `DockerRuntime.Build` replaces the `io.Discard` drain with the
Docker SDK's `jsonmessage` decoder writing plain text into a tail-capped
buffer (1 MiB), so a pathological build can't balloon memory. Side
benefit: a failed build now returns the real build error from the stream
instead of the current `inspect built image (build may have failed)`
indirection.

### Deploy

`buildRunHealthy` assembles one text blob per deployment:

- the build log, always;
- on run or health-check failure, the container's output (existing
  `Runtime.Logs(containerID)`, fetched **before** `stopPartial`
  force-removes the container) appended under a
  `--- container output ---` separator. A failure fetching container
  logs never masks the deploy error — the blob just ends at the build log.

**Cap**: 1 MiB per deployment, keeping the **tail** (errors live at the
end), with a leading `[log truncated]` marker. Enforced in the deployer so
it bounds build + container output combined.

Both the success path and the `recordFailed` callback pass the assembled
log into the store. `runtime.Fake` gets settable build/container logs so
deploy unit tests stay Docker-free.

## 3. API

Two endpoints on the existing mux, behind the same `RequireToken` gate as
the rest of the control API — so they arrive remotely via the relay proxy
(`/agents/<base>/v1/...`) with no new auth machinery:

- `GET /v1/apps/{name}/deployments` — JSON array, newest first: `id`,
  `status`, `image_id`, `created_at`, matching the existing deployment
  JSON shape. `404` for an unknown app.
- `GET /v1/apps/{name}/deployments/{id}/logs` — the complete log as
  `text/plain`. `404` when the deployment doesn't exist under that app;
  `200` with an empty body when the log was pruned by retention.

## 4. Testing

TDD throughout, per plan discipline:

- **Store**: logs round-trip; list ordering (newest first); retention
  prunes to 20 per app without touching other apps' logs; cross-app id
  lookup is `ErrNotFound`; migration adds the column to a pre-existing DB.
- **Deploy** (fake runtime): a failed build persists a `failed` deployment
  *with* the build log; a health-check failure appends container output;
  the 1 MiB cap keeps the tail and adds the truncation marker.
- **Runtime** (Docker-gated, skips without Docker): a real failing build
  yields a non-empty `BuildResult.Log` containing the error.
- **API**: both endpoints 401 without a token; 404 for unknown app /
  deployment; list shape and `text/plain` content type.

## Acceptance criteria → design

| Criterion | Where satisfied |
| --- | --- |
| Failed deploy's build output retrievable remotely after the fact | §2 capture + §3 logs endpoint over the relay proxy |
| Deployment history listable with status + timestamps | §3 `GET /v1/apps/{name}/deployments` |
| Same token gates as the rest of the control API | §3 — endpoints sit behind the existing `RequireToken` middleware |
