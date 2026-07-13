# TUI Phase 5 — Boxes View Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the multi-box config (schema v2) a UI: a full-screen boxes view that lists configured boxes, switches the active box, and adds/edits/removes them.

**Architecture:** A root-held `Dialer` factory (injected by `cmd/piper`, faked in tests) builds a client for any saved box, keeping `tui` decoupled from `internal/client`. A new `boxesView` (`boxes.go`) lists boxes read fresh from `config.LoadClientFile()` with per-box async reachability probes; a `boxFormView` (`boxform.go`) adds/edits a box, verifying via an authenticated `ListApps` probe before writing the config file. The root wires the global `t` key (like `?`), owns box-switching (swap client, reset the stack to a fresh apps view), and persists add/edit/remove through small config helpers. Relay-box switching is deferred to phase 6.

**Tech Stack:** Go, Bubble Tea (`github.com/charmbracelet/bubbletea`), Bubbles (`github.com/charmbracelet/bubbles/textinput`), Lip Gloss (`github.com/charmbracelet/lipgloss`), `internal/config` (schema v2).

## Global Constraints

- **No cgo.** All builds pass with `CGO_ENABLED=0`. No new dependencies (bubbletea/bubbles/lipgloss/config already present).
- **Module path** `github.com/getpiper/piper`.
- **Layering:** pure `internal/tui` change plus one `cmd/piper` wiring change. `tui` may import `internal/config` (a *down* dependency, for the `Box` type) — it must **not** import `internal/client`. Nothing imports up. No `piperd`/`piper-relay` change, no API-surface change.
- **Deployment status strings** unchanged; not touched here.
- **Relay boxes** (a `Box` with a non-empty `RelayAPI`) are **listed** but **not switchable** in this phase: they render `—` for status and decline `↵`. The box form edits only name/addr/token and preserves `RelayAPI`/`AccountCredential` untouched.
- **Commits:** conventional-commit style, `Part of #183` in the body, ending with:
  ```
  Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
  ```
- **Verify gate:** `make verify` (gofmt → vet → test → cross) must pass before the work is done. If gofmt flags files, run `make fmt` and re-run.
- **Existing test helpers** (in `internal/tui/app_test.go`, same package — reuse, do not redefine): `keyRunes(r rune) tea.KeyMsg`, `keyEnter()`, `keyBackspace()`, `keyTab()`, `pump(t, m, cmd) Model`, and the `fakeAPI` fake. Existing `NewModel(box, addr string, remote bool, c API) Model` keeps its 4-arg signature; the dialer is attached via `.WithDialer(...)`.
- **Config on disk:** `config.LoadClientFile()`/`SaveClientFile(cf)` resolve the path from `os.UserHomeDir()` (`~/.piper/piper/config.json`). Config-mutation tests set `HOME` to a temp dir with `t.Setenv` and assert the on-disk `ClientFile`.

---

### Task 1: Dialer seam, boxes list view, switch, and `t` wiring

Introduce the injected `Dialer`, a `boxesView` that lists boxes and marks the current one, root-owned box switching, and the global `t` key. Reachability probes come in Task 2; add/edit/remove in Tasks 3–4.

**Files:**
- Modify: `internal/tui/tui.go` — add `Dialer` type + `boxesLoadedMsg`/`switchBoxMsg` messages; import `internal/config`.
- Modify: `internal/tui/app.go` — add `dial Dialer` field + `WithDialer`; widen `Run`; handle `switchBoxMsg`; wire `t`.
- Modify: `internal/tui/apps.go` — add `t boxes` to the footer legend.
- Create: `internal/tui/boxes.go` — the `boxesView` (list + switch; probes stubbed until Task 2).
- Modify: `cmd/piper/main.go` — define `dialBox`, pass it to `tui.Run`.
- Test: `internal/tui/boxes_test.go` (new).

**Interfaces:**
- Produces:
  - `type Dialer func(config.Box) (c API, addr string, remote bool, err error)` (in `tui.go`).
  - `boxesLoadedMsg struct{ boxes []config.Box; current string }`, `switchBoxMsg struct{ box config.Box }` (in `tui.go`).
  - `func (m Model) WithDialer(d Dialer) Model` (in `app.go`).
  - `func Run(box, addr string, remote bool, c API, dial Dialer) error` (widened, in `app.go`).
  - `func newBoxesView(dial Dialer) boxesView` satisfying `view`; `title()` → `"boxes"`; `footer()` → `"↵ connect · a add · e edit · x remove · esc back · ? help"` (in `boxes.go`).
  - Test helpers `seedConfig(t, cf)` and `fakeDialer(...)` in `boxes_test.go` (reused by later tasks).

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/boxes_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/config"
)

// seedConfig points HOME at a temp dir and writes cf there, so config
// Load/Save in the view hit an isolated file.
func seedConfig(t *testing.T, cf config.ClientFile) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveClientFile(cf); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

// fakeDialer returns a Dialer that always yields the given result.
func fakeDialer(c API, addr string, remote bool, err error) Dialer {
	return func(config.Box) (API, string, bool, error) { return c, addr, remote, err }
}

func TestBoxesViewLoadsFromConfig(t *testing.T) {
	// refresh reads the seeded config off disk and yields boxesLoadedMsg.
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "pi4", Addr: "192.168.1.6:8088"}, {Name: "blog", Addr: "192.168.1.9:8088"}},
		Current: "pi4",
	})
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	msg := v.refresh(fakeAPI{})()
	loaded, ok := msg.(boxesLoadedMsg)
	if !ok {
		t.Fatalf("refresh should yield boxesLoadedMsg, got %T", msg)
	}
	if len(loaded.boxes) != 2 || loaded.current != "pi4" {
		t.Fatalf("config not loaded: %+v current=%q", loaded.boxes, loaded.current)
	}
}

func TestBoxesViewListsBoxesAndMarksCurrent(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "pi4", Addr: "192.168.1.6:8088"}, {Name: "blog", Addr: "192.168.1.9:8088"}},
		current: "pi4",
	})
	out := vv.(boxesView).View()
	for _, want := range []string{"pi4", "192.168.1.6:8088", "blog", "current"} {
		if !strings.Contains(out, want) {
			t.Fatalf("boxes view missing %q:\n%s", want, out)
		}
	}
}

