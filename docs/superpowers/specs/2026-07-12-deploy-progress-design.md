# Deploy progress: async deploys followed by polling — design

Implements [#140](https://github.com/getpiper/piper/issues/140). `piper deploy`
is synchronous — it POSTs the tarball and blocks until piperd finishes
build → run → health-check → route, printing nothing in between. On a first
deploy the base-image pull can run for minutes; during Pi onboarding that read
as a hang, and the only way to see progress was `journalctl -u piperd -f` on
the box.

## Approach

Reuse the mechanism the dashboard already uses, rather than invent a second
one. The dashboard follows a deploy by polling a **`building` deployment row
whose stored log grows** (`app-detail.tsx`: when `status === "building"` it
`setInterval`-refetches `deployments/{id}/logs` + the deployments list). But
piperd doesn't feed that today — `Deployer.Deploy` creates the deployment row
only *at the end* with `running`/`failed` and the complete log in one shot, so
there is never a `building` row to observe. **#140 and the dashboard's live-log
feature are the same missing piperd capability.**

So: piperd creates the `building` row up front and grows its log as the build
runs; the deploy POST becomes async (returns the id immediately); `piper
deploy` follows by polling the same endpoints the dashboard uses. One
mechanism, both clients, and webhook deploys light up in the dashboard as a
side benefit.

## Scope decisions

- **Async, not streaming.** No SSE/NDJSON/chunked response. The client follows
  by polling the existing `deployments` + `deployments/{id}/logs` endpoints.
- **Deploy stays one code path.** `Deployer.Deploy` remains a synchronous
  orchestration used unchanged by the webhook; the API handler is what runs it
  in a goroutine. Webhook deploys get a `building` row + live logs for free.
- **Breaking the deploy response** (200 `DeployResult` → 202 `{id, app,
  status}`) is acceptable pre-1.0; the CLI and piperd move in lockstep.
- **No new endpoint.** The CLI reads status from the existing deployments
  list, matching the dashboard.
- **Known limitation, out of scope:** a piperd crash mid-deploy leaves a
  `building` row that never finalizes. Startup reconciliation (mark stale
  `building` rows `failed`) is a follow-up, not this issue.

## 1. Store

The deployment row's lifecycle changes from "created once, fully populated at
the end" to "created empty at start, filled in as it runs." No schema change —
`status`, `logs`, and the id/port columns already exist.

- `CreateDeployment(app, "", "", 0, "building", "")` up front — the existing
  method, called earlier with empty ids and a `building` status. Its trailing
  log-retention prune still rides `created_at` and is unaffected.
- **New** `UpdateDeploymentLogs(id, logs string) error` — overwrites the `logs`
  column with the accumulated (tail-capped) blob.
- **New** `FinalizeDeployment(id, imageID, containerID string, hostPort int,
  status, logs string) error` — one `UPDATE` that fills in the real
  image/container/port and flips `status` to `running`/`failed`, plus a final
  log write.
- Unchanged and correct as-is: `LatestRunning` filters `status='running'`, so a
  `building` row is not mistaken for the previous running deployment;
  `LatestDeployment` returns it, so the app's status reads `building`
  mid-deploy (a canonical status per CLAUDE.md).

## 2. Deploy

`Deploy` is split so sync (webhook) and async (API) callers share one
orchestration:

- `Begin(appName) (store.Deployment, error)` — captures the previous running
  deployment (for later retirement), then creates the `building` row and
  returns it. Its id is what the API hands back immediately.
- `Finish(ctx, dep store.Deployment, srcDir string) error` — build → run →
  health → route → hostname → `FinalizeDeployment`. On failure it finalizes the
  row `failed` with the captured log (replacing today's separate `recordFailed`
  insert). Retires the previous running container/route on success, as today.
- `Deploy(ctx, appName, srcDir)` becomes a thin `Begin`+`Finish` wrapper, so
  the webhook path is unchanged in shape.

**Log capture.** `Finish` passes a **store-backed log sink** as the build's
progress `io.Writer`. The sink is a `runtime.TailBuffer` (existing 1 MiB tail
cap) fronted by a mutex; a ~1s ticker persists the accumulated blob via
`UpdateDeploymentLogs`, with a final flush when the build stream ends. `Finish`
also writes coarse stage lines into the same sink at the phase boundaries that
produce no output of their own — `→ starting container`, `→ health-checking`,
`→ routing` — so a poll during those phases still shows life. On run/health
failure the container's stdout/stderr is appended before `stopPartial`, exactly
as today.

**Runtime.** `Build` gains a progress writer:
`Build(ctx, srcDir, imageTag string, progress io.Writer)`. Internally
`DisplayJSONMessagesStream` writes to `io.MultiWriter(&tailBuffer, progress)` —
the buffer still feeds the finalized log; `progress` gets the same plain-text
lines live. `progress == nil` preserves today's behavior. The fake runtime
gains a settable build-output string it writes to `progress`. `Build`'s single
caller is `deploy`, and `DeployPreview` passes the same store-backed sink so
preview deploys also grow a `building` row.

## 3. API

`POST /v1/apps/{name}/deploy` becomes asynchronous:

1. Unknown-app (404) and bad-tar (400) checks run first, surfacing
   synchronously as today.
2. Untar into a temp dir, `Begin(name)` to create the `building` row.
3. Respond **202** with `{ "id": ..., "app": ..., "status": "building" }`.
4. Run `Finish` in a goroutine that **owns the temp dir** (`defer
   os.RemoveAll`) and a **background context** — not the request's, which is
   cancelled once the 202 is written. The context derives from piperd's
   lifetime so shutdown can cancel in-flight deploys.

The build outcome is observed by polling, exactly as the dashboard sees it. The
`GET deployments` and `GET deployments/{id}/logs` endpoints are unchanged. The
API's `Deployer` interface grows `Begin` and `Finish` (it keeps `Deploy` for
completeness); the webhook's interface keeps `Deploy`/`DeployPreview`.

