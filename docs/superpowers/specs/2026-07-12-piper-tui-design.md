# Piper TUI — dual-mode CLI design

**Date:** 2026-07-12
**Status:** approved design, pre-implementation
**Surface:** `[cli]` — the `piper` binary only; no `piperd` or `piper-relay` changes

## Goal

Turn `piper` into a dual-mode CLI: every existing subcommand stays scriptable and
byte-for-byte unchanged, and bare `piper` in a terminal opens a full-screen
interactive TUI that is a **full control surface** — monitor, deploy, lifecycle,
box management, and the login/GitHub wizards.

## Decisions (settled during brainstorming)

| Question | Decision |
| --- | --- |
| Purpose | Full control surface (not just a dashboard) |
| Entry | Bare `piper` in a TTY opens the TUI; non-TTY bare invocation prints usage and exits 2, as today; all subcommands untouched |
| Layout | Stacked full-screen views (k9s-style): one view owns the screen, `↵` pushes, `esc` pops, breadcrumb in header |
| Targets | In-TUI box switcher; requires multi-box config (below) |
| Stack | `charmbracelet/bubbletea` + `bubbles` + `lipgloss`; pure Go, `CGO_ENABLED=0` safe |
| v1 scope | Monitor + logs, deploy + lifecycle, login/connect wizard, GitHub setup/link, boxes view (switcher + config editor) |

## Architecture

New package `internal/tui`, importing `internal/client` and `internal/config`
only — a sibling frontend to `cmd/piper`'s command switch; nothing imports up.
`cmd/piper` gains one call: `tui.Run(...)` behind the TTY check
(`golang.org/x/term.IsTerminal`, injected as a func var for tests).

- `app.go` — root `tea.Model`: owns the view stack (`[]tea.Model`), the active
  box + its `*client.Client`, the global 2s tick, and global keys
  (`esc` pop, `q` quit-at-root, `r` refresh, `t` boxes, `?` help).
- One file per view, each an independent `tea.Model`.
- Views call the API through a narrow interface defined in `tui` (the methods
  they use: `ListApps`, `App`, `Deployments`, `DeploymentLogs`, `Deploy`,
  `FollowDeploy`, `CreateApp`, `StopApp`, `DeleteApp`, `LinkApp`, `Liveness`, …).
  `*client.Client` satisfies it as-is; tests inject a fake — the same
  interface-seam pattern `deploy` uses for runtime/caddy.

## Multi-box config (schema v2)

Today `ClientConfig` is one flat `{addr, token, relay_api, account_credential}`.
The box switcher needs named targets:

```json
{
  "boxes": [
    {"name": "pi4", "addr": "192.168.1.6:8088", "token": "…",
     "relay_api": "…", "account_credential": "…"}
  ],
  "current": "pi4"
}
```

- **Silent migration:** a legacy flat file loads as one box named `default`,
  marked current; the file is rewritten in v2 form on first save. Round-trips
  preserve relay fields.
- **No CLI behavior change:** `LoadClient`/`SaveClient` keep their signatures
  and return/accept the *current* box's view of the config. `piper login`,
  `connect`, and `--remote` operate on the current box exactly as they do on
  the flat config today.
- Relay fields are wizard-managed; the box edit form exposes only
  name / addr / token.

## View map

Stack depths; `↵` pushes, `esc` pops. Breadcrumb (e.g. `apps › blog › logs`)
renders from the stack.

**Depth 0 — Apps table** (home): NAME · STATUS · URL · LAST DEPLOY, refreshed
every 2s. Keys: `↵` open, `d` deploy, `n` new, `L` login, `g` github,
`t` boxes, `q` quit. If unauthenticated, a hint bar points at `L`.

**Depth 1** (from Apps):
- **App detail** — app header (url, port, repo) + deployments table.
- **Deploy** — source-dir input (default cwd) → live progress
  (upload → build → health → route). `esc` returns home; the deploy continues
  server-side and the apps table shows `◐ building` from normal polling.
- **New app** — form: name, port (default 8080) → create → offer deploy.
- **Boxes** — switcher **and** config editor: table of boxes with per-box
  reachability. `↵` connect, `a` add, `e` edit, `x` remove (confirm).
- **Login/connect wizard** — target type → token input → verify with an
  authenticated `ListApps` probe → save to current box.
- **GitHub setup wizard** — org input → opens browser for the manifest flow →
  TUI shows the URL + spinner while waiting for the callback; `esc` cancels.

