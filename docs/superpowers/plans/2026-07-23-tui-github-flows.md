# TUI Relay GitHub Flows Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring the v0.6.0 relay-brokered GitHub App flows into the TUI: a `g` wizard (relay login → App install → installations/repos), a repo picker + root-dir field on the link form.

**Architecture:** A new `RelayAPI` interface + `RelayDialer` factory seam in `internal/tui` (mirroring the existing `API`/`Dialer` pattern) lets views talk to the relay; `relayclient.GitHubStatus` is extended in place to return the `github_app`/`install_url` fields the relay already sends. The existing manifest-flow view is renamed `manifestView` and becomes a sub-view of the new wizard. Relay errors never enter the `errMsg`/`pollResult` machinery (which drives the *box* status bar).

**Tech Stack:** Go, Bubble Tea (`charmbracelet/bubbletea`, `bubbles/textinput`), `internal/relayclient`, `internal/config`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-07-23-tui-github-flows-design.md` — read it first.

## Global Constraints

- **No cgo**: everything must build with `CGO_ENABLED=0` (checked by `make cross`).
- **Pre-1.x compatibility policy**: break APIs in place — no shims, no deprecated aliases, no legacy readers.
- **Layering**: `tui` may import `relayclient` and `config` (both CLI-side); nothing imports "up".
- Match the style of the surrounding package: value-receiver views, `Update` as a pure `(msg) → (model, cmd)` machine, all I/O inside `tea.Cmd` closures, messages declared in `tui.go`.
- Commits are conventional-commit style, reference the tracking issue with `Part of #N`, and end with:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`
- Run tests with plain `go test` per package while iterating; run `make verify` (gofmt → vet → test → cross) before the PR.
- Work happens on the existing branch `ozykhan/tui-github-flows` (the spec is already committed there). Never commit to `main`.

---

### Task 1: `relayclient.Status`, `DefaultAPI`, CLI call sites

**Files:**
- Modify: `internal/relayclient/relayclient.go` (GitHubStatus, ~line 213-237; add `Status` type near `Installation`, ~line 205; add `DefaultAPI` const near `New`, ~line 67)
- Modify: `internal/relayclient/relayclient_test.go` (`TestGitHubStatus`, ~line 180)
- Modify: `cmd/piper/relayonboard.go` (delete `defaultRelayAPI` const at line 17-19; `waitForInstall` line 151-166; `githubRepos` line 188-210)
- Modify: `cmd/piper/main.go` (line 203: `defaultRelayAPI` → `relayclient.DefaultAPI`; add `relayclient` import)

**Interfaces:**
- Consumes: nothing new.
- Produces: `relayclient.Status{GitHubApp bool; InstallURL string; Installations []Installation}`, `func (c *Client) GitHubStatus(ctx context.Context, accountCredential string) (Status, error)`, `const relayclient.DefaultAPI = "https://api.public.getpiper.dev"`. Tasks 2–4 depend on these exact names.

- [ ] **Step 1: Open the tracking issue**

```bash
gh issue create \
  --title "[cli] TUI: relay GitHub onboarding wizard + repo picker" \
  --label cli --label enhancement --label P2 --label "size/M" \
  --body "$(cat <<'EOF'
Bring the v0.6.0 relay-brokered GitHub flows into the TUI, per
docs/superpowers/specs/2026-07-23-tui-github-flows-design.md:

- [ ] relayclient: GitHubStatus returns Status (github_app, install_url, installations); DefaultAPI const
- [ ] tui: RelayAPI seam + RelayDialer factory; githubView renamed manifestView
- [ ] tui: g wizard — relay login (code + URL, poll), install wait, installations + repos browse
- [ ] tui: link form repo picker (flat, filterable, target-labeled) + root-dir field

Acceptance: `make verify` green; wizard states unit-tested against a fake relay;
relay errors never render the box status bar unreachable.
EOF
)"
```

Note the issue number; it is `#N` in every commit below.

- [ ] **Step 2: Write the failing test — `Status` fields parse**

In `internal/relayclient/relayclient_test.go`, replace the assertion half of `TestGitHubStatus` (the existing server handler already returns `"github_app":true` and `"install_url":"x"` — keep it) so it consumes the new return type, and add a no-App case:

```go
	st, err := New(srv.URL).GitHubStatus(context.Background(), "cred-xyz")
	if err != nil {
		t.Fatalf("GitHubStatus: %v", err)
	}
	if !st.GitHubApp || st.InstallURL != "x" {
		t.Fatalf("status flags = %+v", st)
	}
	insts := st.Installations
	if len(insts) != 2 || insts[0].ID != "66" || insts[0].TargetLogin != "getpiper" || insts[1].ID != "55" {
		t.Fatalf("installations = %+v", insts)
	}
}

func TestGitHubStatusNoApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"github_app":false,"installations":[],"install_url":""}`))
	}))
	defer srv.Close()
	st, err := New(srv.URL).GitHubStatus(context.Background(), "cred-xyz")
	if err != nil {
		t.Fatalf("GitHubStatus: %v", err)
	}
	if st.GitHubApp || st.InstallURL != "" || len(st.Installations) != 0 {
		t.Fatalf("want empty no-app status, got %+v", st)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/relayclient/ -run TestGitHubStatus -v`
Expected: compile FAIL — `st.GitHubApp undefined (type []Installation has no field or method GitHubApp)`.

- [ ] **Step 4: Implement `Status` + `DefaultAPI`**

In `internal/relayclient/relayclient.go`, below the `Installation` type (~line 211), add:

```go
// Status is the relay's GitHub App report for an account: whether the relay
// brokers an App at all, where to install it, and the account's installations.
type Status struct {
	GitHubApp     bool           `json:"github_app"`
	InstallURL    string         `json:"install_url"`
	Installations []Installation `json:"installations"`
}
```

Change `GitHubStatus` to return it (doc comment updated to match):

```go
// GitHubStatus reports the account's GitHub App state: whether the relay
// brokers an App, its install page, and every installation linked to the
// account. It never 404s on a missing installation — an empty Installations
// is the answer — so a poll loop can wait for the first install to appear.
func (c *Client) GitHubStatus(ctx context.Context, accountCredential string) (Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/github/status", nil)
	if err != nil {
		return Status{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accountCredential)
	resp, err := c.http.Do(req)
	if err != nil {
		return Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("relay github status: %s", resp.Status)
	}
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return Status{}, err
	}
	return st, nil
}
```

Above `New` (~line 67), add:

```go
// DefaultAPI is the hosted public relay's control API base URL. Override with
// `piper login --relay <url>` for a self-hosted relay.
const DefaultAPI = "https://api.public.getpiper.dev"
```

- [ ] **Step 5: Update the two CLI call sites and the const**

In `cmd/piper/relayonboard.go`:
- Delete the `defaultRelayAPI` const block (lines 17-19) — its doc comment moved to `relayclient.DefaultAPI`.
- In `waitForInstall` (~line 151), replace the status handling:

```go
		st, err := rc.GitHubStatus(context.Background(), cred)
		if err != nil {
			return err
		}
		if len(st.Installations) > 0 {
			n := 0
			for _, in := range st.Installations {
				// Best-effort repo count for the message; a transient error here
				// must not fail a login whose install already succeeded.
				if repos, err := rc.GitHubRepos(context.Background(), cred, in.ID); err == nil {
					n += len(repos)
				}
			}
			fmt.Printf("\rInstalled — %d repo(s) available.\n", n)
			return nil
		}
```

- In `githubRepos` (~line 188), same shape:

```go
	st, err := rc.GitHubStatus(ctx, cc.AccountCredential)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if len(st.Installations) == 0 {
		fmt.Fprintln(stdout, "No repositories yet — run `piper login` to install the Piper GitHub App on the repos you want to deploy.")
		return 0
	}
	for _, in := range st.Installations {
```

In `cmd/piper/main.go` line 203, change `defaultRelayAPI` → `relayclient.DefaultAPI` and add `"github.com/getpiper/piper/internal/relayclient"` to the imports.

Verify no stragglers: `grep -rn defaultRelayAPI cmd/ internal/` → no hits.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/relayclient/ ./cmd/piper/`
Expected: PASS (the `cmd/piper` login/onboard tests exercise both call sites through fake relay servers).

- [ ] **Step 7: Commit**

```bash
git add internal/relayclient/ cmd/piper/
git commit -m "feat(relay): GitHubStatus returns Status (github_app, install_url)

The relay already sends both fields; the client discarded them. Also
hoists the hosted-relay base URL to relayclient.DefaultAPI so the TUI
can offer login with no config present. Pre-1.x: signature changed in
place, both CLI call sites updated.

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: TUI `RelayAPI` seam + rename `githubView` → `manifestView`

**Files:**
- Modify: `internal/tui/tui.go` (add `RelayAPI`, `RelayDialer`; imports `context`, `relayclient`)
- Modify: `internal/tui/app.go` (Model field `relay`, `WithRelay`, `Run` signature; rename type assertions at lines 104, 139-141, 240, 250)
- Rename: `internal/tui/github.go` → `internal/tui/manifest.go` (type rename inside)
- Rename: `internal/tui/github_test.go` → `internal/tui/manifest_test.go` (type rename inside)
- Modify: `cmd/piper/main.go` (`launchTUI` ~line 84-104: pass the relay dialer; add `relayDial`)

**Interfaces:**
- Consumes: `relayclient.Status`, `relayclient.DefaultAPI` (Task 1).
- Produces (Tasks 3-4 rely on these exactly):
  - `type RelayAPI interface { CLILoginStart(ctx context.Context) (handle, userCode string, err error); CLILoginPoll(ctx context.Context, handle string) (relayclient.Account, error); GitHubStatus(ctx context.Context, cred string) (relayclient.Status, error); GitHubRepos(ctx context.Context, cred, installationID string) ([]relayclient.Repo, error) }`
  - `type RelayDialer func(base string) RelayAPI`
  - `func (m Model) WithRelay(r RelayDialer) Model`; Model field `relay RelayDialer`
  - `func Run(box, addr string, remote bool, c API, dial Dialer, relay RelayDialer) error`
  - view type `manifestView`, constructor `newManifestView()`, title `"manifest"`

- [ ] **Step 1: Write the failing test — `WithRelay` attaches the factory**

Append to `internal/tui/app_test.go`:

```go
func TestWithRelayAttachesFactory(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(func(string) RelayAPI { return nil })
	if m.relay == nil {
		t.Fatal("WithRelay should attach the relay dialer")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/tui/ -run TestWithRelayAttachesFactory`
Expected: compile FAIL — `undefined: RelayAPI` / `m.relay undefined`.

- [ ] **Step 3: Add the seam**

In `internal/tui/tui.go`, add to the imports `"context"` and `"github.com/getpiper/piper/internal/relayclient"`, and below the `Dialer` type add:

```go
// RelayAPI is the slice of the relay control API the TUI consumes.
// *relayclient.Client satisfies it; tests inject fakes.
type RelayAPI interface {
	CLILoginStart(ctx context.Context) (handle, userCode string, err error)
	CLILoginPoll(ctx context.Context, handle string) (relayclient.Account, error)
	GitHubStatus(ctx context.Context, cred string) (relayclient.Status, error)
	GitHubRepos(ctx context.Context, cred, installationID string) ([]relayclient.Repo, error)
}

// RelayDialer builds a relay client for a base URL. cmd/piper supplies the
// real one; tests inject fakes. A factory, not a client: a fresh user logs in
// against the default relay, a configured user against their saved RelayAPI.
type RelayDialer func(base string) RelayAPI
```

In `internal/tui/app.go`, add the field to `Model` (after `dial Dialer`): `relay RelayDialer`, and after `WithDialer`:

```go
// WithRelay attaches the relay client factory used by the github wizard and
// the link form's repo picker. Kept separate from NewModel so existing call
// sites and tests stay four-argument.
func (m Model) WithRelay(r RelayDialer) Model { m.relay = r; return m }
```

Change `Run`:

```go
// Run starts the interactive TUI against c, identified as box/addr in the
// status bar. remote marks a relay-backed box (HTTPS URLs). dial builds clients
// for the box switcher; relay builds relay clients for the github wizard and
// repo picker. It blocks until quit.
func Run(box, addr string, remote bool, c API, dial Dialer, relay RelayDialer) error {
	m := NewModel(box, addr, remote, c).WithDialer(dial).WithRelay(relay)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}
```

In `cmd/piper/main.go`, change the `launchTUI` call to `tui.Run(box, addr, relay, c, dialBox, relayDial)` and add below `dialBox`:

```go
// relayDial builds the TUI's relay client; a thin adapter over relayclient.
func relayDial(base string) tui.RelayAPI { return relayclient.New(base) }
```

- [ ] **Step 4: Rename the manifest flow**

```bash
git mv internal/tui/github.go internal/tui/manifest.go
git mv internal/tui/github_test.go internal/tui/manifest_test.go
```

In `manifest.go`: rename `githubView` → `manifestView`, `newGithubView` → `newManifestView`; change `title()` to return `"manifest"`; update the type-doc comment's first line to `// manifestView runs the GitHub App manifest flow: …` (rest unchanged). `beginManifestFlow`, `manifestActionURL`, `openBrowser`, and the `githubStartMsg`/`githubFormReadyMsg`/`githubDoneMsg` messages keep their names.

In `manifest_test.go`: same two renames; also rename `TestGKeyOpensGithub` → `TestGKeyOpensManifest` (Task 3 rewrites it for the wizard).

In `app.go`, update the three type assertions and the push site:
- line ~104 (esc): `if _, ok := m.top().(manifestView); ok && m.githubCancel != nil {`
- line ~139 (`g` key): `if _, ok := m.top().(manifestView); !ok { return m, func() tea.Msg { return pushMsg{newManifestView()} } }` (temporary — Task 3 points `g` at the wizard)
- line ~240 (`githubFormReadyMsg`): `if gv, ok := m.top().(manifestView); ok {`
- line ~250 (`githubDoneMsg`): `if gv, ok := m.top().(manifestView); ok {`

- [ ] **Step 5: Run the package tests**

Run: `go test ./internal/tui/ ./cmd/piper/`
Expected: PASS, including the renamed manifest tests and `TestWithRelayAttachesFactory`.

- [ ] **Step 6: Commit**

```bash
git add internal/tui/ cmd/piper/main.go
git commit -m "feat(cli): TUI RelayAPI seam; rename githubView to manifestView

RelayAPI/RelayDialer mirror the API/Dialer pattern so relay-backed
views unit-test against fakes. The manifest flow keeps working
unchanged under its new name; g still opens it until the wizard lands.

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: the `g` github wizard

**Files:**
- Create: `internal/tui/github.go` (fresh — `githubWizardView`, `wizardReposView`, cmd helpers)
- Create: `internal/tui/github_test.go` (fresh — `fakeRelay` + wizard state tests)
- Modify: `internal/tui/tui.go` (wizard messages)
- Modify: `internal/tui/app.go` (`g` pushes the wizard)

**Interfaces:**
- Consumes: `RelayAPI`, `RelayDialer`, `Model.relay`, `newManifestView` (Task 2); `relayclient.Status`, `relayclient.DefaultAPI`, `relayclient.ErrAuthPending`, `relayclient.Account`, `relayclient.Installation`, `relayclient.Repo`; `config.LoadClient`/`config.SaveClient`; the package's `openBrowser` var (in `manifest.go`).
- Produces: `githubWizardView` (constructor `newGithubWizard(relay RelayDialer) githubWizardView`), states `wizLoading/wizLogin/wizLoginPolling/wizByo/wizInstall/wizInstalled`, messages `wizStatusMsg`/`wizLoginStartedMsg`/`wizLoginDoneMsg`/`wizReposMsg`, sub-view `wizardReposView`, test helpers `fakeRelay` and `relayFor(fakeRelay) RelayDialer`. Task 4's tests reuse `fakeRelay`/`relayFor`.

- [ ] **Step 1: Add the wizard messages to `tui.go`**

Inside the existing message type block, after `githubStartMsg`:

```go
	// wizStatusMsg is the github wizard's config+status probe result. noCred
	// means no account credential is saved (→ login step); base is the relay
	// base the probe used (saved RelayAPI or the default). Deliberately NOT a
	// pollResult: a relay error must not render the box status bar unreachable.
	wizStatusMsg struct {
		noCred bool
		base   string
		cred   string
		st     relayclient.Status
		err    error
	}

	// wizLoginStartedMsg carries the brokered-login handle + the user code the
	// human enters in the browser.
	wizLoginStartedMsg struct {
		handle string
		code   string
		err    error
	}

	// wizLoginDoneMsg is one brokered-login poll outcome. pending means the
	// user hasn't finished in the browser; on success the credential was
	// already saved to client config inside the cmd (off the UI thread).
	wizLoginDoneMsg struct {
		acc     relayclient.Account
		pending bool
		err     error
	}

	// wizReposMsg is one installation's repo listing for the wizard's pushed
	// repos sub-view.
	wizReposMsg struct {
		repos []relayclient.Repo
		err   error
	}
```

- [ ] **Step 2: Write the failing tests — wizard state machine**

Create `internal/tui/github_test.go`:

```go
package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/relayclient"
)

// fakeRelay is a scriptable RelayAPI for wizard and picker tests.
type fakeRelay struct {
	handle, code string
	startErr     error
	acc          relayclient.Account
	pollErr      error
	st           relayclient.Status
	stErr        error
	repos        []relayclient.Repo
	reposErr     error
}

func (f fakeRelay) CLILoginStart(context.Context) (string, string, error) {
	return f.handle, f.code, f.startErr
}
func (f fakeRelay) CLILoginPoll(context.Context, string) (relayclient.Account, error) {
	return f.acc, f.pollErr
}
func (f fakeRelay) GitHubStatus(context.Context, string) (relayclient.Status, error) {
	return f.st, f.stErr
}
func (f fakeRelay) GitHubRepos(context.Context, string, string) ([]relayclient.Repo, error) {
	return f.repos, f.reposErr
}

func relayFor(f fakeRelay) RelayDialer { return func(string) RelayAPI { return f } }

func TestWizardNoCredShowsLogin(t *testing.T) {
	v := newGithubWizard(relayFor(fakeRelay{}))
	next, _ := v.Update(wizStatusMsg{noCred: true, base: "https://r.example"})
	out := next.(githubWizardView).View()
	if !strings.Contains(out, "sign in with GitHub") || !strings.Contains(out, "https://r.example") {
		t.Fatalf("want the armed login step, got:\n%s", out)
	}
}

func TestWizardEnterStartsLoginAndShowsCode(t *testing.T) {
	orig := openBrowser
	opened := ""
	openBrowser = func(u string) error { opened = u; return nil }
	defer func() { openBrowser = orig }()

	v := newGithubWizard(relayFor(fakeRelay{handle: "h1", code: "ABCD-1234"}))
	next, _ := v.Update(wizStatusMsg{noCred: true, base: "https://r.example"})
	next, cmd := next.(githubWizardView).Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter on the login step should start the flow")
	}
	started, ok := cmd().(wizLoginStartedMsg)
	if !ok || started.handle != "h1" || started.code != "ABCD-1234" {
		t.Fatalf("want wizLoginStartedMsg{h1, ABCD-1234}, got %#v", cmd())
	}
	if opened != "https://r.example/v1/login/cli" {
		t.Fatalf("browser should open the verify URL, got %q", opened)
	}
	next, _ = next.(githubWizardView).Update(started)
	out := next.(githubWizardView).View()
	if !strings.Contains(out, "ABCD-1234") || !strings.Contains(out, "https://r.example/v1/login/cli") {
		t.Fatalf("polling view must show code + URL, got:\n%s", out)
	}
	if c := next.(githubWizardView).refresh(nil); c == nil {
		t.Fatal("the polling state must poll on the tick")
	}
}

func TestWizardLoginPendingKeepsPolling(t *testing.T) {
	v := newGithubWizard(relayFor(fakeRelay{}))
	v.state, v.base, v.handle = wizLoginPolling, "https://r.example", "h1"
	next, _ := v.Update(wizLoginDoneMsg{pending: true})
	if next.(githubWizardView).state != wizLoginPolling {
		t.Fatal("pending must stay in the polling state")
	}
}

func TestWizardPollLoginSavesConfigAndReportsAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	relay := relayFor(fakeRelay{acc: relayclient.Account{
		AccountCredential: "cred-1", Username: "alice", InstallURL: "https://gh/install",
	}})
	msg := pollLogin(relay, "https://r.example", "h1")
	done, ok := msg.(wizLoginDoneMsg)
	if !ok || done.err != nil || done.pending {
		t.Fatalf("want a success wizLoginDoneMsg, got %#v", msg)
	}
	cc, err := config.LoadClient()
	if err != nil || cc.AccountCredential != "cred-1" || cc.RelayAPI != "https://r.example" {
		t.Fatalf("credential not saved: %+v err=%v", cc, err)
	}
	// the view advances to the install step because login carried install_url
	v := newGithubWizard(relay)
	v.state = wizLoginPolling
	next, _ := v.Update(done)
	nv := next.(githubWizardView)
	if nv.state != wizInstall || !strings.Contains(nv.View(), "https://gh/install") {
		t.Fatalf("want the install step showing the URL, got state %d:\n%s", nv.state, nv.View())
	}
}

func TestWizardPollLoginPendingMapsErrAuthPending(t *testing.T) {
	msg := pollLogin(relayFor(fakeRelay{pollErr: relayclient.ErrAuthPending}), "b", "h")
	if done := msg.(wizLoginDoneMsg); !done.pending || done.err != nil {
		t.Fatalf("ErrAuthPending must map to pending, got %#v", msg)
	}
}

func TestWizardByoOffersManifest(t *testing.T) {
	v := newGithubWizard(relayFor(fakeRelay{}))
	next, _ := v.Update(wizStatusMsg{base: "b", cred: "c", st: relayclient.Status{GitHubApp: false}})
	out := next.(githubWizardView).View()
	if !strings.Contains(out, "doesn't broker") {
		t.Fatalf("want the BYO explanation, got:\n%s", out)
	}
	_, cmd := next.(githubWizardView).Update(keyRunes('m'))
	if cmd == nil {
		t.Fatal("m should push the manifest view")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if _, ok := push.view.(manifestView); !ok {
		t.Fatalf("want manifestView pushed, got %T", push.view)
	}
}

func TestWizardNoInstallPollsUntilInstalled(t *testing.T) {
	v := newGithubWizard(relayFor(fakeRelay{st: relayclient.Status{
		GitHubApp:     true,
		InstallURL:    "https://gh/install",
		Installations: []relayclient.Installation{{ID: "66", TargetType: "org", TargetLogin: "getpiper"}},
	}}))
	next, _ := v.Update(wizStatusMsg{base: "b", cred: "c",
		st: relayclient.Status{GitHubApp: true, InstallURL: "https://gh/install"}})
	nv := next.(githubWizardView)
	if nv.state != wizInstall || !strings.Contains(nv.View(), "https://gh/install") {
		t.Fatalf("want the install step, got state %d", nv.state)
	}
	cmd := nv.refresh(nil)
	if cmd == nil {
		t.Fatal("the install state must poll status on the tick")
	}
	next, _ = nv.Update(cmd()) // fakeRelay now reports one installation
	nv = next.(githubWizardView)
	if nv.state != wizInstalled || !strings.Contains(nv.View(), "getpiper") {
		t.Fatalf("an appearing install must flip to installed, got state %d:\n%s", nv.state, nv.View())
	}
}

func TestWizardInstalledBrowsesRepos(t *testing.T) {
	insts := []relayclient.Installation{
		{ID: "55", TargetType: "user", TargetLogin: "alice"},
		{ID: "66", TargetType: "org", TargetLogin: "getpiper"},
	}
	v := newGithubWizard(relayFor(fakeRelay{}))
	next, _ := v.Update(wizStatusMsg{base: "b", cred: "c",
		st: relayclient.Status{GitHubApp: true, Installations: insts}})
	nv := next.(githubWizardView)
	out := nv.View()
	if !strings.Contains(out, "alice (user)") || !strings.Contains(out, "getpiper (org)") {
		t.Fatalf("installations must list with their target, got:\n%s", out)
	}
	next, _ = nv.Update(keyRunes('j'))
	next, cmd := next.(githubWizardView).Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should push the repos view")
	}
	push := cmd().(pushMsg)
	sub, ok := push.view.(wizardReposView)
	if !ok || sub.inst.ID != "66" {
		t.Fatalf("want the second installation's repos view, got %#v", push.view)
	}
}

func TestWizardReposViewLoadsOnceAndRenders(t *testing.T) {
	sub := wizardReposView{
		relay: relayFor(fakeRelay{repos: []relayclient.Repo{
			{FullName: "getpiper/piper"}, {FullName: "getpiper/secrets", Visibility: "private"},
		}}),
		base: "b", cred: "c", inst: relayclient.Installation{ID: "66", TargetLogin: "getpiper", TargetType: "org"},
	}
	cmd := sub.refresh(nil)
	if cmd == nil {
		t.Fatal("first refresh must load repos")
	}
	next, _ := sub.Update(cmd())
	sub = next.(wizardReposView)
	out := sub.View()
	if !strings.Contains(out, "getpiper/piper") || !strings.Contains(out, "getpiper/secrets (private)") {
		t.Fatalf("repos must render with visibility badges, got:\n%s", out)
	}
	if sub.refresh(nil) != nil {
		t.Fatal("repos load once — GitHubRepos proxies to GitHub's API, no tick polling")
	}
}

func TestWizardRelayErrorBannersWithoutMarkingBoxDown(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(relayFor(fakeRelay{}))
	next, _ := m.Update(pushMsg{view: newGithubWizard(m.relay)})
	m = next.(Model)
	next, _ = m.Update(wizStatusMsg{err: errors.New("relay 502")})
	m = next.(Model)
	if m.down {
		t.Fatal("a relay error must not render the box unreachable")
	}
	if !strings.Contains(m.top().View(), "relay 502") {
		t.Fatalf("relay error should banner in the wizard, got:\n%s", m.top().View())
	}
}

func TestGKeyOpensWizard(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(relayFor(fakeRelay{}))
	next, cmd := m.Update(keyRunes('g'))
	m = pump(t, next.(Model), cmd)
	if _, ok := m.top().(githubWizardView); !ok {
		t.Fatalf("g should push the github wizard, got %T", m.top())
	}
}
```

Also in `manifest_test.go`, delete `TestGKeyOpensManifest` (superseded by `TestGKeyOpensWizard` + `TestWizardByoOffersManifest`).

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/tui/ -run TestWizard -v`
Expected: compile FAIL — `undefined: newGithubWizard` etc.

- [ ] **Step 4: Implement the wizard**

Create `internal/tui/github.go`:

```go
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/relayclient"
)

// wizardState is where the github wizard stands in the onboarding story.
type wizardState int

const (
	wizLoading      wizardState = iota // probing config + relay status
	wizLogin                           // no credential: armed, ↵ starts the browser login
	wizLoginPolling                    // code + URL shown, polling CLILoginPoll
	wizByo                             // relay brokers no App; manifest flow is the path
	wizInstall                         // logged in, no installation: URL shown, polling status
	wizInstalled                       // installations listed; ↵ browses repos
)

// githubWizardView is the relay-brokered GitHub onboarding wizard behind `g`:
// login → App install → installations. It decides its state from client config
// and GET /v1/github/status, and rides the root's 2s tick for all polling —
// every cmd is a one-shot HTTP call, so esc simply pops the view and polling
// stops with it (an abandoned login handle expires on the relay).
type githubWizardView struct {
	relay      RelayDialer
	state      wizardState
	base       string // relay base in use: saved RelayAPI or relayclient.DefaultAPI
	cred       string // account credential once known
	handle     string // brokered-login poll handle
	code       string // user code the human enters in the browser
	installURL string
	insts      []relayclient.Installation
	sel        int
	err        error
}

func newGithubWizard(relay RelayDialer) githubWizardView {
	return githubWizardView{relay: relay, state: wizLoading}
}

func (v githubWizardView) Init() tea.Cmd { return nil }

func (v githubWizardView) title() string { return "github" }

// refresh ignores the box API: the wizard polls the relay. Only the states
// that are waiting on something return a cmd; settled states poll nothing.
func (v githubWizardView) refresh(API) tea.Cmd {
	relay := v.relay
	switch v.state {
	case wizLoading:
		return func() tea.Msg { return probeStatus(relay) }
	case wizLoginPolling:
		base, handle := v.base, v.handle
		return func() tea.Msg { return pollLogin(relay, base, handle) }
	case wizInstall:
		base, cred := v.base, v.cred
		return func() tea.Msg { return probeWith(relay, base, cred) }
	}
	return nil
}

func (v githubWizardView) footer() string {
	switch v.state {
	case wizLogin:
		return "↵ sign in · m manifest app · esc cancel · ? help"
	case wizInstall:
		return "o open install page · esc cancel · ? help"
	case wizInstalled:
		return "↑↓ move · ↵ repos · m manifest app · esc back · ? help"
	case wizByo:
		return "m manifest app · esc back · ? help"
	}
	return "esc cancel · ? help"
}

func (v githubWizardView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case wizStatusMsg:
		if msg.err != nil {
			v.err = msg.err // banner; the state keeps polling, so a transient error retries
			return v, nil
		}
		v.err = nil
		v.base, v.cred = msg.base, msg.cred
		switch {
		case msg.noCred:
			v.state = wizLogin
		case !msg.st.GitHubApp:
			v.state = wizByo
		case len(msg.st.Installations) == 0:
			v.state, v.installURL = wizInstall, msg.st.InstallURL
		default:
			v.state, v.insts = wizInstalled, msg.st.Installations
			if v.sel >= len(v.insts) {
				v.sel = 0
			}
		}
		return v, nil
	case wizLoginStartedMsg:
		if msg.err != nil {
			v.err, v.state = msg.err, wizLogin
			return v, nil
		}
		v.handle, v.code, v.err = msg.handle, msg.code, nil
		v.state = wizLoginPolling
		return v, nil
	case wizLoginDoneMsg:
		if msg.pending {
			return v, nil
		}
		if msg.err != nil {
			v.err = msg.err // banner; stay polling — the next tick retries
			return v, nil
		}
		v.err = nil
		v.cred = msg.acc.AccountCredential
		if msg.acc.InstallURL != "" {
			// One-trip carry-over: the relay already bounced the browser to the
			// install page; show the URL and watch status until it lands.
			v.state, v.installURL = wizInstall, msg.acc.InstallURL
			return v, nil
		}
		v.state = wizLoading // has installs already (or BYO relay): re-probe decides
		return v, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			switch v.state {
			case wizLogin:
				return v.startLogin()
			case wizInstalled:
				if len(v.insts) > 0 {
					sub := wizardReposView{relay: v.relay, base: v.base, cred: v.cred, inst: v.insts[v.sel]}
					return v, func() tea.Msg { return pushMsg{view: sub} }
				}
			}
		case "m":
			if v.state == wizLogin || v.state == wizByo || v.state == wizInstalled {
				return v, func() tea.Msg { return pushMsg{view: newManifestView()} }
			}
		case "o":
			if v.state == wizInstall && v.installURL != "" {
				_ = openBrowser(v.installURL)
			}
		case "up", "k":
			if v.state == wizInstalled && v.sel > 0 {
				v.sel--
			}
		case "down", "j":
			if v.state == wizInstalled && v.sel < len(v.insts)-1 {
				v.sel++
			}
		}
	}
	return v, nil
}

// startLogin asks the relay for a login handle + user code and opens the
// browser at the verify page. The human side happens there; the wizard then
// polls the handle on the tick.
func (v githubWizardView) startLogin() (tea.Model, tea.Cmd) {
	relay, base := v.relay, v.base
	v.err = nil
	return v, func() tea.Msg {
		handle, code, err := relay(base).CLILoginStart(context.Background())
		if err != nil {
			return wizLoginStartedMsg{err: err}
		}
		_ = openBrowser(verifyURL(base))
		return wizLoginStartedMsg{handle: handle, code: code}
	}
}

func verifyURL(base string) string { return strings.TrimRight(base, "/") + "/v1/login/cli" }

// probeStatus loads client config and, when a credential exists, asks the
// relay for GitHub App status. Runs inside a tea.Cmd, off the UI thread.
func probeStatus(relay RelayDialer) tea.Msg {
	cc, err := config.LoadClient()
	if err != nil {
		return wizStatusMsg{err: err}
	}
	base := cc.RelayAPI
	if base == "" {
		base = relayclient.DefaultAPI
	}
	if cc.AccountCredential == "" {
		return wizStatusMsg{noCred: true, base: base}
	}
	return probeWith(relay, base, cc.AccountCredential)
}

func probeWith(relay RelayDialer, base, cred string) tea.Msg {
	st, err := relay(base).GitHubStatus(context.Background(), cred)
	if err != nil {
		return wizStatusMsg{base: base, cred: cred, err: err}
	}
	return wizStatusMsg{base: base, cred: cred, st: st}
}

// pollLogin polls one brokered-login round and, on success, saves the
// credential + relay base to client config before reporting — all off the UI
// thread, mirroring the CLI's relayLoginWeb.
func pollLogin(relay RelayDialer, base, handle string) tea.Msg {
	acc, err := relay(base).CLILoginPoll(context.Background(), handle)
	if errors.Is(err, relayclient.ErrAuthPending) {
		return wizLoginDoneMsg{pending: true}
	}
	if err != nil {
		return wizLoginDoneMsg{err: err}
	}
	cc, err := config.LoadClient()
	if err != nil {
		return wizLoginDoneMsg{err: err}
	}
	cc.RelayAPI = base
	cc.AccountCredential = acc.AccountCredential
	if err := config.SaveClient(cc); err != nil {
		return wizLoginDoneMsg{err: err}
	}
	return wizLoginDoneMsg{acc: acc}
}

func (v githubWizardView) View() string {
	var b strings.Builder
	switch v.state {
	case wizLoading:
		b.WriteString("  checking GitHub status…\n")
	case wizLogin:
		fmt.Fprintf(&b, "  sign in with GitHub via %s\n\n", v.base)
		b.WriteString("  ↵ opens your browser; you'll enter a short code there.\n")
	case wizLoginPolling:
		b.WriteString("  finish signing in — enter this code in your browser:\n\n")
		fmt.Fprintf(&b, "      %s\n\n      %s\n\n", v.code, verifyURL(v.base))
		b.WriteString("  waiting…\n")
	case wizByo:
		b.WriteString("  this relay doesn't broker a GitHub App.\n\n")
		b.WriteString("  press m to create a self-held App on this box (manifest flow).\n")
	case wizInstall:
		b.WriteString("  install the Piper GitHub App on the repos you want to deploy:\n\n")
		fmt.Fprintf(&b, "      %s\n\n", v.installURL)
		b.WriteString("  waiting for the install…\n")
	case wizInstalled:
		b.WriteString("  GitHub connected — installations:\n\n")
		for i, in := range v.insts {
			marker := "  "
			if i == v.sel {
				marker = "▸ "
			}
			fmt.Fprintf(&b, "  %s%s (%s)\n", marker, in.TargetLogin, in.TargetType)
		}
	}
	if v.err != nil {
		fmt.Fprintf(&b, "\n ⚠ %v\n", v.err)
	}
	return b.String()
}

// wizardReposView is the wizard's read-only repo listing for one installation,
// pushed by ↵ on the installations list; esc pops back to the wizard. Repos
// load once, not per tick — GitHubRepos proxies to GitHub's API and the
// listing has no live state worth the rate-limit spend.
type wizardReposView struct {
	relay      RelayDialer
	base, cred string
	inst       relayclient.Installation
	repos      []relayclient.Repo
	loaded     bool
	err        error
}

func (v wizardReposView) Init() tea.Cmd { return nil }

func (v wizardReposView) title() string { return "repos" }

func (v wizardReposView) footer() string { return "esc back · ? help" }

func (v wizardReposView) refresh(API) tea.Cmd {
	if v.loaded {
		return nil
	}
	relay, base, cred, id := v.relay, v.base, v.cred, v.inst.ID
	return func() tea.Msg {
		repos, err := relay(base).GitHubRepos(context.Background(), cred, id)
		return wizReposMsg{repos: repos, err: err}
	}
}

func (v wizardReposView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m, ok := msg.(wizReposMsg); ok {
		v.loaded = true
		v.repos, v.err = m.repos, m.err
	}
	return v, nil
}

func (v wizardReposView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s (%s) — repositories:\n\n", v.inst.TargetLogin, v.inst.TargetType)
	switch {
	case !v.loaded:
		b.WriteString("  loading…\n")
	case len(v.repos) == 0 && v.err == nil:
		b.WriteString("  no repositories\n")
	}
	for _, r := range v.repos {
		if r.Visibility != "" && r.Visibility != "public" {
			fmt.Fprintf(&b, "  %s (%s)\n", r.FullName, r.Visibility)
		} else {
			fmt.Fprintf(&b, "  %s\n", r.FullName)
		}
	}
	if v.err != nil {
		fmt.Fprintf(&b, "\n ⚠ %v\n", v.err)
	}
	return b.String()
}
```

In `app.go`, point `g` at the wizard (replacing the Task 2 temporary):

```go
			case "g":
				if _, ok := m.top().(githubWizardView); !ok {
					return m, func() tea.Msg { return pushMsg{newGithubWizard(m.relay)} }
				}
				return m, nil
```

(The esc / `githubFormReadyMsg` / `githubDoneMsg` cases keep asserting `manifestView` — the manifest flow still runs exactly as before when pushed from the wizard.)

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/tui/ -v -run 'TestWizard|TestGKey'`
Expected: all PASS.
Then the whole package: `go test ./internal/tui/` — PASS (manifest, login, boxes, apps tests untouched).

- [ ] **Step 6: Commit**

```bash
git add internal/tui/
git commit -m "feat(cli): TUI github wizard — relay login, install wait, installations

g now opens a status-driven wizard: no credential → one-trip browser
login (code + URL, poll); no installation → install URL + status poll;
installed → installations with a pushed per-installation repo listing.
BYO relays route to the manifest flow via m. Relay errors banner
in-view and never mark the box unreachable.

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: link form — repo picker + root-dir

**Files:**
- Modify: `internal/tui/linkform.go` (picker + third field)
- Modify: `internal/tui/linkform_test.go`
- Modify: `internal/tui/tui.go` (`linkAppMsg` gains `rootDir`; new `linkReposMsg`)
- Modify: `internal/tui/app.go` (`pushMsg` injects `m.relay` into a pushed `linkFormView`; `linkAppMsg` passes `rootDir`)
- Modify: `internal/tui/app_test.go` (`apiCalls.linkRootDir`, `fakeAPI.LinkApp` records it)

**Interfaces:**
- Consumes: `RelayDialer`, `Model.relay` (Task 2); `fakeRelay`/`relayFor` (Task 3); `relayclient.Status`/`Repo`/`Installation`; `config.LoadClient`, `config.SaveClient` (test setup); `footerStyle` (app.go).
- Produces: `linkAppMsg{name, repo, branch, rootDir string}`, `linkReposMsg{repos []pickRepo; multi, noCred bool}`, `pickRepo{fullName, target string}`, `loadLinkRepos(relay RelayDialer) tea.Msg`. `newLinkForm(app string)` keeps its one-arg signature — the root injects `relay` on push.

- [ ] **Step 1: Write the failing tests**

In `internal/tui/app_test.go`, add `linkRootDir string` to `apiCalls` (after `linkBranch`) and record it in `fakeAPI.LinkApp`:

```go
func (f fakeAPI) LinkApp(name, repo, branch, rootDir string) error {
	if f.rec != nil {
		f.rec.linkName, f.rec.linkRepo, f.rec.linkBranch, f.rec.linkRootDir = name, repo, branch, rootDir
	}
	return f.linkErr
}
```

In `internal/tui/linkform_test.go`, update `TestLinkAppRootRunsClientAndPops`'s intent + assertion:

```go
	next, cmd := m.Update(linkAppMsg{name: "blog", repo: "octo/blog", branch: "main", rootDir: "apps/web"})
	...
	if rec.linkRepo != "octo/blog" || rec.linkName != "blog" || rec.linkRootDir != "apps/web" {
		t.Fatalf("LinkApp not called with the right args: %+v", rec)
	}
```

and append the new tests:

```go
func TestLinkFormRootDirSubmitted(t *testing.T) {
	v := typeLinkRepo(t, newLinkForm("blog"), "octo/blog")
	next, _ := v.Update(keyTab()) // → branch
	next, _ = next.(linkFormView).Update(keyTab()) // → root dir
	lf := next.(linkFormView)
	for _, r := range "apps/web" {
		n, _ := lf.Update(keyRunes(r))
		lf = n.(linkFormView)
	}
	_, cmd := lf.Update(keyEnter())
	if cmd == nil {
		t.Fatal("expected a link cmd")
	}
	msg := cmd().(linkAppMsg)
	if msg.rootDir != "apps/web" || msg.repo != "octo/blog" || msg.branch != "main" {
		t.Fatalf("unexpected linkAppMsg: %+v", msg)
	}
}

func fixturePickRepos() linkReposMsg {
	return linkReposMsg{repos: []pickRepo{
		{fullName: "octo/blog", target: "octo"},
		{fullName: "octo/site", target: "octo"},
		{fullName: "acme/api", target: "acme-org"},
	}}
}

func TestLinkFormPickerFiltersAndFills(t *testing.T) {
	next, _ := newLinkForm("blog").Update(fixturePickRepos())
	v := typeLinkRepo(t, next.(linkFormView), "octo")
	out := v.View()
	if !strings.Contains(out, "octo/blog") || !strings.Contains(out, "octo/site") || strings.Contains(out, "acme/api") {
		t.Fatalf("filter should keep octo repos only, got:\n%s", out)
	}
	n, _ := v.Update(tea.KeyMsg(tea.Key{Type: tea.KeyDown})) // select first match
	n, _ = n.(linkFormView).Update(keyEnter())               // accept → fills repo, focus branch
	v = n.(linkFormView)
	if got := strings.TrimSpace(v.repo.Value()); got != "octo/blog" {
		t.Fatalf("accept should fill the repo field, got %q", got)
	}
	_, cmd := v.Update(keyEnter()) // enter from branch submits
	msg := cmd().(linkAppMsg)
	if msg.repo != "octo/blog" {
		t.Fatalf("submit after pick: %+v", msg)
	}
}

func TestLinkFormEnterWithoutSelectionSubmitsFreeText(t *testing.T) {
	next, _ := newLinkForm("blog").Update(fixturePickRepos())
	v := typeLinkRepo(t, next.(linkFormView), "someone/else")
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("free text must submit without a selection")
	}
	if msg := cmd().(linkAppMsg); msg.repo != "someone/else" {
		t.Fatalf("want the typed repo, got %+v", msg)
	}
}

func TestLinkFormTargetLabelOnlyWhenMultipleInstallations(t *testing.T) {
	multi := fixturePickRepos()
	multi.multi = true
	next, _ := newLinkForm("blog").Update(multi)
	if out := next.(linkFormView).View(); !strings.Contains(out, "acme-org") {
		t.Fatalf("multi-installation matches must carry their target, got:\n%s", out)
	}
	next, _ = newLinkForm("blog").Update(fixturePickRepos())
	if out := next.(linkFormView).View(); strings.Contains(out, "acme-org") {
		t.Fatalf("single-installation matches must not repeat the target, got:\n%s", out)
	}
}

func TestLinkFormNoCredShowsHint(t *testing.T) {
	next, _ := newLinkForm("blog").Update(linkReposMsg{noCred: true})
	if out := next.(linkFormView).View(); !strings.Contains(out, "press g to connect GitHub") {
		t.Fatalf("want the login hint, got:\n%s", out)
	}
}

func TestLinkFormLoadsRelayReposOnceAndFlattens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveClient(config.ClientConfig{RelayAPI: "https://r.example", AccountCredential: "cred-1"}); err != nil {
		t.Fatal(err)
	}
	relay := relayFor(fakeRelay{
		st: relayclient.Status{GitHubApp: true, Installations: []relayclient.Installation{
			{ID: "55", TargetLogin: "octo"}, {ID: "66", TargetLogin: "acme-org"},
		}},
		repos: []relayclient.Repo{{FullName: "octo/blog"}},
	})
	msg := loadLinkRepos(relay).(linkReposMsg)
	// the fake returns the same repos for both installations: 2 flattened entries
	if len(msg.repos) != 2 || !msg.multi || msg.repos[1].target != "acme-org" {
		t.Fatalf("want flattened multi-install repos, got %+v", msg)
	}
	v := newLinkForm("blog")
	v.relay = relay
	if v.refresh(nil) == nil {
		t.Fatal("with a relay and no repos yet, refresh must load")
	}
	next, _ := v.Update(msg)
	if next.(linkFormView).refresh(nil) != nil {
		t.Fatal("repos load once; refresh must go quiet after the load")
	}
}

func TestRootInjectsRelayIntoLinkForm(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(relayFor(fakeRelay{}))
	next, _ := m.Update(pushMsg{view: newLinkForm("blog")})
	lf, ok := next.(Model).top().(linkFormView)
	if !ok || lf.relay == nil {
		t.Fatal("pushing a link form must inject the root's relay dialer")
	}
}
```

Add `"github.com/getpiper/piper/internal/config"` and `"github.com/getpiper/piper/internal/relayclient"` to `linkform_test.go`'s imports.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tui/ -run TestLink -v`
Expected: compile FAIL — `msg.rootDir undefined`, `undefined: pickRepo`, etc.

- [ ] **Step 3: Extend the messages and the root**

In `tui.go`, change `linkAppMsg` and add `linkReposMsg` beside it:

```go
	// linkAppMsg is the link form's intent; the root runs LinkApp off the UI
	// thread and reports via actionResultMsg (pop back to app detail on success).
	linkAppMsg struct{ name, repo, branch, rootDir string }

	// linkReposMsg is the link form's repo-picker load: every installation's
	// repos flattened, multi marking >1 installation (matches get target
	// labels), noCred meaning no relay login (the form hints at g and stays
	// free-text). Errors degrade to an empty list — never a banner, never a
	// box-status change: linking by hand must always work.
	linkReposMsg struct {
		repos  []pickRepo
		multi  bool
		noCred bool
	}
```

In `app.go`:
- `pushMsg` case — inject the relay factory before stacking:

```go
	case pushMsg:
		if lf, ok := msg.view.(linkFormView); ok && lf.relay == nil {
			lf.relay = m.relay // the pushing view doesn't hold the factory; the root does
			msg.view = lf
		}
		m.stack = append(m.stack, msg.view)
```

- `linkAppMsg` case — pass `rootDir` through (the stale comment about #316 goes away):

```go
	case linkAppMsg:
		name, repo, branch, rootDir, c := msg.name, msg.repo, msg.branch, msg.rootDir, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.LinkApp(name, repo, branch, rootDir), popLevels: 1} }
```

- [ ] **Step 4: Rewrite `linkform.go`**

```go
package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/config"
)

// pickRepo is one repo-picker entry: the owner/name to link and the
// installation target it came from (shown only when >1 installation, #322).
type pickRepo struct{ fullName, target string }

// linkFormView attaches a git repo to an app: repo (owner/name), branch
// (default main), and an optional monorepo root dir. When the CLI is logged in
// to a relay, the repo field doubles as a filter over the account's
// installation repos; free text always submits, so LAN/BYO boxes and relay
// outages never block linking by hand. On submit it emits linkAppMsg; the root
// runs LinkApp and pops back to app detail on success.
type linkFormView struct {
	app     string
	relay   RelayDialer // injected by the root on push; nil in bare tests → free text
	repo    textinput.Model
	branch  textinput.Model
	rootDir textinput.Model
	focus   int        // 0 repo, 1 branch, 2 root dir
	repos   []pickRepo // nil until loaded; non-nil (possibly empty) stops reloading
	multi   bool       // >1 installation: matches carry their target label
	noCred  bool       // not logged in to a relay: show the g hint
	sel     int        // selected match; -1 = none (enter submits free text)
	err     error
}

func newLinkForm(app string) linkFormView {
	repo := textinput.New()
	repo.Placeholder = "owner/name"
	repo.Focus()
	branch := textinput.New()
	branch.Placeholder = "main"
	branch.SetValue("main")
	rootDir := textinput.New()
	rootDir.Placeholder = "(repo root)"
	return linkFormView{app: app, repo: repo, branch: branch, rootDir: rootDir, sel: -1}
}

func (v linkFormView) Init() tea.Cmd { return nil }

func (v linkFormView) title() string { return "link" }

// refresh loads the picker list once. It rides the root's tick, so it retries
// until the load lands; a non-nil (even empty) repos stops it.
func (v linkFormView) refresh(API) tea.Cmd {
	if v.relay == nil || v.repos != nil || v.noCred {
		return nil
	}
	relay := v.relay
	return func() tea.Msg { return loadLinkRepos(relay) }
}

// loadLinkRepos fetches every installation's repos as one flat picker list.
// All failures degrade to free text — a LAN/BYO box or an unreachable relay
// must never block linking by hand.
func loadLinkRepos(relay RelayDialer) tea.Msg {
	cc, err := config.LoadClient()
	if err != nil || cc.RelayAPI == "" || cc.AccountCredential == "" {
		return linkReposMsg{noCred: true}
	}
	rc := relay(cc.RelayAPI)
	st, err := rc.GitHubStatus(context.Background(), cc.AccountCredential)
	if err != nil {
		return linkReposMsg{repos: []pickRepo{}}
	}
	repos := []pickRepo{}
	for _, in := range st.Installations {
		rs, err := rc.GitHubRepos(context.Background(), cc.AccountCredential, in.ID)
		if err != nil {
			continue
		}
		for _, r := range rs {
			repos = append(repos, pickRepo{fullName: r.FullName, target: in.TargetLogin})
		}
	}
	return linkReposMsg{repos: repos, multi: len(st.Installations) > 1}
}

func (v linkFormView) capturesText() bool { return true }

func (v linkFormView) footer() string {
	return "↵ link · tab switch · ↑↓ pick · esc cancel · ? help"
}

func (v *linkFormView) applyFocus() {
	v.repo.Blur()
	v.branch.Blur()
	v.rootDir.Blur()
	switch v.focus {
	case 0:
		v.repo.Focus()
	case 1:
		v.branch.Focus()
	default:
		v.rootDir.Focus()
	}
}

// matches returns up to six loaded repos whose full name contains the typed
// filter, case-insensitively. An empty filter matches the first six.
func (v linkFormView) matches() []pickRepo {
	q := strings.ToLower(strings.TrimSpace(v.repo.Value()))
	var out []pickRepo
	for _, r := range v.repos {
		if strings.Contains(strings.ToLower(r.fullName), q) {
			out = append(out, r)
			if len(out) == 6 {
				break
			}
		}
	}
	return out
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
	name, rootDir := v.app, strings.TrimSpace(v.rootDir.Value())
	return v, func() tea.Msg { return linkAppMsg{name: name, repo: repo, branch: branch, rootDir: rootDir} }
}

func (v linkFormView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case linkReposMsg:
		v.repos, v.multi, v.noCred = msg.repos, msg.multi, msg.noCred
		return v, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "tab":
			v.focus = (v.focus + 1) % 3
			v.sel = -1
			v.applyFocus()
			return v, nil
		case "up":
			if v.focus == 0 && v.sel >= 0 {
				v.sel--
			}
			return v, nil
		case "down":
			if v.focus == 0 && v.sel < len(v.matches())-1 {
				v.sel++
			}
			return v, nil
		case "enter":
			if v.focus == 0 && v.sel >= 0 {
				v.repo.SetValue(v.matches()[v.sel].fullName)
				v.repo.CursorEnd()
				v.sel = -1
				v.focus = 1
				v.applyFocus()
				return v, nil
			}
			return v.submit()
		}
	}
	v.err = nil // any keystroke clears a stale banner
	var cmd tea.Cmd
	switch v.focus {
	case 0:
		v.repo, cmd = v.repo.Update(msg)
		v.sel = -1 // typing resets the pick; enter now submits free text
	case 1:
		v.branch, cmd = v.branch.Update(msg)
	default:
		v.rootDir, cmd = v.rootDir.Update(msg)
	}
	return v, cmd
}

func (v linkFormView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  link %s to a repo\n\n", v.app)
	fmt.Fprintf(&b, "  repo      %s\n", v.repo.View())
	if v.focus == 0 {
		for i, r := range v.matches() {
			marker := "    "
			if i == v.sel {
				marker = "  ▸ "
			}
			line := r.fullName
			if v.multi {
				line += footerStyle.Render(" · " + r.target)
			}
			fmt.Fprintf(&b, "%s%s\n", marker, line)
		}
	}
	fmt.Fprintf(&b, "  branch    %s\n", v.branch.View())
	fmt.Fprintf(&b, "  root dir  %s\n\n", v.rootDir.View())
	if v.noCred {
		b.WriteString(footerStyle.Render("  press g to connect GitHub") + "\n\n")
	}
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ link   tab switch   esc cancel")
	return b.String()
}
```

Note: `up`/`down` no longer cycle fields (spec: cycling is `tab` only — the arrows drive the match list).

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/tui/`
Expected: PASS — including the pre-existing `TestLinkFormSubmitEmitsLinkAppMsg` (free text, no picker) and `TestLinkFormEmptyRepoRejected`.

- [ ] **Step 6: Commit**

```bash
git add internal/tui/
git commit -m "feat(cli): TUI link form — installation repo picker + root-dir field

The repo input doubles as a filter over all installations' repos
(target-labeled when the account has several, #322); enter accepts a
pick or submits free text. New root-dir input wires apps.root_dir
(#318) through linkAppMsg instead of the hardcoded empty string.

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: help text, PROGRESS.md, verify, PR

**Files:**
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/help_test.go` (only if its assertions reference changed lines — check first)
- Modify: `PROGRESS.md`

**Interfaces:** consumes everything above; produces nothing new.

- [ ] **Step 1: Update the help overlay**

In `help.go`, extend the keymap (keep the existing lines' style; `g github` under Apps list stays):

```go
		"  App detail  ↑/k ↓/j move · enter logs / domain · d deploy · s stop / start · x delete app / remove domain · l link · a add domain\n" +
		"  GitHub      ↵ sign in / open repos · ↑↓ move · m manifest app · o open install page\n" +
```

Run `go test ./internal/tui/ -run TestHelp` — if `help_test.go` asserts on the full text, update its expectations to match.

- [ ] **Step 2: PROGRESS.md**

Read `PROGRESS.md`, find the TUI/CLI section, and add one line in the file's established style, e.g.:

```markdown
- TUI: relay GitHub wizard (login → install → repos) + link-form repo picker/root-dir [#N]
```

- [ ] **Step 3: Full verification**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests pass, arm64 cross-build succeeds. Fix anything it flags before continuing.

- [ ] **Step 4: Commit and open the PR**

```bash
git add internal/tui/help.go internal/tui/help_test.go PROGRESS.md
git commit -m "chore: help keymap + PROGRESS entry for the TUI github flows

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push -u origin ozykhan/tui-github-flows
gh pr create --base main \
  --title "[cli] TUI: relay GitHub onboarding wizard + repo picker" \
  --body "$(cat <<'EOF'
Brings the v0.6.0 relay-brokered GitHub flows into the TUI, per
docs/superpowers/specs/2026-07-23-tui-github-flows-design.md:

- relayclient.GitHubStatus now returns Status (github_app, install_url,
  installations — fields the relay already sent); DefaultAPI hoisted.
- New RelayAPI/RelayDialer seam in internal/tui; the old githubView is
  renamed manifestView and survives as the wizard's BYO path.
- `g` opens a status-driven wizard: one-trip browser login (code + URL,
  poll) → App install wait → installations with per-installation repo
  browsing. Relay errors banner in-view, never as box-unreachable.
- Link form: the repo field filters all installations' repos
  (target-labeled when >1 installation), and a root-dir field wires
  apps.root_dir through (previously hardcoded "").

Closes #N

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```
