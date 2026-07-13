# Piper TUI — Phase 4 (actions) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the TUI read-write — create an app, deploy (confirm → live build), stop, and delete — each with a proportional guard, per the approved design ([`docs/superpowers/specs/2026-07-13-tui-phase4-actions-design.md`](../specs/2026-07-13-tui-phase4-actions-design.md)).

**Architecture:** Stay on the phase-2/3 `view` stack. A mutating view emits a small **intent message** (`createAppMsg`, `stopAppMsg`, `deleteAppMsg`, `deployMsg`); the root — which owns the client — runs the call off the UI thread and reports the outcome as `actionResultMsg` (pop N + refresh, or banner) or, for deploy, `deployStartedMsg` (replace the confirm with a phase-3 follow `logsView`). Three new pushed views — `formView` (new app), `confirmView` (stop/delete), `deployView` (deploy confirm) — plus key triggers on the existing apps/detail views.

**Tech Stack:** Go, `charmbracelet/bubbletea` v1 + `lipgloss` v1 + `bubbles` v1 (all already present). This phase newly imports `bubbles/textinput` (same module — no new direct dep, but `go mod tidy` pulls its transitive `github.com/atotto/clipboard`).

## Global Constraints

- `CGO_ENABLED=0` everywhere; `make cross` (linux/arm64) must stay green. (`atotto/clipboard` is pure Go — safe.)
- Module `github.com/getpiper/piper`; nothing imports "up": `internal/tui` imports `internal/api` + `internal/store` + charmbracelet libs + stdlib only — never `internal/client`. `cmd/piper` imports `internal/tui`.
- `bubbles/textinput` is pinned to the **v1** major already in use (bubbles v1.0.0, present since phase 3). Do **not** bump bubbles/bubbletea/lipgloss.
- Deployment status strings are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`; `""` means never deployed → `—`.
- **No agent or `internal/client` changes.** `*client.Client` already implements `CreateApp(name string, port int) error`, `Deploy(name, srcDir string) (store.Deployment, error)`, `StopApp(name string) error`, `DeleteApp(name string) error` — this phase only widens the TUI's `API` interface to include them.
- Deploy always ships the launch **cwd**; the deploy kickoff runs on the TUI's existing 5s-capped client (a very large build context could time out — deferred known limitation, out of scope).
- Read-write, but scoped: **only** new-app, deploy, stop, delete. No link-repo (phase 6), no bulk actions, no auto "offer deploy" after create.
- TDD: every task writes the failing test first. Run `make verify` before pushing.
- One commit per task, conventional-commit style, ending with:
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`
- Trunk-based: never commit to `main`; one PR into `main` for this phase, squash-merged. Work proceeds on branch `ozykhan/tui-phase4-actions` (already holds the spec).

**Reference — current shapes (already in the codebase, verified on this branch):**
- `internal/tui/tui.go` defines `type API interface { ListApps; App; Deployments; DeploymentLogs }`, the message block, and `pollResult` (implemented by `appsLoadedMsg`/`errMsg`/`appDetailLoadedMsg`/`logsLoadedMsg`).
- `internal/tui/app.go` `Model` holds `stack []view`, `width,height int`; `Update` has cases for `tea.KeyMsg` (ctrl+c/q/esc/r), `tickMsg`, `pushMsg` (seeds the pushed view with the window size when `width>0`), `tea.WindowSizeMsg` (stores size, falls through); then a `pollResult` check and forward-to-top.
- `appsView` (`apps.go`) handles `up/k`, `down/j`, `enter`→`pushMsg{newAppDetailView(name, remote)}`; has `cursor int`, `count() int`.
- `appDetailView` (`appdetail.go`) handles `up/k`, `down/j`, `enter`→`pushMsg{newLogsView(name, id, status)}`; has `name`, `remote`, `cursor`.
- `logsView` (`logs.go`) `newLogsView(app, id, status string) logsView` — follow view; `follow = status=="building"`; needs a `tea.WindowSizeMsg` to become `ready`.
- `app_test.go` has `type fakeAPI struct{...}` (value-receiver methods), `keyRunes(r rune)`, `keyEnter()`, `pump(t, m, cmd)`. `apps_test.go` has `fixtureApps()` (blog/shop/new).

---

### Task 1: Phase-4 tracker + plan commit

**Files:** the plan doc (already written); GitHub issue.

**Interfaces:**
- Produces: phase-4 child issue number `#P4` — used in every later commit/PR body. Record it in the plan-execution notes.

- [ ] **Step 1: Create the phase-4 child issue** under epic #183

```bash
gh issue create --title "[cli] TUI phase 4: actions (new app, deploy, stop, delete)" \
  --label cli --label enhancement --label P3 --label "size/M" \
  --body "Phase 4 of the TUI epic #183. Read-write actions: new-app form, deploy (confirm → live build via the phase-3 follow view), stop confirm (y/n), delete confirm (type-the-name). Design: docs/superpowers/specs/2026-07-13-tui-phase4-actions-design.md."
```

- [ ] **Step 2: Commit the plan** (the spec is already committed on this branch)

