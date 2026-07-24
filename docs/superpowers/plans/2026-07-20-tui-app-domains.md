# TUI Per-App Domains Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface per-app custom domains (list + add/remove, live status) in the TUI app drilldown, per issue #285 and spec `docs/superpowers/specs/2026-07-20-tui-app-domains-design.md`.

**Architecture:** `appDetailView` gains an inline `DOMAINS` table with a unified cursor spanning deployments then domains; keys are context-sensitive by section. Add is a one-field form whose success is replaced by a live-polling domain detail view showing the CNAME. Remove reuses `confirmView` y/n mode. All mutations flow through root-owned intent messages (`app.go`), the pattern every existing action uses.

**Tech Stack:** Go, Bubble Tea (`charmbracelet/bubbletea`), existing `internal/client` methods `AppDomains`/`AddAppDomain`/`RemoveAppDomain`, `internal/domain.AppDomainStatus`.

## Global Constraints

- No cgo; `make verify` (gofmt → vet → test → cross) must pass before pushing.
- Branch `ozykhan/tui-app-domains`, PR into `main`, squash-merge, body carries `Closes #285`.
- One commit per task step, conventional-commit style, co-author trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` (append to every commit below).
- Domain status vocabulary is exactly `pending` / `issuing` / `active` / `failed` (`internal/domain` constants); DNS renders `ok`/`no`; cert expiry renders `2006-01-02` or `-` — same as `piper domains list`.
- Match surrounding TUI style: value-receiver `tea.Model` views, intents as messages, no view mutates the stack.

---

### Task 1: Domains section in the app detail view (poll + render)

**Files:**
- Modify: `internal/tui/tui.go` (API interface, `appDetailLoadedMsg`)
- Modify: `internal/tui/render.go` (add `domainStatusIcon`)
- Modify: `internal/tui/appdetail.go` (field, refresh, View)
- Test: `internal/tui/app_test.go` (fakeAPI grows domain methods), `internal/tui/appdetail_test.go`

**Interfaces:**
- Consumes: `domain.AppDomainStatus` / `domain.DNSRecord` (`internal/domain`), existing view plumbing.
- Produces: `API` gains `AppDomains(app string) ([]domain.AppDomainStatus, error)`, `AddAppDomain(app, dom string) (domain.AppDomainStatus, error)`, `RemoveAppDomain(app, dom string) error`; `appDetailLoadedMsg.domains []domain.AppDomainStatus`; `fakeAPI` fields `domains []domain.AppDomainStatus`, `addSt domain.AppDomainStatus`, `addErr`, `removeErr error` and `apiCalls` fields `addedApp, addedDomain, removedApp, removedDomain string`; `domainStatusIcon(status string) string`; test fixture `fixtureDomains() []domain.AppDomainStatus`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/tui/appdetail_test.go`:

```go
func fixtureDomains() []domain.AppDomainStatus {
	exp := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return []domain.AppDomainStatus{
		{Domain: "blog.example.com", App: "blog", Status: "pending",
			DNSRecords: []domain.DNSRecord{{Type: "CNAME", Name: "blog.example.com", Value: "relay.getpiper.dev"}}},
		{Domain: "www.example.com", App: "blog", Status: "active", CertNotAfter: &exp, DNSOK: true},
	}
}

func TestAppDetailRendersDomainsSection(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps(), domains: fixtureDomains(),
	})
	out := m.View()
	for _, want := range []string{
		"DOMAIN", "CERT EXPIRES", "DNS",
		"blog.example.com", "◌ pending",
		"www.example.com", "● active", "2026-10-01", "ok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestAppDetailDomainsRenderWithoutDeployments(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, domains: fixtureDomains(),
	})
	out := m.View()
	if !strings.Contains(out, "no deployments yet") || !strings.Contains(out, "blog.example.com") {
		t.Fatalf("want empty deployments plus domains table:\n%s", out)
	}
}

func TestAppDetailRefreshIncludesDomains(t *testing.T) {
	msg := newAppDetailView("blog", false).refresh(fakeAPI{domains: fixtureDomains()})()
	loaded, ok := msg.(appDetailLoadedMsg)
	if !ok {
		t.Fatalf("want appDetailLoadedMsg, got %T", msg)
	}
	if len(loaded.domains) != 2 {
		t.Fatalf("want 2 domains, got %d", len(loaded.domains))
	}
}
```

