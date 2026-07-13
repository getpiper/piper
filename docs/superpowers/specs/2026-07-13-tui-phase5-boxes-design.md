# TUI Phase 5 — boxes view (switcher + config editor)

**Date:** 2026-07-13
**Status:** approved design, pre-implementation
**Surface:** `[cli]` — the `piper` binary only; no `piperd` or `piper-relay` changes

Phase 5 of the Interactive TUI epic [#183](https://github.com/getpiper/piper/issues/183).
The multi-box config (schema v2) landed in phase 1 (#184); this phase gives it a
UI: a full-screen view that lists the configured boxes, switches the active box,
and adds/edits/removes boxes. Parent design:
`docs/superpowers/specs/2026-07-12-piper-tui-design.md` (View map → **Boxes**).

## Problem

The TUI runs against exactly one box — the one `cmd/piper` dialed at launch and
passed into `NewModel`. The schema-v2 config already holds many named boxes and a
`current` selection, but nothing in the TUI reads the other boxes, switches
between them, or edits them. The `t` boxes key named in the parent design
(`2026-07-12-piper-tui-design.md:34`) is unbuilt.

## Proposal

Two new leaf views plus small root wiring, following the established
one-file-per-view layout and the `API`-interface seam.

### 1. Client construction — injected dial factory

Switching boxes must build a **new** client for the selected box. `tui` talks
only to the `API` interface today (it imports `api`+`store`, never
`internal/client`); box-switching keeps that seam by injecting a factory that
`cmd/piper` owns:

```go
// Dialer builds a client for a box. cmd/piper supplies the real one (wrapping
// dialClient); tests inject a fake. remote marks a relay-backed box (HTTPS URLs).
type Dialer func(config.Box) (c API, addr string, remote bool, err error)
```

`NewModel`/`Run` gain a `Dialer` parameter; the root threads it into the two
views that dial other boxes — `newBoxesView(dial)` (per-box reachability probes)
and `newBoxForm(dial, …)` (the pre-save verify probe). `cmd/piper`'s injected
dialer builds a LAN client for the box:

```go
func(b config.Box) (tui.API, string, bool, error) {
    return client.New(b.Addr, b.Token).WithTimeout(tuiRequestTimeout), b.Addr, false, nil
}
```

**Relay-box switching is out of scope for phase 5.** `dialClient`'s relay path
needs a `--remote` base domain / agent name that no saved `Box` carries yet; that
mapping is the phase-6 login/connect wizard's to define. The boxes table still
*lists* relay-configured boxes, but selecting one is deferred (see Out of scope).
Injecting the factory now means phase 6 widens the dialer without re-architecting.

### 2. `boxes.go` — the switcher/editor view

Pushed by the **root** on `t` — a global key like `?` (parent design lists
`t boxes` among the root's global keys), wired in the existing
`!m.topCapturesText()` block so text fields still receive a literal `t`, with the
same already-on-top guard `?` uses. The root holds the `Dialer`, so it constructs
`newBoxesView(m.dial)`. Reads `config.LoadClientFile()` fresh on each
refresh — the boxes view is the one view that owns **local config** state, not
piperd state. Renders a table:

```
  NAME      ADDR                 STATUS
▸ ● pi4     192.168.1.6:8088     current
  ○ blog    192.168.1.9:8088     unreachable
```

- The current box is marked `current`; others show live reachability `●`/`○`/`…`.
- Keys: `↵` connect · `a` add · `e` edit · `x` remove · `↑/k ↓/j` move · `esc` back.
- `footer()` legend (phase-4 discoverability pattern):
  `↵ connect · a add · e edit · x remove · esc back · ? help`.
- The `?` help overlay gains a **Boxes** row (`t` from apps opens it; the parent
  design's placeholder `t boxes (phase 5)` is now real).

**Per-box reachability.** On every refresh (entry, the 2s tick, and `r`) the view
fires one independent async probe per box: dial the box, call `ListApps`,
reachable ⇔ no error. Each probe is its own `tea.Cmd` that lands as a
`boxProbeMsg{name string, reachable bool}`; a dead box resolves to `○` after the
client's 5s timeout without blocking the UI or any other row. Probing is
non-blocking and independent, so the every-refresh cadence costs nothing
perceptible for a home PaaS's handful of boxes. The current box's row shows
`current` rather than a probe result — its reachability already lives in the
status bar. Relay boxes (not switchable in phase 5) render `—` rather than a
probe.

### 3. `boxform.go` — add / edit box form

Reuses the `formView`/`bubbles/textinput` pattern. Fields: **name**, **addr**,
**token** (masked via `textinput.EchoPassword`). Add opens an empty form; edit
pre-fills from the selected box. `capturesText()` is true (the root hands it every
keystroke).

On `↵` the form **verifies before it saves** (parent design: "verifies with an
authenticated `ListApps` probe before saving"):

1. Validate name is non-empty and unique among boxes (edit exempts its own name);
   addr non-empty.
2. Dial `{name, addr, token}` and call `ListApps`. On error, banner it in the form
   (`⚠ can't reach box: …`) and do **not** write — the field stays focused.
3. On success, `config.SaveClientFile` with the box added or updated, then pop back
   to the boxes view. **Relay fields round-trip untouched** — edit reads the
   existing `Box`, overwrites only name/addr/token, and preserves
   `RelayAPI`/`AccountCredential`.

If the edited box is the **current** box, saving also re-dials so the live session
picks up the new addr/token (same mechanism as switch, below).

### 4. Remove

Reuses the existing `confirmView` y/n overlay (parent design: "box-remove is
y/n"). On confirm, drop the box from `ClientFile.Boxes` and `SaveClientFile`. If
the removed box was `current`, the first remaining box becomes current; removing
the last box is refused with a banner (the CLI always needs one target). Removing
the current box also re-dials the new current box.

### 5. Switch semantics

`↵` on a row emits `switchBoxMsg{box config.Box}` (the view already loaded the
box, so the root needn't re-read config). The root:

1. Calls `dial(box)`. On error, banner in the boxes view; keep the old client.
2. On success, swap `client` / `box` / `addr` / `remote`, reset the stack to a
   fresh apps view (`[]view{newAppsView(remote)}` — a different box means a fresh
   depth-0 home), and refresh.

`switchBoxMsg` carrying the full `Box` means `tui` imports `internal/config` for
the `Box` type — a legitimate *down* dependency the parent design already
sanctions (`… importing internal/client and internal/config only`).

## Architecture / layering

Pure `internal/tui` + one `cmd/piper` wiring change (build and inject the dialer,
widen the `tui.Run` call). `tui` adds an `internal/config` import (down-dep, for
the `Box` type). No `piperd`/`piper-relay` change, no API-surface change, nothing
imports up. `boxes.go` and `boxform.go` are leaf views alongside the existing
per-view files; `NewModel` grows one parameter (the `Dialer`).

## Testing (TDD, failing-test-first)

Views are pure `Update(msg) → (model, cmd)` state machines; tests are table-driven
Go with a fake `API` behind the seam and a fake `Dialer`.

1. **Boxes list renders** — `boxesView.View()` over a fake `ClientFile` shows each
   box's name/addr and marks the current one `current`.
2. **Reachability probe** — a `boxProbeMsg{name, reachable:true}` flips that row to
   `●`; `reachable:false` to `○`. `refresh` emits one probe cmd per box.
3. **Switch** — `↵` on a non-current row emits `switchBoxMsg{box}`; at the root, a
   fake dialer returning a new client swaps the active box, resets the stack to
   depth-0 apps, and the status bar shows the new box/addr.
4. **Switch failure** — a dialer error banners in the boxes view and leaves the
   old client/box in place (status bar unchanged).
5. **Add** — `a` pushes an empty `boxform`; a valid submit whose probe succeeds
   calls `SaveClientFile` with the new box appended and pops back.
6. **Add — bad token** — a submit whose `ListApps` probe errors banners in the
   form and does **not** write the file.
7. **Add — duplicate/empty name** — validation banners; no probe, no write.
8. **Edit preserves relay fields** — editing a box that has
   `RelayAPI`/`AccountCredential` and changing addr/token round-trips the relay
   fields unchanged in the saved file.
9. **Remove** — `x` → confirm `y` drops the box and saves; removing the current
   box promotes the first remaining box to current; removing the last box is
   refused with a banner.
10. **`t` opens boxes / help overlay** — `t` from the apps view pushes the boxes
    view; the `?` overlay contains a Boxes keymap row.

Config-mutation tests write to a temp `HOME` (the config path derives from
`os.UserHomeDir`), asserting the on-disk `ClientFile` after each operation — this
is the highest-stakes code (it rewrites the user's credential file), so it gets
the densest coverage.

## Out of scope

- **Relay-box switching / reachability** — selecting or probing a relay-backed box
  is deferred to phase 6 (the login/connect wizard owns the relay `Box` → client
  mapping). Relay boxes are listed but render `—` for status and decline `↵`.
- **The login/connect and GitHub wizards** — phase 6. The box form is
  name/addr/token only; relay fields stay wizard-managed and are never shown or
  edited here.
- **Free-form config editing** beyond the box form (parent design's stated v1
  omission).