```bash
git add docs/superpowers/plans/2026-07-13-tui-phase4-actions.md
git commit -m "docs: add TUI phase-4 actions implementation plan

Part of #183

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Action plumbing — widen `API`, intent + result messages, root handlers

Add the read-write seam: widen the `API` interface, extend `fakeAPI` to record calls, add the create/stop/delete intent messages + `actionResultMsg` + `popMsg`, and the root handlers that run the client call and pop/banner. No new views yet — nothing emits these until Tasks 3–4, so this task tests the root plumbing by feeding the messages directly.

**Files:**
- Modify: `internal/tui/tui.go` (widen `API`; add messages)
- Modify: `internal/tui/app.go` (root: intent→client→result handlers, `popMsg`, `popN`)
- Modify: `internal/tui/app_test.go` (extend `fakeAPI` with a recorder + error fields)
- Test: `internal/tui/app_test.go`

**Interfaces:**
- Produces (used by Tasks 3–5):
  - `API` gains `CreateApp(name string, port int) error`, `Deploy(name, srcDir string) (store.Deployment, error)`, `StopApp(name string) error`, `DeleteApp(name string) error`.
  - `createAppMsg{ name string; port int }`, `stopAppMsg{ name string }`, `deleteAppMsg{ name string }` — intents a view emits; the root runs the call.
  - `actionResultMsg{ err error; popLevels int }` — success pops `popLevels`, error banners the top view.
  - `popMsg{ n int }` — pop `n` views.
  - `Model.topCapturesText() bool` + the optional interface `interface{ capturesText() bool }` — a view implementing `capturesText()` and returning true suppresses the root's `q`/`r` shortcuts (Tasks 3–4 implement it).
  - `fakeAPI` gains a `*apiCalls` recorder + `createErr/stopErr/deleteErr/deployErr error` + `deployDep store.Deployment`.

- [ ] **Step 1: Extend `fakeAPI` (failing — new methods + fields)** — in `internal/tui/app_test.go`, replace the `fakeAPI` block (the struct + its four methods) with:

```go
// apiCalls records the mutating calls a test drives, so assertions can check
// the TUI passed the right arguments through to the client.
type apiCalls struct {
	createName string
	createPort int
	deployName string
	deployDir  string
	stopped    string
	deleted    string
}

type fakeAPI struct {
	apps []api.App
	err  error
	app  api.App
	deps []store.Deployment
	logs string

	rec       *apiCalls
	deployDep store.Deployment
	createErr error
	deployErr error
	stopErr   error
	deleteErr error
}

func (f fakeAPI) ListApps() ([]api.App, error)                   { return f.apps, f.err }
func (f fakeAPI) App(string) (api.App, error)                    { return f.app, f.err }
func (f fakeAPI) Deployments(string) ([]store.Deployment, error) { return f.deps, f.err }
func (f fakeAPI) DeploymentLogs(string, string) (string, error)  { return f.logs, f.err }

func (f fakeAPI) CreateApp(name string, port int) error {
	if f.rec != nil {
		f.rec.createName, f.rec.createPort = name, port
	}
	return f.createErr
}

func (f fakeAPI) Deploy(name, srcDir string) (store.Deployment, error) {
	if f.rec != nil {
		f.rec.deployName, f.rec.deployDir = name, srcDir
	}
	return f.deployDep, f.deployErr
}

func (f fakeAPI) StopApp(name string) error {
	if f.rec != nil {
		f.rec.stopped = name
	}
	return f.stopErr
}

func (f fakeAPI) DeleteApp(name string) error {
	if f.rec != nil {
		f.rec.deleted = name
	}
	return f.deleteErr
}
```

Add the root-plumbing tests to `internal/tui/app_test.go`:

```go
func TestRootRunsCreateStopDeleteIntents(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("b", "a", false, fakeAPI{rec: rec})

	_, cmd := m.Update(createAppMsg{name: "blog", port: 9000})
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil || res.popLevels != 1 {
		t.Fatalf("create: want actionResultMsg{nil,1}, got %#v (ok=%v)", cmd(), ok)
	}
	if rec.createName != "blog" || rec.createPort != 9000 {
		t.Fatalf("create not passed through: %+v", rec)
	}

	_, cmd = m.Update(stopAppMsg{name: "blog"})
	if res := cmd().(actionResultMsg); res.popLevels != 1 || rec.stopped != "blog" {
		t.Fatalf("stop: got %#v rec=%+v", res, rec)
	}

	_, cmd = m.Update(deleteAppMsg{name: "blog"})
	if res := cmd().(actionResultMsg); res.popLevels != 2 || rec.deleted != "blog" {
		t.Fatalf("delete: want popLevels 2, got %#v rec=%+v", res, rec)
	}
}

func TestRootActionResultSuccessPopsAndErrorBanners(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	// stack: apps -> detail -> detail (simulate depth 3)
	m2, _ := m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	m2, _ = m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	if len(m.stack) != 3 {
		t.Fatalf("setup: want depth 3, got %d", len(m.stack))
	}

	// success pops popLevels and does not exceed the root
	m2, _ = m.Update(actionResultMsg{err: nil, popLevels: 2})
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("success: want popped to root (1), got %d", len(m.stack))
	}

	// error keeps the stack and banners the top view
	m2, _ = m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	m2, _ = m.Update(actionResultMsg{err: errors.New("name taken")})
	m = m2.(Model)
	if len(m.stack) != 2 {
		t.Fatalf("error must not pop: got depth %d", len(m.stack))
	}
	if !strings.Contains(m.View(), "name taken") {
		t.Fatalf("error banner missing:\n%s", m.View())
	}
}

