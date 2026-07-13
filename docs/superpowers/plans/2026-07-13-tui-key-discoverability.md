# TUI Key Discoverability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the TUI's two navigation views' keys discoverable via an always-visible dim footer legend and a `?` help overlay showing the full keymap.

**Architecture:** The root `Model.View()` renders a dim footer line between the view body and the status bar, sourced from an optional `footered` interface that only `appsView` and `appDetailView` implement — mirroring the existing `capturesText()` seam. A new static `helpView` is pushed onto the stack when the global `?` key fires (gated behind `topCapturesText()` so text fields still receive a literal `?`), and dismissed by the root's existing `esc`/`q` pop.

**Tech Stack:** Go, Bubble Tea (`github.com/charmbracelet/bubbletea`), Lip Gloss (`github.com/charmbracelet/lipgloss`).

## Global Constraints

- **No cgo.** All builds pass with `CGO_ENABLED=0`. (This change is pure UI code; no new deps.)
- **Module path** `github.com/getpiper/piper`.
- **Layering:** pure `internal/tui` change; nothing imports up. No API-surface change.
- **Commits:** conventional-commit style, `Part of #196` in the body, ending with:
  ```
  Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
  ```
- **Verify gate:** `make verify` (gofmt → vet → test → cross) must pass before the work is done.
- **Existing test helpers** (in `internal/tui/app_test.go`, same package): `keyRunes(r rune) tea.KeyMsg`, `keyEnter()`, `keyTab()`, `pump(t, m, cmd) Model`, and the `fakeAPI` fake. Reuse them; do not redefine.

---

### Task 1: Contextual footer on the two navigation views

Add a `footered` optional interface, render it dim in the root between body and status bar, and implement `footer()` on `appsView` and `appDetailView`. Modal views (form, confirm, deploy, logs) do not implement it and render no footer.

**Files:**
- Modify: `internal/tui/app.go` — add `footered` interface + `topFooter()` helper; render in `View()`.
- Modify: `internal/tui/apps.go` — add `func (v appsView) footer() string`.
- Modify: `internal/tui/appdetail.go` — add `func (v appDetailView) footer() string`.
- Test: `internal/tui/app_test.go` (footer-present + footer-absent tests).

**Interfaces:**
- Produces:
  - `type footered interface{ footer() string }` (in `app.go`).
  - `func (v appsView) footer() string` → `"n new · ↵ open · r refresh · q quit · ? help"`.
  - `func (v appDetailView) footer() string` → `"d deploy · s stop · x delete · ↵ logs · r refresh · esc back · ? help"`.
  - `func (m Model) topFooter() string` → the dim-rendered footer line for the top view, or `""` if it doesn't implement `footered`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/tui/app_test.go`:

```go
func TestNavViewsRenderFooterLegend(t *testing.T) {
	// apps list (root) footer
	f := fakeAPI{apps: []api.App{{App: store.App{Name: "blog"}, Status: "running"}}}
	m := NewModel("pi4", "addr", false, f)
	m = pump(t, m, m.refresh())
	if out := m.View(); !strings.Contains(out, "n new") || !strings.Contains(out, "? help") {
		t.Fatalf("apps-list footer missing keys:\n%s", out)
	}

	// app detail footer
	m2, _ := m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	m = pump(t, m, m.refresh())
	out := m.View()
	for _, want := range []string{"d deploy", "s stop", "x delete", "esc back", "? help"} {
		if !strings.Contains(out, want) {
			t.Fatalf("app-detail footer missing %q:\n%s", want, out)
		}
	}
}

func TestModalViewsRenderNoFooterLegend(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	// push the new-app form (a text-capturing modal, not footered)
	m2, _ := m.Update(pushMsg{newFormView()})
	m = m2.(Model)
	if got := m.topFooter(); got != "" {
		t.Fatalf("modal must have no footer, got %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestNavViewsRenderFooterLegend|TestModalViewsRenderNoFooterLegend' -v`
Expected: FAIL — `m.topFooter undefined` (compile error) / footer keys absent.

- [ ] **Step 3: Add the interface, helper, and root render in `app.go`**

After the `topCapturesText` method (around `app.go:58`), add:

```go
// footered is a view that offers a one-line key legend. The root renders it dim
// between the body and the status bar; views without it render no footer.
type footered interface{ footer() string }

var footerStyle = lipgloss.NewStyle().Faint(true)

// topFooter returns the dim key legend for the top view, or "" if it offers none.
func (m Model) topFooter() string {
	f, ok := m.top().(footered)
	if !ok {
		return ""
	}
	return footerStyle.Render(" " + f.footer())
}
```

