# Piper TUI — Phase 3 (drill-down) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete the TUI's read-only surface — drill from the apps table into an app (header + live deployments table) and into a single deployment's logs (scrollable viewport with follow) — per the approved design ([`docs/superpowers/specs/2026-07-13-tui-phase3-drilldown-design.md`](../specs/2026-07-13-tui-phase3-drilldown-design.md)).

**Architecture:** Generalize the phase-2 skeleton's single-view root into a view **stack**. Every stacked view implements a `view` interface (`tea.Model` + `refresh(API) tea.Cmd` + `title() string`); the root's 2s poll refreshes the top view, views request navigation with a `pushMsg`, and the header renders a breadcrumb from the stack. Two new views — `appDetailView` (depth 1) and `logsView` (depth 2, a `bubbles/viewport`) — sit on top of the existing `appsView`. Closes the phase-2 polish issue [#189](https://github.com/piperbox/piper/issues/189).

**Tech Stack:** Go, `charmbracelet/bubbletea` v1 + `lipgloss` v1 (already present), `charmbracelet/bubbles` v1 (added here for `viewport`).

## Global Constraints

- `CGO_ENABLED=0` everywhere; `make cross` (linux/arm64) must stay green.
- Module path `github.com/piperbox/piper`; nothing imports "up": `internal/tui` imports `internal/api` + `internal/store` (types) + charmbracelet libs only; it must NOT import `internal/client`. `cmd/piper` imports `internal/tui`.
- New dep `github.com/charmbracelet/bubbles` pinned to the **v1** major (matching bubbletea/lipgloss). `go mod tidy` drops a dep with no importer — only add `bubbles` in the task that imports it (Task 4).
- Deployment status strings are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`; `""` means never deployed → `—` icon. **Follow is meaningful only while a deployment is `"building"`** (the build log grows then); it auto-stops when the deployment leaves `"building"`.
- Read-only only. No deploy / stop / delete / link / create actions (phase 4+). No `internal/config` changes (leave #186/#187 for phase 5). No new agent endpoints.
- TDD: every task writes the failing test first. Run `make verify` before pushing.
- One commit per task step group, conventional-commit style, ending with:
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`
- Trunk-based: never commit to `main`; one PR into `main` for this phase, squash-merged. Work proceeds on branch `ozykhan/tui-drilldown` (already holds the spec + this plan).

**Reference — current shapes (already in the codebase):**
- `api.App` = `struct { store.App; Status string }`; `store.App` has `Name, Port, Repo, Branch, Hostname string`, `CreatedAt time.Time`.
- `store.Deployment` has `ID, App string; PR int; ImageID, ContainerID string; HostPort int; Status string; CreatedAt time.Time`.
- `*client.Client` already implements `ListApps() ([]api.App, error)`, `App(name) (api.App, error)`, `Deployments(name) ([]store.Deployment, error)`, `DeploymentLogs(name, id) (string, error)`.

---

### Task 1: Phase-3 tracker + plan commit

**Files:** the plan doc (already written); GitHub issue.

**Interfaces:**
- Produces: phase-3 child issue number `#P3` — used in every later commit/PR body. Record it in the plan-execution notes.

- [ ] **Step 1: Create the phase-3 child issue** under epic #183

```bash
gh issue create --title "[cli] TUI phase 3: drill-down (app detail, deployments, logs + follow)" \
  --label cli --label enhancement --label P3 --label "size/M" \
  --body "Phase 3 of the TUI epic #183. Read-only drill-down: app detail (header + live deployments table), per-deployment log viewer with follow, apps-table cursor, stack breadcrumb. Design: docs/superpowers/specs/2026-07-13-tui-phase3-drilldown-design.md. Closes #189 (phase-3 polish) alongside."
```

- [ ] **Step 2: Commit the plan** (the spec is already committed on this branch)

```bash
git add docs/superpowers/plans/2026-07-13-tui-phase3-drilldown.md
git commit -m "docs: add TUI phase-3 drill-down implementation plan

Part of #183

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 3: Edit the epic** — check the Phase 3 line's context if desired and note `#P3` for later steps.

---

### Task 2: Root refactor — `view` stack, per-view refresh, breadcrumb, relay presentation

Generalize the root from one hardcoded view to a stack of `view`s. Behavior stays identical for the single home view except: header is a breadcrumb, the status bar pluralizes correctly (`1 app`), and a relay box shows its base domain + `https` URLs (closing three of #189's four items; the breadcrumb closes the fourth). No new views yet — nothing pushes.

**Files:**
- Modify: `internal/tui/tui.go` (add `view`, `pushMsg`, `pollResult`)
- Modify: `internal/tui/render.go` (`appURL` gains `remote`; add `pluralApps`)
- Modify: `internal/tui/apps.go` (appsView gains `remote` + `refresh`/`title`/`count`)
- Modify: `internal/tui/app.go` (Model → stack of `view`, delegated refresh, breadcrumb, generic reachability)
- Modify: `cmd/piper/main.go` (`launchTUI` passes a relay flag + base domain; `Run`/`NewModel` new signature)
- Test: `internal/tui/apps_test.go`, `internal/tui/app_test.go`

**Interfaces:**
- Produces (used by Tasks 3–4):
  - `type view interface { tea.Model; refresh(API) tea.Cmd; title() string }`
  - `type pushMsg struct{ view view }`
  - `type pollResult interface{ reachable() bool }` — implemented by `appsLoadedMsg` (true) and `errMsg` (false).
  - `func NewModel(box, addr string, remote bool, c API) Model`; `func Run(box, addr string, remote bool, c API) error`
  - `func newAppsView(remote bool) appsView`; `appsView` implements `view` and exposes `count() int`.
  - `func appURL(hostname string, remote bool) string`; `func pluralApps(n int) string`.

- [ ] **Step 1: Update the existing tests to the new signatures (failing)** — this is the RED step: the refactor changes `NewModel`/`newAppsView` signatures and the `"1 apps"` grammar.

In `internal/tui/apps_test.go`, change every `newAppsView()` call to `newAppsView(false)` (three call sites: the two in `TestAppsViewLoadingAndEmptyStates` and the one in each of `TestAppsViewRendersRows` / `TestAppsViewErrorBannerKeepsLastRows`).

In `internal/tui/app_test.go`, change every `NewModel(...)` call to pass a `remote` argument of `false` after `addr`:
- `NewModel("pi4", "http://192.168.1.6:8088", false, f)`
- `NewModel("pi4", "http://192.168.1.6:8088", false, fakeAPI{err: ...})`
- `NewModel("pi4", "addr", false, fakeAPI{err: ...})`
- `NewModel("pi4", "addr", false, fakeAPI{})` (two of these)

In `TestModelPollSuccessUpdatesStatusBar`, change the expected substring `"1 apps"` to `"1 app"`.

Add a new test to `app_test.go` for the breadcrumb + relay presentation:

```go
func TestModelBreadcrumbAndRelayBar(t *testing.T) {
	f := fakeAPI{apps: []api.App{{App: store.App{Name: "blog", Hostname: "blog.example.dev"}, Status: "running"}}}
	m := NewModel("pi4", "pi4.example.dev", true, f)
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "piper") || !strings.Contains(out, "apps") {
		t.Fatalf("breadcrumb missing:\n%s", out)
	}
	if !strings.Contains(out, "pi4.example.dev") {
		t.Fatalf("relay base domain missing from bar:\n%s", out)
	}
	if !strings.Contains(out, "https://blog.example.dev") {
		t.Fatalf("relay app should render https:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ 2>&1 | head`
Expected: FAIL to compile — `not enough arguments in call to NewModel` / `newAppsView`.

- [ ] **Step 3: Extend `tui.go`** — replace the whole file body below the package doc with:

```go
package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/api"
)

// API is the slice of the piperd control API the TUI consumes.
// *client.Client satisfies it; tests inject fakes.
type API interface {
	ListApps() ([]api.App, error)
}

// view is a stack entry: a Bubble Tea model that refreshes its own data off the
// UI thread and names itself for the breadcrumb. The root owns the stack; a
// view never mutates it — it requests navigation with a pushMsg.
type view interface {
	tea.Model
	refresh(API) tea.Cmd
	title() string
}

// Messages flowing into Update. All API calls run as tea.Cmd goroutines and
// land as exactly one of these; the UI thread never blocks.
type (
	appsLoadedMsg struct{ apps []api.App }
	errMsg        struct{ err error }
	tickMsg       struct{}
	pushMsg       struct{ view view }
)

// pollResult is implemented by every message that is the outcome of a view's
// poll, so the root updates reachability without knowing the view type.
type pollResult interface{ reachable() bool }

func (appsLoadedMsg) reachable() bool { return true }
func (errMsg) reachable() bool        { return false }
```

- [ ] **Step 4: Update `render.go`** — `appURL` gains a `remote` flag and a `pluralApps` helper is added. Replace the file with:

```go
package tui

import "fmt"

// appURL renders the URL a box serves an app on from its stored hostname. A
// relay-terminated box serves over HTTPS; a local/BYO box over HTTP. Empty
// hostname (never deployed) yields "".
func appURL(hostname string, remote bool) string {
	if hostname == "" {
		return ""
	}
	if remote {
		return "https://" + hostname
	}
	return "http://" + hostname
}

// pluralApps renders an app count for the status bar ("1 app", "3 apps").
func pluralApps(n int) string {
	if n == 1 {
		return "1 app"
	}
	return fmt.Sprintf("%d apps", n)
}

// statusIcon maps a deployment status to its one-glyph indicator; "" (never
// deployed) and unknown values render as "—".
func statusIcon(status string) string {
	switch status {
	case "running":
		return "●"
	case "building":
		return "◐"
	case "failed":
		return "✗"
	case "stopped":
		return "○"
	}
	return "—"
}
```

- [ ] **Step 5: Update `apps.go`** — appsView becomes a `view` (gains `remote`, `refresh`, `title`, `count`). Replace the file with:

```go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/api"
)

// appsView is the depth-0 home view: a table of apps. Cursor + drill-in arrive
// in Task 3.
type appsView struct {
	apps   []api.App
	err    error
	loaded bool
	remote bool
}

func newAppsView(remote bool) appsView { return appsView{remote: remote} }

func (v appsView) Init() tea.Cmd { return nil }

func (v appsView) title() string { return "apps" }

func (v appsView) count() int { return len(v.apps) }

func (v appsView) refresh(c API) tea.Cmd {
	return func() tea.Msg {
		apps, err := c.ListApps()
		if err != nil {
			return errMsg{err}
		}
		return appsLoadedMsg{apps}
	}
}

func (v appsView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case appsLoadedMsg:
		v.apps, v.err, v.loaded = msg.apps, nil, true
	case errMsg:
		v.err = msg.err
	}
	return v, nil
}

func (v appsView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	if len(v.apps) == 0 {
		b.WriteString(" no apps yet — create one with `piper create <name>`")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-16s %-12s %s\n", "NAME", "STATUS", "URL")
	for _, a := range v.apps {
		status := strings.TrimSpace(statusIcon(a.Status) + " " + a.Status)
		fmt.Fprintf(&b, "  %-16s %-12s %s\n", a.Name, status, appURL(a.Hostname, v.remote))
	}
	return b.String()
}
```

- [ ] **Step 6: Rewrite `app.go`** — the root becomes a `view` stack with delegated refresh, generic reachability, `pushMsg` handling, and a breadcrumb. Replace the file with:

```go
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const pollInterval = 2 * time.Second

// Model is the root of the TUI: it owns the view stack, the poll tick, the
// active box's client, and the status bar. The top view handles its own
// messages; the root intercepts global keys, navigation, and connectivity.
type Model struct {
	box    string
	addr   string
	remote bool
	client API

	stack  []view
	loaded bool // at least one successful poll
	down   bool // last poll failed
}

func NewModel(box, addr string, remote bool, c API) Model {
	return Model{box: box, addr: addr, remote: remote, client: c, stack: []view{newAppsView(remote)}}
}

// Run starts the interactive TUI against c, identified as box/addr in the
// status bar. remote marks a relay-backed box (HTTPS URLs). It blocks until quit.
func Run(box, addr string, remote bool, c API) error {
	_, err := tea.NewProgram(NewModel(box, addr, remote, c), tea.WithAltScreen()).Run()
	return err
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.refresh(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) top() view { return m.stack[len(m.stack)-1] }

// refresh polls the top view's data off the UI thread.
func (m Model) refresh() tea.Cmd { return m.top().refresh(m.client) }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			if len(m.stack) == 1 {
				return m, tea.Quit
			}
			m.stack = m.stack[:len(m.stack)-1]
			return m, m.refresh()
		case "esc":
			if len(m.stack) > 1 {
				m.stack = m.stack[:len(m.stack)-1]
				return m, m.refresh()
			}
			return m, nil
		case "r":
			return m, m.refresh()
		}
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case pushMsg:
		m.stack = append(m.stack, msg.view)
		return m, m.refresh()
	}
	if pr, ok := msg.(pollResult); ok {
		if pr.reachable() {
			m.loaded = true
		}
		m.down = !pr.reachable()
	}
	next, cmd := m.top().Update(msg)
	m.stack[len(m.stack)-1] = next.(view)
	return m, cmd
}

func (m Model) View() string {
	crumbs := make([]string, len(m.stack))
	for i, v := range m.stack {
		crumbs[i] = v.title()
	}
	header := lipgloss.NewStyle().Bold(true).Render(" piper ") + "· " + strings.Join(crumbs, " › ")
	return header + "\n\n" + m.top().View() + "\n\n" + m.statusBar()
}

func (m Model) statusBar() string {
	loc := fmt.Sprintf("%s · %s", m.box, m.addr)
	switch {
	case m.down:
		return fmt.Sprintf(" ○ %s · unreachable — retrying…", loc)
	case !m.loaded:
		return fmt.Sprintf(" … %s · connecting…", loc)
	default:
		return fmt.Sprintf(" ● %s · %s", loc, pluralApps(m.stack[0].(appsView).count()))
	}
}
```

- [ ] **Step 7: Update `cmd/piper/main.go`'s `launchTUI`** — pass a relay flag and the base domain instead of the `"via relay"` string. Replace the body of the `launchTUI` var:

```go
var launchTUI = func(remote string, stderr io.Writer) int {
	c, ok := dialClient(remote, stderr)
	if !ok {
		return 1
	}
	c = c.WithTimeout(tuiRequestTimeout)
	box, addr := "default", ""
	if cf, err := config.LoadClientFile(); err == nil {
		if b, ok := cf.CurrentBox(); ok {
			box = b.Name
		}
	}
	if cc, err := config.LoadClient(); err == nil {
		addr = cc.Addr // env overrides + localhost default applied
	}
	relay := remote != ""
	if relay {
		addr = remote // the relay base domain
	}
	if err := tui.Run(box, addr, relay, c); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}
```

- [ ] **Step 8: Run to verify pass**

Run: `go test ./internal/tui/ ./cmd/piper/ -v 2>&1 | tail -30`
Expected: all PASS (updated + new tests). `cmd/piper` tests stub `launchTUI`, so its internals don't affect them; the package must still compile against the new `tui.Run` signature.

- [ ] **Step 9: Full gate + commit**

Run: `gofmt -l internal/tui/ cmd/piper/` (clean), `go vet ./internal/tui/ ./cmd/piper/`, `make cross`.

```bash
git add internal/tui/tui.go internal/tui/render.go internal/tui/apps.go internal/tui/app.go internal/tui/apps_test.go internal/tui/app_test.go cmd/piper/main.go
git commit -m "feat(cli): make the TUI root a view stack with breadcrumb and relay-aware bar

Generalizes the single-view root into a stack of views that each refresh
their own data; adds the breadcrumb, correct app-count grammar, and
relay base-domain + https URL rendering. Resolves the #189 polish items.

Part of #P3

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: App detail view + apps cursor + drill-in

Add the depth-1 `appDetailView` (app header + live deployments table with a cursor), widen `API` with the read methods it needs, and give `appsView` a cursor so `↵` opens the selected app.

**Files:**
- Modify: `internal/tui/tui.go` (widen `API`; add `appDetailLoadedMsg`)
- Modify: `internal/tui/render.go` (add `relTime`, `shortID`, `repoLine`)
- Create: `internal/tui/appdetail.go`
- Modify: `internal/tui/apps.go` (cursor + `up`/`down` + `enter` → push)
- Modify: `internal/tui/app_test.go` (extend `fakeAPI`)
- Test: `internal/tui/appdetail_test.go`, `internal/tui/apps_test.go`

**Interfaces:**
- Consumes: Task 2's `view`, `pushMsg`, `pollResult`, `appURL`, `statusIcon`.
- Produces (used by Task 4): `func newAppDetailView(name string, remote bool) appDetailView` (implements `view`); its deployments-table `enter` emits `pushMsg{newLogsView(app, id, status)}` — Task 4 defines `newLogsView`. `type appDetailLoadedMsg struct{ app api.App; deps []store.Deployment }` (implements `pollResult`).

- [ ] **Step 1: Widen the `API` interface and fake, and add the message** — in `internal/tui/tui.go`, replace the `API` interface and add `store` to imports:

```go
import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/store"
)

// API is the slice of the piperd control API the TUI consumes.
// *client.Client satisfies it; tests inject fakes.
type API interface {
	ListApps() ([]api.App, error)
	App(name string) (api.App, error)
	Deployments(name string) ([]store.Deployment, error)
	DeploymentLogs(name, id string) (string, error)
}
```

Add to the message block (after `pushMsg`):

```go
	appDetailLoadedMsg struct {
		app  api.App
		deps []store.Deployment
	}
```

and its reachability, next to the others:

```go
func (appDetailLoadedMsg) reachable() bool { return true }
```

In `internal/tui/app_test.go`, extend `fakeAPI` to satisfy the wider interface:

```go
type fakeAPI struct {
	apps []api.App
	err  error
	app  api.App
	deps []store.Deployment
	logs string
}

func (f fakeAPI) ListApps() ([]api.App, error)                 { return f.apps, f.err }
func (f fakeAPI) App(string) (api.App, error)                  { return f.app, f.err }
func (f fakeAPI) Deployments(string) ([]store.Deployment, error) { return f.deps, f.err }
func (f fakeAPI) DeploymentLogs(string, string) (string, error)  { return f.logs, f.err }
```

- [ ] **Step 2: Write the failing tests** — `internal/tui/appdetail_test.go`:

```go
package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/store"
)

