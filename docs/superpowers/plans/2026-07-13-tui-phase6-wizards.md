# TUI Phase 6 — Wizards Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the three flows the TUI still shells out to the CLI for — **login** (LAN token), **GitHub App setup**, and **link repo** — plus an unauthenticated hint on the apps home, as full-screen views in the existing Bubble Tea stack.

**Architecture:** Three new leaf views (`login.go`, `github.go`, `linkform.go`) alongside the existing per-view files, each mapping onto a method the CLI subcommands already use. The `tui.API` interface gains `Manifest`/`ExchangeGitHub`/`LinkApp` (already on `*client.Client`); login reuses the phase-5 `saveBox` + `boxSavedMsg` re-dial path, so it needs no new API method. A one-line `StatusError.Unauthorized()` helper in `internal/client`, consumed through a local anonymous interface, lets the apps view show a "press L" hint on a 401 without `tui` importing `internal/client`.

**Tech Stack:** Go, Bubble Tea (`github.com/charmbracelet/bubbletea`), Bubbles (`github.com/charmbracelet/bubbles/textinput`, `.../spinner`), Lip Gloss, `internal/config` (schema v2), `internal/client`.

## Global Constraints

- **No cgo.** All builds pass with `CGO_ENABLED=0`. No new third-party dependencies beyond the already-present bubbletea/bubbles/lipgloss/config tree (the `spinner` bubble ships in the same `github.com/charmbracelet/bubbles` module already vendored via `textinput`).
- **Module path** `github.com/getpiper/piper`.
- **Layering:** `internal/tui` change plus one surgical `internal/client` addition (`StatusError.Unauthorized()`). `tui` may import `internal/config` (down-dep, for the `Box` type) but **must not** import `internal/client` — the 401 check goes through a local anonymous interface. No `piperd`/`piper-relay` change, no API *wire* change (the three client methods already exist). Nothing imports up.
- **Login is LAN-token only — interim.** Relay login (device-flow → account credential) is a later phase. Write the login view with a target-type seam and a code comment flagging the interim split; do **not** build the relay branch here.
- **Deployment status strings** (`"building"`, `"running"`, `"failed"`, `"stopped"`) unchanged; not touched here.
- **Commits:** conventional-commit style, one per task, `Part of #200` in the body (phase-6 child issue), ending with:
  ```
  Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
  ```
- **Verify gate:** `make verify` (gofmt → vet → test → cross) must pass before the work is done. If gofmt flags files, run `make fmt` and re-run.
- **Existing test helpers** (in `internal/tui/app_test.go` and `internal/tui/boxes_test.go`, same package — reuse, do not redefine): `keyRunes(r rune) tea.KeyMsg`, `keyEnter()`, `keyBackspace()`, `keyTab()`, `pump(t, m, cmd) Model`, the `fakeAPI` struct + `apiCalls` recorder, `seedConfig(t, config.ClientFile)`, and `fakeDialer(c API, addr string, remote bool, err error) Dialer`.
- **Config on disk:** `config.LoadClientFile()`/`config.SaveClientFile(cf)` resolve the path from `os.UserHomeDir()` (`~/.piper/piper/config.json`). Config-mutation tests set `HOME` via `seedConfig` and assert the on-disk `ClientFile`.
- **`config.Box` fields:** `Name`, `Addr`, `Token`, `RelayAPI`, `AccountCredential`.

---

### Task 1: `StatusError.Unauthorized()` + apps unauthenticated hint

Add the 401 classifier to `internal/client`, a `tui`-local wrapper that detects it without importing the client, and render a "press L" hint on the apps home when the last poll was a 401.

**Files:**
- Modify: `internal/client/client.go` (add method after `StatusError.Error`, ~line 307)
- Test: `internal/client/client_test.go` (add one test; package `client`)
- Create: `internal/tui/auth.go` (the `isUnauthorized` helper)
- Modify: `internal/tui/apps.go` (hint bar in `View`)
- Test: `internal/tui/apps_test.go` (add tests)

**Interfaces:**
- Produces:
  - `func (e *StatusError) Unauthorized() bool` — reports HTTP 401 (in `internal/client`).
  - `func isUnauthorized(err error) bool` — true iff `err` (or a wrapped error) satisfies `interface{ Unauthorized() bool }` and returns true (in `internal/tui`).

- [ ] **Step 1: Write the failing client test**

Add to `internal/client/client_test.go`:

```go
func TestStatusErrorUnauthorized(t *testing.T) {
	if !(&StatusError{Code: http.StatusUnauthorized}).Unauthorized() {
		t.Fatal("401 StatusError should report Unauthorized")
	}
	if (&StatusError{Code: http.StatusInternalServerError}).Unauthorized() {
		t.Fatal("500 StatusError should not report Unauthorized")
	}
}
```

If `net/http` is not already imported in `client_test.go`, add it to the import block.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/client/ -run TestStatusErrorUnauthorized`
Expected: FAIL — `(*StatusError).Unauthorized undefined`.

- [ ] **Step 3: Add the method**

In `internal/client/client.go`, immediately after the `func (e *StatusError) Error()` method (`http` is already imported by this file):

```go
// Unauthorized reports whether the server rejected the request as
// unauthenticated (HTTP 401), letting callers tell a bad or absent token from
// other HTTP errors and from transport errors (which are never a StatusError).
func (e *StatusError) Unauthorized() bool { return e.Code == http.StatusUnauthorized }
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/client/ -run TestStatusErrorUnauthorized`
Expected: PASS.

- [ ] **Step 5: Write the failing tui tests**

Add to `internal/tui/apps_test.go` (reuse `NewModel`, `pump`, `fakeAPI`; import `errors` if not present):

```go
// a401 is a stand-in for *client.StatusError{Code:401} — the tui must classify
// it without importing internal/client, so a local type satisfying the same
// interface exercises isUnauthorized and the hint bar.
type a401 struct{}