func TestTPushesBoxesView(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	_, cmd := m.Update(keyRunes('t'))
	if cmd == nil {
		t.Fatal("t should push a boxes view")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if _, ok := push.view.(boxesView); !ok {
		t.Fatalf("want boxesView pushed, got %T", push.view)
	}
}

func TestTDoesNotStackBoxes(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	depth := len(m.stack)
	_, cmd := m.Update(keyRunes('t'))
	if cmd != nil {
		if _, ok := cmd().(pushMsg); ok {
			t.Fatal("t on the boxes view must not push a second boxes view")
		}
	}
	if len(m.stack) != depth {
		t.Fatalf("stack depth changed: %d -> %d", depth, len(m.stack))
	}
}

func TestEnterOnBoxEmitsSwitch(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}}, current: "pi4"})
	v = vv.(boxesView)
	// cursor starts at 0 (pi4, current); move to blog and connect
	vv, _ = v.Update(keyRunes('j'))
	v = vv.(boxesView)
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should emit a switch")
	}
	sw, ok := cmd().(switchBoxMsg)
	if !ok || sw.box.Name != "blog" {
		t.Fatalf("want switchBoxMsg for blog, got %#v", cmd())
	}
}

func TestRootSwitchSwapsBoxAndResetsStack(t *testing.T) {
	m := NewModel("pi4", "192.168.1.6:8088", false, fakeAPI{}).
		WithDialer(fakeDialer(fakeAPI{apps: nil}, "192.168.1.9:8088", false, nil))
	// go deep so we can prove the stack resets
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(switchBoxMsg{box: config.Box{Name: "blog", Addr: "192.168.1.9:8088"}})
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("switch should reset to a single apps view, got depth %d", len(m.stack))
	}
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "blog") || !strings.Contains(out, "192.168.1.9:8088") {
		t.Fatalf("status bar did not switch to blog:\n%s", out)
	}
}