func TestRootPopMsg(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	m2, _ := m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	m2, _ = m.Update(popMsg{1})
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("popMsg{1} should return to root, got depth %d", len(m.stack))
	}
	// popN never removes the root
	m2, _ = m.Update(popMsg{5})
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("popMsg over-pop must keep root, got depth %d", len(m.stack))
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ 2>&1 | head`
Expected: FAIL to compile — `undefined: createAppMsg`, `actionResultMsg`, `popMsg`, and `fakeAPI` missing `CreateApp` (the interface isn't widened yet).

- [ ] **Step 3: Widen `API` and add the messages in `tui.go`** — replace the `API` interface block and append to the message `type ( … )` block:

```go
// API is the slice of the piperd control API the TUI consumes.
// *client.Client satisfies it; tests inject fakes.
type API interface {
	ListApps() ([]api.App, error)
	App(name string) (api.App, error)
	Deployments(name string) ([]store.Deployment, error)
	DeploymentLogs(name, id string) (string, error)
	CreateApp(name string, port int) error
	Deploy(name, srcDir string) (store.Deployment, error)
	StopApp(name string) error
	DeleteApp(name string) error
}
```

Add to the `type ( … )` message block (after `logsLoadedMsg`):

```go
	// Action intents: a mutating view emits one of these; the root owns the
	// client, runs the call off the UI thread, and reports the outcome.
	createAppMsg struct {
		name string
		port int
	}
	stopAppMsg   struct{ name string }
	deleteAppMsg struct{ name string }

	// actionResultMsg is a mutating action's outcome. On success the root pops
	// popLevels views and refreshes; on error it banners the top overlay.
	actionResultMsg struct {
		err       error
		popLevels int
	}

	// popMsg pops n views off the stack (e.g. a y/n confirm answered "no").
	popMsg struct{ n int }
```

(These intentionally do **not** implement `pollResult`: an action's outcome must not flip the connectivity indicator.)

- [ ] **Step 4: Stop the root from swallowing letters a form needs** — the root intercepts `q` (quit/pop) and `r` (refresh) as global shortcuts, but those are letters the new-app name and delete type-name fields must receive. Gate them behind a text-capturing check so a form on top gets its keystrokes. Replace the `case tea.KeyMsg:` block in `app.go`'s `Update` with:

```go
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			if len(m.stack) > 1 {
				m.stack = m.stack[:len(m.stack)-1]
				return m, m.refresh()
			}
			return m, nil
		}
		if !m.topCapturesText() {
			switch msg.String() {
			case "q":
				if len(m.stack) == 1 {
					return m, tea.Quit
				}
				m.stack = m.stack[:len(m.stack)-1]
				return m, m.refresh()
			case "r":
				return m, m.refresh()
			}
		}
```

Add the helper (next to `top`). It uses an **optional interface**, so only the text-capturing views (Tasks 3–4) implement `capturesText`; the phase-3 views need no change:

```go
// topCapturesText reports whether the top view wants raw keystrokes (a text
// field), so the root suppresses its single-letter shortcuts (q, r) for it.
func (m Model) topCapturesText() bool {
	if c, ok := m.top().(interface{ capturesText() bool }); ok {
		return c.capturesText()
	}
	return false
}
```

(`ctrl+c` and `esc` stay global — neither is a text character; `esc` doubles as cancel for forms/confirms.)

- [ ] **Step 5: Add the action handlers + `popN` in `app.go`** — inside `Update`'s `switch msg := msg.(type)`, add these cases immediately after the existing `case pushMsg:` block (before `case tea.WindowSizeMsg:`):

```go
	case createAppMsg:
		name, port, c := msg.name, msg.port, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.CreateApp(name, port), popLevels: 1} }
	case stopAppMsg:
		name, c := msg.name, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.StopApp(name), popLevels: 1} }
	case deleteAppMsg:
		name, c := msg.name, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.DeleteApp(name), popLevels: 2} }
	case actionResultMsg:
		if msg.err != nil {
			next, _ := m.top().Update(errMsg{msg.err})
			m.stack[len(m.stack)-1] = next.(view)
			return m, nil
		}
		m = m.popN(msg.popLevels)
		return m, m.refresh()
	case popMsg:
		m = m.popN(msg.n)
		return m, m.refresh()
```

Add the helper (next to `top`):

```go
// popN drops n views off the top of the stack without ever removing the root.
func (m Model) popN(n int) Model {
	if n > len(m.stack)-1 {
		n = len(m.stack) - 1
	}
	m.stack = m.stack[:len(m.stack)-n]
	return m
}
```

- [ ] **Step 6: Run to verify pass**

Run: `go test ./internal/tui/ -run 'TestRoot|TestModel' -v 2>&1 | tail -25` then `go test ./internal/tui/`
Expected: all PASS (including the unchanged `TestModelQuitKeys` — `q` at the root still quits, since `appsView` doesn't capture text).

- [ ] **Step 7: Full gate + commit**

Run: `gofmt -l internal/tui/` (clean), `go vet ./internal/tui/`, `make cross`.

```bash
git add internal/tui/tui.go internal/tui/app.go internal/tui/app_test.go
git commit -m "feat(cli): add TUI action plumbing — intents, results, pop

Widens the TUI API interface with the four mutating client methods and
adds the intent/result message seam (createAppMsg/stopAppMsg/deleteAppMsg
→ actionResultMsg) plus popMsg, handled in the root which owns the client.

Part of #P4

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: New-app form (`formView`) + apps `n` trigger