func (a401) Error() string       { return "unauthorized" }
func (a401) Unauthorized() bool  { return true }

func TestAppsUnauthorizedShowsLoginHint(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{err: a401{}})
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "press L") {
		t.Fatalf("expected login hint, got:\n%s", out)
	}
	if strings.Contains(out, "⚠") {
		t.Fatalf("401 should show the hint, not a raw error banner:\n%s", out)
	}
}

func TestAppsNonAuthErrorStillBanners(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{err: errors.New("dial tcp: refused")})
	m = pump(t, m, m.refresh())
	out := m.View()
	if strings.Contains(out, "press L") {
		t.Fatalf("a transport error must not show the login hint:\n%s", out)
	}
	if !strings.Contains(out, "⚠") {
		t.Fatalf("expected an error banner, got:\n%s", out)
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/tui/ -run 'TestAppsUnauthorizedShowsLoginHint|TestAppsNonAuthErrorStillBanners'`
Expected: FAIL — `undefined: isUnauthorized` (compile) and/or missing "press L".

- [ ] **Step 7: Add the `isUnauthorized` helper**

Create `internal/tui/auth.go`:

```go
package tui

import "errors"

// isUnauthorized reports whether err (or an error it wraps) is an HTTP-401
// rejection. It matches on behaviour, not type, so the tui classifies a
// *client.StatusError without importing internal/client (preserving the
// API/Dialer seam the earlier phases established).
func isUnauthorized(err error) bool {
	var u interface{ Unauthorized() bool }
	return errors.As(err, &u) && u.Unauthorized()
}
```

- [ ] **Step 8: Render the hint in the apps view**

In `internal/tui/apps.go`, replace the error-banner block at the top of `View` (currently `if v.err != nil { fmt.Fprintf(&b, " ⚠ %v\n\n", v.err) }`) with:

```go
	if v.err != nil {
		if isUnauthorized(v.err) {
			b.WriteString(" not logged in — press L to log in\n\n")
		} else {
			fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
		}
	}
```

- [ ] **Step 9: Run the tui tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestAppsUnauthorizedShowsLoginHint|TestAppsNonAuthErrorStillBanners'`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go internal/tui/auth.go internal/tui/apps.go internal/tui/apps_test.go
git commit -m "feat(cli): TUI apps home hints at login on a 401 poll

Part of #200

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Login wizard (`L` from apps home, LAN token only)

A masked-token leaf view that verifies with an authenticated `ListApps` probe, then saves the token to the current box and re-dials — reusing the phase-5 `saveBox` + `boxSavedMsg` machinery. Wire `L` as a root global key.

**Files:**
- Create: `internal/tui/login.go`
- Modify: `internal/tui/app.go` (`L` key in the `!m.topCapturesText()` block)
- Modify: `internal/tui/apps.go` (add `L login` to the footer legend)
- Test: `internal/tui/login_test.go` (new)

**Interfaces:**
- Consumes: `Dialer` (`tui.go`); `saveBox(box config.Box, replacing string) error` and the `boxSavedMsg{box config.Box; replacing string}` handler (`boxes.go`/`app.go`, phase 5); `config.LoadClientFile()` (`internal/config`).
- Produces:
  - `type loginTarget int` with `const targetLAN loginTarget = iota` (interim seam; one value today).
  - `func newLoginView(dial Dialer, box string) loginView` — `box` is the current box's name, for display and to locate it in the config. Satisfies `view`; `title()` → `"login"`; `capturesText()` → `true`; `footer()` → `"↵ verify & save · esc cancel · ? help"`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/login_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/config"
)

// typeText feeds each rune of s to the top view through the model, like a user
// typing into a focused field.
func typeText(t *testing.T, v loginView, s string) loginView {
	t.Helper()
	for _, r := range s {
		next, _ := v.Update(keyRunes(r))
		v = next.(loginView)
	}
	return v
}

func TestLoginVerifiesAndSaves(t *testing.T) {
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "pi4", Addr: "192.168.1.6:8088", Token: "old"}},
		Current: "pi4",
	})
	v := typeText(t, newLoginView(fakeDialer(fakeAPI{}, "192.168.1.6:8088", false, nil), "pi4"), "newtok")
	next, cmd := v.Update(keyEnter())
	v = next.(loginView)
	if cmd == nil {
		t.Fatal("enter on a filled token should return a verify+save cmd")
	}
	msg := cmd()
	saved, ok := msg.(boxSavedMsg)
	if !ok {
		t.Fatalf("want boxSavedMsg, got %T (%v)", msg, msg)
	}
	if saved.box.Token != "newtok" || saved.replacing != "pi4" {
		t.Fatalf("unexpected save: %+v", saved)
	}
	cf, err := config.LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if cf.Boxes[0].Token != "newtok" {
		t.Fatalf("token not persisted: %+v", cf.Boxes[0])
	}
}