Then change `View()` (currently `app.go:159-166`) to insert the footer slot:

```go
func (m Model) View() string {
	crumbs := make([]string, len(m.stack))
	for i, v := range m.stack {
		crumbs[i] = v.title()
	}
	header := lipgloss.NewStyle().Bold(true).Render(" piper ") + "· " + strings.Join(crumbs, " › ")
	body := header + "\n\n" + m.top().View()
	if f := m.topFooter(); f != "" {
		body += "\n\n" + f
	}
	return body + "\n\n" + m.statusBar()
}
```

(`lipgloss` is already imported in `app.go`.)

- [ ] **Step 4: Implement `footer()` on `appsView`**

In `internal/tui/apps.go`, after `func (v appsView) count() int` (around `apps.go:26`), add:

```go
func (v appsView) footer() string { return "n new · ↵ open · r refresh · q quit · ? help" }
```

- [ ] **Step 5: Implement `footer()` on `appDetailView`**

In `internal/tui/appdetail.go`, after `func (v appDetailView) title() string` (around `appdetail.go:33`), add:

```go
func (v appDetailView) footer() string {
	return "d deploy · s stop · x delete · ↵ logs · r refresh · esc back · ? help"
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestNavViewsRenderFooterLegend|TestModalViewsRenderNoFooterLegend' -v`
Expected: PASS (both).

- [ ] **Step 7: Run the full package to check nothing regressed**

Run: `go test ./internal/tui/`
Expected: `ok  github.com/getpiper/piper/internal/tui`

- [ ] **Step 8: Commit**

```bash
git add internal/tui/app.go internal/tui/apps.go internal/tui/appdetail.go internal/tui/app_test.go
git commit -m "feat(cli): add key-legend footer to TUI navigation views

Part of #196

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: `?` help overlay with the full keymap

Add a static `helpView` rendering the full keymap, and wire the global `?` key in the root to push it (gated behind `topCapturesText()`, with a guard against stacking a second help view).

**Files:**
- Create: `internal/tui/help.go` — the `helpView` model.
- Modify: `internal/tui/app.go:82-93` — add the `?` case in the `!topCapturesText()` block.
- Test: `internal/tui/help_test.go` (new file).

**Interfaces:**
- Consumes: the `view` interface (`tea.Model` + `refresh(API) tea.Cmd` + `title() string`) from `tui.go`; the root's `pushMsg{view}` and `topCapturesText()` from Task's existing code.
- Produces:
  - `type helpView struct{}` satisfying `view`.
  - `func (v helpView) title() string` → `"help"`.
  - `func (v helpView) View() string` → the full keymap text (contains e.g. `toggle follow`, `deploy`, `new app`).

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/help_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

func TestQuestionMarkPushesHelpOverlay(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	_, cmd := m.Update(keyRunes('?'))
	if cmd == nil {
		t.Fatal("? should push a help view")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if _, ok := push.view.(helpView); !ok {
		t.Fatalf("want helpView pushed, got %T", push.view)
	}
	// the rendered overlay carries the full keymap
	out := helpView{}.View()
	for _, want := range []string{"new app", "deploy", "toggle follow", "refresh"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help overlay missing %q:\n%s", want, out)
		}
	}
}

func TestQuestionMarkIsLiteralInTextFields(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	// push the new-app form: capturesText() is true
	m2, _ := m.Update(pushMsg{newFormView()})
	m = m2.(Model)
	_, cmd := m.Update(keyRunes('?'))
	if cmd != nil {
		if _, ok := cmd().(pushMsg); ok {
			t.Fatal("? must not push help while a text field is focused")
		}
	}
}

func TestQuestionMarkDoesNotStackHelp(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	m2, _ := m.Update(pushMsg{helpView{}})
	m = m2.(Model)
	depth := len(m.stack)
	_, cmd := m.Update(keyRunes('?'))
	if cmd != nil {
		if _, ok := cmd().(pushMsg); ok {
			t.Fatal("? on the help view must not push a second help")
		}
	}
	if len(m.stack) != depth {
		t.Fatalf("stack depth changed: %d -> %d", depth, len(m.stack))
	}
}

func TestEscPopsHelpOverlay(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	m2, _ := m.Update(pushMsg{helpView{}})
	m = m2.(Model)
	if len(m.stack) != 2 {
		t.Fatalf("setup: want depth 2, got %d", len(m.stack))
	}
	m2, _ = m.Update(keyEsc())
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("esc should pop help back to root, got depth %d", len(m.stack))
	}
}

func keyEsc() tea.KeyMsg { return tea.KeyMsg(tea.Key{Type: tea.KeyEsc}) }
```