Add `"github.com/piperbox/piper/internal/domain"` to the file's imports.

In `internal/tui/app_test.go`, extend `apiCalls`:

```go
	addedApp      string
	addedDomain   string
	removedApp    string
	removedDomain string
```

extend `fakeAPI`:

```go
	domains   []domain.AppDomainStatus
	addSt     domain.AppDomainStatus
	addErr    error
	removeErr error
```

and append the methods (plus the `internal/domain` import):

```go
func (f fakeAPI) AppDomains(string) ([]domain.AppDomainStatus, error) { return f.domains, f.err }

func (f fakeAPI) AddAppDomain(app, dom string) (domain.AppDomainStatus, error) {
	if f.rec != nil {
		f.rec.addedApp, f.rec.addedDomain = app, dom
	}
	return f.addSt, f.addErr
}

func (f fakeAPI) RemoveAppDomain(app, dom string) error {
	if f.rec != nil {
		f.rec.removedApp, f.rec.removedDomain = app, dom
	}
	return f.removeErr
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestAppDetail' -v`
Expected: compile FAIL — `appDetailLoadedMsg` has no field `domains`, `fakeAPI` doesn't satisfy new interface yet (unknown fields).

- [ ] **Step 3: Implement**

`internal/tui/tui.go` — add to the `API` interface (after `LinkApp`):

```go
	AppDomains(app string) ([]domain.AppDomainStatus, error)
	AddAppDomain(app, dom string) (domain.AppDomainStatus, error)
	RemoveAppDomain(app, dom string) error
```

add `"github.com/piperbox/piper/internal/domain"` to imports, and extend the message:

```go
	appDetailLoadedMsg struct {
		app     api.App
		deps    []store.Deployment
		domains []domain.AppDomainStatus
	}
```

`internal/tui/render.go` — append:

```go
// domainStatusIcon maps a per-app custom-domain status (#285) to its one-glyph
// indicator; unknown values render as "—".
func domainStatusIcon(status string) string {
	switch status {
	case "active":
		return "●"
	case "issuing":
		return "◐"
	case "pending":
		return "◌"
	case "failed":
		return "✗"
	}
	return "—"
}
```

`internal/tui/appdetail.go` — add the field and import:

```go
type appDetailView struct {
	name    string
	remote  bool
	app     api.App
	deps    []store.Deployment
	domains []domain.AppDomainStatus
	cursor  int
	loaded  bool
	err     error
}
```

extend `refresh` (after the `Deployments` call):

```go
		domains, err := c.AppDomains(name)
		if err != nil {
			return errMsg{err}
		}
		return appDetailLoadedMsg{app: app, deps: deps, domains: domains}
```

in `Update`, the `appDetailLoadedMsg` case becomes:

```go
	case appDetailLoadedMsg:
		v.app, v.deps, v.domains, v.loaded, v.err = msg.app, msg.deps, msg.domains, true, nil
		if total := len(v.deps) + len(v.domains); v.cursor >= total {
			v.cursor = max(0, total-1)
		}
```

in `View`, replace the empty/table block (everything after the `loading…` return) with:

```go
	if len(v.deps) == 0 {
		b.WriteString(" no deployments yet\n")
	} else {
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
	}
	if len(v.domains) > 0 {
		fmt.Fprintf(&b, "\n  %-24s %-12s %-13s %s\n", "DOMAIN", "STATUS", "CERT EXPIRES", "DNS")
		for i, d := range v.domains {
			cursor := "  "
			if v.cursor == len(v.deps)+i {
				cursor = "▸ "
			}
			expires := "-"
			if d.CertNotAfter != nil {
				expires = d.CertNotAfter.Format("2006-01-02")
			}
			dns := "no"
			if d.DNSOK {
				dns = "ok"
			}
			status := strings.TrimSpace(domainStatusIcon(d.Status) + " " + d.Status)
			fmt.Fprintf(&b, "%s%-24s %-12s %-13s %s\n", cursor, d.Domain, status, expires, dns)
		}
	}
	return b.String()
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tui/ -v`
Expected: PASS (whole package — the fake must still satisfy `API` everywhere).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): domains section in the app drilldown, polled live

