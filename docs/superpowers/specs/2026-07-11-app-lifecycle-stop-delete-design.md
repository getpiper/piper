# App lifecycle: stop and delete

**Issue:** [#103](https://github.com/getpiper/piper/issues/103) · **Date:** 2026-07-11 · **Status:** approved

Phase 2 dependency of the dashboard roadmap (getpiper/dashboard — part of #76).

## Problem

The control API covers create / deploy / list / status, but the dashboard's app-lifecycle page also needs **delete** and **stop**, and the CLI needs parity commands. Verified before design: neither exists — `internal/api` registers no stop/delete route for apps, `internal/deploy` has no `Stop`/`Delete` (only `TeardownPreview`), and `internal/store` has no `DeleteApp`.

## Decisions (with rationale)

1. **Orchestration lives in the `deploy` package.** `Stop` and `Delete` become `Deployer` methods mirroring `TeardownPreview`, keeping the documented layering: `deploy` drives store/runtime/routes through interfaces, `api` stays thin transport, and unit tests use the existing fakes.
2. **Stop is idempotent.** Stopping an app with no running production deployment (never deployed, already stopped, last deploy failed) is a no-op `204`. Friendlier for the dashboard; only an unknown app is `404`.
3. **Delete tears down everything**, including any running PR-preview containers and their `pr-N-<app>` routes — no orphaned containers.
4. **Relay cleanup is best-effort.** In relay-terminated mode the routed hostname is relay-assigned; stop/delete recovers it via the idempotent `Register(app)` and delete additionally `Deregister`s it. If the tunnel is down (`ErrNotConnected`), those steps are skipped: container stop, local route removal, and state changes always proceed, so delete never wedges on connectivity. Local Caddy `RemoveRoute` failures remain real errors (retryable).
5. **No server-side confirmation.** Delete is destructive; remote callers confirm client-side — the dashboard with a modal, the CLI with a `y/N` prompt (`--yes` to skip).
6. **No relay `Deregister` on stop.** The hostname stays reserved for the app; removing the local route is what stops it serving. Deregister happens only on delete.

## API surface

Both endpoints register on the existing mux, inheriting `RequireToken` and relay-proxy reachability (the token-gate acceptance criterion needs no new code).

### `POST /v1/apps/{name}/stop`

Retires the latest running production container: stop container, remove its route(s), mark the deployment row `stopped`. App and full deployment history remain; `GET /v1/apps/{name}` then reports status `stopped`. Responses: `204` on success **and** when nothing is running; `404` unknown app; `500` otherwise. No body.

### `DELETE /v1/apps/{name}`

Stops every running deployment (production + previews), removes all routes, deregisters the relay hostname (best-effort), then deletes the app row and all its deployment rows. The hostname stops serving. Responses: `204`; `404` unknown app; `500` otherwise (state intact, retryable).

## Deployer orchestration

`api`'s `Deployerer` interface grows `Stop(ctx, name) error` and `Delete(ctx, name) error`; handlers map `store.ErrNotFound` → 404.

**`Stop(ctx, name)`**
1. `GetApp` — `ErrNotFound` bubbles up.
2. `LatestRunning` — `ErrNotFound` → return nil (idempotent).
3. `runtime.Stop(containerID)` best-effort (matches the existing retire path).
4. Remove routes: primary host `hostFor(name)`, or in registrar mode the name from `Register(name)` (skip on `ErrNotConnected`); plus `<app>.<custom>` when a BYO domain is active.
5. `UpdateDeploymentStatus(dep.ID, "stopped")`.

**`Delete(ctx, name)`**
1. `GetApp` — `ErrNotFound` bubbles up.
2. `ListDeployments`; for each `running` row: `runtime.Stop` best-effort, remove its route — production host as above, previews via `hostForPreview(name, pr)`.
3. Remove the custom-domain route if active; in registrar mode `Deregister(hostname)` best-effort.
4. `store.DeleteApp(name)` **last**, so a failed teardown leaves state intact and delete is retryable.

Delete during an in-flight deploy is not serialized — no locking exists in the deployer today; out of scope.

## Store

One new method, no schema change:

- `DeleteApp(name) error` — one transaction: delete the app's `deployments` rows, then the `apps` row; zero rows affected on the app delete → `ErrNotFound`. This is the single scoped exception to "deployment rows are never deleted"; the `logRetentionPerApp` comment gets a clause noting it.

## Client + CLI

`internal/client`: `StopApp(name) error` and `DeleteApp(name) error` — thin wrappers over `do()`/`responseError`, so `--remote` works unchanged.

`cmd/piper`, following the existing dispatch style, both added to `usage()`:

- `piper stop <name>` — prints `stopped <name>`.
- `piper delete <name> [--yes]` — without `--yes`, prompts `delete app "<name>" and all its history? [y/N]` on stdin; anything but `y`/`yes` aborts with exit 0. Prints `deleted <name>`.

## Testing

TDD, failing-test-first:

- **deploy** (fakes): stop retires container + primary and custom-domain routes + marks `stopped`; stop no-ops when nothing runs; delete tears down production and running previews and deregisters in registrar mode; both skip relay steps on `ErrNotConnected`; delete propagates local route-removal failure with state intact.
- **store**: `DeleteApp` removes the app and its deployments, leaves other apps untouched, `ErrNotFound` for unknown names.
- **api** (fake `Deployerer`): 204/404 contracts for both endpoints, including idempotent stop.
- **client / CLI**: wrappers hit the right method+path; `--yes`, prompt-confirm, and prompt-abort paths.
- **e2e**: extend the existing Docker+Caddy e2e — deploy → stop (hostname stops serving, app listed `stopped`, history intact) → delete (API 404, route gone) — subject to the current harness covering the deploy flow (verify during planning).

## Acceptance criteria (from #103)

- Delete removes container, route, and state; the hostname stops serving.
- Stop leaves the app listed as `stopped` with history intact.
- Both behind the existing token gates, reachable via the relay proxy.