**Depth 2** (from App detail):
- **Logs** — scrollable viewport; `f` toggles follow.
- **Link repo** — form: repo, branch → `LinkApp`.
- **Deploy** — same view, app pre-selected.
- **Box form** (from Boxes) — name / addr / token (masked); `↵` verifies with
  an authenticated `ListApps` probe before saving.

**Overlays (any depth):**
- **Confirm** — delete keeps the CLI's type-the-app-name guard; stop and
  box-remove are y/n.
- **Error banner** — inline in the owning view; never crashes or pops the stack.
- **Status bar** — bottom line of every view:
  `● pi4 · 192.168.1.6:8088 · N apps · t: switch box`;
  flips to `○ unreachable — retrying…` (views keep last-known data, grayed).
  Reachability derives from the outcome of the normal poll — piperd has no
  health/version endpoint, and `client.Liveness()` is relay-only (agent
  connectedness), used solely for per-box status of relay-backed boxes in the
  Boxes view. No piperd version in the bar for v1 (adding an endpoint would be
  `[agent]` scope); the client's own version lives in `?` help.

**Deliberate v1 omissions:** no bulk actions; no separate version/status views
(version in `?` help; liveness is the status bar); no free-form config editing
beyond the box form.

## Data flow

- **Polling:** one global 2s `tea.Tick` in the root model. Each tick refreshes
  the *current* view; the status bar's reachability and app count derive from
  the latest poll result (success/failure), so it costs no extra request.
  Background views don't poll. Every API call is a `tea.Cmd` goroutine returning a typed message
  (`appsLoadedMsg`, `liveMsg`, `errMsg`, …); the UI never blocks.
- **Box switch:** construct a new `*client.Client` from the selected box,
  assign it in the root, refresh. Views read the client via the root.
- **Deploy streaming:** `FollowDeploy(ctx, …, progress io.Writer)` — the deploy
  view bridges the writer to a channel; each line arrives as
  `deployProgressMsg`, the final `store.Deployment` as `deployDoneMsg`.
  Popping the view detaches the goroutine; the server-side deploy is unaffected.
- **Log follow:** no streaming endpoint exists, so follow mode re-fetches
  `DeploymentLogs` on the tick while the deployment is `building`/`running`
  and appends only the tail (diff by length). No new agent endpoint in v1.
- **Errors:** all failures land as `errMsg` in the owning view → inline banner.
  Connection loss is state, not error. Wizard verification failures render in
  the form with the field re-focused.

## Testing

TDD throughout; views are pure `Update(msg) → (model, cmd)` state machines, so
tests are table-driven Go with no terminal and no Docker:

- **View logic:** feed fixture messages + keypresses through `Update`, assert
  state and `View()` substrings, with a fake client behind the interface seam.
- **Stack navigation:** root-model tests for push/pop/breadcrumb/quit rules.
- **Config migration:** flat→v2 load, first-save rewrite, relay-field
  round-trip. Highest-stakes code (touches every user's credential file) —
  densest tests.
- **Deploy streaming:** scripted fake `FollowDeploy`; ordered progress,
  terminal done/failed.
- **Entry point:** `run()` cases — bare + non-TTY → usage (guards scripts);
  bare + TTY via the injected `isTerminal` seam.
- **e2e smoke:** `teatest` against a real `piperd` (skips without Docker, like
  the existing e2e): apps table renders a deployed app.

## Phasing — one PR per phase, epic + child issues

| # | PR | Contents | Size |
| --- | --- | --- | --- |
| 1 | multi-box config | Schema v2 + migration + current-box selection; no TUI, no behavior change | S |
| 2 | TUI skeleton | Deps, entry point (bare `piper` + TTY), root model/stack, status bar, read-only apps table with polling | M |
| 3 | drill-down | App detail, deployments, log viewer + follow — read-only surface complete | M |
| 4 | actions | Deploy view with streaming, new-app form, stop/delete confirms | M |
| 5 | boxes view | Switcher + editor over the phase-1 schema | S |
| 6 | wizards | Login/connect, GitHub setup, link repo | M |

Phase 2 flips bare `piper` (safe: non-TTY untouched). After phase 3 the TUI is
a useful read-only dashboard even if later phases slip.

## Dependencies

Adds `bubbletea`, `bubbles`, `lipgloss` as direct deps (the module already
carries a far larger tree via Caddy; `x/term` is already present indirectly).
All pure Go — `make cross` must stay green.