func TestRootSwitchFailureBannersAndKeepsBox(t *testing.T) {
	m := NewModel("pi4", "192.168.1.6:8088", false, fakeAPI{}).
		WithDialer(fakeDialer(nil, "", false, errors.New("dial refused")))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(switchBoxMsg{box: config.Box{Name: "blog", Addr: "x"}})
	m = m2.(Model)
	if m.box != "pi4" {
		t.Fatalf("failed switch must keep the old box, got %q", m.box)
	}
	if !strings.Contains(m.View(), "dial refused") {
		t.Fatalf("switch error should banner in the boxes view:\n%s", m.View())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestBoxes|TestTPushes|TestTDoesNot|TestEnterOnBox|TestRootSwitch' -v`
Expected: FAIL — compile errors (`undefined: newBoxesView`, `boxesLoadedMsg`, `switchBoxMsg`, `Dialer`, `WithDialer`).

- [ ] **Step 3: Add the `Dialer` type and messages in `tui.go`**

Add `internal/config` to the import block in `internal/tui/tui.go`:

```go
import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/store"
)
```

After the `API` interface (around `tui.go:23`), add:

```go
// Dialer builds a client for a saved box. cmd/piper supplies the real one
// (LAN path); tests inject a fake. addr identifies the box in the status bar;
// remote marks a relay-backed box (HTTPS app URLs).
type Dialer func(config.Box) (c API, addr string, remote bool, err error)
```

In the message block (the `type ( … )` group), add:

```go
	// boxesLoadedMsg carries the client config the boxes view renders. It is a
	// local-config load, not a piperd poll, so it does not implement pollResult
	// (the status bar keeps its last-known reachability while browsing boxes).
	boxesLoadedMsg struct {
		boxes   []config.Box
		current string
	}

	// switchBoxMsg is the boxes view's connect intent; the root dials the box,
	// swaps the active client, and resets the stack to a fresh apps view.
	switchBoxMsg struct{ box config.Box }
```

- [ ] **Step 4: Add the dialer field, `WithDialer`, widen `Run`, handle `switchBoxMsg`, and wire `t` in `app.go`**

Add `internal/config` to `app.go`'s imports:

```go
import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/getpiper/piper/internal/config"
)
```

Add a `dial Dialer` field to `Model` (after the `client API` field, around `app.go:21`):

```go
	client API
	dial   Dialer
```

After `NewModel` (around `app.go:31`), add the builder:

```go
// WithDialer attaches the box-switch client factory and returns the model for
// chaining. Kept separate from NewModel so existing call sites (and tests that
// never switch boxes) stay four-argument.
func (m Model) WithDialer(d Dialer) Model { m.dial = d; return m }
```

Widen `Run` (currently `app.go:35`) to accept and attach the dialer:

```go
// Run starts the interactive TUI against c, identified as box/addr in the
// status bar. remote marks a relay-backed box (HTTPS URLs). dial builds clients
// for the box switcher. It blocks until quit.
func Run(box, addr string, remote bool, c API, dial Dialer) error {
	m := NewModel(box, addr, remote, c).WithDialer(dial)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}
```

In `Update`, inside the `if !m.topCapturesText() {` block (alongside the `q`/`r`/`?` cases), add the `t` case:

```go
			case "t":
				if _, ok := m.top().(boxesView); !ok {
					return m, func() tea.Msg { return pushMsg{newBoxesView(m.dial)} }
				}
				return m, nil
```

Add a `switchBoxMsg` case among the other message cases in `Update` (e.g. after the `pushMsg` case, around `app.go:122`):

```go
	case switchBoxMsg:
		c, addr, remote, err := m.dial(msg.box)
		if err != nil {
			next, _ := m.top().Update(errMsg{err})
			m.stack[len(m.stack)-1] = next.(view)
			return m, nil
		}
		m.client, m.box, m.addr, m.remote = c, msg.box.Name, addr, remote
		m.loaded, m.down = false, false
		m.stack = []view{newAppsView(remote)}
		if m.width > 0 {
			seeded, _ := m.top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack[len(m.stack)-1] = seeded.(view)
		}
		return m, m.refresh()
```

- [ ] **Step 5: Add `t boxes` to the apps footer**

In `internal/tui/apps.go`, change the `footer()` method (currently `apps.go:28`):

```go
func (v appsView) footer() string { return "n new · t boxes · ↵ open · r refresh · q quit · ? help" }
```

- [ ] **Step 6: Create `internal/tui/boxes.go`**

```go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/config"
)

// boxesView is the depth-1 box switcher/editor: a table of the configured boxes
// read fresh from the client config. ↵ connects (switches the active box), a/e
// add/edit via a form, x removes. It is the one view that owns local config
// state rather than piperd state. Relay boxes are listed but not switchable.
type boxesView struct {
	dial    Dialer
	boxes   []config.Box
	current string
	loaded  bool
	cursor  int
	err     error
}

func newBoxesView(dial Dialer) boxesView { return boxesView{dial: dial} }

func (v boxesView) Init() tea.Cmd { return nil }

func (v boxesView) title() string { return "boxes" }

func (v boxesView) footer() string {
	return "↵ connect · a add · e edit · x remove · esc back · ? help"
}

// refresh reloads the client config off the UI thread. (Per-box reachability
// probes are added in a later task.)
func (v boxesView) refresh(API) tea.Cmd {
	return func() tea.Msg {
		cf, err := config.LoadClientFile()
		if err != nil {
			return errMsg{err}
		}
		return boxesLoadedMsg{boxes: cf.Boxes, current: cf.Current}
	}
}

// isRelay reports whether the box at i is relay-backed (not switchable here).
func (v boxesView) isRelay(i int) bool { return v.boxes[i].RelayAPI != "" }

func (v boxesView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case boxesLoadedMsg:
		v.boxes, v.current, v.loaded, v.err = msg.boxes, msg.current, true, nil
		if v.cursor >= len(v.boxes) {
			v.cursor = max(0, len(v.boxes)-1)
		}
	case errMsg:
		v.err = msg.err
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if v.cursor > 0 {
				v.cursor--
			}
		case "down", "j":
			if v.cursor < len(v.boxes)-1 {
				v.cursor++
			}
		case "enter":
			if len(v.boxes) > 0 && !v.isRelay(v.cursor) {
				box := v.boxes[v.cursor]
				return v, func() tea.Msg { return switchBoxMsg{box: box} }
			}
		}
	}
	return v, nil
}

func (v boxesView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-16s %-22s %s\n", "NAME", "ADDR", "STATUS")
	for i, box := range v.boxes {
		cursor := "  "
		if i == v.cursor {
			cursor = "▸ "
		}
		fmt.Fprintf(&b, "%s%-16s %-22s %s\n", cursor, box.Name, box.Addr, v.status(i))
	}
	return b.String()
}

// status renders the STATUS column for row i: "current" for the active box, "—"
// for relay boxes (not switchable here); reachability probes fill the rest in a
// later task.
func (v boxesView) status(i int) string {
	switch {
	case v.boxes[i].Name == v.current:
		return "current"
	case v.isRelay(i):
		return "—"
	default:
		return ""
	}
}
```

- [ ] **Step 7: Wire the dialer in `cmd/piper/main.go`**

Add `dialBox` near `launchTUI` (after it, around `cmd/piper/main.go:108`):

```go
// dialBox builds a TUI client for an arbitrary saved box (LAN path), for the
// in-TUI box switcher. Relay boxes are switched via the phase-6 wizard, not here.
func dialBox(b config.Box) (tui.API, string, bool, error) {
	return client.New(b.Addr, b.Token).WithTimeout(tuiRequestTimeout), b.Addr, false, nil
}
```

Change the `tui.Run` call (currently `cmd/piper/main.go:103`) to pass it:

```go
	if err := tui.Run(box, addr, relay, c, dialBox); err != nil {
```

(`config` and `client` are already imported in `cmd/piper/main.go`; confirm `tui` is too.)

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestBoxes|TestTPushes|TestTDoesNot|TestEnterOnBox|TestRootSwitch' -v`
Expected: PASS (all).

- [ ] **Step 9: Run the full package + the cmd build**

Run: `go test ./internal/tui/ && go build ./cmd/piper/`
Expected: `ok  github.com/getpiper/piper/internal/tui`; build succeeds.

- [ ] **Step 10: Commit**

```bash
git add internal/tui/tui.go internal/tui/app.go internal/tui/apps.go internal/tui/boxes.go internal/tui/boxes_test.go cmd/piper/main.go
git commit -m "feat(cli): add TUI boxes view with switcher and t key

Part of #183

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Per-box reachability probes

On each refresh the boxes view fires one independent async probe per non-current, non-relay box (dial + `ListApps`); results flip the row's STATUS to `●`/`○`. Probes are non-blocking, so the every-refresh cadence is cheap.

**Files:**
- Modify: `internal/tui/tui.go` — add `boxProbeMsg`.
- Modify: `internal/tui/boxes.go` — store probe results; emit probes on load; render `●`/`○`/`…`.
- Test: `internal/tui/boxes_test.go` — append.

**Interfaces:**
- Consumes: `Dialer`, `boxesLoadedMsg` from Task 1.
- Produces: `boxProbeMsg struct{ name string; reachable bool }` (in `tui.go`); `boxesView.reach map[string]bool` populated by probes; `boxesView.Update(boxesLoadedMsg)` now returns a `tea.Batch` of probe cmds.

- [ ] **Step 1: Write the failing tests**

Append to `internal/tui/boxes_test.go`, and add the Bubble Tea import to its import block (the Task-2 tests reference `tea.BatchMsg`):

```go
import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/config"
)
```

```go
func TestBoxesRefreshEmitsProbePerRemoteBox(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	// current box (pi4) is not probed; blog and shop are.
	vv, cmd := v.Update(boxesLoadedMsg{
		boxes:   []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}, {Name: "shop", Addr: "b"}},
		current: "pi4",
	})
	_ = vv
	if cmd == nil {
		t.Fatal("loading boxes should emit reachability probes")
	}
	msg := cmd() // tea.Batch aggregates into a BatchMsg of cmds
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("want tea.BatchMsg of probes, got %T", msg)
	}
	if len(batch) != 2 {
		t.Fatalf("want 2 probes (non-current, non-relay), got %d", len(batch))
	}
}

func TestBoxProbeMsgFlipsRowStatus(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}}, current: "pi4"})
	v = vv.(boxesView)

	vv, _ = v.Update(boxProbeMsg{name: "blog", reachable: true})
	if out := vv.(boxesView).View(); !strings.Contains(out, "●") {
		t.Fatalf("reachable box should show ●:\n%s", out)
	}

	vv, _ = v.Update(boxProbeMsg{name: "blog", reachable: false})
	if out := vv.(boxesView).View(); !strings.Contains(out, "○") {
		t.Fatalf("unreachable box should show ○:\n%s", out)
	}
}