func fixtureDeps() []store.Deployment {
	return []store.Deployment{
		{ID: "dep-aaaaaaaaaaaa", Status: "running", CreatedAt: time.Now().Add(-2 * time.Minute)},
		{ID: "dep-bbbbbbbbbbbb", Status: "failed", PR: 7, CreatedAt: time.Now().Add(-90 * time.Minute)},
	}
}

func TestAppDetailRendersHeaderAndDeployments(t *testing.T) {
	v := newAppDetailView("blog", false)
	m, _ := v.Update(appDetailLoadedMsg{
		app:  api.App{App: store.App{Name: "blog", Hostname: "blog.piper.localhost", Port: 8080, Repo: "me/blog", Branch: "main"}},
		deps: fixtureDeps(),
	})
	out := m.View()
	for _, want := range []string{
		"blog", "http://blog.piper.localhost", "8080", "me/blog", "main",
		"DEPLOYMENT", "STATUS", "CREATED", "PR",
		"dep-aaaaaaaa", "● running", "2m ago",
		"✗ failed", "#7",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestAppDetailLoadingAndEmpty(t *testing.T) {
	if out := newAppDetailView("blog", false).View(); !strings.Contains(out, "loading") {
		t.Fatalf("want loading, got:\n%s", out)
	}
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{app: api.App{App: store.App{Name: "blog"}}, deps: nil})
	if out := m.View(); !strings.Contains(out, "no deployments yet") {
		t.Fatalf("want empty state, got:\n%s", out)
	}
}

func TestAppDetailCursorAndEnterPushesLogs(t *testing.T) {
	v := newAppDetailView("blog", false)
	m, _ := v.Update(appDetailLoadedMsg{app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps()})
	// move to the second deployment
	m, _ = m.Update(keyRunes('j'))
	// enter → a command that yields a pushMsg for the selected deployment's logs
	_, cmd := m.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "logs" {
		t.Fatalf("want logs view pushed, got title %q", pm.view.title())
	}
}
```

Add these key helpers to `internal/tui/app_test.go` (used across view tests):

```go
func keyRunes(r rune) tea.KeyMsg { return tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune{r}}) }
func keyEnter() tea.KeyMsg       { return tea.KeyMsg(tea.Key{Type: tea.KeyEnter}) }
```

Add the apps-cursor test to `internal/tui/apps_test.go`:

```go
func TestAppsViewCursorAndEnterPushesDetail(t *testing.T) {
	m, _ := newAppsView(false).Update(appsLoadedMsg{apps: fixtureApps()})
	m, _ = m.Update(keyRunes('j')) // cursor to "shop"
	_, cmd := m.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "shop" {
		t.Fatalf("want detail for shop, got title %q", pm.view.title())
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/tui/ 2>&1 | head`
Expected: FAIL — `undefined: newAppDetailView`, `undefined: newLogsView` (referenced via the pushed view's title in the app-detail test).

Note: `newLogsView` is defined in Task 4. To keep Task 3 self-contained and compiling, this task's `appDetailView.Update` references `newLogsView`; add a **minimal temporary stub** at the bottom of `appdetail.go` so the package compiles, and Task 4 replaces it with the real file:

```go
// TEMP stub replaced by logs.go in the next task.
func newLogsView(app, id, status string) logsView { return logsView{app: app, id: id, status: status} }

type logsView struct{ app, id, status string }

func (logsView) Init() tea.Cmd                       { return nil }
func (v logsView) Update(tea.Msg) (tea.Model, tea.Cmd) { return v, nil }
func (v logsView) View() string                      { return "" }
func (logsView) title() string                       { return "logs" }
func (v logsView) refresh(API) tea.Cmd               { return nil }
```

- [ ] **Step 4: Update `render.go`** — add the deployments-table helpers. Add `"time"` to the imports and append:

```go
import (
	"fmt"
	"time"
)

// ... existing appURL, pluralApps, statusIcon ...

// relTime renders a compact "time ago" for the deployments table.
func relTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	switch d := time.Since(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// shortID trims a deployment id for table display.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// repoLine renders an app's source for the detail header.
func repoLine(a api.App) string {
	if a.Repo == "" {
		return "(no repo)"
	}
	if a.Branch != "" {
		return a.Repo + "@" + a.Branch
	}
	return a.Repo
}
```

Add `"github.com/piperbox/piper/internal/api"` to `render.go`'s imports (for `repoLine`).

- [ ] **Step 5: Create `internal/tui/appdetail.go`**:

```go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/store"
)

// appDetailView is the depth-1 view: an app header plus its deployment history.
// It re-polls every tick while on top, so a deploy started elsewhere surfaces
// live. Read-only; actions arrive in phase 4.
type appDetailView struct {
	name   string
	remote bool
	app    api.App
	deps   []store.Deployment
	cursor int
	loaded bool
	err    error
}

func newAppDetailView(name string, remote bool) appDetailView {
	return appDetailView{name: name, remote: remote}
}

func (v appDetailView) Init() tea.Cmd { return nil }

func (v appDetailView) title() string { return v.name }

func (v appDetailView) refresh(c API) tea.Cmd {
	name := v.name
	return func() tea.Msg {
		app, err := c.App(name)
		if err != nil {
			return errMsg{err}
		}
		deps, err := c.Deployments(name)
		if err != nil {
			return errMsg{err}
		}
		return appDetailLoadedMsg{app: app, deps: deps}
	}
}

func (v appDetailView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case appDetailLoadedMsg:
		v.app, v.deps, v.loaded, v.err = msg.app, msg.deps, true, nil
		if v.cursor >= len(v.deps) {
			v.cursor = max(0, len(v.deps)-1)
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
			if v.cursor < len(v.deps)-1 {
				v.cursor++
			}
		case "enter":
			if len(v.deps) > 0 {
				d := v.deps[v.cursor]
				return v, func() tea.Msg { return pushMsg{newLogsView(v.name, d.ID, d.Status)} }
			}
		}
	}
	return v, nil
}

func (v appDetailView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	url := appURL(v.app.Hostname, v.remote)
	if url == "" {
		url = "—"
	}
	fmt.Fprintf(&b, "  %s   %s   :%d   %s\n\n", v.name, url, v.app.Port, repoLine(v.app))
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	if len(v.deps) == 0 {
		b.WriteString(" no deployments yet")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-14s %-12s %-10s %s\n", "DEPLOYMENT", "STATUS", "CREATED", "PR")
	for i, d := range v.deps {
		cursor := "  "
		if i == v.cursor {
			cursor = "▸ "
		}
		status := strings.TrimSpace(statusIcon(d.Status) + " " + d.Status)
		pr := ""
		if d.PR > 0 {
			pr = fmt.Sprintf("#%d", d.PR)
		}
		fmt.Fprintf(&b, "%s%-14s %-12s %-10s %s\n", cursor, shortID(d.ID), status, relTime(d.CreatedAt), pr)
	}
	return b.String()
}
```

Then add the temporary `logsView` stub from Step 3 to the bottom of this file.

- [ ] **Step 6: Update `apps.go`** — give `appsView` a cursor and `enter`. Add a `cursor int` field to the struct, and replace `Update` and the row loop in `View`:

```go
type appsView struct {
	apps   []api.App
	err    error
	loaded bool
	remote bool
	cursor int
}
```

```go
func (v appsView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case appsLoadedMsg:
		v.apps, v.err, v.loaded = msg.apps, nil, true
		if v.cursor >= len(v.apps) {
			v.cursor = max(0, len(v.apps)-1)
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
			if v.cursor < len(v.apps)-1 {
				v.cursor++
			}
		case "enter":
			if len(v.apps) > 0 {
				name := v.apps[v.cursor].Name
				return v, func() tea.Msg { return pushMsg{newAppDetailView(name, v.remote)} }
			}
		}
	}
	return v, nil
}
```

Replace the table loop in `View` (keep the `NAME/STATUS/URL` header line):

```go
	for i, a := range v.apps {
		cursor := "  "
		if i == v.cursor {
			cursor = "▸ "
		}
		status := strings.TrimSpace(statusIcon(a.Status) + " " + a.Status)
		fmt.Fprintf(&b, "%s%-16s %-12s %s\n", cursor, a.Name, status, appURL(a.Hostname, v.remote))
	}
```

- [ ] **Step 7: Run to verify pass**

Run: `go test ./internal/tui/ -v 2>&1 | tail -30`
Expected: all PASS.

- [ ] **Step 8: Full gate + commit**

Run: `gofmt -l internal/tui/` (clean), `go vet ./internal/tui/`, `make cross`.

```bash
git add internal/tui/tui.go internal/tui/render.go internal/tui/appdetail.go internal/tui/appdetail_test.go internal/tui/apps.go internal/tui/apps_test.go internal/tui/app_test.go
git commit -m "feat(cli): add app-detail view with live deployments table and apps cursor

Part of #P3

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Logs view + follow (adds `bubbles`)

Replace the Task-3 stub with the real depth-2 `logsView`: a `bubbles/viewport` over `DeploymentLogs`, with `f`-toggled follow that tail-appends on the tick while the deployment is still `building` and auto-stops when it leaves. Adds the terminal-size plumbing the viewport needs.

**Files:**
- Modify: `go.mod`, `go.sum` (add `bubbles` v1)
- Modify: `internal/tui/tui.go` (add `logsLoadedMsg`)
- Modify: `internal/tui/app.go` (root tracks window size; seeds a pushed view with it)
- Create: `internal/tui/logs.go` (replaces the Task-3 stub in `appdetail.go` — delete that stub)
- Test: `internal/tui/logs_test.go`

**Interfaces:**
- Consumes: Task 3's `pushMsg`, the wide `API`, `shortID`.
- Produces: `func newLogsView(app, id, status string) logsView` (implements `view`); `type logsLoadedMsg struct{ logs, status string }` (implements `pollResult`).

- [ ] **Step 1: Add the dependency** (import lands in this task, so `go mod tidy` keeps it)

```bash
go get github.com/charmbracelet/bubbles@v1
```

- [ ] **Step 2: Add `logsLoadedMsg` to `tui.go`** — in the message block:

```go
	logsLoadedMsg struct {
		logs   string
		status string
	}
```

and its reachability:

```go
func (logsLoadedMsg) reachable() bool { return true }
```

- [ ] **Step 3: Remove the temporary stub** from `internal/tui/appdetail.go` (the `newLogsView` + `logsView` block added in Task 3 Step 3/5).

- [ ] **Step 4: Write the failing tests** — `internal/tui/logs_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

func TestLogsFollowStartsWhileBuilding(t *testing.T) {
	v := newLogsView("blog", "dep-123456789abc", "building")
	if !v.follow {
		t.Fatal("follow should start on for a building deployment")
	}
	if v.refresh(fakeAPI{}) == nil {
		t.Fatal("first refresh must fetch even before load")
	}
}

func TestLogsTailAppendAndAutoStop(t *testing.T) {
	v := newLogsView("blog", "dep-123456789abc", "building")
	m, _ := v.Update(logsLoadedMsg{logs: "line1\n", status: "building"})
	m, _ = m.Update(logsLoadedMsg{logs: "line1\nline2\n", status: "building"})
	lv := m.(logsView)
	if lv.logs != "line1\nline2\n" {
		t.Fatalf("tail not appended: %q", lv.logs)
	}
	if !lv.follow {
		t.Fatal("should still follow while building")
	}
	// a shorter/equal payload must not shrink the buffer
	m, _ = m.Update(logsLoadedMsg{logs: "line1\n", status: "building"})
	if m.(logsView).logs != "line1\nline2\n" {
		t.Fatalf("buffer shrank on shorter payload: %q", m.(logsView).logs)
	}
	// leaving building auto-stops follow, and a non-following loaded view stops polling
	m, _ = m.Update(logsLoadedMsg{logs: "line1\nline2\ndone\n", status: "running"})
	lv = m.(logsView)
	if lv.follow {
		t.Fatal("follow must auto-stop when the deployment leaves building")
	}
	if lv.refresh(fakeAPI{}) != nil {
		t.Fatal("a loaded, non-following logs view must not poll")
	}
}

func TestLogsViewShowsContextAndFollowTag(t *testing.T) {
	v := newLogsView("blog", "dep-123456789abc", "building")
	m, _ := v.Update(logsLoadedMsg{logs: "hello\n", status: "building"})
	out := m.View()
	if !strings.Contains(out, "blog") || !strings.Contains(out, "dep-12345678") {
		t.Fatalf("missing context header:\n%s", out)
	}
	if !strings.Contains(out, "following") {
		t.Fatalf("missing follow indicator:\n%s", out)
	}
}

func TestLogsRefreshMatchesDeploymentStatus(t *testing.T) {
	// refresh should report the status of THIS deployment id from Deployments()
	v := newLogsView("blog", "dep-2", "building")
	f := fakeAPI{
		logs: "building…\n",
		deps: []store.Deployment{{ID: "dep-1", Status: "running"}, {ID: "dep-2", Status: "running"}},
	}
	msg := v.refresh(f)()
	lm, ok := msg.(logsLoadedMsg)
	if !ok {
		t.Fatalf("want logsLoadedMsg, got %T", msg)
	}
	if lm.status != "running" {
		t.Fatalf("want status running for dep-2, got %q", lm.status)
	}
}
```

Add `"github.com/piperbox/piper/internal/store"` to `logs_test.go` imports (for the last test).

- [ ] **Step 5: Run to verify failure**

Run: `go test ./internal/tui/ 2>&1 | head`
Expected: FAIL — `undefined: newLogsView` (stub removed), missing `logsLoadedMsg` fields.

- [ ] **Step 6: Create `internal/tui/logs.go`**:

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// chromeHeight is the rows the root spends on header + blank lines + status bar
// around a view; the log viewport takes the rest of the terminal height.
const chromeHeight = 6

// logsView is the depth-2 view: one deployment's build log in a scrollable
// viewport. Follow re-fetches the log tail each tick while the deployment is
// still building and auto-stops when it leaves that state (the log stops
// growing). Read-only.
type logsView struct {
	app    string
	id     string
	status string
	follow bool
	loaded bool
	err    error
	logs   string
	vp     viewport.Model
	ready  bool
}

func newLogsView(app, id, status string) logsView {
	return logsView{app: app, id: id, status: status, follow: status == "building"}
}

func (v logsView) Init() tea.Cmd { return nil }

func (v logsView) title() string { return "logs" }

// refresh fetches the full log once, then only while following. It also reports
// this deployment's current status so the view can auto-stop follow.
func (v logsView) refresh(c API) tea.Cmd {
	if v.loaded && !v.follow {
		return nil
	}
	app, id := v.app, v.id
	return func() tea.Msg {
		logs, err := c.DeploymentLogs(app, id)
		if err != nil {
			return errMsg{err}
		}
		status := ""
		if deps, err := c.Deployments(app); err == nil {
			for _, d := range deps {
				if d.ID == id {
					status = d.Status
					break
				}
			}
		}
		return logsLoadedMsg{logs: logs, status: status}
	}
}

func (v logsView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case logsLoadedMsg:
		v.loaded, v.err = true, nil
		if msg.status != "" {
			v.status = msg.status
		}
		if v.status != "building" {
			v.follow = false // build finished: the log is static
		}
		if len(msg.logs) > len(v.logs) {
			v.logs = msg.logs
			if v.ready {
				v.vp.SetContent(v.logs)
				v.vp.GotoBottom()
			}
		}
	case errMsg:
		v.err = msg.err
	case tea.KeyMsg:
		if msg.String() == "f" {
			v.follow = !v.follow
		}
	case tea.WindowSizeMsg:
		h := msg.Height - chromeHeight
		if h < 1 {
			h = 1
		}
		if !v.ready {
			v.vp = viewport.New(msg.Width, h)
			v.ready = true
		} else {
			v.vp.Width, v.vp.Height = msg.Width, h
		}
		v.vp.SetContent(v.logs)
		v.vp.GotoBottom()
	}
	if v.ready {
		var cmd tea.Cmd
		v.vp, cmd = v.vp.Update(msg)
		return v, cmd
	}
	return v, nil
}