Note: this test file needs the `tea` import for `keyEsc`. Add it to the import block:

```go
import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestQuestionMark|TestEscPopsHelpOverlay' -v`
Expected: FAIL — `undefined: helpView` (compile error).

- [ ] **Step 3: Create `internal/tui/help.go`**

```go
package tui

import tea "github.com/charmbracelet/bubbletea"

// helpView is a static, pushed overlay listing the full keymap. It holds no
// state and polls nothing; the root's esc/q pop dismisses it.
type helpView struct{}

func (helpView) Init() tea.Cmd { return nil }

func (helpView) title() string { return "help" }

func (helpView) refresh(API) tea.Cmd { return nil }

func (v helpView) Update(tea.Msg) (tea.Model, tea.Cmd) { return v, nil }

func (helpView) View() string {
	return "  Global      esc back/cancel · q quit (root) / back · r refresh · ctrl+c quit\n" +
		"  Apps list   ↑/k ↓/j move · enter open · n new app\n" +
		"  App detail  ↑/k ↓/j move · enter logs · d deploy · s stop · x delete\n" +
		"  Logs        f toggle follow · esc back\n\n" +
		"  esc back"
}
```

- [ ] **Step 4: Wire the `?` key in the root**

In `internal/tui/app.go`, inside the `if !m.topCapturesText() {` block (currently `app.go:82-93`, holding the `q` and `r` cases), add a `?` case:

```go
			case "?":
				if _, ok := m.top().(helpView); !ok {
					return m, func() tea.Msg { return pushMsg{helpView{}} }
				}
				return m, nil
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestQuestionMark|TestEscPopsHelpOverlay' -v`
Expected: PASS (all four).

- [ ] **Step 6: Run the full package**

Run: `go test ./internal/tui/`
Expected: `ok  github.com/getpiper/piper/internal/tui`

- [ ] **Step 7: Commit**

```bash
git add internal/tui/help.go internal/tui/app.go internal/tui/help_test.go
git commit -m "feat(cli): add ? help overlay with full keymap to the TUI

Part of #196

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Record progress and verify

Update `PROGRESS.md` and run the full CI-mirroring gate.

**Files:**
- Modify: `PROGRESS.md` — one line under the TUI area referencing #196.

- [ ] **Step 1: Find the TUI line in PROGRESS.md**

Run: `grep -n "TUI\|tui\|#183\|#196" PROGRESS.md`
Expected: shows the existing Interactive TUI epic line(s) to append near.

- [ ] **Step 2: Add the progress entry**

Add a terse one-line entry (matching the file's existing `[#N]` style) noting key discoverability landed, e.g.:

```
- TUI key discoverability: footer legend + `?` help overlay [#196]
```

Place it alongside the other TUI phase entries. Keep it one line — detail lives in the issue.

- [ ] **Step 3: Run the full verify gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests pass, cross-compile succeeds. If gofmt flags files, run `make fmt` and re-run `make verify`.

- [ ] **Step 4: Commit**

```bash
git add PROGRESS.md
git commit -m "docs: record TUI key discoverability in PROGRESS.md

Part of #196

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:**
- Contextual footer on apps list + app detail → Task 1 (both `footer()` methods + root render). ✓
- Footer is dim, one line, always visible, between body and status bar → Task 1 Step 3 (`footerStyle.Faint`, inserted before `statusBar()`). ✓
- Modals keep their own inline hints untouched → Task 1 `TestModalViewsRenderNoFooterLegend`; no modal files modified. ✓
- `?` help overlay, full keymap, pushed view, `esc` dismiss → Task 2 (`helpView` + wiring + `TestEscPopsHelpOverlay`). ✓
- `?` gated behind `topCapturesText()` so text fields get a literal `?` → Task 2 Step 4 + `TestQuestionMarkIsLiteralInTextFields`. ✓
- Stacking guard → Task 2 `TestQuestionMarkDoesNotStackHelp`. ✓
- `t` boxes deferred to phase 5 → not in keymap; noted in spec "out of scope". ✓
- PROGRESS.md update → Task 3. ✓

**Placeholder scan:** No TBD/TODO; every code step shows full code. ✓

**Type consistency:** `footered`/`footer()`/`topFooter()` consistent across Task 1 steps and the root render. `helpView{}` (zero-value struct) used identically in `pushMsg{helpView{}}`, the type assertion `m.top().(helpView)`, and tests. `keyRunes('?')`, `keyEnter`, `pump`, `fakeAPI`, `newFormView`, `newAppDetailView` all reuse existing helpers verified in `app_test.go` / the view files. ✓