Part of #285"
```

---

### Task 2: Unified cursor + remove flow (`x` on a domain row)

**Files:**
- Modify: `internal/tui/appdetail.go` (cursor bounds, `selectedDomain`, `x` key, footer)
- Modify: `internal/tui/confirm.go` (`newRemoveDomainConfirm`)
- Modify: `internal/tui/tui.go` (`removeDomainMsg`)
- Modify: `internal/tui/app.go` (root `removeDomainMsg` case)
- Test: `internal/tui/appdetail_test.go`, `internal/tui/confirm_test.go`, `internal/tui/app_test.go`

**Interfaces:**
- Consumes: Task 1's `domains` field and fixtures.
- Produces: `removeDomainMsg struct{ app, domain string }`; `newRemoveDomainConfirm(app, dom string) confirmView`; `(appDetailView).selectedDomain() (domain.AppDomainStatus, bool)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/tui/appdetail_test.go`:

```go
func TestAppDetailCursorSpansDomainsAndXRemoves(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps(), domains: fixtureDomains(),
	})
	// two deployments first: j, j lands on the first domain row
	m, _ = m.Update(keyRunes('j'))
	m, _ = m.Update(keyRunes('j'))
	_, cmd := m.Update(keyRunes('x'))
	if cmd == nil {
		t.Fatal("x on a domain row should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if !strings.Contains(pm.view.View(), "Remove blog.example.com") {
		t.Fatalf("want remove-domain confirm, got:\n%s", pm.view.View())
	}
}

func TestAppDetailXOnDeploymentStillDeletesApp(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps(), domains: fixtureDomains(),
	})
	_, cmd := m.Update(keyRunes('x'))
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if !strings.Contains(pm.view.View(), "Delete blog") {
		t.Fatalf("want delete-app confirm, got:\n%s", pm.view.View())
	}
}