func (v logsView) View() string {
	var b strings.Builder
	tag := ""
	if v.follow {
		tag = " · following…"
	}
	fmt.Fprintf(&b, "  %s · %s%s\n\n", v.app, shortID(v.id), tag)
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	if v.ready {
		b.WriteString(v.vp.View())
	} else {
		b.WriteString(v.logs)
	}
	return b.String()
}
```

- [ ] **Step 7: Add window-size plumbing to `app.go`** — the viewport needs the terminal size, and a view pushed mid-session must be seeded with it. Add `width, height int` to `Model`; handle `tea.WindowSizeMsg`; seed on push.

Add the fields:

```go
type Model struct {
	box    string
	addr   string
	remote bool
	client API

	stack         []view
	loaded        bool
	down          bool
	width, height int
}
```

In `Update`, change the `pushMsg` case to seed size, and add a `tea.WindowSizeMsg` case that stores size then falls through to forwarding:

```go
	case pushMsg:
		m.stack = append(m.stack, msg.view)
		if m.width > 0 {
			seeded, _ := m.top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack[len(m.stack)-1] = seeded.(view)
		}
		return m, m.refresh()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// no return: fall through to forward the size to the top view
	}
```

(The `tea.WindowSizeMsg` case sits inside the existing `switch msg := msg.(type)` block, after `pushMsg`. Because it does not `return`, execution continues to the `pollResult` check and the forward-to-top lines, so the active view is resized too.)

- [ ] **Step 8: Run to verify pass**

Run: `go test ./internal/tui/ -v 2>&1 | tail -30`
Expected: all PASS.

- [ ] **Step 9: Tidy, full gate, commit**

Run: `go mod tidy` (bubbles is now imported, so it stays); `gofmt -l internal/tui/` (clean); `go vet ./internal/tui/`; `make cross`.

```bash
git add go.mod go.sum internal/tui/tui.go internal/tui/app.go internal/tui/appdetail.go internal/tui/logs.go internal/tui/logs_test.go
git commit -m "feat(cli): add deployment log viewer with follow

