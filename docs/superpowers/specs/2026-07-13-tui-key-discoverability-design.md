# TUI key discoverability — contextual footer + `?` help overlay

Closes the gap tracked in [#196](https://github.com/piperbox/piper/issues/196),
part of the Interactive TUI epic #183. The original TUI design spec
(`2026-07-12-piper-tui-design.md:34`) already lists `?` help as a global key; it
was never built, and the two navigation views expose no key hints at all.

## Problem

The TUI's two navigation views render no key legend, so their actions are
undiscoverable:

- **Apps list** — `n` new · `↵` open · `↑/k ↓/j` move · `r` refresh · `q` quit.
- **App detail** — `d` deploy · `s` stop · `x` delete · `↵` logs · `↑/k ↓/j` move.

The modal action views (new-app form, deploy/stop/delete confirms) already render
their own inline hints (e.g. `↵ create   tab switch   esc cancel`). The bottom
bar is a **status bar** (connectivity · box · app count), not a legend. This
issue does not touch the modals — only the two nav views and the new help view.

## Proposal

Two additions, no refactoring of existing views.

### 1. Contextual footer line

A single dim line listing the top view's keys, rendered by the **root** between
the view body and the status bar, always visible, one line of height.

The root already forwards optional behaviour to the top view through a narrow
interface seam (`topCapturesText()` checks `interface{ capturesText() bool }`).
The footer follows the same pattern:

```go
// footered is a view that offers a one-line key legend. The root renders it dim
// between the body and the status bar; views without it render no footer.
type footered interface{ footer() string }
```

`Model.View()` becomes:

```
header + "\n\n" + body + "\n\n" + [dim footer + "\n\n"] + statusBar
```

The footer slot is emitted only when the top view implements `footered`. Only
`appsView` and `appDetailView` do. The dim styling lives in one place
(`lipgloss.NewStyle().Faint(true)`), applied by the root.

Footer text (matches the issue's phrasing; both views end with `· ? help`):

| View | `footer()` |
| --- | --- |
| Apps list | `n new · ↵ open · r refresh · q quit · ? help` |
| App detail | `d deploy · s stop · x delete · ↵ logs · r refresh · esc back · ? help` |

Move keys (`↑/k ↓/j`) are omitted from the footer to keep it to one line; they
live in the `?` overlay's full keymap.

### 2. `?` help overlay

A new `helpView` (`help.go`), a static view pushed onto the stack, rendering the
full keymap table. It satisfies the `view` interface:

- `title()` → `"help"` (shows in the breadcrumb: `piper · apps › help`).
- `refresh(API)` → `nil` (no data to poll).
- `Update` → no-op (dismissal is the root's `esc`/`q` pop).
- `View()` → the full keymap, grouped by section.

Keymap rendered (from the issue):

```
Global      esc back/cancel · q quit (root) / back · r refresh · ctrl+c quit
Apps list   ↑/k ↓/j move · enter open · n new app
App detail  ↑/k ↓/j move · enter logs · d deploy · s stop · x delete
Logs        f toggle follow · esc back
```

`t` boxes (phase 5) is added here when that view lands — out of scope now.

Wiring in the root's `Update`, in the existing `!m.topCapturesText()` block so
text-entry views (new-app name, delete type-name) still receive a literal `?`:

```go
case "?":
    if _, ok := m.top().(helpView); !ok {
        return m, func() tea.Msg { return pushMsg{helpView{}} }
    }
    return m, nil
```

The stacking guard prevents pushing a second help view when one is already on
top. Dismissal reuses the root's existing `esc`/`q` pop — no new handling.

## Architecture / layering

Pure `internal/tui` change. No new dependencies, no API-surface change, nothing
imports up. `help.go` is a leaf view alongside the existing one-file-per-view
layout. The footer seam mirrors the established `capturesText()` seam.

## Testing (TDD, failing-test-first)

`render_test.go` / `app_test.go` / new coverage:

1. **Footer present** — `appsView.View()` (via the root, or the view's `footer()`
   directly) contains `n new` and `? help`; `appDetailView` contains `d deploy`
   and `esc back`.
2. **Footer absent on modals** — the root renders no footer line for a view that
   doesn't implement `footered` (e.g. the form view): the status bar still
   renders and no legend text leaks in.
3. **`?` pushes help** — from the apps list, a `?` key produces a `pushMsg` whose
   view is `helpView`; the root's render then contains keymap text
   (e.g. `toggle follow`).
4. **`?` is literal in text fields** — with the new-app form on top
   (`capturesText()` true), `?` does **not** push help; it reaches the field.
5. **Stacking guard** — `?` while `helpView` is already on top is a no-op.
6. **`esc` pops help** — existing root `esc` handling returns to the previous
   view (covered by the stack; assert depth returns to 1).

## Out of scope

- Any change to the modal views' existing inline hints.
- `t` boxes key (phase 5) — added to the overlay when that view lands.
- Move-key hints in the footer (they live in the overlay).