func TestLoginBadTokenBannersNoWrite(t *testing.T) {
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "pi4", Addr: "a", Token: "old"}},
		Current: "pi4",
	})
	v := typeText(t, newLoginView(fakeDialer(fakeAPI{err: errors.New("401")}, "a", false, nil), "pi4"), "bad")
	next, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("expected a verify cmd")
	}
	// The verify cmd fails the ListApps probe → errMsg, which the view banners.
	next, _ = next.(loginView).Update(cmd())
	v = next.(loginView)
	if !strings.Contains(v.View(), "⚠") {
		t.Fatalf("expected an error banner, got:\n%s", v.View())
	}
	cf, _ := config.LoadClientFile()
	if cf.Boxes[0].Token != "old" {
		t.Fatalf("token must not change on a failed probe: %+v", cf.Boxes[0])
	}
}

func TestLoginEmptyTokenRejected(t *testing.T) {
	v := newLoginView(fakeDialer(fakeAPI{}, "a", false, nil), "pi4")
	next, cmd := v.Update(keyEnter())
	if cmd != nil {
		t.Fatal("empty token should not run a probe")
	}
	if !strings.Contains(next.(loginView).View(), "token required") {
		t.Fatalf("expected 'token required', got:\n%s", next.(loginView).View())
	}
}

func TestLKeyOpensLogin(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	next, _ := m.Update(keyRunes('L'))
	m = next.(Model)
	// pushMsg is emitted via a cmd; run it and feed it back.
	_, cmd := m.Update(keyRunes('L'))
	_ = cmd
	m2 := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	nn, c := m2.Update(keyRunes('L'))
	m2 = pump(t, nn.(Model), c)
	if _, ok := m2.top().(loginView); !ok {
		t.Fatalf("L should push the login view, got %T", m2.top())
	}
}
```

> Note: `TestLKeyOpensLogin` above is intentionally simple — the reliable assertion is the last block (press `L`, `pump` the returned cmd, assert the top view). Keep that block; the earlier lines are scaffolding and may be trimmed.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/tui/ -run 'TestLogin|TestLKeyOpensLogin'`
Expected: FAIL — `undefined: newLoginView` / `undefined: loginView`.

- [ ] **Step 3: Write the login view**

Create `internal/tui/login.go`:

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/config"
)

// loginTarget selects the login flow. Only targetLAN exists today: the wizard
// takes a token from `piperd token create` and saves it to the current box.
// Relay login (GitHub device-flow → account credential) is a later phase; it
// slots in as a second target here without changing this view's shape.
type loginTarget int

const targetLAN loginTarget = iota

// loginView authenticates the current box. LAN only for now: enter a token,
// verify it with an authenticated ListApps probe against the current box's
// addr, and — on success — persist it to that box and re-dial the session.
type loginView struct {
	dial   Dialer
	box    string // current box name: display + config lookup
	target loginTarget
	token  textinput.Model
	err    error
}

func newLoginView(dial Dialer, box string) loginView {
	token := textinput.New()
	token.Placeholder = "token"
	token.EchoMode = textinput.EchoPassword
	token.Focus()
	return loginView{dial: dial, box: box, target: targetLAN, token: token}
}

func (v loginView) Init() tea.Cmd { return nil }

func (v loginView) title() string { return "login" }

func (v loginView) refresh(API) tea.Cmd { return nil }

func (v loginView) capturesText() bool { return true }

func (v loginView) footer() string { return "↵ verify & save · esc cancel · ? help" }

// submit verifies the token against the current box and, on success, saves it
// and emits boxSavedMsg{replacing: box} — the root re-dials the current box via
// the same path a phase-5 box edit uses.
func (v loginView) submit() (tea.Model, tea.Cmd) {
	token := strings.TrimSpace(v.token.Value())
	if token == "" {
		v.err = fmt.Errorf("token required")
		return v, nil
	}
	dial, name := v.dial, v.box
	return v, func() tea.Msg {
		cf, err := config.LoadClientFile()
		if err != nil {
			return errMsg{err}
		}
		box, ok := currentBox(cf, name)
		if !ok {
			return errMsg{fmt.Errorf("box %q not found in config", name)}
		}
		box.Token = token
		c, _, _, err := dial(box)
		if err == nil {
			_, err = c.ListApps()
		}
		if err != nil {
			return errMsg{err}
		}
		if err := saveBox(box, box.Name); err != nil {
			return errMsg{err}
		}
		return boxSavedMsg{box: box, replacing: box.Name}
	}
}

func (v loginView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case tea.KeyMsg:
		if msg.String() == "enter" {
			return v.submit()
		}
	}
	v.err = nil // any keystroke clears a stale banner
	var cmd tea.Cmd
	v.token, cmd = v.token.Update(msg)
	return v, cmd
}