func TestBoxProbeReflectsDialerResult(t *testing.T) {
	// a dialer whose client ListApps errors => unreachable
	v := newBoxesView(fakeDialer(fakeAPI{err: errors.New("refused")}, "", false, nil))
	probe := v.probe(config.Box{Name: "blog", Addr: "a"})
	msg := probe().(boxProbeMsg)
	if msg.name != "blog" || msg.reachable {
		t.Fatalf("want blog unreachable, got %#v", msg)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestBoxesRefreshEmitsProbe|TestBoxProbe' -v`
Expected: FAIL — `undefined: boxProbeMsg`, `v.probe undefined`, `●` absent.

- [ ] **Step 3: Add `boxProbeMsg` in `tui.go`**

In the message block, after `switchBoxMsg`, add:

```go
	// boxProbeMsg is one box's reachability probe result, rendered in the boxes
	// view's STATUS column. Each box is probed by its own tea.Cmd, so a dead box
	// resolves after its client timeout without blocking the others.
	boxProbeMsg struct {
		name      string
		reachable bool
	}
```

- [ ] **Step 4: Store results, emit probes, and render status in `boxes.go`**

Add a `reach` map to the struct (after `err error`):

```go
type boxesView struct {
	dial    Dialer
	boxes   []config.Box
	current string
	loaded  bool
	cursor  int
	err     error
	reach   map[string]bool // box name -> last probe result; absent = probing
}
```

Change `newBoxesView` to initialise the map:

```go
func newBoxesView(dial Dialer) boxesView {
	return boxesView{dial: dial, reach: map[string]bool{}}
}
```

Add a `probe` helper:

```go
// probe returns a cmd that dials box and calls ListApps; reachable is true iff
// both succeed. One cmd per box keeps a dead box from blocking the others.
func (v boxesView) probe(box config.Box) tea.Cmd {
	dial := v.dial
	return func() tea.Msg {
		c, _, _, err := dial(box)
		if err == nil {
			_, err = c.ListApps()
		}
		return boxProbeMsg{name: box.Name, reachable: err == nil}
	}
}
```

In `Update`, replace the `boxesLoadedMsg` case to reset probe state and emit the probe batch, and add a `boxProbeMsg` case:

```go
	case boxesLoadedMsg:
		v.boxes, v.current, v.loaded, v.err = msg.boxes, msg.current, true, nil
		v.reach = map[string]bool{}
		if v.cursor >= len(v.boxes) {
			v.cursor = max(0, len(v.boxes)-1)
		}
		var probes []tea.Cmd
		for i, box := range v.boxes {
			if box.Name == v.current || v.isRelay(i) {
				continue
			}
			probes = append(probes, v.probe(box))
		}
		return v, tea.Batch(probes...)
	case boxProbeMsg:
		v.reach[msg.name] = msg.reachable
```

Update `status` to render the probe result for non-current, non-relay boxes:

```go
func (v boxesView) status(i int) string {
	switch {
	case v.boxes[i].Name == v.current:
		return "current"
	case v.isRelay(i):
		return "—"
	}
	reachable, probed := v.reach[v.boxes[i].Name]
	switch {
	case !probed:
		return "…"
	case reachable:
		return "●"
	default:
		return "○"
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestBoxesRefreshEmitsProbe|TestBoxProbe' -v`
Expected: PASS (all three).

- [ ] **Step 6: Run the full package**

Run: `go test ./internal/tui/`
Expected: `ok  github.com/getpiper/piper/internal/tui`

- [ ] **Step 7: Commit**

```bash
git add internal/tui/tui.go internal/tui/boxes.go internal/tui/boxes_test.go
git commit -m "feat(cli): add per-box reachability probes to the TUI boxes view

Part of #183

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Add/edit box form with verify-before-save

A `boxFormView` collects name/addr/token (token masked). On submit it validates locally, then dials + probes `ListApps`; only on success does it write the config (add or update, preserving relay fields) and return to the boxes view. Editing the current box re-dials.

**Files:**
- Modify: `internal/tui/tui.go` — add `boxSavedMsg`.
- Modify: `internal/tui/app.go` — handle `boxSavedMsg`.
- Modify: `internal/tui/boxes.go` — add `saveBox` helper; wire `a`/`e` to push the form.
- Create: `internal/tui/boxform.go` — the `boxFormView`.
- Test: `internal/tui/boxform_test.go` (new).

**Interfaces:**
- Consumes: `Dialer`, `errMsg`, `popMsg`, `boxesLoadedMsg` from earlier tasks; `config.Box`, `config.LoadClientFile`, `config.SaveClientFile`.
- Produces:
  - `func saveBox(box config.Box, replacing string) error` (in `boxes.go`) — appends `box`, or updates the box named `replacing` (name may change), preserving all other boxes.
  - `boxSavedMsg struct{ box config.Box }` (in `tui.go`).
  - `func newBoxForm(dial Dialer, boxes []config.Box) boxFormView` (add) and `func newBoxFormEdit(dial Dialer, boxes []config.Box, orig config.Box) boxFormView` (edit) (in `boxform.go`); `title()` → `"box"`; `capturesText()` → `true`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/boxform_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/config"
)

// submitForm fills the fields and returns the cmd produced by Enter.
func submitForm(t *testing.T, v boxFormView, name, addr, token string) (boxFormView, tea.Cmd) {
	t.Helper()
	v.name.SetValue(name)
	v.addr.SetValue(addr)
	v.token.SetValue(token)
	m, cmd := v.Update(keyEnter())
	if cmd == nil {
		return m.(boxFormView), nil
	}
	return m.(boxFormView), cmd
}

func TestBoxFormValidSubmitVerifiesThenSaves(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4", Addr: "a"}}, Current: "pi4"})
	v := newBoxForm(fakeDialer(fakeAPI{}, "", false, nil), []config.Box{{Name: "pi4", Addr: "a"}})
	_, cmd := submitForm(t, v, "blog", "192.168.1.9:8088", "tok")
	if cmd == nil {
		t.Fatal("valid submit should emit a verify+save cmd")
	}
	if _, ok := cmd().(boxSavedMsg); !ok {
		t.Fatalf("want boxSavedMsg on success, got %T", cmd())
	}
	cf, err := config.LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 2 || cf.Boxes[1].Name != "blog" || cf.Boxes[1].Addr != "192.168.1.9:8088" {
		t.Fatalf("box not saved: %+v", cf.Boxes)
	}
}

func TestBoxFormBadTokenBannersNoWrite(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4", Addr: "a"}}, Current: "pi4"})
	v := newBoxForm(fakeDialer(fakeAPI{err: errors.New("401 unauthorized")}, "", false, nil), []config.Box{{Name: "pi4", Addr: "a"}})
	_, cmd := submitForm(t, v, "blog", "x", "bad")
	msg := cmd()
	if _, ok := msg.(errMsg); !ok {
		t.Fatalf("want errMsg on failed probe, got %T", msg)
	}
	cf, _ := config.LoadClientFile()
	if len(cf.Boxes) != 1 {
		t.Fatalf("failed probe must not write: %+v", cf.Boxes)
	}
}

func TestBoxFormRejectsEmptyAndDuplicateName(t *testing.T) {
	boxes := []config.Box{{Name: "pi4", Addr: "a"}}
	// empty name
	v := newBoxForm(fakeDialer(fakeAPI{}, "", false, nil), boxes)
	vv, cmd := submitForm(t, v, "", "x", "")
	if cmd != nil {
		t.Fatal("empty name must not submit")
	}
	if !strings.Contains(vv.View(), "name") {
		t.Fatalf("expected a name validation banner:\n%s", vv.View())
	}
	// duplicate name
	v = newBoxForm(fakeDialer(fakeAPI{}, "", false, nil), boxes)
	vv, cmd = submitForm(t, v, "pi4", "x", "")
	if cmd != nil {
		t.Fatal("duplicate name must not submit")
	}
	if !strings.Contains(vv.View(), "exists") {
		t.Fatalf("expected a duplicate-name banner:\n%s", vv.View())
	}
}

func TestBoxFormEditPreservesRelayFields(t *testing.T) {
	orig := config.Box{Name: "cloud", Addr: "old", Token: "t1", RelayAPI: "https://relay.example", AccountCredential: "cred"}
	seedConfig(t, config.ClientFile{Boxes: []config.Box{orig}, Current: "cloud"})
	v := newBoxFormEdit(fakeDialer(fakeAPI{}, "", false, nil), []config.Box{orig}, orig)
	_, cmd := submitForm(t, v, "cloud", "new-addr", "t2")
	if _, ok := cmd().(boxSavedMsg); !ok {
		t.Fatalf("edit should save, got %T", cmd())
	}
	cf, _ := config.LoadClientFile()
	got := cf.Boxes[0]
	if got.Addr != "new-addr" || got.Token != "t2" {
		t.Fatalf("edit did not update addr/token: %+v", got)
	}
	if got.RelayAPI != "https://relay.example" || got.AccountCredential != "cred" {
		t.Fatalf("edit dropped relay fields: %+v", got)
	}
}

func TestBoxesKeyOpensForms(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4", Addr: "a"}}, current: "pi4"})
	v = vv.(boxesView)

	_, cmd := v.Update(keyRunes('a'))
	if push, ok := cmd().(pushMsg); !ok {
		t.Fatalf("a should push a form, got %T", cmd())
	} else if _, ok := push.view.(boxFormView); !ok {
		t.Fatalf("a should push boxFormView, got %T", push.view)
	}

	_, cmd = v.Update(keyRunes('e'))
	if push, ok := cmd().(pushMsg); !ok {
		t.Fatalf("e should push a form, got %T", cmd())
	} else if _, ok := push.view.(boxFormView); !ok {
		t.Fatalf("e should push boxFormView, got %T", push.view)
	}
}

func TestRootBoxSavedPopsToBoxes(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(pushMsg{newBoxForm(m.dial, nil)})
	m = m2.(Model)
	if len(m.stack) != 3 {
		t.Fatalf("setup: want depth 3, got %d", len(m.stack))
	}
	// saving a non-current box pops back to the boxes view
	m2, _ = m.Update(boxSavedMsg{box: config.Box{Name: "blog"}})
	m = m2.(Model)
	if len(m.stack) != 2 {
		t.Fatalf("box saved should pop to boxes view (depth 2), got %d", len(m.stack))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestBoxForm|TestBoxesKeyOpensForms|TestRootBoxSaved' -v`
Expected: FAIL — compile errors (`undefined: boxFormView`, `newBoxForm`, `newBoxFormEdit`, `boxSavedMsg`, `saveBox`).

- [ ] **Step 3: Add `boxSavedMsg` in `tui.go`**

After `boxProbeMsg`, add:

```go
	// boxSavedMsg is the box form's success outcome. The root pops back to the
	// boxes view; if the saved box is the current one, it re-dials (its addr or
	// token may have changed) via the same path as a switch.
	boxSavedMsg struct{ box config.Box }
```

- [ ] **Step 4: Add the `boxSavedMsg` handler in `app.go`**

Add this case in `Update` (after the `switchBoxMsg` case):

```go
	case boxSavedMsg:
		if msg.box.Name == m.box {
			return m.Update(switchBoxMsg{box: msg.box}) // current box changed: re-dial
		}
		m = m.popN(1)
		return m, m.refresh()
```

- [ ] **Step 5: Add the `saveBox` helper and `a`/`e` wiring in `boxes.go`**

Add `saveBox` at the end of `boxes.go`:

```go
// saveBox writes box to the client config: it updates the box named replacing
// (whose name may change), else appends box. All other boxes are preserved; a
// first box (empty config) becomes current. replacing == "" means add.
func saveBox(box config.Box, replacing string) error {
	cf, err := config.LoadClientFile()
	if err != nil {
		return err
	}
	if replacing != "" {
		for i := range cf.Boxes {
			if cf.Boxes[i].Name == replacing {
				if cf.Current == replacing {
					cf.Current = box.Name
				}
				cf.Boxes[i] = box
				return config.SaveClientFile(cf)
			}
		}
	}
	cf.Boxes = append(cf.Boxes, box)
	if cf.Current == "" {
		cf.Current = box.Name
	}
	return config.SaveClientFile(cf)
}
```

Add `a`/`e` cases to the boxes view `Update`'s `tea.KeyMsg` switch (alongside `enter`):

```go
		case "a":
			boxes := v.boxes
			return v, func() tea.Msg { return pushMsg{newBoxForm(v.dial, boxes)} }
		case "e":
			if len(v.boxes) > 0 {
				boxes, orig := v.boxes, v.boxes[v.cursor]
				return v, func() tea.Msg { return pushMsg{newBoxFormEdit(v.dial, boxes, orig)} }
			}
```

- [ ] **Step 6: Create `internal/tui/boxform.go`**

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/config"
)

// boxFormView adds or edits a box: name, addr, and a masked token. On submit it
// validates locally, then verifies the box is reachable (dial + ListApps) before
// writing the config. Editing preserves the box's wizard-managed relay fields.
type boxFormView struct {
	dial     Dialer
	existing []config.Box // for duplicate-name checks
	orig     config.Box   // the box being edited; zero value for add
	editing  bool
	name     textinput.Model
	addr     textinput.Model
	token    textinput.Model
	focus    int // 0 name, 1 addr, 2 token
	err      error
}

func newBoxForm(dial Dialer, boxes []config.Box) boxFormView {
	name := textinput.New()
	name.Placeholder = "name"
	name.Focus()
	addr := textinput.New()
	addr.Placeholder = "host:8088"
	token := textinput.New()
	token.Placeholder = "token"
	token.EchoMode = textinput.EchoPassword
	return boxFormView{dial: dial, existing: boxes, name: name, addr: addr, token: token}
}

func newBoxFormEdit(dial Dialer, boxes []config.Box, orig config.Box) boxFormView {
	v := newBoxForm(dial, boxes)
	v.orig, v.editing = orig, true
	v.name.SetValue(orig.Name)
	v.addr.SetValue(orig.Addr)
	v.token.SetValue(orig.Token)
	return v
}

func (v boxFormView) Init() tea.Cmd { return nil }

func (v boxFormView) title() string { return "box" }

func (v boxFormView) refresh(API) tea.Cmd { return nil }

func (v boxFormView) capturesText() bool { return true }

func (v *boxFormView) applyFocus() {
	inputs := []*textinput.Model{&v.name, &v.addr, &v.token}
	for i, in := range inputs {
		if i == v.focus {
			in.Focus()
		} else {
			in.Blur()
		}
	}
}

// validate checks the name is present and unique (an edit may keep its own name)
// and the addr is present; it returns the assembled box or an error.
func (v boxFormView) validate() (config.Box, error) {
	name := strings.TrimSpace(v.name.Value())
	if name == "" {
		return config.Box{}, fmt.Errorf("name is required")
	}
	for _, b := range v.existing {
		if b.Name == name && !(v.editing && b.Name == v.orig.Name) {
			return config.Box{}, fmt.Errorf("a box named %q already exists", name)
		}
	}
	addr := strings.TrimSpace(v.addr.Value())
	if addr == "" {
		return config.Box{}, fmt.Errorf("addr is required")
	}
	// Preserve wizard-managed relay fields on edit.
	return config.Box{
		Name:              name,
		Addr:              addr,
		Token:             strings.TrimSpace(v.token.Value()),
		RelayAPI:          v.orig.RelayAPI,
		AccountCredential: v.orig.AccountCredential,
	}, nil
}

func (v boxFormView) submit() (tea.Model, tea.Cmd) {
	box, err := v.validate()
	if err != nil {
		v.err = err
		return v, nil
	}
	dial, replacing := v.dial, ""
	if v.editing {
		replacing = v.orig.Name
	}
	return v, func() tea.Msg {
		c, _, _, err := dial(box)
		if err == nil {
			_, err = c.ListApps()
		}
		if err != nil {
			return errMsg{err}
		}
		if err := saveBox(box, replacing); err != nil {
			return errMsg{err}
		}
		return boxSavedMsg{box: box}
	}
}

func (v boxFormView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "down":
			v.focus = (v.focus + 1) % 3
			v.applyFocus()
			return v, nil
		case "up":
			v.focus = (v.focus + 2) % 3
			v.applyFocus()
			return v, nil
		case "enter":
			return v.submit()
		}
	}
	// Any editing keystroke clears a stale validation banner.
	v.err = nil
	var cmd tea.Cmd
	switch v.focus {
	case 0:
		v.name, cmd = v.name.Update(msg)
	case 1:
		v.addr, cmd = v.addr.Update(msg)
	default:
		v.token, cmd = v.token.Update(msg)
	}
	return v, cmd
}