func TestAppDetailCursorStopsAtLastDomain(t *testing.T) {
	v, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps(), domains: fixtureDomains(),
	})
	for range 10 {
		v, _ = v.Update(keyRunes('j'))
	}
	if c := v.(appDetailView).cursor; c != 3 { // 2 deps + 2 domains - 1
		t.Fatalf("cursor overran: %d", c)
	}
}
```

Append to `internal/tui/confirm_test.go`:

```go
func TestRemoveDomainConfirmYesEmitsRemove(t *testing.T) {
	v := newRemoveDomainConfirm("blog", "blog.example.com")
	if !strings.Contains(v.View(), "Remove blog.example.com") {
		t.Fatalf("prompt missing:\n%s", v.View())
	}
	_, cmd := v.Update(keyRunes('y'))
	rm, ok := cmd().(removeDomainMsg)
	if !ok || rm.app != "blog" || rm.domain != "blog.example.com" {
		t.Fatalf("want removeDomainMsg{blog, blog.example.com}, got %#v", cmd())
	}
}
```

Append to `internal/tui/app_test.go`:

```go
func TestRootRemoveDomainCallsClientAndPops(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("b", "a", false, fakeAPI{rec: rec})
	m.stack = append(m.stack, newAppDetailView("blog", false), newRemoveDomainConfirm("blog", "blog.example.com"))
	_, cmd := m.Update(removeDomainMsg{app: "blog", domain: "blog.example.com"})
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil || res.popLevels != 1 {
		t.Fatalf("want actionResultMsg{nil, 1}, got %#v", cmd())
	}
	if rec.removedApp != "blog" || rec.removedDomain != "blog.example.com" {
		t.Fatalf("client not called with app+domain: %#v", rec)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestAppDetailCursor|TestAppDetailXOn|TestRemoveDomain|TestRootRemoveDomain' -v`
Expected: compile FAIL — `newRemoveDomainConfirm` and `removeDomainMsg` undefined.

- [ ] **Step 3: Implement**

`internal/tui/tui.go` — add beside the other action intents:

```go
	// removeDomainMsg is the remove-domain confirm's intent; the root runs
	// RemoveAppDomain and pops back to app detail via actionResultMsg.
	removeDomainMsg struct{ app, domain string }
```

`internal/tui/confirm.go` — append:

```go
func newRemoveDomainConfirm(app, dom string) confirmView {
	return confirmView{
		name:   dom,
		prompt: fmt.Sprintf("Remove %s from %s? Its certificate and route are torn down.", dom, app),
		mode:   confirmYesNo,
		intent: func(d string) tea.Msg { return removeDomainMsg{app: app, domain: d} },
	}
}
```

`internal/tui/appdetail.go` — add the section helper:

```go
// selectedDomain returns the domain row under the cursor, if the cursor is in
// the domains section (past the deployments).
func (v appDetailView) selectedDomain() (domain.AppDomainStatus, bool) {
	if i := v.cursor - len(v.deps); i >= 0 && i < len(v.domains) {
		return v.domains[i], true
	}
	return domain.AppDomainStatus{}, false
}
```

change the `down`/`j` bound and the `x` key:

```go
		case "down", "j":
			if v.cursor < len(v.deps)+len(v.domains)-1 {
				v.cursor++
			}
```

```go
		case "x":
			if d, ok := v.selectedDomain(); ok {
				app := v.name
				return v, func() tea.Msg { return pushMsg{newRemoveDomainConfirm(app, d.Domain)} }
			}
			return v, func() tea.Msg { return pushMsg{newDeleteConfirm(v.name)} }
```

and make the footer section-aware:

```go
func (v appDetailView) footer() string {
	if _, ok := v.selectedDomain(); ok {
		return "a add domain · x remove · ↵ details · d deploy · r refresh · esc back · ? help"
	}
	return "d deploy · s stop · x delete · l link · a domain · ↵ logs · r refresh · esc back · ? help"
}
```

`internal/tui/app.go` — add beside the other intent cases (e.g. after `linkAppMsg`):

```go
	case removeDomainMsg:
		app, dom, c := msg.app, msg.domain, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.RemoveAppDomain(app, dom), popLevels: 1} }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tui/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): remove a per-app domain from the drilldown (unified cursor + confirm)

Part of #285"
```

---

### Task 3: Domain detail view (CNAME + live status), reached via `enter`

**Files:**
- Create: `internal/tui/domaindetail.go`
- Modify: `internal/tui/tui.go` (`domainDetailLoadedMsg` + pollResult)
- Modify: `internal/tui/appdetail.go` (`enter` on a domain row)
- Test: `internal/tui/domaindetail_test.go` (new), `internal/tui/appdetail_test.go`

**Interfaces:**
- Consumes: Task 1's fixtures, `domainStatusIcon`, `fakeAPI.domains`.
- Produces: `newDomainDetailView(app string, st domain.AppDomainStatus) domainDetailView` with `title() == "domain"`; `domainDetailLoadedMsg struct{ st domain.AppDomainStatus; found bool }` implementing `pollResult`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/domaindetail_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/domain"
)

func TestDomainDetailShowsCNAMEStatusAndNote(t *testing.T) {
	v := newDomainDetailView("blog", fixtureDomains()[0])
	out := v.View()
	for _, want := range []string{
		"blog.example.com", "blog", "◌ pending", "dns  no",
		"CNAME", "relay.getpiper.dev", "issuance starts once DNS points at the relay",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestDomainDetailPollTracksStatus(t *testing.T) {
	v := newDomainDetailView("blog", fixtureDomains()[0])
	active := fixtureDomains()[0]
	active.Status = "active"
	active.DNSOK = true
	msg := v.refresh(fakeAPI{domains: []domain.AppDomainStatus{active}})()
	loaded, ok := msg.(domainDetailLoadedMsg)
	if !ok || !loaded.found {
		t.Fatalf("want found domainDetailLoadedMsg, got %#v", msg)
	}
	m, _ := v.Update(loaded)
	out := m.View()
	if !strings.Contains(out, "● active") || !strings.Contains(out, "dns  ok") {
		t.Fatalf("want live active status:\n%s", out)
	}
	if strings.Contains(out, "issuance starts") {
		t.Fatalf("active domain should drop the issuance note:\n%s", out)
	}
}

func TestDomainDetailGoneKeepsLastState(t *testing.T) {
	v := newDomainDetailView("blog", fixtureDomains()[0])
	msg := v.refresh(fakeAPI{})()
	loaded, ok := msg.(domainDetailLoadedMsg)
	if !ok || loaded.found {
		t.Fatalf("want not-found result, got %#v", msg)
	}
	m, _ := v.Update(loaded)
	if !strings.Contains(m.View(), "blog.example.com") {
		t.Fatalf("last-known state dropped:\n%s", m.View())
	}
}

func TestDomainDetailFailedShowsError(t *testing.T) {
	st := fixtureDomains()[0]
	st.Status = "failed"
	st.Error = "acme: challenge timed out"
	if out := newDomainDetailView("blog", st).View(); !strings.Contains(out, "challenge timed out") {
		t.Fatalf("failed error missing:\n%s", out)
	}
}

func TestDomainDetailErrBanner(t *testing.T) {
	v := newDomainDetailView("blog", fixtureDomains()[0])
	m, _ := v.Update(errMsg{err: errors.New("connection refused")})
	if !strings.Contains(m.View(), "connection refused") {
		t.Fatalf("want error banner:\n%s", m.View())
	}
}
```

Append to `internal/tui/appdetail_test.go`:

```go
func TestAppDetailEnterOnDomainPushesDetail(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps(), domains: fixtureDomains(),
	})
	m, _ = m.Update(keyRunes('j'))
	m, _ = m.Update(keyRunes('j'))
	_, cmd := m.Update(keyEnter())
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "domain" {
		t.Fatalf("want domain detail pushed, got title %q", pm.view.title())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestDomainDetail|TestAppDetailEnterOnDomain' -v`
Expected: compile FAIL — `newDomainDetailView`, `domainDetailLoadedMsg` undefined.

- [ ] **Step 3: Implement**

`internal/tui/tui.go` — add the message and pollResult:

```go
	// domainDetailLoadedMsg is the domain detail view's poll result. found is
	// false when the domain no longer exists; the view keeps its last-known
	// state (the box answered, so it still counts as reachable).
	domainDetailLoadedMsg struct {
		st    domain.AppDomainStatus
		found bool
	}
```

```go
func (domainDetailLoadedMsg) reachable() bool { return true }
```

Create `internal/tui/domaindetail.go`:

```go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/domain"
)

// domainDetailView shows one per-app custom domain: live status, the CNAME the
// user must create, and the issuance note. Reached from enter on a domain row
// and from the add flow (the form is replaced with it on success). It re-polls
// on every tick, so pending → issuing → active surfaces without leaving the TUI.
type domainDetailView struct {
	app string
	st  domain.AppDomainStatus
	err error
}

func newDomainDetailView(app string, st domain.AppDomainStatus) domainDetailView {
	return domainDetailView{app: app, st: st}
}

func (v domainDetailView) Init() tea.Cmd { return nil }

func (v domainDetailView) title() string { return "domain" }

func (v domainDetailView) footer() string { return "r refresh · esc back · ? help" }

func (v domainDetailView) refresh(c API) tea.Cmd {
	app, dom := v.app, v.st.Domain
	return func() tea.Msg {
		ds, err := c.AppDomains(app)
		if err != nil {
			return errMsg{err}
		}
		for _, d := range ds {
			if d.Domain == dom {
				return domainDetailLoadedMsg{st: d, found: true}
			}
		}
		return domainDetailLoadedMsg{}
	}
}

func (v domainDetailView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case domainDetailLoadedMsg:
		if msg.found {
			v.st = msg.st
		}
		v.err = nil
	case errMsg:
		v.err = msg.err
	}
	return v, nil
}

func (v domainDetailView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	st := v.st
	dns := "no"
	if st.DNSOK {
		dns = "ok"
	}
	expires := "-"
	if st.CertNotAfter != nil {
		expires = st.CertNotAfter.Format("2006-01-02")
	}
	fmt.Fprintf(&b, "  %s → %s\n\n", st.Domain, st.App)
	status := strings.TrimSpace(domainStatusIcon(st.Status) + " " + st.Status)
	fmt.Fprintf(&b, "  status  %s   cert expires  %s   dns  %s\n", status, expires, dns)
	if st.Status == domain.StatusFailed && st.Error != "" {
		fmt.Fprintf(&b, "\n ⚠ %s\n", st.Error)
	}
	if len(st.DNSRecords) > 0 {
		b.WriteString("\n  create this record at your DNS host:\n")
		for _, rec := range st.DNSRecords {
			fmt.Fprintf(&b, "    %s\t%s\t%s\n", rec.Name, rec.Type, rec.Value)
		}
	}
	if st.Status != domain.StatusActive {
		b.WriteString("\n  issuance starts once DNS points at the relay; status updates live\n")
	}
	return b.String()
}
```

`internal/tui/appdetail.go` — the `enter` case becomes:

```go
		case "enter":
			if d, ok := v.selectedDomain(); ok {
				app := v.name
				return v, func() tea.Msg { return pushMsg{newDomainDetailView(app, d)} }
			}
			if len(v.deps) > 0 && v.cursor < len(v.deps) {
				d := v.deps[v.cursor]
				return v, func() tea.Msg { return pushMsg{newLogsView(v.name, d.ID, d.Status)} }
			}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tui/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): domain detail view with CNAME and live issuance status

Part of #285"
```

---

### Task 4: Add-domain form (`a` key), success replaced by the detail view

**Files:**
- Create: `internal/tui/domainform.go`
- Modify: `internal/tui/tui.go` (`addDomainMsg`, `domainAddedMsg`)
- Modify: `internal/tui/app.go` (root cases)
- Modify: `internal/tui/appdetail.go` (`a` key)
- Test: `internal/tui/domainform_test.go` (new), `internal/tui/appdetail_test.go`, `internal/tui/app_test.go`

**Interfaces:**
- Consumes: Task 3's `newDomainDetailView`, Task 1's fake fields `addSt`/`addErr` and recorder.
- Produces: `newDomainForm(app string) domainFormView` with `title() == "add domain"`; `addDomainMsg struct{ app, domain string }`; `domainAddedMsg struct{ app string; st domain.AppDomainStatus; err error }`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/domainform_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"
)

// typeIntoDomain feeds each rune of s into the domain form's input.
func typeIntoDomain(v domainFormView, s string) domainFormView {
	for _, r := range s {
		m, _ := v.Update(keyRunes(r))
		v = m.(domainFormView)
	}
	return v
}

func TestDomainFormSubmitEmitsAddDomain(t *testing.T) {
	v := typeIntoDomain(newDomainForm("blog"), "blog.example.com")
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should emit a command")
	}
	am, ok := cmd().(addDomainMsg)
	if !ok || am.app != "blog" || am.domain != "blog.example.com" {
		t.Fatalf("want addDomainMsg{blog, blog.example.com}, got %#v", cmd())
	}
}

func TestDomainFormRequiresDomain(t *testing.T) {
	m, cmd := newDomainForm("blog").Update(keyEnter())
	if cmd != nil {
		t.Fatal("empty domain must not submit")
	}
	if !strings.Contains(m.View(), "required") {
		t.Fatalf("want required banner:\n%s", m.View())
	}
}

func TestDomainFormErrBannerClearsOnTyping(t *testing.T) {
	v := newDomainForm("blog")
	m, _ := v.Update(errMsg{err: errors.New("domain already attached")})
	if !strings.Contains(m.(domainFormView).View(), "already attached") {
		t.Fatalf("want error banner:\n%s", m.(domainFormView).View())
	}
	if out := typeIntoDomain(m.(domainFormView), "x").View(); strings.Contains(out, "already attached") {
		t.Fatalf("banner should clear on typing:\n%s", out)
	}
}
```

Append to `internal/tui/appdetail_test.go`:

```go
func TestAppDetailAKeyPushesDomainForm(t *testing.T) {
	_, cmd := newAppDetailView("blog", false).Update(keyRunes('a'))
	if cmd == nil {
		t.Fatal("a should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "add domain" {
		t.Fatalf("want the domain form, got title %q", pm.view.title())
	}
}
```

Append to `internal/tui/app_test.go`:

```go
func TestRootAddDomainCallsClient(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("b", "a", false, fakeAPI{rec: rec, addSt: fixtureDomains()[0]})
	_, cmd := m.Update(addDomainMsg{app: "blog", domain: "blog.example.com"})
	added, ok := cmd().(domainAddedMsg)
	if !ok || added.err != nil || added.st.Domain != "blog.example.com" {
		t.Fatalf("want domainAddedMsg with status, got %#v", cmd())
	}
	if rec.addedApp != "blog" || rec.addedDomain != "blog.example.com" {
		t.Fatalf("client not called with app+domain: %#v", rec)
	}
}

func TestRootDomainAddedReplacesFormWithDetail(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	m.stack = append(m.stack, newAppDetailView("blog", false), newDomainForm("blog"))
	next, _ := m.Update(domainAddedMsg{app: "blog", st: fixtureDomains()[0]})
	nm := next.(Model)
	if nm.top().title() != "domain" {
		t.Fatalf("want the domain detail view on top, got %q", nm.top().title())
	}
	if !strings.Contains(nm.top().View(), "CNAME") {
		t.Fatalf("detail should show the CNAME:\n%s", nm.top().View())
	}
}

func TestRootDomainAddedErrorBannersForm(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	m.stack = append(m.stack, newAppDetailView("blog", false), newDomainForm("blog"))
	next, _ := m.Update(domainAddedMsg{app: "blog", err: errors.New("invalid domain")})
	nm := next.(Model)
	if nm.top().title() != "add domain" || !strings.Contains(nm.top().View(), "invalid domain") {
		t.Fatalf("want bannered form, got %q:\n%s", nm.top().title(), nm.top().View())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestDomainForm|TestAppDetailAKey|TestRootAddDomain|TestRootDomainAdded' -v`
Expected: compile FAIL — `domainFormView`, `addDomainMsg`, `domainAddedMsg` undefined.

- [ ] **Step 3: Implement**

`internal/tui/tui.go` — add beside the other intents:

```go
	// addDomainMsg is the domain form's intent; the root runs AddAppDomain off
	// the UI thread and reports via domainAddedMsg.
	addDomainMsg struct{ app, domain string }

	// domainAddedMsg is the add's outcome. On success the root replaces the
	// form with the domain detail view (CNAME + live status); on error it
	// banners the form.
	domainAddedMsg struct {
		app string
		st  domain.AppDomainStatus
		err error
	}
```

Create `internal/tui/domainform.go`:

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// domainFormView attaches a custom domain to an app: one field, the domain.
// On submit it emits addDomainMsg; the root runs AddAppDomain and replaces the
// form with the domain detail view on success. Validation beyond non-empty
// stays server-side.
type domainFormView struct {
	app   string
	input textinput.Model
	err   error
}

func newDomainForm(app string) domainFormView {
	in := textinput.New()
	in.Placeholder = "blog.example.com"
	in.Focus()
	return domainFormView{app: app, input: in}
}

func (v domainFormView) Init() tea.Cmd { return nil }

func (v domainFormView) title() string { return "add domain" }

func (v domainFormView) refresh(API) tea.Cmd { return nil }

func (v domainFormView) capturesText() bool { return true }

func (v domainFormView) footer() string { return "↵ add · esc cancel · ? help" }

func (v domainFormView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyEnter {
			dom := strings.TrimSpace(v.input.Value())
			if dom == "" {
				v.err = fmt.Errorf("domain is required")
				return v, nil
			}
			app := v.app
			return v, func() tea.Msg { return addDomainMsg{app: app, domain: dom} }
		}
	}
	v.err = nil // any keystroke clears a stale banner
	var cmd tea.Cmd
	v.input, cmd = v.input.Update(msg)
	return v, cmd
}

func (v domainFormView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  attach a domain to %s\n\n", v.app)
	fmt.Fprintf(&b, "  domain  %s\n\n", v.input.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ add   esc cancel")
	return b.String()
}
```

`internal/tui/appdetail.go` — add the key (beside `l`):

```go
		case "a":
			return v, func() tea.Msg { return pushMsg{newDomainForm(v.name)} }
```

`internal/tui/app.go` — add the root cases (after `removeDomainMsg`):

```go
	case addDomainMsg:
		app, dom, c := msg.app, msg.domain, m.client
		return m, func() tea.Msg {
			st, err := c.AddAppDomain(app, dom)
			return domainAddedMsg{app: app, st: st, err: err}
		}
	case domainAddedMsg:
		if _, ok := m.top().(domainFormView); !ok {
			return m, nil // user navigated away before the add returned
		}
		if msg.err != nil {
			next, _ := m.top().Update(errMsg{msg.err})
			m.stack[len(m.stack)-1] = next.(view)
			return m, nil
		}
		m.stack[len(m.stack)-1] = newDomainDetailView(msg.app, msg.st)
		if m.width > 0 {
			seeded, _ := m.top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack[len(m.stack)-1] = seeded.(view)
		}
		return m, m.refresh()
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tui/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): add-domain form with CNAME handoff to the detail view

Part of #285"
```

---

### Task 5: Help overlay + verify gate

**Files:**
- Modify: `internal/tui/help.go`
- Test: `internal/tui/help_test.go`

**Interfaces:**
- Consumes: the key set from Tasks 2–4.
- Produces: nothing new — discoverability only.

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/help_test.go`:

```go
func TestHelpListsDomainKeys(t *testing.T) {
	out := helpView{}.View()
	for _, want := range []string{"a add domain", "x delete app / remove domain", "Domain"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q:\n%s", want, out)
		}
	}
}
```

(If the file lacks a `strings` import, add it.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestHelpListsDomainKeys -v`
Expected: FAIL — help view lacks the new keys.

- [ ] **Step 3: Implement**

`internal/tui/help.go` — replace the App detail line and add a Domain line:

```go
		"  App detail  ↑/k ↓/j move · enter logs / domain · d deploy · s stop · x delete app / remove domain · l link · a add domain\n" +
		"  Domain      live status + the CNAME to create · esc back\n" +
```

If an existing help test asserts the old App detail line verbatim, update it to the new text.

- [ ] **Step 4: Run the full verify gate**

Run: `make verify`
Expected: gofmt clean, vet clean, all tests PASS, cross-compile OK.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): domain keys in the help overlay

Part of #285"
```

---

### Task 6: PR

**Files:** none (git/gh only).

- [ ] **Step 1: Re-run the gate and push**

Run: `make verify && git push -u origin ozykhan/tui-app-domains`
Expected: gate green, branch pushed.

- [ ] **Step 2: Open the PR**

```bash
gh pr create --base main --title "[cli] TUI: per-app domains in the app drilldown" --body "Adds the #285 domain surface to the TUI app drilldown: an inline DOMAINS table polled on the 2s tick, a unified cursor across deployments and domains, an add-domain form that hands off to a live domain detail view showing the exact CNAME, and a y/n remove confirm. Keys are in the footer legend and ? help overlay.

Spec: docs/superpowers/specs/2026-07-20-tui-app-domains-design.md

Closes #285

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

Expected: PR URL printed.
