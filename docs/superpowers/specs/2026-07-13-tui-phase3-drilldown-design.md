# Piper TUI — Phase 3 (drill-down) design

> Detailed design for **phase 3** of the TUI epic ([#183](https://github.com/piperbox/piper/issues/183)),
> elaborating the phase-3 row of the master spec
> ([`2026-07-12-piper-tui-design.md`](2026-07-12-piper-tui-design.md)). Phases 1
> ([#184](https://github.com/piperbox/piper/issues/184)) and 2
> ([#185](https://github.com/piperbox/piper/issues/185)) are merged; this builds
> directly on that skeleton.

**Goal:** make the read-only surface complete. Bare `piper` already opens a live
apps table; phase 3 lets you drill into an app (header + live deployments table)
and into a single deployment's logs (scrollable viewport with follow). No actions
(deploy / stop / delete / link) — those are phase 4+.

**Non-goals / deferred (decided in brainstorming):**

- **LAST DEPLOY column** stays off the depth-0 apps table. `ListApps` carries no
  deploy timestamp, and per-app `Deployments` fetches every 2s would be an N+1
  storm. The table stays `NAME · STATUS · URL`; deploy times appear in the
  app-detail deployments table instead. LAST DEPLOY returns when the apps API
  grows a timestamp (its own `[agent]`/`[store]`/`[api]` issue).
- **Config-layer follow-ups** [#186](https://github.com/piperbox/piper/issues/186)
  (SaveClient `CurrentBox` fallback) and
  [#187](https://github.com/piperbox/piper/issues/187) (atomic config write) are
  left for phase 5's boxes view, which starts writing config. Phase 3 does not
  touch `internal/config`.
- No new agent endpoints. Phase 3 consumes only the client methods that already
  exist.

**Closes** [#189](https://github.com/piperbox/piper/issues/189) (TUI phase-3
polish) in full — see [§6](#6--189-cleanups-folded-in).

## Architecture

Phase 3 stays inside `internal/tui` (plus a one-line touch in `cmd/piper` for the
relay presentation). It introduces two views and refactors the root's polling and
navigation so the skeleton's single-view assumptions generalize to a stack.

### 1 — API seam & dependency

`tui.API` today is just `ListApps`. Extend it with the three read methods the new
views need — all already implemented on `*client.Client`:

```go
type API interface {
	ListApps() ([]api.App, error)
	App(name string) (api.App, error)
	Deployments(name string) ([]store.Deployment, error)
	DeploymentLogs(name, id string) (string, error)
}
```

The test fake grows to satisfy the wider interface. Add
`github.com/charmbracelet/bubbles@v1` (pinned to the v1 major like bubbletea and
lipgloss) for the `viewport` widget the log view uses. This is the "bubbles
arrives with the first widget need" deferral noted in the phase-1/2 plan.

### 2 — Per-view refresh (`view` interface)

The skeleton's `Model.refresh()` is hardcoded to `ListApps` and always emits
`appsLoadedMsg`. Phase 3 makes the poll refresh whichever view is on top of the
stack. Every stacked view implements:

```go
// view is a stack entry: a Bubble Tea model that can refresh its own data off
// the UI thread and name itself for the breadcrumb.
type view interface {
	tea.Model
	refresh(API) tea.Cmd // poll this view's data; returns a typed loaded msg or errMsg
	title() string       // breadcrumb segment, e.g. "apps", "blog", "logs"
}
```

The root stack becomes `[]view`. The 2s tick and the `r` key call
`top.refresh(m.client)` instead of the hardcoded `ListApps`. Only the top view
polls; background views keep their last-known data. Each view's `refresh` runs its
own API call(s) and returns its own loaded message (`appsLoadedMsg`,
`appDetailLoadedMsg`, `logsLoadedMsg`) or the shared `errMsg`.

**Reachability decoupled from payload.** The status bar's up/down state must track
the active view's poll outcome without the root knowing view specifics. A marker
interface carries it:

```go
// pollResult is implemented by every message that is the outcome of a view's
// poll, so the root updates reachability without knowing which view produced it.
type pollResult interface{ reachable() bool }
```

Loaded messages return `true`; `errMsg` returns `false`. The root, on any
`pollResult`, sets `down = !r.reachable()` and `loaded = true` on success — then
still forwards the message to the top view for its own state/inline banner.

### 3 — Navigation via messages (`pushMsg`)

Views own selection; the **root owns the stack**. A view never mutates the stack;
on `↵` it returns a command that emits:

```go
type pushMsg struct{ view view }
```

The root handles `pushMsg` by appending the view and immediately running its
`refresh(client)`, so the child populates without waiting for the next tick.
`esc`/`q` pop exactly as in the skeleton (`q` at depth 0 quits; both pop when
deep). This keeps stack ownership in one place and each view ignorant of its
parent.

Navigation chain:

- `appsView` (depth 0) gains a row cursor → `↵` returns `pushMsg{appDetailView(app)}`.
- `appDetailView` (depth 1) deployments table gains a cursor → `↵` returns
  `pushMsg{logsView(app, depID)}`.

## Views

### 4a — App detail (depth 1)

`appDetailView` renders an app header and its deployment history:

- **Header:** name, URL (`appURL(hostname, remote)`), port, repo/branch — from
  `App(name)`.
- **Deployments table** (cursor'd): short deployment ID · status icon+text
  (reusing `statusIcon`) · relative CREATED (e.g. `2m ago`) · `PR #N` when
  `PR > 0` — from `Deployments(name)`, newest first.

`refresh` fetches `App` and `Deployments` and emits `appDetailLoadedMsg`. Because
it re-polls every tick while on top, a deploy started elsewhere surfaces live: a
new `◐ building` row appears and flips to `● running`. Keys: `up`/`down`/`j`/`k`
move the cursor; `↵` opens the selected deployment's logs. Empty state ("no
deployments yet") when the slice is empty.

### 4b — Logs (depth 2)

`logsView` shows one deployment's build/deploy log in a `bubbles/viewport`:

- Initial `refresh` fetches `DeploymentLogs(app, id)` and loads it into the
  viewport.
- `f` toggles **follow**. While following, each tick re-fetches the logs and
  appends only the tail (diff by length — if the new text is longer, append the
  delta), keeping the viewport pinned to the bottom. `DeploymentLogs` returns the
  build/deploy log, which only grows while the deployment is `building`; once the
  build finishes the log is static. So the follow poll also reads the deployment's
  current status (via `Deployments(app)`, matched by ID) and **auto-stops follow
  when the deployment leaves `building`** (→ `running` / `failed` / `stopped`).
  Two light GETs per tick, only while actively following a still-building
  deployment.
- Not following, or the deployment already left `building`: static — the log
  won't grow, so no polling.

Keys: viewport scrolling (`up`/`down`/`pgup`/`pgdn`, and `g`/`G` for top/bottom)
handled by the `viewport` widget; `f` toggles follow. A follow indicator renders
in the view (e.g. `following…`) so the mode is visible.

## Data flow & errors

- One global 2s `tea.Tick` in the root, unchanged in cadence; it now refreshes the
  **top** view via `refresh`. Pushing a view refreshes it immediately.
- Every API call is a `tea.Cmd` goroutine returning a typed message; the UI thread
  never blocks. The phase-2 request timeout on the TUI's client
  (`client.WithTimeout`) still applies, so a blackholed box surfaces as
  unreachable on any view.
- All failures land as `errMsg` in the owning view → inline banner; the view keeps
  its last-known data (never crashes, never pops the stack). Connection loss is
  state (`down`), not an error dialog.

## 6 — #189 cleanups, folded in

Phase 3 already refactors the root and the apps view, so it closes
[#189](https://github.com/piperbox/piper/issues/189) entirely:

- **Breadcrumb.** The root header is rendered from the stack's `title()`s joined
  with ` › ` (e.g. `apps › blog › logs`), replacing the hardcoded ` piper · apps`.
- **App count off the root.** The status bar's `N apps` is read from the home view
  (bottom of stack) rather than a duplicate `appCount` field on `Model`, with
  correct pluralization (`1 app` / `N apps`).
- **`appURL` / relay presentation.** The `Model` carries whether the current box
  is relay-backed (passed from `launchTUI` instead of the `"via relay"` string).
  The status bar then shows the real base domain for a relay box, and
  `appURL(hostname, remote)` renders `https` for relay-terminated apps — bringing
  the TUI's URL rendering in line with the CLI's `appURL(hostname, remote bool)`
  semantics.

## Testing

TDD throughout; views are pure `Update(msg) → (model, cmd)` state machines, so
tests are table-driven Go with no terminal and no Docker (the phase-2 pattern):

- **View logic:** feed fixture messages + keypresses through each view's `Update`,
  assert state and `View()` substrings, against a fake `API` extended with
  `App`/`Deployments`/`DeploymentLogs`.
- **Stack navigation:** root-model tests for `↵`→`pushMsg`→push (+ immediate
  refresh), `esc`/`q` pop, and breadcrumb rendering across depths.
- **Reachability:** a `pollResult` from a deep view flips the status bar
  down/up without the root knowing the view type.
- **Log follow:** a scripted fake returning growing logs → assert tail-only
  append, viewport pinned to bottom, and follow auto-stop when the deployment
  leaves `building`.
- **App detail live update:** a fake whose `Deployments` gains a `building` row on
  the second poll → assert the new row renders.

## Deliverables

One PR into `main`, a new **Phase 3** child issue under epic #183, `Closes` that
issue and #189.

New/changed files (indicative):

- `internal/tui/tui.go` — widen `API`; add `pushMsg`, `pollResult`, the `view`
  interface, and the new loaded message types.
- `internal/tui/app.go` — stack becomes `[]view`; refresh delegates to the top
  view; `pushMsg` handling; breadcrumb; count from the home view; relay flag.
- `internal/tui/apps.go` — row cursor; `↵` → `pushMsg`; `title`/`refresh`/count.
- `internal/tui/appdetail.go` *(new)* — header + cursor'd deployments table.
- `internal/tui/logs.go` *(new)* — `bubbles/viewport` + follow + auto-stop.
- `internal/tui/render.go` — `appURL(hostname, remote)`; relative-time helper.
- `cmd/piper/main.go` — pass the relay/base-domain into the `Model`.
- `go.mod` / `go.sum` — add `bubbles` v1.
- matching `_test.go` files.