## 4. Client + CLI

`Client.Deploy` returns the `building` deployment id from the 202. A new poll
loop in `cmd/piper deploy` (or a `Client.FollowDeploy` helper) then, every ~1s:

- `GET /v1/apps/{name}/deployments` → find the row by id, read `status`;
- `GET /v1/apps/{name}/deployments/{id}/logs` → the full log so far, printing
  only the **new** bytes since the last poll to **stderr**;

until `status` is `running`, `failed`, or `stopped`. On `running`: `GET
/v1/apps/{name}` for the routed hostname, print `deployed <name>: <url>
(running)` to **stdout**, exit 0. On `failed`: the streamed log already showed
why; exit 1. Progress on stderr keeps stdout clean and scriptable; the final
line stays on stdout as today.

## 5. Testing

TDD, failing-test-first per layer:

- **Store**: a `building` row round-trips; `UpdateDeploymentLogs` grows the
  blob; `FinalizeDeployment` sets ids/port and flips status; retention still
  prunes by count across the new lifecycle.
- **Deploy** (fake runtime): `Begin` yields a `building` row; `Finish`
  persists incremental logs and finalizes `running`; a build failure finalizes
  `failed` with the captured log; the `Deploy` wrapper equals `Begin`+`Finish`;
  `nil` progress path stays silent.
- **Runtime** (Docker-gated, skips without Docker): a real build writes
  non-empty live output to a `progress` buffer.
- **API**: POST returns 202 with a `building` id; the goroutine drives the row
  to `running`/`failed` (test polls status); unknown app still 404, bad tar
  still 400.
- **Client / cmd**: the poll loop prints log deltas and stops on a terminal
  status; `running` prints the deployed URL to stdout and exits 0; `failed`
  exits 1.

## Acceptance criteria → design

| Criterion | Where satisfied |
| --- | --- |
| First-time `piper deploy` visibly shows progress instead of a silent multi-minute block | §2 store-backed log sink + §3 async POST + §4 poll-and-print |
| Progress is real (build log covers the slow image pull) | §2 `Build` progress tee → incremental `logs` |
| Same mechanism the dashboard uses | §1–§3: a `building` row with a growing log, polled over the existing endpoints |