Add the depth-1 `formView` (name + port textinputs) and wire `n` on the apps view to open it. Submitting emits `createAppMsg`; the root (Task 2) runs `CreateApp` and pops back on success.

**Files:**
- Create: `internal/tui/form.go`
- Modify: `internal/tui/apps.go` (add `n` → push form)
- Modify: `go.mod`, `go.sum` (`go mod tidy` pulls `textinput`'s transitive `atotto/clipboard`)
- Test: `internal/tui/form_test.go`, `internal/tui/apps_test.go`

**Interfaces:**
- Consumes: Task 2's `createAppMsg`, `actionResultMsg`; `pushMsg`.
- Produces: `func newFormView() formView` (implements `view`, `title()=="new app"`); apps `n` emits `pushMsg{newFormView()}`.

- [ ] **Step 1: Write the failing tests** — `internal/tui/form_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

func typeInto(v formView, s string) formView {
	for _, r := range s {
		m, _ := v.Update(keyRunes(r))
		v = m.(formView)
	}
	return v
}

func TestFormRendersFieldsWithPortDefault(t *testing.T) {
	out := newFormView().View()
	for _, want := range []string{"new app", "name", "port", "8080", "create"} {
		if !strings.Contains(out, want) {
			t.Fatalf("form view missing %q:\n%s", want, out)
		}
	}
}

func TestFormSubmitEmitsCreateIntent(t *testing.T) {
	v := typeInto(newFormView(), "blog")
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("valid submit should emit a command")
	}
	msg, ok := cmd().(createAppMsg)
	if !ok {
		t.Fatalf("want createAppMsg, got %T", cmd())
	}
	if msg.name != "blog" || msg.port != 8080 {
		t.Fatalf("want {blog,8080}, got %+v", msg)
	}
}

func TestFormValidationBanners(t *testing.T) {
	// empty name
	v := newFormView()
	m, cmd := v.Update(keyEnter())
	if cmd != nil {
		t.Fatal("empty name must not submit")
	}
	if !strings.Contains(m.(formView).View(), "name") {
		t.Fatalf("want name-required banner:\n%s", m.(formView).View())
	}
	// bad port: clear the 8080 default and type letters
	v = newFormView()
	m2, _ := v.Update(keyTab()) // focus port
	v = m2.(formView)
	// wipe default then type non-numeric
	for range "8080" {
		mm, _ := v.Update(keyBackspace())
		v = mm.(formView)
	}
	v = typeInto(v, " abc")
	m3, cmd := v.Update(keyEnter())
	if cmd != nil {
		t.Fatal("bad port must not submit")
	}
	if !strings.Contains(m3.(formView).View(), "port") {
		t.Fatalf("want port banner:\n%s", m3.(formView).View())
	}
}
```

Add backspace + tab key helpers to `internal/tui/app_test.go` (next to `keyRunes`/`keyEnter`):

```go
func keyBackspace() tea.KeyMsg { return tea.KeyMsg(tea.Key{Type: tea.KeyBackspace}) }
func keyTab() tea.KeyMsg       { return tea.KeyMsg(tea.Key{Type: tea.KeyTab}) }
```

Add a root-level test to `internal/tui/form_test.go` proving `q`/`r` reach the field (not the root shortcuts) — this exercises `topCapturesText` end-to-end:

```go
func TestFormCapturesLetterShortcuts(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	m2, _ := m.Update(pushMsg{newFormView()})
	m = m2.(Model)
	// "qr" are the root's quit/refresh shortcuts; the form must receive them
	for _, r := range "qr" {
		m2, _ = m.Update(keyRunes(r))
		m = m2.(Model)
	}
	if len(m.stack) != 2 {
		t.Fatalf("q/r must not pop the form; stack depth %d", len(m.stack))
	}
	_, cmd := m.Update(keyEnter())
	if msg, ok := cmd().(createAppMsg); !ok || msg.name != "qr" {
		t.Fatalf("form should have captured \"qr\" as the name, got %#v", cmd())
	}
}
```

Add the apps-`n` trigger test to `internal/tui/apps_test.go`:

```go
func TestAppsViewNKeyPushesForm(t *testing.T) {
	m, _ := newAppsView(false).Update(appsLoadedMsg{apps: fixtureApps()})
	_, cmd := m.Update(keyRunes('n'))
	if cmd == nil {
		t.Fatal("n should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "new app" {
		t.Fatalf("want the new-app form, got title %q", pm.view.title())
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ 2>&1 | head`
Expected: FAIL — `undefined: newFormView`, `keyBackspace` (once added, then) `formView` undefined.

- [ ] **Step 3: Create `internal/tui/form.go`**:

```go
package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// formView is the depth-1 new-app form: a name field and a port field
// (default 8080). On a valid submit it emits createAppMsg; the root runs the
// create and pops back to the apps list. esc cancels (the root pops).
type formView struct {
	name  textinput.Model
	port  textinput.Model
	focus int // 0 = name, 1 = port
	err   error
}

func newFormView() formView {
	name := textinput.New()
	name.Placeholder = "name"
	name.Focus()
	port := textinput.New()
	port.Placeholder = "8080"
	port.SetValue("8080")
	return formView{name: name, port: port}
}

func (v formView) Init() tea.Cmd { return nil }

func (v formView) title() string { return "new app" }

func (v formView) refresh(API) tea.Cmd { return nil }

// capturesText tells the root to hand this view every keystroke (including q
// and r), so the name/port fields receive them instead of the root shortcuts.
func (v formView) capturesText() bool { return true }

func (v *formView) applyFocus() {
	if v.focus == 0 {
		v.name.Focus()
		v.port.Blur()
	} else {
		v.port.Focus()
		v.name.Blur()
	}
}

func (v formView) submit() (tea.Model, tea.Cmd) {
	name := strings.TrimSpace(v.name.Value())
	if name == "" {
		v.err = fmt.Errorf("name is required")
		return v, nil
	}
	port, err := strconv.Atoi(strings.TrimSpace(v.port.Value()))
	if err != nil || port < 1 || port > 65535 {
		v.err = fmt.Errorf("port must be a number 1–65535")
		return v, nil
	}
	return v, func() tea.Msg { return createAppMsg{name: name, port: port} }
}

func (v formView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "up", "down":
			v.focus = (v.focus + 1) % 2
			v.applyFocus()
			return v, nil
		case "enter":
			return v.submit()
		}
	}
	var cmd tea.Cmd
	if v.focus == 0 {
		v.name, cmd = v.name.Update(msg)
	} else {
		v.port, cmd = v.port.Update(msg)
	}
	return v, cmd
}

func (v formView) View() string {
	var b strings.Builder
	b.WriteString("  new app\n\n")
	fmt.Fprintf(&b, "  name  %s\n", v.name.View())
	fmt.Fprintf(&b, "  port  %s\n\n", v.port.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ create   tab switch   esc cancel")
	return b.String()
}
```

- [ ] **Step 4: Add the `n` trigger to `apps.go`** — in `appsView.Update`'s `tea.KeyMsg` switch, add a case after `"enter"`:

```go
		case "n":
			return v, func() tea.Msg { return pushMsg{newFormView()} }
```

- [ ] **Step 5: Tidy modules** (textinput pulls `atotto/clipboard`)

Run: `go mod tidy`
Then confirm `internal/tui` builds: `go build ./internal/tui/`
Expected: `go.mod`/`go.sum` gain `github.com/atotto/clipboard` (indirect); build succeeds.

- [ ] **Step 6: Run to verify pass**

Run: `go test ./internal/tui/ -v 2>&1 | tail -30`
Expected: all PASS.

- [ ] **Step 7: Full gate + commit**

Run: `gofmt -l internal/tui/` (clean), `go vet ./internal/tui/`, `make cross`.

```bash
git add go.mod go.sum internal/tui/form.go internal/tui/form_test.go internal/tui/apps.go internal/tui/apps_test.go internal/tui/app_test.go
git commit -m "feat(cli): add new-app form to the TUI

Part of #P4

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Stop + delete confirms (`confirmView`) + app-detail `s`/`x` triggers

Add the modal `confirmView` — y/n for stop, type-the-app-name for delete — and wire `s`/`x` on the app-detail view. Confirming emits `stopAppMsg`/`deleteAppMsg`; answering "no" emits `popMsg{1}`.

**Files:**
- Create: `internal/tui/confirm.go`
- Modify: `internal/tui/appdetail.go` (add `s`/`x` → push confirm)
- Test: `internal/tui/confirm_test.go`, `internal/tui/appdetail_test.go`

**Interfaces:**
- Consumes: Task 2's `stopAppMsg`, `deleteAppMsg`, `popMsg`; `pushMsg`.
- Produces: `func newStopConfirm(name string) confirmView`, `func newDeleteConfirm(name string) confirmView` (both implement `view`, `title()=="confirm"`).

- [ ] **Step 1: Write the failing tests** — `internal/tui/confirm_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

func TestStopConfirmYesEmitsStop(t *testing.T) {
	v := newStopConfirm("blog")
	if !strings.Contains(v.View(), "Stop blog") {
		t.Fatalf("prompt missing:\n%s", v.View())
	}
	_, cmd := v.Update(keyRunes('y'))
	if _, ok := cmd().(stopAppMsg); !ok {
		t.Fatalf("y should emit stopAppMsg, got %T", cmd())
	}
}

func TestStopConfirmNoPops(t *testing.T) {
	_, cmd := newStopConfirm("blog").Update(keyRunes('n'))
	if cmd == nil {
		t.Fatal("n should emit a command")
	}
	if pm, ok := cmd().(popMsg); !ok || pm.n != 1 {
		t.Fatalf("n should emit popMsg{1}, got %#v", cmd())
	}
}

func TestDeleteConfirmGatesOnTypedName(t *testing.T) {
	v := newDeleteConfirm("blog")
	// wrong name: enter does not delete, shows mismatch
	wrong := typeInto2(v, "bloop")
	m, cmd := wrong.Update(keyEnter())
	if cmd != nil {
		t.Fatal("mismatched name must not delete")
	}
	if !strings.Contains(m.(confirmView).View(), "match") {
		t.Fatalf("want mismatch banner:\n%s", m.(confirmView).View())
	}
	// exact name: enter deletes
	right := typeInto2(newDeleteConfirm("blog"), "blog")
	_, cmd = right.Update(keyEnter())
	if _, ok := cmd().(deleteAppMsg); !ok {
		t.Fatalf("exact name should emit deleteAppMsg, got %v", cmd())
	}
}

// typeInto2 feeds each rune of s into a confirmView's text input.
func typeInto2(v confirmView, s string) confirmView {
	for _, r := range s {
		m, _ := v.Update(keyRunes(r))
		v = m.(confirmView)
	}
	return v
}
```

Add the app-detail trigger tests to `internal/tui/appdetail_test.go`:

```go
func TestAppDetailStopAndDeleteKeysPushConfirm(t *testing.T) {
	base := newAppDetailView("blog", false)
	for _, tc := range []struct {
		key  rune
		want string // substring the confirm prompt must contain
	}{
		{'s', "Stop blog"},
		{'x', "Delete blog"},
	} {
		_, cmd := base.Update(keyRunes(tc.key))
		if cmd == nil {
			t.Fatalf("%c should emit a push command", tc.key)
		}
		pm, ok := cmd().(pushMsg)
		if !ok {
			t.Fatalf("%c: want pushMsg, got %T", tc.key, cmd())
		}
		if pm.view.title() != "confirm" || !strings.Contains(pm.view.View(), tc.want) {
			t.Fatalf("%c: wrong confirm view: title=%q view=%s", tc.key, pm.view.title(), pm.view.View())
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ 2>&1 | head`
Expected: FAIL — `undefined: newStopConfirm`, `newDeleteConfirm`, `confirmView`.

- [ ] **Step 3: Create `internal/tui/confirm.go`**:

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// confirmMode distinguishes a y/n confirm from a type-the-name confirm.
type confirmMode int

const (
	confirmYesNo confirmMode = iota
	confirmTypeName
)

// confirmView is a modal confirm pushed over the app-detail view: y/n for stop,
// type-the-app-name for delete (matching the CLI's delete guard). On confirm it
// emits the pending intent; "no"/mismatch cancels via the root.
type confirmView struct {
	name   string
	prompt string
	mode   confirmMode
	intent func(string) tea.Msg
	input  textinput.Model
	err    error
}

func newStopConfirm(name string) confirmView {
	return confirmView{
		name:   name,
		prompt: fmt.Sprintf("Stop %s? Its running deployment will be halted.", name),
		mode:   confirmYesNo,
		intent: func(n string) tea.Msg { return stopAppMsg{n} },
	}
}

func newDeleteConfirm(name string) confirmView {
	in := textinput.New()
	in.Placeholder = name
	in.Focus()
	return confirmView{
		name:   name,
		prompt: fmt.Sprintf("Delete %s? This cannot be undone. Type the app name to confirm.", name),
		mode:   confirmTypeName,
		intent: func(n string) tea.Msg { return deleteAppMsg{n} },
		input:  in,
	}
}

func (v confirmView) Init() tea.Cmd { return nil }

func (v confirmView) title() string { return "confirm" }

func (v confirmView) refresh(API) tea.Cmd { return nil }

// capturesText is true only in type-name (delete) mode, where the app name is
// typed into a field; the y/n mode leaves the root's shortcuts active.
func (v confirmView) capturesText() bool { return v.mode == confirmTypeName }

func (v confirmView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case tea.KeyMsg:
		if v.mode == confirmYesNo {
			switch msg.String() {
			case "y":
				return v, func() tea.Msg { return v.intent(v.name) }
			case "n":
				return v, func() tea.Msg { return popMsg{1} }
			}
			return v, nil
		}
		// type-name mode
		if msg.Type == tea.KeyEnter {
			if strings.TrimSpace(v.input.Value()) == v.name {
				return v, func() tea.Msg { return v.intent(v.name) }
			}
			v.err = fmt.Errorf("that doesn't match %q", v.name)
			return v, nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return v, cmd
	}
	return v, nil
}

func (v confirmView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s\n\n", v.prompt)
	if v.mode == confirmYesNo {
		b.WriteString("  y  yes      n / esc  no")
	} else {
		fmt.Fprintf(&b, "  %s\n\n  ↵ confirm   esc cancel", v.input.View())
	}
	if v.err != nil {
		fmt.Fprintf(&b, "\n\n ⚠ %v", v.err)
	}
	return b.String()
}
```

- [ ] **Step 4: Add the `s`/`x` triggers to `appdetail.go`** — in `appDetailView.Update`'s `tea.KeyMsg` switch, add after `"enter"`:

```go
		case "s":
			return v, func() tea.Msg { return pushMsg{newStopConfirm(v.name)} }
		case "x":
			return v, func() tea.Msg { return pushMsg{newDeleteConfirm(v.name)} }
```

- [ ] **Step 5: Run to verify pass**

Run: `go test ./internal/tui/ -v 2>&1 | tail -30`
Expected: all PASS.

- [ ] **Step 6: Full gate + commit**

Run: `gofmt -l internal/tui/` (clean), `go vet ./internal/tui/`, `make cross`.

```bash
git add internal/tui/confirm.go internal/tui/confirm_test.go internal/tui/appdetail.go internal/tui/appdetail_test.go
git commit -m "feat(cli): add stop and delete confirms to the TUI

Part of #P4

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Deploy (`deployView` + kickoff + replace-with-logs) + app-detail `d` trigger

Add the depth-2 `deployView` (confirm gate showing cwd + Dockerfile), the `deployMsg`/`deployStartedMsg` messages, the root handlers that kick off `Deploy` and **replace** the confirm with a follow `logsView`, and the `d` trigger on app-detail.

**Files:**
- Modify: `internal/tui/tui.go` (add `deployMsg`, `deployStartedMsg`)
- Modify: `internal/tui/app.go` (root: `deployMsg` → `Deploy`; `deployStartedMsg` → replace/banner)
- Create: `internal/tui/deploy.go`
- Modify: `internal/tui/appdetail.go` (add `d` → push deploy; imports `os`, `path/filepath`)
- Test: `internal/tui/deploy_test.go`, `internal/tui/appdetail_test.go`

**Interfaces:**
- Consumes: Task 2's `pushMsg`, the wide `API`; `newLogsView` (phase 3).
- Produces: `func newDeployView(name, cwd string, dockerfile bool) deployView` (implements `view`, `title()=="deploy"`); `deployMsg{ name, cwd string }`; `deployStartedMsg{ app, id string; err error }`.

- [ ] **Step 1: Add the messages to `tui.go`** — append to the `type ( … )` message block:

```go
	// deployMsg is the deploy confirm's intent; the root kicks off Deploy.
	deployMsg struct {
		name string
		cwd  string
	}

	// deployStartedMsg is the deploy kickoff's outcome. On success the root
	// replaces the deploy confirm with a follow logs view on the new build.
	deployStartedMsg struct {
		app string
		id  string
		err error
	}
```

(Like the other action messages, neither implements `pollResult`.)

- [ ] **Step 2: Write the failing tests** — `internal/tui/deploy_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/store"
)

func TestDeployViewConfirmRender(t *testing.T) {
	out := newDeployView("blog", "/Users/me/code/blog", true).View()
	for _, want := range []string{"deploy blog", "/Users/me/code/blog", "found ✓", "ship it"} {
		if !strings.Contains(out, want) {
			t.Fatalf("deploy confirm missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(newDeployView("blog", "/x", false).View(), "not found ✗") {
		t.Fatal("missing-Dockerfile state not rendered")
	}
}

func TestDeployViewShipEmitsDeployIntent(t *testing.T) {
	v := newDeployView("blog", "/x/blog", true)
	m, cmd := v.Update(keyRunes('y'))
	if cmd == nil {
		t.Fatal("y should ship")
	}
	msg, ok := cmd().(deployMsg)
	if !ok || msg.name != "blog" || msg.cwd != "/x/blog" {
		t.Fatalf("want deployMsg{blog,/x/blog}, got %#v", cmd())
	}
	if !strings.Contains(m.(deployView).View(), "shipping") {
		t.Fatalf("should show shipping state:\n%s", m.(deployView).View())
	}
}

func TestRootDeployMsgKicksOffAndRecords(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("b", "a", false, fakeAPI{rec: rec, deployDep: store.Deployment{ID: "dep-xyz"}})
	_, cmd := m.Update(deployMsg{name: "blog", cwd: "/x/blog"})
	msg, ok := cmd().(deployStartedMsg)
	if !ok || msg.app != "blog" || msg.id != "dep-xyz" || msg.err != nil {
		t.Fatalf("want deployStartedMsg{blog,dep-xyz,nil}, got %#v", cmd())
	}
	if rec.deployName != "blog" || rec.deployDir != "/x/blog" {
		t.Fatalf("Deploy not passed through: %+v", rec)
	}
}

func TestRootDeployStartedReplacesConfirmWithLogs(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	// stack: apps -> detail -> deploy
	m2, _ := m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	m2, _ = m.Update(pushMsg{newDeployView("blog", "/x", true)})
	m = m2.(Model)
	depth := len(m.stack)

	// success: the deploy confirm is replaced (not stacked) by a logs view
	m2, _ = m.Update(deployStartedMsg{app: "blog", id: "dep-xyz"})
	m = m2.(Model)
	if len(m.stack) != depth {
		t.Fatalf("replace should keep depth %d, got %d", depth, len(m.stack))
	}
	if m.top().title() != "logs" {
		t.Fatalf("top should be the logs view, got %q", m.top().title())
	}
}

func TestRootDeployStartedErrorBannersDeployView(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	m2, _ := m.Update(pushMsg{newDeployView("blog", "/x", true)})
	m = m2.(Model)
	m2, _ = m.Update(deployStartedMsg{app: "blog", err: errors.New("upload failed")})
	m = m2.(Model)
	if m.top().title() != "deploy" {
		t.Fatalf("error must not replace the deploy view, got %q", m.top().title())
	}
	if !strings.Contains(m.View(), "upload failed") {
		t.Fatalf("kickoff error banner missing:\n%s", m.View())
	}
}
```

Add the `d`-trigger test to `internal/tui/appdetail_test.go`:

```go
func TestAppDetailDKeyPushesDeploy(t *testing.T) {
	_, cmd := newAppDetailView("blog", false).Update(keyRunes('d'))
	if cmd == nil {
		t.Fatal("d should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "deploy" {
		t.Fatalf("want the deploy view, got title %q", pm.view.title())
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/tui/ 2>&1 | head`
Expected: FAIL — `undefined: newDeployView`, `deployMsg`, `deployStartedMsg`.

- [ ] **Step 4: Create `internal/tui/deploy.go`**:

```go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// deployView is the depth-2 confirm gate before shipping the launch directory
// to an app. On "y" it emits deployMsg; the root kicks off Deploy (which
// returns fast with a building deployment) and replaces this view with a
// follow logs view on that build. It has no poll of its own.
type deployView struct {
	name       string
	cwd        string
	dockerfile bool
	shipping   bool
	err        error
}

func newDeployView(name, cwd string, dockerfile bool) deployView {
	return deployView{name: name, cwd: cwd, dockerfile: dockerfile}
}

func (v deployView) Init() tea.Cmd { return nil }

func (v deployView) title() string { return "deploy" }

func (v deployView) refresh(API) tea.Cmd { return nil }

func (v deployView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err, v.shipping = msg.err, false
		return v, nil
	case tea.KeyMsg:
		if msg.String() == "y" && !v.shipping {
			v.shipping = true
			name, cwd := v.name, v.cwd
			return v, func() tea.Msg { return deployMsg{name: name, cwd: cwd} }
		}
	}
	return v, nil
}

func (v deployView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  deploy %s\n\n", v.name)
	fmt.Fprintf(&b, "  source:     %s\n", v.cwd)
	dockerfile := "not found ✗"
	if v.dockerfile {
		dockerfile = "found ✓"
	}
	fmt.Fprintf(&b, "  Dockerfile: %s\n\n", dockerfile)
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	if v.shipping {
		b.WriteString("  shipping…")
		return b.String()
	}
	b.WriteString("  y  ship it     esc  cancel")
	return b.String()
}
```

- [ ] **Step 5: Add the root handlers in `app.go`** — inside `Update`'s type-switch, add after the `case popMsg:` block from Task 2:

```go
	case deployMsg:
		name, cwd, c := msg.name, msg.cwd, m.client
		return m, func() tea.Msg {
			dep, err := c.Deploy(name, cwd)
			return deployStartedMsg{app: name, id: dep.ID, err: err}
		}
	case deployStartedMsg:
		if _, ok := m.top().(deployView); !ok {
			return m, nil // user navigated away before the kickoff returned
		}
		if msg.err != nil {
			next, _ := m.top().Update(errMsg{msg.err})
			m.stack[len(m.stack)-1] = next.(view)
			return m, nil
		}
		m.stack[len(m.stack)-1] = newLogsView(msg.app, msg.id, "building")
		if m.width > 0 {
			seeded, _ := m.top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack[len(m.stack)-1] = seeded.(view)
		}
		return m, m.refresh()
```

- [ ] **Step 6: Add the `d` trigger to `appdetail.go`** — add `"os"` and `"path/filepath"` to the imports, and in `appDetailView.Update`'s `tea.KeyMsg` switch add after `"enter"` (alongside the Task-4 `s`/`x` cases):

```go
		case "d":
			cwd, _ := os.Getwd()
			_, statErr := os.Stat(filepath.Join(cwd, "Dockerfile"))
			hasDockerfile := statErr == nil
			return v, func() tea.Msg { return pushMsg{newDeployView(v.name, cwd, hasDockerfile)} }
```

- [ ] **Step 7: Run to verify pass**

Run: `go test ./internal/tui/ -v 2>&1 | tail -40`
Expected: all PASS.

- [ ] **Step 8: Full gate + commit**

Run: `gofmt -l internal/tui/` (clean), `go vet ./internal/tui/`, `make cross`.

```bash
git add internal/tui/tui.go internal/tui/app.go internal/tui/deploy.go internal/tui/deploy_test.go internal/tui/appdetail.go internal/tui/appdetail_test.go
git commit -m "feat(cli): add deploy action with confirm gate and live build follow

Part of #P4

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Verification + PR

**Files:**
- Modify: `PROGRESS.md` (one line)

- [ ] **Step 1: Full gate**

Run: `make verify`
Expected: gofmt clean, vet clean, all tests pass, arm64 cross-compile green.

- [ ] **Step 2: Manual smoke** (real terminal; a `piperd` running locally, launched from a project dir with a `Dockerfile`)

```bash
make build && ./bin/piper
```

Expected: at the apps table, `n` opens the **new app** form (name, port 8080) — `tab` switches fields, `↵` creates and returns to the list (or banners a bad name/port). `↵` into an app, then: `d` opens the **deploy** confirm showing the launch cwd + Dockerfile status; `y` ships and the view becomes a live **build log** that tails and auto-stops (breadcrumb `… › logs`); `esc` mid-build returns to detail with the build still running server-side. `s` shows a y/n **stop** confirm; `x` shows a **delete** confirm that only deletes when you type the exact app name, then returns to the apps list. `esc`/`q`/`ctrl+c` behave as before. Confirm `./bin/piper | cat` still prints usage (non-TTY guard) and `./bin/piper list` still works.

- [ ] **Step 3: PROGRESS.md** — under the Interactive TUI epic section, add after the phase-3 line:

```markdown
- ✅ Actions: new-app form, deploy (confirm → live build), stop/delete confirms — [#P4](https://github.com/getpiper/piper/issues/P4)
```

(substitute the real `#P4` number), and commit:

```bash
git add PROGRESS.md
git commit -m "docs: record TUI actions in PROGRESS.md

Part of #P4

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin ozykhan/tui-phase4-actions
gh pr create --base main --title "feat(cli): TUI actions — new app, deploy, stop, delete" \
  --body "Phase 4 of the TUI epic #183. Read-write actions on the view stack: intent→result plumbing in the root, a new-app form, stop (y/n) and delete (type-the-name) confirms, and a deploy confirm gate that kicks off Deploy and hands the live build to the phase-3 follow log view. No agent/client changes. Part of #183. Closes #P4."
```

---

## Out of scope for this plan (next plan docs, per spec phasing)

Phase 5 (boxes view — switcher + editor over the phase-1 schema; consumes #186/#187), phase 6 (wizards: login/connect, GitHub setup, **link repo**). Deploy-client timeout robustness (a separate longer-timeout deploy client) is a deferred known limitation. The phase-3 polish follow-ups remain tracked in #193.