func (v loginView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  log in to %s\n\n", v.box)
	fmt.Fprintf(&b, "  token  %s\n\n", v.token.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ verify & save   esc cancel")
	return b.String()
}

// currentBox returns the box named name from cf (falling back to cf.Current),
// so login edits the box the user is authenticated against.
func currentBox(cf config.ClientFile, name string) (config.Box, bool) {
	for _, b := range cf.Boxes {
		if b.Name == name {
			return b, true
		}
	}
	for _, b := range cf.Boxes {
		if b.Name == cf.Current {
			return b, true
		}
	}
	return config.Box{}, false
}
```

- [ ] **Step 4: Wire `L` at the root**

In `internal/tui/app.go`, inside the `if !m.topCapturesText() { switch msg.String() { ... } }` block (alongside the existing `"t"` case), add:

```go
			case "L":
				if _, ok := m.top().(loginView); !ok {
					return m, func() tea.Msg { return pushMsg{newLoginView(m.dial, m.box)} }
				}
				return m, nil
```

- [ ] **Step 5: Add `L login` to the apps footer**

In `internal/tui/apps.go`, change the `footer()` return to include `L`:

```go
func (v appsView) footer() string {
	return "n new · L login · g github · t boxes · ↵ open · r refresh · q quit · ? help"
}
```

(`g github` is wired in Task 4; adding it to the legend now keeps the footer stable across the two tasks.)

- [ ] **Step 6: Run the login tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestLogin|TestLKeyOpensLogin'`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/login.go internal/tui/login_test.go internal/tui/app.go internal/tui/apps.go
git commit -m "feat(cli): TUI login wizard — verify a LAN token and save to the current box

LAN token only for now; relay login lands later via the target seam.
Part of #200

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Link repo (`l` from app detail)

A two-field form (repo, branch) that emits `linkAppMsg`; the root runs `LinkApp` off the UI thread and reports via the existing `actionResultMsg`. Widen the `API` interface with `LinkApp`.

**Files:**
- Modify: `internal/tui/tui.go` (add `linkAppMsg`; add `LinkApp` to the `API` interface)
- Modify: `internal/tui/app.go` (handle `linkAppMsg`)
- Modify: `internal/tui/appdetail.go` (`l` key + footer legend)
- Create: `internal/tui/linkform.go`
- Test: `internal/tui/app_test.go` (add `LinkApp` to `fakeAPI` + recorder fields)
- Test: `internal/tui/linkform_test.go` (new)

**Interfaces:**
- Consumes: `actionResultMsg{err error; popLevels int}` handler (`app.go`, phase 4).
- Produces:
  - `API.LinkApp(name, repo, branch string) error` (interface method; `*client.Client` already implements it).
  - `linkAppMsg struct{ name, repo, branch string }` (`tui.go`).
  - `func newLinkForm(app string) linkFormView` — satisfies `view`; `title()` → `"link"`; `capturesText()` → `true`; `footer()` → `"↵ link · tab switch · esc cancel · ? help"`.

- [ ] **Step 1: Extend `fakeAPI` with `LinkApp`**

In `internal/tui/app_test.go`, add recorder fields to `apiCalls`:

```go
	linkName   string
	linkRepo   string
	linkBranch string
```

add a `linkErr error` field to `fakeAPI` (next to `deleteErr`), and add the method:

```go
func (f fakeAPI) LinkApp(name, repo, branch string) error {
	if f.rec != nil {
		f.rec.linkName, f.rec.linkRepo, f.rec.linkBranch = name, repo, branch
	}
	return f.linkErr
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/tui/linkform_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

func typeLinkRepo(t *testing.T, v linkFormView, s string) linkFormView {
	t.Helper()
	for _, r := range s {
		next, _ := v.Update(keyRunes(r))
		v = next.(linkFormView)
	}
	return v
}

func TestLinkFormSubmitEmitsLinkAppMsg(t *testing.T) {
	v := typeLinkRepo(t, newLinkForm("blog"), "octo/blog")
	next, cmd := v.Update(keyEnter())
	_ = next
	if cmd == nil {
		t.Fatal("a filled repo should emit a link cmd")
	}
	msg, ok := cmd().(linkAppMsg)
	if !ok {
		t.Fatalf("want linkAppMsg, got %T", cmd())
	}
	if msg.name != "blog" || msg.repo != "octo/blog" || msg.branch != "main" {
		t.Fatalf("unexpected linkAppMsg: %+v", msg)
	}
}

func TestLinkFormEmptyRepoRejected(t *testing.T) {
	next, cmd := newLinkForm("blog").Update(keyEnter())
	if cmd != nil {
		t.Fatal("empty repo should not emit a cmd")
	}
	if !strings.Contains(next.(linkFormView).View(), "repo is required") {
		t.Fatalf("expected 'repo is required', got:\n%s", next.(linkFormView).View())
	}
}

func TestLinkAppRootRunsClientAndPops(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("pi4", "a", false, fakeAPI{rec: rec})
	// push app detail then the link form so a successful link pops back to detail
	m, _ = pushView(t, m, newAppDetailView("blog", false))
	m, _ = pushView(t, m, newLinkForm("blog"))
	depth := len(m.stack)
	next, cmd := m.Update(linkAppMsg{name: "blog", repo: "octo/blog", branch: "main"})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("expected LinkApp to run as a cmd")
	}
	m = pump(t, m, cmd) // actionResultMsg{nil,1}
	if rec.linkRepo != "octo/blog" || rec.linkName != "blog" {
		t.Fatalf("LinkApp not called with the right args: %+v", rec)
	}
	if len(m.stack) != depth-1 {
		t.Fatalf("success should pop one level: was %d now %d", depth, len(m.stack))
	}
}

func TestLKeyLowercaseOpensLinkFromDetail(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{})
	m, _ = pushView(t, m, newAppDetailView("blog", false))
	next, cmd := m.top().Update(keyRunes('l'))
	m.stack[len(m.stack)-1] = next.(view)
	m = pump(t, m, cmd)
	if _, ok := m.top().(linkFormView); !ok {
		t.Fatalf("l from app detail should push the link form, got %T", m.top())
	}
}
```

Add this small helper to `internal/tui/linkform_test.go` (it pushes a view through the root the way the runtime does):

```go
// pushView emits a pushMsg for v and applies it, returning the model with v on
// top. It mirrors what the root does when a view requests navigation.
func pushView(t *testing.T, m Model, v view) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(pushMsg{view: v})
	return next.(Model), cmd
}
```

and add `tea "github.com/charmbracelet/bubbletea"` to that file's imports.

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/tui/ -run 'TestLinkForm|TestLinkApp|TestLKeyLowercase'`
Expected: FAIL — `undefined: newLinkForm` / `undefined: linkAppMsg`.

- [ ] **Step 4: Add `linkAppMsg` and the `LinkApp` interface method**

In `internal/tui/tui.go`, add `LinkApp` to the `API` interface (after `DeleteApp`):

```go
	LinkApp(name, repo, branch string) error
```

and add the message type in the message `type (...)` block (near `createAppMsg`):

```go
	// linkAppMsg is the link form's intent; the root runs LinkApp off the UI
	// thread and reports via actionResultMsg (pop back to app detail on success).
	linkAppMsg struct{ name, repo, branch string }
```

- [ ] **Step 5: Handle `linkAppMsg` at the root**

In `internal/tui/app.go`, add a case alongside `createAppMsg`:

```go
	case linkAppMsg:
		name, repo, branch, c := msg.name, msg.repo, msg.branch, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.LinkApp(name, repo, branch), popLevels: 1} }
