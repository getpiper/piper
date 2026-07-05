# PR-preview URLs + teardown — design

_Date: 2026-07-05 · Issue: [#32](https://github.com/getpiper/piper/issues/32) · Part of Plan 3 ([#11](https://github.com/getpiper/piper/issues/11))_

## Goal

Finish Plan 3: an opened/updated pull request gets its own ephemeral live HTTPS
URL; closing the PR tears it down.

```
PR opened/synced  => build PR head → run → health → route at pr-<N>-<app>.<base>
PR closed         => stop container, drop hostname + Caddy route, report gone
```

The slice rides the existing `source` provider seam introduced in #31, which
deliberately deferred PR handling behind it. `deploy` stays ignorant of the
source; `webhook` translates PR events into two new orchestrator calls. Layering
is unchanged and nothing imports "up".

## Preview hostname: flattened to one label

The on-box TLS cert is issued for exactly `*.<base>` + `<base>`
(`cmd/piperd/main.go:178`). A wildcard covers a **single** DNS label, so
`myapp.<base>` is covered but `pr-5.myapp.<base>` (two labels deep) is **not** —
that would need a per-app `*.<app>.<base>` cert (new DNS-01 issuance + renewal)
plus per-app wildcard DNS, expanding scope into Plan 2.

Therefore the preview host flattens the PR label into a single DNS label:

```
pr-<N>-<app>.<base>        e.g. pr-5-myapp.alice.example.com
```

This is covered by the existing `*.<base>` cert, resolves under existing
wildcard DNS, and suffix-matches at the relay's SNI router — **zero Plan-2
changes**. It differs cosmetically from the issue title (`pr-N.<app>.<base>`);
that divergence is intentional and recorded here.

## Components

Ordered by layer, bottom-up. Each is independently testable with fakes.

### 1. `internal/source` — types

- Add `StatusInactive` to the `Status` enum. Teardown reports a preview
  deployment as gone; the enum is provider-agnostic.
- `Kind` values (`KindPROpened`/`KindPRSynced`/`KindPRClosed`) and `Event.PR`
  already exist. For PR events: `Event.SHA` = PR **head** SHA, `Event.Ref` =
  head ref (informational), `Event.PR` = number.

### 2. `internal/source/github/parse.go` — parse `pull_request`

- New `case "pull_request"` on `X-GitHub-Event`. Unmarshal `action`, `number`,
  and `pull_request.head.{ref,sha}`.
- Map action → kind: `opened`/`reopened` → `KindPROpened`;
  `synchronize` → `KindPRSynced`; `closed` → `KindPRClosed`. Other actions
  (e.g. `labeled`) → `KindOther`.
- Populate `ev.PR`, `ev.SHA` (head sha), `ev.Ref` (head ref).

**Known limitation (documented in code + here):** the head SHA is fetched via
the base repo's tarball endpoint (`Fetch` is unchanged), which works for
same-repo branch PRs. Cross-fork PRs — where the head commit lives in a fork —
are out of scope; a self-hosted, private-repo PaaS deploys its own branches.

### 3. `internal/source/github/report.go` — per-PR environment

- `createDeployment`: when `ev.PR > 0`, set `environment: "pr-<N>"` and
  `transient_environment: true` so GitHub groups the preview separately and
  auto-inactivates it on replacement. When `ev.PR == 0` keep `"production"` as
  today.
- `Report`: handle `StatusInactive` → resolve the latest deployment for
  `ev.SHA` (the closed PR payload carries the head sha) and post state
  `"inactive"`.

### 4. `internal/store` — persist previews keyed by (app, PR)

- Migrate additively: `ALTER TABLE deployments ADD COLUMN pr INTEGER NOT NULL
  DEFAULT 0`, ignoring "duplicate column" (existing `migrate` pattern).
- `Deployment` gains a `PR int` field.
- New `CreatePreviewDeployment(app string, pr int, imageID, containerID string,
  hostPort int, status string) (Deployment, error)` — like `CreateDeployment`
  but records `pr`.
- New `PreviewRunning(app string, pr int) (Deployment, error)` — latest
  `running` deployment for that (app, pr); `ErrNotFound` when none.
- The main path (`CreateDeployment`, `LatestRunning`) is untouched; existing
  rows default `pr = 0`.

### 5. `internal/deploy` — preview orchestration

- `hostForPreview(app string, pr int) string` →
  `fmt.Sprintf("pr-%d-%s.%s", pr, app, d.baseDom)`.
- Extract a private `buildRunHealthy(ctx, app, srcDir)` helper carrying the
  shared build → run → health steps (with failed-status recording), used by both
  `Deploy` and `DeployPreview` to avoid duplication. Low risk; behaviour of the
  existing `Deploy` path is preserved.
- `DeployPreview(ctx, app string, pr int, srcDir string) (store.Deployment,
  error)`: `buildRunHealthy` → `CreatePreviewDeployment(running)` →
  `UpsertRoute(hostForPreview)` → retire only the **previous preview for this
  (app, pr)** (looked up via `PreviewRunning` before the new record). It
  **never** consults the app's main `LatestRunning`, so a production deployment
  is never disturbed.
- `TeardownPreview(ctx, app string, pr int) error`: `PreviewRunning(app, pr)` →
  `runtime.Stop` → `RemoveRoute(hostForPreview)` → mark the record `stopped`.
  `ErrNotFound` is a no-op success (idempotent close).

### 6. `internal/webhook` — route PR events

- The `Deployer` interface gains:
  - `DeployPreview(ctx context.Context, app string, pr int, srcDir string)
    (store.Deployment, error)`
  - `TeardownPreview(ctx context.Context, app string, pr int) error`
- `process()` branches by kind:
  - `KindPush` → unchanged (branch-matched, routes `app.<base>`).
  - `KindPROpened` / `KindPRSynced` → `AppByRepo` (no branch match — any PR
    branch previews), dedupe on (app, pr, sha) via the in-memory `lastSHA` map
    keyed `"<app>#<pr>"`, Report `pending`, tmpdir → `Fetch` → `DeployPreview` →
    Report `success` with the preview URL.
  - `KindPRClosed` → `AppByRepo` → `TeardownPreview` → Report `inactive`. No
    fetch or build.
- The per-app lock (`appLock(app.Name)`) is shared between pushes and previews,
  serializing all deploy work for one app — simple and safe; different hosts and
  containers, one lock.
- The preview URL string (`https://pr-<N>-<app>.<base>`) is built in the handler,
  mirroring the existing push-URL line — matching current convention (the
  handler already owns the push URL format).

## Data flow

```
GitHub ──webhook──▶ Handler.ServeHTTP
                      │ prov.Parse → Event{Kind, Repo, PR, SHA, Ref}
                      ▼
                    Handler.process
       ┌──────────────┼───────────────────────────┐
   KindPush      KindPROpened/Synced          KindPRClosed
       │              │                            │
   deploy.Deploy  prov.Fetch → deploy.DeployPreview  deploy.TeardownPreview
       │              │                            │
   Report success  Report success (preview URL)  Report inactive
```

`deploy` calls `runtime` (build/run/health/stop), `store`
(create/lookup/update), and `routes` (Caddy upsert/remove) — never `source`.

## Error handling

- Parse failures / bad signature: unchanged (`401` / `400` in `ServeHTTP`).
- Any step in a preview deploy fails → log + `Report(StatusFailure)`; the tmpdir
  is always cleaned via `defer os.RemoveAll`.
- Teardown when no running preview exists → success no-op (a `closed` event may
  arrive with nothing deployed, e.g. a PR that never triggered a build).
- All GitHub API `Report`/`Fetch` errors are logged; `Report` errors are
  non-fatal to the deploy (consistent with the push path's `_ = h.prov.Report`).

## Testing

TDD, failing-test-first per step, `-race` clean. Fakes for `runtime` and
`Deployer`; `httptest` for GitHub.

- **parse** — table cases opened/reopened/synchronize/closed → kind, PR number,
  head sha/ref; non-PR actions → `KindOther`.
- **report** — preview `createDeployment` emits `environment: "pr-<N>"` +
  `transient_environment`; `StatusInactive` posts `"inactive"`.
- **store** — `CreatePreviewDeployment` + `PreviewRunning` round-trip; the `pr`
  migration is idempotent on an existing DB.
- **deploy** — `DeployPreview` routes `pr-<N>-<app>.<base>` and leaves the app's
  main deployment running; `TeardownPreview` stops the container and removes the
  route; `ErrNotFound` teardown is a no-op.
- **webhook** — fake deployer: open → `DeployPreview` + Report `success`;
  close → `TeardownPreview` + Report `inactive`; same-sha dedupe skips; untracked
  repo skipped.
- **integration** — `httptest`-stubbed GitHub + fake runtime proving
  open → live → close → gone without Docker.
- `make test` and `make cross` green; gofmt clean.

## Out of scope

- GitHub Actions status checks; raw / central-App webhook variants (deferred in
  the Plan 3 doc).
- Cross-fork PRs; BYO-domain; non-GitHub providers (the seam keeps these open).
- Two-label `pr-N.<app>.<base>` hosts and any per-app cert issuance.