Part of #P3

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Verification + PR

**Files:**
- Modify: `PROGRESS.md` (one line)

- [ ] **Step 1: Full gate**

Run: `make verify`
Expected: gofmt clean, vet clean, all tests pass, arm64 cross-compile green.

- [ ] **Step 2: Manual smoke** (real terminal; a `piperd` running locally with at least one deployed app)

```bash
make build && ./bin/piper
```

Expected: apps table with a `▸` cursor; `↓`/`↑` move it; `↵` on an app opens **app detail** (header + deployments table) and the breadcrumb reads `piper · apps › <app>`; `↵` on a deployment opens **logs** in a scrollable viewport (breadcrumb `… › logs`); `f` toggles `following…`; following a `◐ building` deploy appends new log lines and pins to the bottom, and stops on its own when the deploy finishes; `esc` pops back up the stack; `q` at the apps table and `ctrl+c` anywhere exit cleanly with the terminal restored. Confirm `./bin/piper | cat` still prints usage (non-TTY guard) and `./bin/piper list` still works.

- [ ] **Step 3: PROGRESS.md** — under the Interactive TUI epic section, add after the phase-2 line:

```markdown
- ✅ Drill-down: app detail + live deployments table, per-deployment log viewer with follow, breadcrumb navigation — [#P3](https://github.com/piperbox/piper/issues/P3)
```

(substitute the real `#P3` number), and commit:

```bash
git add PROGRESS.md
git commit -m "docs: record TUI drill-down in PROGRESS.md

Part of #P3

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin ozykhan/tui-drilldown
gh pr create --base main --title "feat(cli): TUI drill-down — app detail, deployments, logs + follow" \
  --body "Phase 3 of the TUI epic #183. Read-only drill-down: view stack with breadcrumb, app-detail view with a live deployments table, per-deployment log viewer with follow (tail-append, auto-stop). Adds bubbles v1 for the viewport. Part of #183. Closes #P3. Closes #189."
```

---

## Out of scope for this plan (next plan docs, per spec phasing)

Phase 4 (actions: deploy view with streaming, new-app form, stop/delete confirms), phase 5 (boxes view — switcher + editor over the phase-1 schema; consumes #186/#187), phase 6 (wizards: login/connect, GitHub setup, link repo). LAST DEPLOY on the apps table returns when the apps API grows a deploy timestamp (separate `[agent]`/`[store]`/`[api]` issue).