```

- [ ] **Step 6: Write the link form**

Create `internal/tui/linkform.go`:

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// linkFormView attaches a git repo to an app: repo (owner/name) and branch
// (default main). On submit it emits linkAppMsg; the root runs LinkApp and pops
// back to app detail on success.
type linkFormView struct {
	app    string
	repo   textinput.Model
	branch textinput.Model
	focus  int // 0 repo, 1 branch
	err    error
}

func newLinkForm(app string) linkFormView {
	repo := textinput.New()
	repo.Placeholder = "owner/name"
	repo.Focus()
	branch := textinput.New()
	branch.Placeholder = "main"
	branch.SetValue("main")
	return linkFormView{app: app, repo: repo, branch: branch}
}

func (v linkFormView) Init() tea.Cmd { return nil }

func (v linkFormView) title() string { return "link" }

func (v linkFormView) refresh(API) tea.Cmd { return nil }

func (v linkFormView) capturesText() bool { return true }

func (v linkFormView) footer() string { return "↵ link · tab switch · esc cancel · ? help" }

func (v *linkFormView) applyFocus() {
	if v.focus == 0 {
		v.repo.Focus()
		v.branch.Blur()
	} else {
		v.branch.Focus()
		v.repo.Blur()
	}
}

func (v linkFormView) submit() (tea.Model, tea.Cmd) {
	repo := strings.TrimSpace(v.repo.Value())
	if repo == "" {
		v.err = fmt.Errorf("repo is required")
		return v, nil
	}
	branch := strings.TrimSpace(v.branch.Value())
	if branch == "" {
		branch = "main"
	}
	name := v.app
	return v, func() tea.Msg { return linkAppMsg{name: name, repo: repo, branch: branch} }
}

func (v linkFormView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	v.err = nil // any keystroke clears a stale banner
	var cmd tea.Cmd
	if v.focus == 0 {
		v.repo, cmd = v.repo.Update(msg)
	} else {
		v.branch, cmd = v.branch.Update(msg)
	}
	return v, cmd
}

func (v linkFormView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  link %s to a repo\n\n", v.app)
	fmt.Fprintf(&b, "  repo    %s\n", v.repo.View())
	fmt.Fprintf(&b, "  branch  %s\n\n", v.branch.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ link   tab switch   esc cancel")
	return b.String()
}
```

- [ ] **Step 7: Wire `l` and the footer in app detail**

In `internal/tui/appdetail.go`, add a case to the `tea.KeyMsg` switch in `Update` (alongside `"d"`, `"s"`, `"x"`):

```go
		case "l":
			return v, func() tea.Msg { return pushMsg{newLinkForm(v.name)} }
```

and update `footer()`:

```go
func (v appDetailView) footer() string {
	return "d deploy · s stop · x delete · l link · ↵ logs · r refresh · esc back · ? help"
}
```

- [ ] **Step 8: Run the link tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestLinkForm|TestLinkApp|TestLKeyLowercase'`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/tui/tui.go internal/tui/app.go internal/tui/appdetail.go internal/tui/linkform.go internal/tui/linkform_test.go internal/tui/app_test.go
git commit -m "feat(cli): TUI link-repo form — attach a repo to an app from app detail

Part of #200

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: GitHub setup wizard (`g` from apps home)