func (v boxFormView) View() string {
	var b strings.Builder
	title := "add box"
	if v.editing {
		title = "edit box"
	}
	fmt.Fprintf(&b, "  %s\n\n", title)
	fmt.Fprintf(&b, "  name   %s\n", v.name.View())
	fmt.Fprintf(&b, "  addr   %s\n", v.addr.View())
	fmt.Fprintf(&b, "  token  %s\n\n", v.token.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ verify & save   tab switch   esc cancel")
	return b.String()
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestBoxForm|TestBoxesKeyOpensForms|TestRootBoxSaved' -v`
Expected: PASS (all).

- [ ] **Step 8: Run the full package**

Run: `go test ./internal/tui/`
Expected: `ok  github.com/getpiper/piper/internal/tui`

- [ ] **Step 9: Commit**

```bash
git add internal/tui/tui.go internal/tui/app.go internal/tui/boxes.go internal/tui/boxform.go internal/tui/boxform_test.go
git commit -m "feat(cli): add/edit box form with verify-before-save to the TUI

Part of #183

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Remove a box (y/n confirm)

`x` on the boxes view opens a y/n confirm; on confirm the box is dropped from the config. Removing the current box promotes the first remaining box (and re-dials); removing the last box is refused.

**Files:**
- Modify: `internal/tui/tui.go` — add `removeBoxMsg` and `boxRemovedMsg`.
- Modify: `internal/tui/app.go` — handle both.
- Modify: `internal/tui/boxes.go` — add `removeBox` helper; wire `x`.
- Modify: `internal/tui/confirm.go` — add `newRemoveBoxConfirm`.
- Test: `internal/tui/boxes_test.go` — append.

**Interfaces:**
- Consumes: `confirmView` + `confirmYesNo` from `confirm.go`; `popMsg`, `errMsg`, `switchBoxMsg` from earlier.
- Produces:
  - `func removeBox(name string) (current config.Box, changed bool, err error)` (in `boxes.go`) — drops `name`; if it was current, returns the promoted box with `changed=true`; refuses the last box.
  - `func newRemoveBoxConfirm(name string) confirmView` (in `confirm.go`).
  - `removeBoxMsg struct{ name string }`, `boxRemovedMsg struct{ current config.Box; changed bool }` (in `tui.go`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/tui/boxes_test.go`:

```go
func TestXOpensRemoveConfirm(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4"}, {Name: "blog", Addr: "a"}}, current: "pi4"})
	v = vv.(boxesView)
	vv, _ = v.Update(keyRunes('j')) // move to blog
	v = vv.(boxesView)
	_, cmd := v.Update(keyRunes('x'))
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("x should push a confirm, got %T", cmd())
	}
	if _, ok := push.view.(confirmView); !ok {
		t.Fatalf("x should push confirmView, got %T", push.view)
	}
}

func TestRemoveBoxDropsIt(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4"}, {Name: "blog"}}, Current: "pi4"})
	current, changed, err := removeBox("blog")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatalf("removing a non-current box should not change current: %+v", current)
	}
	cf, _ := config.LoadClientFile()
	if len(cf.Boxes) != 1 || cf.Boxes[0].Name != "pi4" {
		t.Fatalf("blog not removed: %+v", cf.Boxes)
	}
}

func TestRemoveCurrentBoxPromotesFirst(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4"}, {Name: "blog"}}, Current: "pi4"})
	current, changed, err := removeBox("pi4")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || current.Name != "blog" {
		t.Fatalf("removing current should promote blog, got changed=%v current=%+v", changed, current)
	}
	cf, _ := config.LoadClientFile()
	if cf.Current != "blog" {
		t.Fatalf("current not promoted on disk: %q", cf.Current)
	}
}

func TestRemoveLastBoxRefused(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4"}}, Current: "pi4"})
	if _, _, err := removeBox("pi4"); err == nil {
		t.Fatal("removing the last box must be refused")
	}
	cf, _ := config.LoadClientFile()
	if len(cf.Boxes) != 1 {
		t.Fatalf("refused remove must not write: %+v", cf.Boxes)
	}
}

func TestRootBoxRemovedNonCurrentPopsToBoxes(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(pushMsg{newRemoveBoxConfirm("blog")})
	m = m2.(Model)
	if len(m.stack) != 3 {
		t.Fatalf("setup: want depth 3, got %d", len(m.stack))
	}
	m2, _ = m.Update(boxRemovedMsg{changed: false})
	m = m2.(Model)
	if len(m.stack) != 2 {
		t.Fatalf("removed (non-current) should pop to boxes (depth 2), got %d", len(m.stack))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestXOpensRemove|TestRemoveBox|TestRemoveCurrent|TestRemoveLast|TestRootBoxRemoved' -v`
Expected: FAIL — `undefined: removeBox`, `newRemoveBoxConfirm`, `removeBoxMsg`, `boxRemovedMsg`.

- [ ] **Step 3: Add the messages in `tui.go`**

After `boxSavedMsg`, add:

```go
	// removeBoxMsg is the remove confirm's intent; the root drops the box.
	removeBoxMsg struct{ name string }

	// boxRemovedMsg is a successful removal. If it changed the current box the
	// root re-dials the promoted one; otherwise it pops back to the boxes view.
	boxRemovedMsg struct {
		current config.Box
		changed bool
	}
```

- [ ] **Step 4: Add the handlers in `app.go`**

Add these cases in `Update` (after the `boxSavedMsg` case):

```go
	case removeBoxMsg:
		name := msg.name
		return m, func() tea.Msg {
			current, changed, err := removeBox(name)
			if err != nil {
				return errMsg{err}
			}
			return boxRemovedMsg{current: current, changed: changed}
		}
	case boxRemovedMsg:
		if msg.changed {
			return m.Update(switchBoxMsg{box: msg.current}) // current removed: switch to the promoted box
		}
		m = m.popN(1)
		return m, m.refresh()
```

Note: on a refused/failed removal the cmd returns `errMsg`, which the root's existing default path forwards to the top view (the confirm), where it banners — no extra handling needed.

- [ ] **Step 5: Add the `removeBox` helper and `x` wiring in `boxes.go`**

Add `removeBox` after `saveBox`:

```go
// removeBox drops the box named name from the client config. If it was the
// current box, the first remaining box is promoted and returned with
// changed=true. Removing the last box is refused (the CLI always needs one).
func removeBox(name string) (current config.Box, changed bool, err error) {
	cf, err := config.LoadClientFile()
	if err != nil {
		return config.Box{}, false, err
	}
	if len(cf.Boxes) <= 1 {
		return config.Box{}, false, fmt.Errorf("can't remove the last box")
	}
	kept := cf.Boxes[:0]
	for _, b := range cf.Boxes {
		if b.Name != name {
			kept = append(kept, b)
		}
	}
	cf.Boxes = kept
	if cf.Current == name {
		cf.Current = cf.Boxes[0].Name
		current, changed = cf.Boxes[0], true
	}
	return current, changed, config.SaveClientFile(cf)
}
```

Add the `x` case to the boxes view `Update`'s `tea.KeyMsg` switch (alongside `a`/`e`):

```go
		case "x":
			if len(v.boxes) > 0 {
				name := v.boxes[v.cursor].Name
				return v, func() tea.Msg { return pushMsg{newRemoveBoxConfirm(name)} }
			}
```

- [ ] **Step 6: Add `newRemoveBoxConfirm` in `confirm.go`**

After `newStopConfirm` (around `confirm.go:39`), add:

```go
func newRemoveBoxConfirm(name string) confirmView {
	return confirmView{
		name:   name,
		prompt: fmt.Sprintf("Remove box %s? Its saved credentials are deleted from this machine.", name),
		mode:   confirmYesNo,
		intent: func(n string) tea.Msg { return removeBoxMsg{n} },
	}
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestXOpensRemove|TestRemoveBox|TestRemoveCurrent|TestRemoveLast|TestRootBoxRemoved' -v`
Expected: PASS (all).

- [ ] **Step 8: Run the full package**

Run: `go test ./internal/tui/`
Expected: `ok  github.com/getpiper/piper/internal/tui`

- [ ] **Step 9: Commit**

```bash
git add internal/tui/tui.go internal/tui/app.go internal/tui/boxes.go internal/tui/confirm.go internal/tui/boxes_test.go
git commit -m "feat(cli): remove a box from the TUI boxes view with a y/n confirm

Part of #183

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Help overlay rows, PROGRESS.md, and verify gate

Add the Boxes keymap to the `?` overlay and `t boxes` to its Global line, record the phase in `PROGRESS.md`, and run the full CI-mirroring gate.

**Files:**
- Modify: `internal/tui/help.go` — add `t boxes` to Global; add a Boxes row.
- Modify: `internal/tui/help_test.go` — assert the new keymap text.
- Modify: `PROGRESS.md` — one line referencing the phase-5 issue.

**Interfaces:**
- Consumes: the existing `helpView` from `help.go`.
- Produces: no new exported symbols; `helpView.View()` gains `t boxes` (Global) and a `Boxes` row (`connect`, `add`, `edit`, `remove`).

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/help_test.go`:

```go
func TestHelpOverlayIncludesBoxesKeymap(t *testing.T) {
	out := helpView{}.View()
	for _, want := range []string{"t boxes", "connect", "add", "edit", "remove"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help overlay missing boxes keymap %q:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestHelpOverlayIncludesBoxesKeymap' -v`
Expected: FAIL — `t boxes`/`connect` absent.

- [ ] **Step 3: Add the keymap rows in `help.go`**

Replace the `View()` body in `internal/tui/help.go` with:

```go
func (helpView) View() string {
	return "  Global      esc back/cancel · q quit (root) / back · r refresh · t boxes · ? help · ctrl+c quit\n" +
		"  Apps list   ↑/k ↓/j move · enter open · n new app\n" +
		"  App detail  ↑/k ↓/j move · enter logs · d deploy · s stop · x delete\n" +
		"  Logs        f toggle follow · esc back\n" +
		"  Boxes       ↑/k ↓/j move · enter connect · a add · e edit · x remove\n\n" +
		"  esc back"
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/tui/ -run 'TestHelpOverlayIncludesBoxesKeymap|TestQuestionMark' -v`
Expected: PASS (the new test and the existing `?`-overlay tests).

- [ ] **Step 5: Record progress in PROGRESS.md**

Run: `grep -n "#183\|#194\|Key discoverability" PROGRESS.md`
Expected: shows the existing TUI phase lines to append near.

Add a terse one-line entry beside the other TUI phase entries (matching the file's `[#N]`/`— #N` style), e.g.:

```
- ✅ Boxes view: switcher + add/edit/remove config editor over schema v2 — [#N](https://github.com/getpiper/piper/issues/N)
```

Replace `#N` with the phase-5 issue number once it is opened (the controller opens it at execution time; if unknown, use `#183` and note the phase). Keep it one line — detail lives in the issue.

- [ ] **Step 6: Run the full verify gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests pass, cross-compile succeeds. If gofmt flags files, run `make fmt` and re-run `make verify`.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/help.go internal/tui/help_test.go PROGRESS.md
git commit -m "docs: record TUI boxes view; add boxes keymap to help overlay

Part of #183

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:**
- Injected `Dialer` factory, LAN client construction, `NewModel`/`Run` gain the dialer → Task 1 (Steps 3–4, 7). ✓
- Relay-box switching out of scope; relay boxes listed but decline `↵` and render `—` → Task 1 (`isRelay`, `status`) + Task 2 (probes skip relay). ✓
- `boxes.go` table, current marker, keys `↵/a/e/x/↑k↓j/esc`, footer legend → Tasks 1 (list/switch/footer), 3 (`a`/`e`), 4 (`x`). ✓
- Per-box async reachability probes on every refresh, `●/○/…`, current shows `current` → Task 2. ✓
- `boxform.go` name/addr/token (masked), verify via authenticated `ListApps` probe before save, relay fields round-trip, edit re-dials current → Task 3. ✓
- Remove via y/n `confirmView`; promote first on current removal; refuse last box → Task 4. ✓
- Switch semantics: `switchBoxMsg{box}`, dial, swap client/box/addr/remote, reset stack to fresh apps, banner on failure → Task 1 (Step 4 handler + failure test). ✓
- `t` handled at root as a global key with an already-on-top guard, in the `!topCapturesText()` block → Task 1 (Step 4) + `TestTDoesNotStackBoxes`. ✓
- `?` help overlay gains a Boxes row + `t boxes` in Global → Task 5. ✓
- `tui` imports `internal/config` (down-dep) for the `Box` type; never imports `internal/client` → Tasks 1–4 (config import only). ✓
- PROGRESS.md update → Task 5. ✓

**Placeholder scan:** No TBD/TODO; every code step shows full code. The one deferred value is the phase-5 issue number in PROGRESS.md (Task 5 Step 5), which the controller fills when it opens the issue — flagged explicitly, not a silent gap. ✓

**Type consistency:**
- `Dialer` signature `func(config.Box)(API, string, bool, error)` is identical in `tui.go`, `WithDialer`/`Run`/`switchBoxMsg` handler (`app.go`), `newBoxesView`/`probe` (`boxes.go`), `newBoxForm`/`submit` (`boxform.go`), and `dialBox` (`cmd/piper`). ✓
- `boxesLoadedMsg{boxes []config.Box; current string}`, `boxProbeMsg{name string; reachable bool}`, `switchBoxMsg{box config.Box}`, `boxSavedMsg{box config.Box}`, `removeBoxMsg{name string}`, `boxRemovedMsg{current config.Box; changed bool}` — declared once in `tui.go`, consumed with matching fields everywhere. ✓
- `saveBox(box config.Box, replacing string) error` and `removeBox(name string)(config.Box, bool, error)` — signatures match their call sites in `boxform.go`/`app.go`. ✓
- `newBoxesView`/`newBoxForm`/`newBoxFormEdit`/`newRemoveBoxConfirm` return types match the `pushMsg{view}` and type-assertion uses in tests. ✓
- Reuses existing helpers (`keyRunes`, `keyEnter`, `pump`, `fakeAPI`, `newAppsView`, `confirmView`, `confirmYesNo`) verified against the current source. ✓

**Note on `tea.BatchMsg` (Task 2 test):** `tea.Batch(cmds...)` returns a cmd that yields a `tea.BatchMsg` (a `[]tea.Cmd`); the test asserts its length equals the number of probed boxes. This matches Bubble Tea's public `BatchMsg` type. ✓
