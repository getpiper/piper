# Piper TUI — Phase 4 (actions) design

**Status:** approved · **Date:** 2026-07-13 · **Epic:** [#183](https://github.com/getpiper/piper/issues/183)

Phase 4 makes the TUI **read-write** for the first time. Phases 1–3 delivered a read-only surface (multi-box config, live apps dashboard, drill-down to app detail + deployment logs). Phase 4 adds the four mutating actions a developer reaches for from the dashboard: **create an app, deploy, stop, delete** — each with a proportional guard.

It stays **pure TUI**: no agent endpoints, no client changes. It widens the TUI's `API` interface with methods `*client.Client` already implements (the same move phase 3 made), reuses the phase-2/3 `view` stack, and reuses phase 3's follow-the-build log view wholesale for deploy progress.

## Scope

**In:**
- **New app** — form (name, port; default port 8080) → `CreateApp` → back to apps list.
- **Deploy** — confirm (shows the cwd it will ship + whether a `Dockerfile` is present) → `Deploy(name, cwd)` → live build view (reused from phase 3).
- **Stop** — y/n confirm → `StopApp`.
- **Delete** — type-the-app-name confirm (matches the CLI's guard) → `DeleteApp`.

**Out (deliberate):**
- **Link repo** — belongs to phase 6 (wizards: login/connect, GitHub setup, link repo).
- **Deploy source flexibility / real streaming** — deploy always ships the launch cwd; the build is followed by re-polling logs (phase-3 pattern), not a streaming endpoint. A configurable source or a streaming `FollowDeploy` bridge is a later concern.
- **Deploy-client timeout robustness** — see *Known limitation* below; explicitly deferred.
- No bulk actions; no "offer deploy" auto-prompt after create (the user presses `d` themselves).

## Interaction model

Everything rides the existing `view` stack. Three new stack entries, each implementing the existing `view` interface (`tea.Model` + `refresh(API) tea.Cmd` + `title() string`) so the breadcrumb, per-view poll delegation, and `esc`/`q` pop all work unchanged. Confirms and forms are modeled as **ordinary pushed views** (not a separate root overlay layer) — reuses push/pop/esc; the only cost is a transient breadcrumb crumb (`… › new app`, `… › confirm`), which is acceptable and honest about location.

**Key additions:**

| From view | Key | Opens |
| --- | --- | --- |
| apps (depth 0) | `n` | new-app form |
| app detail (depth 1) | `d` | deploy (confirm → build) |
| app detail (depth 1) | `s` | stop confirm (y/n) |
| app detail (depth 1) | `x` | delete confirm (type-name) |

Existing keys (`↑`/`↓`/`↵`/`esc`/`q`/`r`/`f`) are unchanged. Delete lives on app detail only (not the apps row) so post-delete navigation is a clean pop back to the apps list.

## New views

### `formView` (new app)
Two `bubbles/textinput` fields: **name** and **port** (pre-filled `8080`). `tab`/`↑`/`↓` move focus, `↵` submits, `esc` cancels (pops). Submit validates: non-empty name, port parses as an int in range. Invalid → inline banner, no API call, stay on the form. Valid → returns the `CreateApp(name, port)` action (see *Action & navigation model*).

### `deployView` (confirm gate)
Constructed with the target app name, the resolved cwd (`os.Getwd`), and whether `<cwd>/Dockerfile` exists (`os.Stat`). Renders the confirm screen:

```
deploy blog
  source:     /Users/fco/code/blog
  Dockerfile: found ✓          (or: not found ✗)
  ─────────────────────────
  y  ship it     esc  cancel
```

`esc` cancels (pops, nothing shipped). `y` fires `Deploy(name, cwd)` as a Cmd; the kickoff lands as a `deployStartedMsg{id, err}`. `Deploy` returns quickly with the new `building` deployment's ID (it kicks off the server-side build; it does **not** block for the whole build). On success the root **replaces** the deploy confirm with the **existing** follow-the-build log view — it pops the confirm and pushes a phase-3 `logsView(app, id, "building")`, so `esc` from the build returns to app detail (not back to the confirm). That `logsView` already tails the build log and auto-stops when the deployment leaves `building`. Deploy therefore contributes only the confirm gate + kickoff; the live build view is phase-3 code unchanged. On kickoff error → inline banner on the deploy view (no navigation).

### `confirmView` (stop / delete)
A modal confirm with a prompt, a mode, and the pending action:
- **y/n mode** (stop): `y` runs the action, `n`/`esc` cancels.
- **type-name mode** (delete): a `bubbles/textinput`; the action is gated on the typed text exactly equalling the app name; `↵` with a match runs it, `esc` cancels.

## Action & navigation model

Mutating actions run as `tea.Cmd` goroutines off the UI thread and land as one new message:

```go
actionResultMsg struct {
    err       error
    popLevels int // views to pop on success
}
```

The root handles it uniformly:
- **`err == nil`** → pop `popLevels` views, then refresh the now-top view so the change surfaces. `CreateApp` pops 1 (form → apps). `StopApp` pops 1 (confirm → detail). `DeleteApp` pops 2 (confirm + detail → apps list, since the app is gone).
- **`err != nil`** → do **not** pop; forward the error to the top overlay as its inline banner (e.g. duplicate-name on create, or a failed stop), so the user can correct or cancel.

Deploy is the exception: its kickoff yields `deployStartedMsg{id, err}`, not `actionResultMsg`. On success the root replaces the confirm with the build view (pop confirm + push `logsView`); on error it forwards a banner to the deploy view. The build result is then read live from the pushed `logsView`'s own polling.

All existing invariants hold: the stack always has ≥1 entry; only the top view is polled each 2s tick (opening an overlay/form suspends the parent's poll, popping resumes it); `esc`/`q` pop guards are unchanged.

## Data flow

- **Deploy kickoff:** `Deploy(name, cwd)` (fast, returns the building deployment) → `deployStartedMsg{id}` → root replaces the confirm with `logsView(app, id, "building")`. The build is followed by the phase-3 log-follow loop (re-poll `DeploymentLogs`/`Deployments` on the tick, tail by length-diff, auto-stop on terminal status). No streaming endpoint, no new agent work.
- **Errors:** every action failure lands as `actionResultMsg{err}` (or `errMsg` for deploy kickoff) → inline banner in the owning view; never crashes or corrupts the stack.

## API interface

Widen the TUI's `API` interface with the four methods (`*client.Client` already implements all four — verified in `internal/client/client.go`):

```go
CreateApp(name string, port int) error
Deploy(name, srcDir string) (store.Deployment, error)
StopApp(name string) error
DeleteApp(name string) error
```

`FollowDeploy` is **not** added to the interface — the deploy build is followed by the existing `logsView`, which already depends only on `DeploymentLogs`/`Deployments`.

## Known limitation (deferred)

The deploy **kickoff** call runs on the TUI's 5s-capped client (the phase-2 anti-hang `WithTimeout`, a client-wide `http.Client.Timeout`). A normal small-app tar uploads well under 5s, but a very large build context could hit the cap and surface as a deploy error. Fixing this cleanly means giving the TUI a separate longer-timeout deploy client (or per-request timeouts), a client-layer change kept **out of phase 4**. Revisit only if it bites in practice.

## Constraints (unchanged from phases 2–3)

- `CGO_ENABLED=0` everywhere; `make cross` (linux/arm64) stays green.
- Module `github.com/getpiper/piper`; `internal/tui` imports `internal/api` + `internal/store` + charmbracelet libs only — never `internal/client`. `cmd/piper` imports `internal/tui`.
- New dep `github.com/charmbracelet/bubbles/textinput`, pinned to the **v1** major already in use (bubbles is present since phase 3).
- Deployment status strings exactly `"building"`, `"running"`, `"failed"`, `"stopped"`; `""` → `—`.
- TDD: failing test first per task; `make verify` before push. One PR into `main`, squash-merged.

## Testing

- **formView:** field entry + focus movement; submit passes `(name, port)` to a recording `fakeAPI.CreateApp`; empty-name and bad-port → banner, no call.
- **confirmView:** y/n mode `y`→action / `n`/`esc`→cancel; type-name mode wrong text→no action, exact match→action.
- **deployView:** confirm renders cwd + Dockerfile status; `y` fires `Deploy` and hands to a follow `logsView`; kickoff error → banner; `esc` on confirm cancels without shipping.
- **root:** `actionResultMsg` success pops `popLevels` + refreshes; error forwards a banner and does not pop. Delete-from-detail pops to the apps list.
- **fakeAPI:** extended to record the four mutating calls with settable errors.

## Delivery

Read-only → read-write is a phase boundary, so this is its own child issue under epic #183, delivered task-by-task, failing-test-first, on a branch off `main`, one squash-merged PR. Implementation detail lives in the phase-4 plan.