An org-input leaf view that runs the GitHub App manifest flow (mirroring `cmd/piper`'s `githubSetup`) bridged into a `tea.Cmd`, showing a spinner while it waits for the browser callback. Widen the `API` interface with `Manifest`/`ExchangeGitHub`.

**Files:**
- Modify: `internal/tui/tui.go` (add `githubDoneMsg`; add `Manifest`/`ExchangeGitHub` to the `API` interface)
- Modify: `internal/tui/app.go` (`g` key in the `!m.topCapturesText()` block)
- Create: `internal/tui/github.go`
- Test: `internal/tui/app_test.go` (add `Manifest`/`ExchangeGitHub` to `fakeAPI`)
- Test: `internal/tui/github_test.go` (new)

**Interfaces:**
- Consumes: `popMsg{n int}` handler (`app.go`, phase 4).
- Produces:
  - `API.Manifest(redirectURL string) (string, error)` and `API.ExchangeGitHub(code string) error` (interface methods; `*client.Client` already implements both).
  - `githubDoneMsg struct{ err error }` (`tui.go`).
  - `func newGithubView() githubView` — satisfies `view`; `title()` → `"github"`; `capturesText()` → `true`; `footer()` → `"↵ start · esc cancel · ? help"`.
  - `func manifestActionURL(org string) string` — the `settings/apps/new` URL (personal or org).

- [ ] **Step 1: Extend `fakeAPI` with `Manifest`/`ExchangeGitHub`**

In `internal/tui/app_test.go`, add fields to `fakeAPI` (near `deleteErr`):

```go
	manifest    string
	manifestErr error
	exchangeErr error
```

and the methods:

```go
func (f fakeAPI) Manifest(string) (string, error) { return f.manifest, f.manifestErr }
func (f fakeAPI) ExchangeGitHub(string) error     { return f.exchangeErr }
```

- [ ] **Step 2: Write the failing tests**

Create `internal/tui/github_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"
)

func TestManifestActionURL(t *testing.T) {
	if got := manifestActionURL(""); got != "https://github.com/settings/apps/new" {
		t.Fatalf("personal URL wrong: %s", got)
	}
	if got := manifestActionURL("acme"); got != "https://github.com/organizations/acme/settings/apps/new" {
		t.Fatalf("org URL wrong: %s", got)
	}
}

func TestGithubDoneSuccessPops(t *testing.T) {
	next, cmd := newGithubView().Update(githubDoneMsg{err: nil})
	_ = next
	if cmd == nil {
		t.Fatal("success should emit a pop cmd")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Fatalf("want popMsg on success, got %T", cmd())
	}
}

func TestGithubDoneErrorBanners(t *testing.T) {
	next, cmd := newGithubView().Update(githubDoneMsg{err: errors.New("exchange failed")})
	if cmd != nil {
		t.Fatalf("an error should not pop, got a cmd")
	}
	if !strings.Contains(next.(githubView).View(), "⚠") {
		t.Fatalf("expected an error banner, got:\n%s", next.(githubView).View())
	}
}

func TestGKeyOpensGithub(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	next, cmd := m.Update(keyRunes('g'))
	m = pump(t, next.(Model), cmd)
	if _, ok := m.top().(githubView); !ok {
		t.Fatalf("g should push the github view, got %T", m.top())
	}
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/tui/ -run 'TestManifestActionURL|TestGithubDone|TestGKeyOpensGithub'`
Expected: FAIL — `undefined: newGithubView` / `undefined: manifestActionURL` / `undefined: githubDoneMsg`.

- [ ] **Step 4: Add `githubDoneMsg` and the interface methods**

In `internal/tui/tui.go`, add to the `API` interface (after `LinkApp` from Task 3):

```go
	Manifest(redirectURL string) (string, error)
	ExchangeGitHub(code string) error
```

and add the message type (near `deployStartedMsg`):

```go
	// githubDoneMsg is the manifest flow's outcome: nil pops back to apps, an
	// error banners in the github view.
	githubDoneMsg struct{ err error }
```

- [ ] **Step 5: Write the github view**

Create `internal/tui/github.go`:

```go
package tui

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// githubView runs the GitHub App manifest flow: enter an org (blank = personal
// account), press ↵, and it serves a local auto-submitting form that POSTs the
// manifest to GitHub, catches the ?code= redirect, and exchanges it for App
// credentials on the box. It mirrors cmd/piper's `github setup`, bridged into a
// tea.Cmd. The socket plumbing runs in runManifestFlow (below Update), so Update
// stays a pure (msg) -> (model, cmd) machine.
type githubView struct {
	org     textinput.Model
	running bool
	formURL string
	spin    spinner.Model
	err     error
}

func newGithubView() githubView {
	org := textinput.New()
	org.Placeholder = "org (blank for your personal account)"
	org.Focus()
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return githubView{org: org, spin: sp}
}

func (v githubView) Init() tea.Cmd { return nil }

func (v githubView) title() string { return "github" }

func (v githubView) refresh(API) tea.Cmd { return nil }

func (v githubView) capturesText() bool { return true }

func (v githubView) footer() string { return "↵ start · esc cancel · ? help" }

func (v githubView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err, v.running = msg.err, false
		return v, nil
	case githubDoneMsg:
		if msg.err != nil {
			v.err, v.running = msg.err, false
			return v, nil
		}
		return v, func() tea.Msg { return popMsg{n: 1} }
	case spinner.TickMsg:
		var cmd tea.Cmd
		v.spin, cmd = v.spin.Update(msg)
		return v, cmd
	case tea.KeyMsg:
		if msg.String() == "enter" && !v.running {
			return v.start()
		}
	}
	if v.running {
		return v, nil // ignore field edits mid-flow
	}
	v.err = nil
	var cmd tea.Cmd
	v.org, cmd = v.org.Update(msg)
	return v, cmd
}

// start is not a method on API-only state: it needs the client for Manifest/
// ExchangeGitHub, which the view does not hold. The root owns the client, so the
// flow is kicked off by the root instead — see the note in Step 6. start here
// only flips to the running state and returns the flow cmd stub.
func (v githubView) start() (tea.Model, tea.Cmd) {
	v.running = true
	return v, v.spin.Tick
}

func (v githubView) View() string {
	var b strings.Builder
	b.WriteString("  configure a GitHub App\n\n")
	if v.running {
		fmt.Fprintf(&b, "  %s waiting for GitHub App approval…\n", v.spin.View())
		if v.formURL != "" {
			fmt.Fprintf(&b, "  %s\n", v.formURL)
		}
		b.WriteString("\n  esc cancel")
		if v.err != nil {
			fmt.Fprintf(&b, "\n\n ⚠ %v", v.err)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "  org   %s\n\n", v.org.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ start   esc cancel")
	return b.String()
}

// manifestActionURL is the GitHub endpoint the manifest form POSTs to: the
// personal-account creator, or the org creator when org is non-empty.
func manifestActionURL(org string) string {
	if org == "" {
		return "https://github.com/settings/apps/new"
	}
	return fmt.Sprintf("https://github.com/organizations/%s/settings/apps/new", url.PathEscape(org))
}

// openBrowser opens rawURL in the OS browser. Duplicated from cmd/piper (that
// copy is unexported in package main); a package var so tests can stub it.
var openBrowser = func(rawURL string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", rawURL).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	default:
		return exec.Command("xdg-open", rawURL).Start()
	}
}

// runManifestFlow drives the two-server manifest dance and returns a
// githubDoneMsg. It mirrors cmd/piper/main.go's githubSetup: a callback server
// catches GitHub's ?code=, a form server serves an auto-submitting POST of the
// manifest, the browser opens at the form, and the code is exchanged. It is
// exercised by the CLI's githubSetup tests + e2e; unit tests here drive the
// state machine directly (see github_test.go), not this function.
func runManifestFlow(ctx context.Context, c API, org string) tea.Msg {
	codeCh := make(chan string, 1)
	cbLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return githubDoneMsg{err: err}
	}
	defer cbLn.Close()
	redirect := "http://" + cbLn.Addr().String() + "/cb"

	manifest, err := c.Manifest(redirect)
	if err != nil {
		return githubDoneMsg{err: err}
	}

	cbSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := r.URL.Query().Get("code"); code != "" {
			fmt.Fprintln(w, "Piper GitHub App created. You can close this tab.")
			select {
			case codeCh <- code:
			default:
			}
		}
	})}
	go cbSrv.Serve(cbLn)
	defer cbSrv.Close()

	page := fmt.Sprintf(`<form id="f" action="%s" method="post">`+
		`<input type="hidden" name="manifest" value='%s'></form>`+
		`<script>document.getElementById('f').submit()</script>`,
		html.EscapeString(manifestActionURL(org)), html.EscapeString(manifest))
	formLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return githubDoneMsg{err: err}
	}
	formSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	})}
	go formSrv.Serve(formLn)
	defer formSrv.Close()

	_ = openBrowser("http://" + formLn.Addr().String())

	select {
	case code := <-codeCh:
		return githubDoneMsg{err: c.ExchangeGitHub(code)}
	case <-time.After(5 * time.Minute):
		return githubDoneMsg{err: fmt.Errorf("timed out waiting for GitHub App approval")}
	case <-ctx.Done():
		return githubDoneMsg{err: ctx.Err()}
	}
}
```

> **Import note:** `github.go` uses `textinput` — add `"github.com/charmbracelet/bubbles/textinput"` to its import block (omitted above for brevity; include it).

- [ ] **Step 6: Wire `g` at the root and kick off the flow from the root**

The view can't run `runManifestFlow` itself — that needs the client, which only the root holds. So the root both pushes the view (on `g`) and starts the flow when the view enters its running state.

In `internal/tui/app.go`, add the `g` case in the `!m.topCapturesText()` block:

```go
			case "g":
				if _, ok := m.top().(githubView); !ok {
					return m, func() tea.Msg { return pushMsg{newGithubView()} }
				}
				return m, nil
```

Then, so `↵` inside the github view starts the flow with the root's client, handle the running transition at the root: change `githubView.start()` to signal intent via a message the root turns into the flow cmd. Replace the `start` method body in `github.go` with:

```go
func (v githubView) start() (tea.Model, tea.Cmd) {
	v.running, v.err = true, nil
	v.formURL = ""
	org := strings.TrimSpace(v.org.Value())
	return v, tea.Batch(v.spin.Tick, func() tea.Msg { return githubStartMsg{org: org} })
}
```

add the message to `tui.go`:

```go
	// githubStartMsg is the github view's "run it" intent; the root owns the
	// client, so it launches the manifest flow.
	githubStartMsg struct{ org string }
```

and handle it at the root in `app.go`:

```go
	case githubStartMsg:
		org, c := msg.org, m.client
		return m, func() tea.Msg { return runManifestFlow(context.Background(), c, org) }
```

Add `"context"` to `app.go`'s import block.

- [ ] **Step 7: Run the github tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestManifestActionURL|TestGithubDone|TestGKeyOpensGithub'`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/tui.go internal/tui/app.go internal/tui/github.go internal/tui/github_test.go internal/tui/app_test.go
git commit -m "feat(cli): TUI GitHub setup wizard — manifest flow with a live spinner

Part of #200

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Help overlay rows, PROGRESS, and the verify gate

Surface the three new keys in `?`, record the phase in `PROGRESS.md`, close out the epic checkbox, and run the full gate.

**Files:**
- Modify: `internal/tui/help.go`
- Test: `internal/tui/help_test.go` (add an assertion)
- Modify: `PROGRESS.md`

- [ ] **Step 1: Write the failing help test**

Add to `internal/tui/help_test.go` (match the style of the existing overlay tests in that file):

```go
func TestHelpOverlayIncludesWizardKeys(t *testing.T) {
	out := helpView{}.View()
	for _, want := range []string{"L login", "g github", "l link"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help overlay missing %q:\n%s", want, out)
		}
	}
}
```

If `strings` is not imported in `help_test.go`, add it.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/tui/ -run TestHelpOverlayIncludesWizardKeys`
Expected: FAIL — the overlay lacks the wizard rows.

- [ ] **Step 3: Update the help overlay**

In `internal/tui/help.go`, update the `View()` string so the Apps and App-detail rows list the new keys:

```go
func (helpView) View() string {
	return "  Global      esc back/cancel · q quit (root) / back · r refresh · t boxes · ? help · ctrl+c quit\n" +
		"  Apps list   ↑/k ↓/j move · enter open · n new app · L login · g github\n" +
		"  App detail  ↑/k ↓/j move · enter logs · d deploy · s stop · x delete · l link\n" +
		"  Logs        f toggle follow · esc back\n" +
		"  Boxes       ↑/k ↓/j move · enter connect · a add · e edit · x remove\n\n" +
		"  esc back"
}
```

- [ ] **Step 4: Run the help test to verify it passes**

Run: `go test ./internal/tui/ -run TestHelpOverlayIncludesWizardKeys`
Expected: PASS.

- [ ] **Step 5: Record progress in PROGRESS.md**

In `PROGRESS.md`, append under the TUI epic list (after the `#198` boxes line, before the blank line that ends the list):

```
- ✅ Wizards: login (LAN token, verify → save to current box), GitHub App setup, link repo; unauth hint on apps home — [#200](https://github.com/getpiper/piper/issues/200)
```

- [ ] **Step 6: Run the full verify gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests pass, cross-compile succeeds. If gofmt flags files, run `make fmt` and re-run `make verify`.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/help.go internal/tui/help_test.go PROGRESS.md
git commit -m "docs: record TUI wizards; add login/github/link keys to help overlay

Part of #200

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:**
- API seam — `Manifest`/`ExchangeGitHub`/`LinkApp` added to `tui.API` → Task 3 (LinkApp), Task 4 (Manifest/ExchangeGitHub); `fakeAPI` gains all three. Login needs no new method. ✓
- Login wizard (LAN token, verify-before-save, save to current box, re-dial via `boxSavedMsg`, target-type seam + interim comment) → Task 2. ✓
- Unauthenticated hint on apps home + `StatusError.Unauthorized()` + `isUnauthorized` (no client import) → Task 1. ✓
- GitHub setup wizard (org input, manifest two-server flow mirroring `githubSetup`, spinner, `ExchangeGitHub`, esc cancel via ctx, injected `openBrowser`, pure `manifestActionURL`, flow-runner beside `Update`) → Task 4. ✓
- Link repo (repo/branch form from app detail, `LinkApp` via `actionResultMsg`, pop+refresh) → Task 3. ✓
- Help overlay rows for `L`/`g`/`l` → Task 5. ✓
- Layering: pure `tui` + one `internal/client` one-liner; `tui` never imports `internal/client`; `internal/config` down-dep only → all tasks (401 via anonymous interface; login via `config` + `saveBox`). ✓
- PROGRESS update + epic checkbox → Task 5 / done out-of-band (#200 linked in the epic). ✓

**Placeholder scan:** No TBD/TODO. Every code step shows full code. Two explicit import reminders (Task 4 Step 5 `textinput`; Task 3 Step 2 `tea`) are flagged, not silent. `#200` is the concrete phase-6 child issue (already open), not a placeholder.

**Type consistency:**
- `Dialer` = `func(config.Box) (API, string, bool, error)` — used identically in `newLoginView`/`submit` (Task 2) and matches phase-5 `boxes.go`/`boxform.go`. ✓
- `boxSavedMsg{box config.Box; replacing string}` — login emits it (Task 2) with the exact fields the phase-5 root handler reads. ✓
- `saveBox(box config.Box, replacing string) error` and `config.LoadClientFile()`/`SaveClientFile` — signatures match `boxes.go`. ✓
- `actionResultMsg{err error; popLevels int}` — `linkAppMsg` handler emits it (Task 3) exactly as `createAppMsg` does. ✓
- `popMsg{n int}` — github success emits it (Task 4), matching the phase-4 root handler. ✓
- New views satisfy `view` (`Init`/`Update`/`View`/`refresh(API)`/`title`) plus `capturesText()`/`footer()` where the root type-asserts them; `loginView`/`linkFormView`/`githubView` each implement the full set. ✓
- `fakeAPI` method signatures (`Manifest(string)(string,error)`, `ExchangeGitHub(string)error`, `LinkApp(name,repo,branch string)error`) match the interface additions. ✓
- Key routing: `L`/`g` are root globals in the `!topCapturesText()` block with an already-on-top guard (like `t`/`?`); `l` is an app-detail key (lowercase, distinct from `L`). No collision — apps home has no `l`; app detail has no `L`/`g`. ✓

**Note on the `spinner` bubble:** `github.com/charmbracelet/bubbles/spinner` ships in the same already-vendored `bubbles` module as `textinput`; no new go.mod require. Verify with `go build ./...` in Task 4; if `go.sum` needs the subpackage, `go mod tidy` is the fix (no version change).
