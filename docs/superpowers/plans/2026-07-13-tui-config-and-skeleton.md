# Piper TUI — Phases 1+2 (multi-box config + TUI skeleton) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the first two phases of the approved TUI spec ([`docs/superpowers/specs/2026-07-12-piper-tui-design.md`](../specs/2026-07-12-piper-tui-design.md)): multi-box client config (schema v2, silent migration) and the TUI skeleton — bare `piper` in a TTY opens a live, read-only apps dashboard with a status bar.

**Architecture:** Phase 1 restructures `~/.piper/piper/config.json` into named boxes while `LoadClient`/`SaveClient` keep their exact signatures and semantics (all existing CLI callers unchanged). Phase 2 adds `internal/tui` — a Bubble Tea root model owning a view stack, a 2s poll tick, and a status bar — plus the TTY entry seam in `cmd/piper`. Views call the API through a narrow `tui.API` interface; tests inject fakes.

**Tech Stack:** Go, `charmbracelet/bubbletea` v1 + `lipgloss` v1 (pinned to the v1 majors — the code in this plan uses the v1 API), `golang.org/x/term`.

## Global Constraints

- `CGO_ENABLED=0` everywhere; `make cross` (linux/arm64) must stay green.
- Module path `github.com/getpiper/piper`; nothing imports "up" the layering (`tui` imports `client`+`config`+`api` types only; `cmd/piper` imports `tui`).
- Deployment status strings are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`; `""` means never deployed.
- Defaults: control API `127.0.0.1:8088`, base domain `piper.localhost`.
- TDD: every task writes the failing test first. Run `make verify` before every push.
- One commit per task step group, conventional-commit style, ending with:
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`
- Trunk-based: never commit to `main`; one PR per phase, squash-merged.

**Spec deviations locked in this plan (both noted for the spec's later phases):**
1. Apps table columns are `NAME · STATUS · URL` — `ListApps` returns no last-deploy timestamp, so LAST DEPLOY joins in phase 3 (which fetches deployments anyway).
2. `bubbles` is not added yet — the skeleton hand-renders its table; `bubbles` arrives with the first widget need (phase 3 viewport).

---

### Task 1: Tracker + spec PR

**Files:** none (GitHub + git only)

**Interfaces:**
- Produces: epic issue number `#E`, phase issue numbers `#P1`, `#P2` — used in every later commit/PR body. Record them in the plan-execution notes.

- [ ] **Step 1: Open the spec PR** (branch `ozykhan/tui-design` already holds the spec + gitignore commits)

```bash
git push -u origin ozykhan/tui-design
gh pr create --base main --title "docs: piper TUI dual-mode design spec" \
  --body "Approved brainstorm output: design spec for the dual-mode TUI. Plan for phases 1+2 included. Part of the TUI epic (see issue opened alongside)."
```

- [ ] **Step 2: Create the epic and child issues**

```bash
gh issue create --title "[cli] interactive TUI: dual-mode piper (epic)" \
  --label epic --label cli --label enhancement --label P3 --label "size/XL" \
  --body "$(cat <<'EOF'
Bare `piper` in a TTY opens a full-screen control surface; every existing subcommand stays scriptable and unchanged. Spec: docs/superpowers/specs/2026-07-12-piper-tui-design.md

- [ ] Phase 1: multi-box client config (schema v2)
- [ ] Phase 2: TUI skeleton (entry, root model, status bar, apps table)
- [ ] Phase 3: drill-down (app detail, deployments, logs + follow)
- [ ] Phase 4: actions (deploy w/ streaming, new app, stop/delete)
- [ ] Phase 5: boxes view (switcher + config editor)
- [ ] Phase 6: wizards (login/connect, GitHub setup, link repo)
EOF
)"
gh issue create --title "[cli] multi-box client config (schema v2 + migration)" \
  --label cli --label enhancement --label P3 --label "size/S" \
  --body "Phase 1 of the TUI epic #E. Named boxes list + current selection in ~/.piper/piper/config.json; legacy flat file migrates silently; LoadClient/SaveClient signatures and CLI behavior unchanged."
gh issue create --title "[cli] TUI skeleton: bare-piper entry, root model, status bar, apps table" \
  --label cli --label enhancement --label P3 --label "size/M" \
  --body "Phase 2 of the TUI epic #E. bubbletea+lipgloss, internal/tui with root model + view stack + 2s poll, read-only apps table, TTY entry seam in cmd/piper."
```

- [ ] **Step 3: Edit the epic body** replacing the phase-1/2 checklist lines with links to the real issue numbers (`gh issue edit E --body …`), and note `#E`, `#P1`, `#P2` for later steps.

---

### Task 2: Config schema v2 — `Box`, `ClientFile`, load/save + migration

**Files:**
- Modify: `internal/config/config.go` (add below the existing `ClientConfig` block, line ~117)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: existing `clientConfigPath()`, `ClientConfig`.
- Produces (used by Tasks 3 and 9):
  - `type Box struct { Name, Addr, Token, RelayAPI, AccountCredential string }` (json: `name`, `addr`, `token`, `relay_api,omitempty`, `account_credential,omitempty`)
  - `type ClientFile struct { Boxes []Box; Current string }` (json: `boxes`, `current`)
  - `func (cf ClientFile) CurrentBox() (Box, bool)`
  - `func LoadClientFile() (ClientFile, error)`
  - `func SaveClientFile(cf ClientFile) error`

- [ ] **Step 1: Create the working branch**

```bash
git checkout main && git pull && git checkout -b ozykhan/multibox-config
```

- [ ] **Step 2: Write the failing tests** — append to `internal/config/config_test.go`:

```go
func TestLoadClientFileMigratesLegacyFlat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".piper", "piper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"addr":"http://192.168.1.6:8088","token":"tok","relay_api":"https://api.relay","account_credential":"cred"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	cf, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 1 || cf.Current != "default" {
		t.Fatalf("want 1 box current=default, got %+v", cf)
	}
	b := cf.Boxes[0]
	if b.Name != "default" || b.Addr != "http://192.168.1.6:8088" || b.Token != "tok" ||
		b.RelayAPI != "https://api.relay" || b.AccountCredential != "cred" {
		t.Fatalf("migrated box wrong: %+v", b)
	}
}

func TestLoadClientFileMissingIsEmptyNotError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cf, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 0 {
		t.Fatalf("want no boxes, got %+v", cf)
	}
}

func TestSaveClientFileRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	in := ClientFile{
		Boxes: []Box{
			{Name: "pi4", Addr: "http://192.168.1.6:8088", Token: "a"},
			{Name: "local", Addr: "http://127.0.0.1:8088", Token: "b", RelayAPI: "https://api.relay", AccountCredential: "cred"},
		},
		Current: "pi4",
	}
	if err := SaveClientFile(in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestCurrentBoxFallsBackToFirst(t *testing.T) {
	cf := ClientFile{Boxes: []Box{{Name: "a"}, {Name: "b"}}, Current: "missing"}
	b, ok := cf.CurrentBox()
	if !ok || b.Name != "a" {
		t.Fatalf("want first box fallback, got %+v ok=%v", b, ok)
	}
	if _, ok := (ClientFile{}).CurrentBox(); ok {
		t.Fatal("empty file must report no box")
	}
}
```

Add `"reflect"` and (if missing) `"path/filepath"`, `"os"` to the test imports.

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/config/ -run 'ClientFile|CurrentBox' -v`
Expected: FAIL — `undefined: LoadClientFile`, `undefined: Box`, …

- [ ] **Step 4: Implement** — insert into `internal/config/config.go` directly after the `ClientConfig` type (after line 117):

```go
// Box is one named piperd target in the piper CLI's config file. Addr/Token
// are the LAN path; RelayAPI/AccountCredential the relay path (wizard-managed).
type Box struct {
	Name              string `json:"name"`
	Addr              string `json:"addr"`
	Token             string `json:"token"`
	RelayAPI          string `json:"relay_api,omitempty"`
	AccountCredential string `json:"account_credential,omitempty"`
}

// ClientFile is the on-disk shape of ~/.piper/piper/config.json (schema v2):
// named boxes plus the current selection. A legacy flat ClientConfig file
// loads as a single box named "default"; the file itself is only rewritten
// in v2 form by the next save.
type ClientFile struct {
	Boxes   []Box  `json:"boxes"`
	Current string `json:"current"`
}

// CurrentBox returns the box named by Current, falling back to the first box.
func (cf ClientFile) CurrentBox() (Box, bool) {
	for _, b := range cf.Boxes {
		if b.Name == cf.Current {
			return b, true
		}
	}
	if len(cf.Boxes) > 0 {
		return cf.Boxes[0], true
	}
	return Box{}, false
}

// LoadClientFile reads ~/.piper/piper/config.json in v2 form, migrating a
// legacy flat file in-memory. A missing file is not an error.
func LoadClientFile() (ClientFile, error) {
	var cf ClientFile
	path, err := clientConfigPath()
	if err != nil {
		return cf, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cf, nil
	}
	if err != nil {
		return cf, err
	}
	_ = json.Unmarshal(data, &cf)
	if len(cf.Boxes) > 0 {
		if cf.Current == "" {
			cf.Current = cf.Boxes[0].Name
		}
		return cf, nil
	}
	var legacy ClientConfig
	_ = json.Unmarshal(data, &legacy)
	if legacy == (ClientConfig{}) {
		return cf, nil
	}
	cf.Boxes = []Box{{
		Name:              "default",
		Addr:              legacy.Addr,
		Token:             legacy.Token,
		RelayAPI:          legacy.RelayAPI,
		AccountCredential: legacy.AccountCredential,
	}}
	cf.Current = "default"
	return cf, nil
}

// SaveClientFile writes cf to ~/.piper/piper/config.json with 0600 perms,
// creating the directory if needed.
func SaveClientFile(cf ClientFile) error {
	path, err := clientConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
```

- [ ] **Step 5: Run to verify pass**

Run: `go test ./internal/config/ -v`
Expected: all PASS (new and pre-existing).

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(cli): add multi-box client config schema with legacy migration

Part of #P1

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Rewire `LoadClient`/`SaveClient` over `ClientFile`

**Files:**
- Modify: `internal/config/config.go:127-169` (the existing `LoadClient` and `SaveClient` bodies)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: Task 2's `LoadClientFile`, `SaveClientFile`, `CurrentBox`.
- Produces: `LoadClient() (ClientConfig, error)` and `SaveClient(ClientConfig) error` — **signatures and observable CLI semantics unchanged** (env overrides `PIPER_ADDR`/`PIPER_TOKEN`, default addr `http://127.0.0.1:8088`, load-mutate-save callers in `cmd/piper` keep working).

- [ ] **Step 1: Write the failing tests** — append to `internal/config/config_test.go`:

```go
func TestLoadClientReadsCurrentBox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClientFile(ClientFile{
		Boxes: []Box{
			{Name: "pi4", Addr: "http://192.168.1.6:8088", Token: "pt"},
			{Name: "local", Addr: "http://127.0.0.1:8088", Token: "lt"},
		},
		Current: "pi4",
	}); err != nil {
		t.Fatal(err)
	}
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Addr != "http://192.168.1.6:8088" || cc.Token != "pt" {
		t.Fatalf("want current box pi4, got %+v", cc)
	}
}

func TestLoadClientEnvOverridesStillApply(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "http://env.test:9000")
	t.Setenv("PIPER_TOKEN", "envtok")
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Addr != "http://env.test:9000" || cc.Token != "envtok" {
		t.Fatalf("env overrides lost: %+v", cc)
	}
}

func TestSaveClientUpdatesOnlyCurrentBox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := SaveClientFile(ClientFile{
		Boxes: []Box{
			{Name: "pi4", Addr: "http://192.168.1.6:8088", Token: "old", RelayAPI: "https://api.relay", AccountCredential: "cred"},
			{Name: "local", Addr: "http://127.0.0.1:8088", Token: "lt"},
		},
		Current: "pi4",
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveClient(ClientConfig{Addr: "http://192.168.1.6:8088", Token: "new", RelayAPI: "https://api.relay", AccountCredential: "cred"}); err != nil {
		t.Fatal(err)
	}
	cf, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 2 {
		t.Fatalf("other boxes lost: %+v", cf)
	}
	b, _ := cf.CurrentBox()
	if b.Token != "new" || b.RelayAPI != "https://api.relay" {
		t.Fatalf("current box not updated: %+v", b)
	}
	if cf.Boxes[1].Token != "lt" {
		t.Fatalf("sibling box mutated: %+v", cf.Boxes[1])
	}
}

func TestSaveClientMigratesLegacyFlatFileToV2(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".piper", "piper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"addr":"http://127.0.0.1:8088","token":"old"}`
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveClient(ClientConfig{Addr: "http://127.0.0.1:8088", Token: "new"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"boxes"`) || !strings.Contains(string(data), `"default"`) {
		t.Fatalf("file not rewritten in v2 form: %s", data)
	}
}
```

Add `"strings"` to the test imports if missing.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ -run 'LoadClient|SaveClient' -v`
Expected: `TestLoadClientReadsCurrentBox` and `TestSaveClient*` FAIL (flat read/write ignores boxes); `TestLoadClientEnvOverridesStillApply` may already pass — that's fine, it pins existing behavior.

- [ ] **Step 3: Replace the bodies of `LoadClient` and `SaveClient`** (keep their doc comments, updating the SaveClient one):

```go
// LoadClient reads the current box from ~/.piper/piper/config.json, then
// applies PIPER_ADDR / PIPER_TOKEN env overrides and the localhost default
// for Addr. A missing file is not an error.
func LoadClient() (ClientConfig, error) {
	var cc ClientConfig
	cf, err := LoadClientFile()
	if err != nil {
		return cc, err
	}
	if b, ok := cf.CurrentBox(); ok {
		cc = ClientConfig{Addr: b.Addr, Token: b.Token, RelayAPI: b.RelayAPI, AccountCredential: b.AccountCredential}
	}
	if v := os.Getenv("PIPER_ADDR"); v != "" {
		cc.Addr = v
	}
	if cc.Addr == "" {
		cc.Addr = "http://127.0.0.1:8088"
	}
	if v := os.Getenv("PIPER_TOKEN"); v != "" {
		cc.Token = v
	}
	return cc, nil
}

// SaveClient writes cc into the current box of ~/.piper/piper/config.json
// (creating a "default" box if none exists), preserving all other boxes and
// rewriting a legacy flat file in v2 form.
func SaveClient(cc ClientConfig) error {
	cf, err := LoadClientFile()
	if err != nil {
		return err
	}
	name := cf.Current
	if name == "" {
		name = "default"
	}
	updated := false
	for i := range cf.Boxes {
		if cf.Boxes[i].Name == name {
			cf.Boxes[i].Addr = cc.Addr
			cf.Boxes[i].Token = cc.Token
			cf.Boxes[i].RelayAPI = cc.RelayAPI
			cf.Boxes[i].AccountCredential = cc.AccountCredential
			updated = true
			break
		}
	}
	if !updated {
		cf.Boxes = append(cf.Boxes, Box{Name: name, Addr: cc.Addr, Token: cc.Token, RelayAPI: cc.RelayAPI, AccountCredential: cc.AccountCredential})
	}
	cf.Current = name
	return SaveClientFile(cf)
}
```

- [ ] **Step 4: Run the full package + CLI tests**

Run: `go test ./internal/config/ ./cmd/piper/ -v`
Expected: all PASS — `cmd/piper` login/relayonboard tests exercise load-mutate-save through the new path.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(cli): route LoadClient/SaveClient through multi-box config

Part of #P1

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Phase 1 PR

**Files:**
- Modify: `PROGRESS.md` (one line)

- [ ] **Step 1: Verify**

Run: `make verify`
Expected: gofmt clean, vet clean, tests pass, cross-compile green.

- [ ] **Step 2: PROGRESS.md** — add one terse line to the CLI section: `- multi-box client config (schema v2, silent migration) [#P1]`; commit as `docs: record multi-box config in PROGRESS.md` (same trailer).

- [ ] **Step 3: Push and open PR**

```bash
git push -u origin ozykhan/multibox-config
gh pr create --base main --title "feat(cli): multi-box client config (schema v2)" \
  --body "Named boxes + current selection in the CLI config; legacy flat file migrates silently on first save; LoadClient/SaveClient signatures and CLI behavior unchanged. Part of #E. Closes #P1."
```

Wait for merge before opening the phase 2 PR (branch for Task 5 can start immediately, stacked on this one).

---

### Task 5: Add TUI dependencies

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Branch** (stack on phase 1 so the work can proceed pre-merge)

```bash
git checkout -b ozykhan/tui-skeleton
```

- [ ] **Step 2: Add pinned v1 deps**

```bash
go get github.com/charmbracelet/bubbletea@v1 github.com/charmbracelet/lipgloss@v1
go mod tidy
```

Note: `go mod tidy` will drop them again until Task 6's code imports them — if it does, re-run the `go get` together with Task 6's commit instead; either ordering is fine as long as the Task 6 commit builds.

- [ ] **Step 3: Prove the cross-compile**

Run: `make cross`
Expected: exit 0.

- [ ] **Step 4: Commit** (fold into Task 6's commit if tidy dropped the deps)

```bash
git add go.mod go.sum
git commit -m "build: add bubbletea and lipgloss for the TUI

Part of #P2

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: `internal/tui` — API seam, messages, render helpers

**Files:**
- Create: `internal/tui/tui.go` (package doc, `API`, messages)
- Create: `internal/tui/render.go` (`appURL`, `statusIcon`)
- Test: `internal/tui/render_test.go`

**Interfaces:**
- Consumes: `internal/api.App` (embeds `store.App`: `Name`, `Port`, `Repo`, `Branch`, `Hostname`, `CreatedAt`; plus `Status`).
- Produces (used by Tasks 7–9):
  - `type API interface { ListApps() ([]api.App, error) }` — `*client.Client` satisfies it.
  - `type appsLoadedMsg struct{ apps []api.App }`
  - `type errMsg struct{ err error }`
  - `type tickMsg struct{}`
  - `func appURL(hostname string) string` — `""` → `""`, else `"http://" + hostname`.
  - `func statusIcon(status string) string` — `running→●`, `building→◐`, `failed→✗`, `stopped→○`, other→`—`.

- [ ] **Step 1: Write the failing test** — `internal/tui/render_test.go`:

```go
package tui

import "testing"

func TestAppURL(t *testing.T) {
	if got := appURL(""); got != "" {
		t.Fatalf("empty hostname: got %q", got)
	}
	if got := appURL("blog.piper.localhost"); got != "http://blog.piper.localhost" {
		t.Fatalf("got %q", got)
	}
}

func TestStatusIcon(t *testing.T) {
	cases := map[string]string{
		"running": "●", "building": "◐", "failed": "✗", "stopped": "○", "": "—",
	}
	for status, want := range cases {
		if got := statusIcon(status); got != want {
			t.Fatalf("statusIcon(%q) = %q, want %q", status, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ -v`
Expected: FAIL — package doesn't exist yet / undefined functions.

- [ ] **Step 3: Implement** — `internal/tui/tui.go`:

```go
// Package tui is the interactive full-screen frontend of the piper CLI: a
// Bubble Tea program over the same internal/client API the subcommands use.
// Bare `piper` in a terminal lands here; every subcommand stays untouched.
package tui

import "github.com/getpiper/piper/internal/api"

// API is the slice of the piperd control API the TUI consumes.
// *client.Client satisfies it; tests inject fakes.
type API interface {
	ListApps() ([]api.App, error)
}

// Messages flowing into Update. All API calls run as tea.Cmd goroutines and
// land as exactly one of these; the UI thread never blocks.
type (
	appsLoadedMsg struct{ apps []api.App }
	errMsg        struct{ err error }
	tickMsg       struct{}
)
```

`internal/tui/render.go`:

```go
package tui

// appURL renders the URL a local/BYO box serves an app on. Empty hostname
// (never deployed) yields "". Relay-terminated HTTPS rendering arrives with
// the boxes work (phase 5).
func appURL(hostname string) string {
	if hostname == "" {
		return ""
	}
	return "http://" + hostname
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

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/tui/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "feat(cli): scaffold internal/tui with API seam and render helpers

Part of #P2

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Apps view

**Files:**
- Create: `internal/tui/apps.go`
- Test: `internal/tui/apps_test.go`

**Interfaces:**
- Consumes: Task 6's messages + helpers.
- Produces (used by Task 8): `func newAppsView() appsView`; `appsView` implements `tea.Model` (`Init`, `Update(tea.Msg) (tea.Model, tea.Cmd)`, `View() string`). Handles `appsLoadedMsg` (rows), `errMsg` (inline banner, keeps last rows).

- [ ] **Step 1: Write the failing tests** — `internal/tui/apps_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/store"
)

func fixtureApps() []api.App {
	return []api.App{
		{App: store.App{Name: "blog", Hostname: "blog.piper.localhost"}, Status: "running"},
		{App: store.App{Name: "shop", Hostname: "shop.piper.localhost"}, Status: "building"},
		{App: store.App{Name: "new"}, Status: ""},
	}
}

func TestAppsViewRendersRows(t *testing.T) {
	m, _ := newAppsView().Update(appsLoadedMsg{apps: fixtureApps()})
	out := m.View()
	for _, want := range []string{
		"NAME", "STATUS", "URL",
		"blog", "● running", "http://blog.piper.localhost",
		"shop", "◐ building",
		"new", "—",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestAppsViewLoadingAndEmptyStates(t *testing.T) {
	if out := newAppsView().View(); !strings.Contains(out, "loading") {
		t.Fatalf("want loading state, got:\n%s", out)
	}
	m, _ := newAppsView().Update(appsLoadedMsg{apps: nil})
	if out := m.View(); !strings.Contains(out, "no apps yet") {
		t.Fatalf("want empty state, got:\n%s", out)
	}
}

func TestAppsViewErrorBannerKeepsLastRows(t *testing.T) {
	m, _ := newAppsView().Update(appsLoadedMsg{apps: fixtureApps()})
	m, _ = m.Update(errMsg{err: errors.New("connection refused")})
	out := m.View()
	if !strings.Contains(out, "⚠") || !strings.Contains(out, "connection refused") {
		t.Fatalf("want error banner, got:\n%s", out)
	}
	if !strings.Contains(out, "blog") {
		t.Fatalf("stale rows dropped on error:\n%s", out)
	}
	m, _ = m.Update(appsLoadedMsg{apps: fixtureApps()})
	if out := m.View(); strings.Contains(out, "⚠") {
		t.Fatalf("banner must clear on next successful poll:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ -run AppsView -v`
Expected: FAIL — `undefined: newAppsView`.

- [ ] **Step 3: Implement** — `internal/tui/apps.go`:

```go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/api"
)

// appsView is the depth-0 home view: a read-only table of apps. Selection
// and drill-down arrive in phase 3.
type appsView struct {
	apps   []api.App
	err    error
	loaded bool
}

func newAppsView() appsView { return appsView{} }

func (v appsView) Init() tea.Cmd { return nil }

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
		fmt.Fprintf(&b, "  %-16s %-12s %s\n", a.Name, status, appURL(a.Hostname))
	}
	return b.String()
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/tui/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/apps.go internal/tui/apps_test.go
git commit -m "feat(cli): add read-only apps view to the TUI

Part of #P2

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: Root model — stack, tick, status bar, `Run`

**Files:**
- Create: `internal/tui/app.go`
- Test: `internal/tui/app_test.go`

**Interfaces:**
- Consumes: Task 6's `API` + messages, Task 7's `newAppsView`.
- Produces (used by Task 9): `func Run(box, addr string, c API) error` — blocks until quit. Internal: `func NewModel(box, addr string, c API) Model`; `Model` implements `tea.Model`; 2s poll via `tickMsg`; keys: `ctrl+c` quit anywhere, `q` quit at root / pop when deep, `esc` pop, `r` refresh now.

- [ ] **Step 1: Write the failing tests** — `internal/tui/app_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/store"
)

type fakeAPI struct {
	apps []api.App
	err  error
}

func (f fakeAPI) ListApps() ([]api.App, error) { return f.apps, f.err }

// pump runs the poll cmd and feeds its message back, like the tea runtime.
func pump(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	next, _ := m.Update(cmd())
	return next.(Model)
}

func TestModelPollSuccessUpdatesStatusBar(t *testing.T) {
	f := fakeAPI{apps: []api.App{{App: store.App{Name: "blog", Hostname: "blog.piper.localhost"}, Status: "running"}}}
	m := NewModel("pi4", "http://192.168.1.6:8088", f)
	m = pump(t, m, m.refresh())
	out := m.View()
	for _, want := range []string{"● pi4", "http://192.168.1.6:8088", "1 apps", "blog", "piper", "apps"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestModelPollFailureShowsUnreachable(t *testing.T) {
	m := NewModel("pi4", "http://192.168.1.6:8088", fakeAPI{err: errors.New("dial tcp: refused")})
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "○ pi4") || !strings.Contains(out, "unreachable") {
		t.Fatalf("want unreachable bar, got:\n%s", out)
	}
}

func TestModelRecoversAfterFailure(t *testing.T) {
	m := NewModel("pi4", "addr", fakeAPI{err: errors.New("down")})
	m = pump(t, m, m.refresh())
	m.client = fakeAPI{apps: nil}
	m = pump(t, m, m.refresh())
	if out := m.View(); !strings.Contains(out, "● pi4") {
		t.Fatalf("bar did not recover:\n%s", out)
	}
}

func TestModelTickReschedulesAndRefreshes(t *testing.T) {
	m := NewModel("pi4", "addr", fakeAPI{})
	if _, cmd := m.Update(tickMsg{}); cmd == nil {
		t.Fatal("tick must return refresh+tick batch")
	}
}

func TestModelQuitKeys(t *testing.T) {
	m := NewModel("pi4", "addr", fakeAPI{})
	keys := []tea.KeyMsg{
		tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune{'q'}}), // q at root quits
		tea.KeyMsg(tea.Key{Type: tea.KeyCtrlC}),                     // ctrl+c quits anywhere
	}
	for _, key := range keys {
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Fatalf("%s: expected quit cmd", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s: expected tea.QuitMsg, got %T", key, cmd())
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ -run Model -v`
Expected: FAIL — `undefined: NewModel`.

- [ ] **Step 3: Implement** — `internal/tui/app.go`:

```go
package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const pollInterval = 2 * time.Second

// Model is the root of the TUI: it owns the view stack, the poll tick, the
// active box's client, and the status bar. Child views handle their own
// messages; the root intercepts global keys and connectivity state.
type Model struct {
	box    string
	addr   string
	client API

	stack    []tea.Model
	loaded   bool // at least one successful poll
	down     bool // last poll failed
	appCount int
}

func NewModel(box, addr string, c API) Model {
	return Model{box: box, addr: addr, client: c, stack: []tea.Model{newAppsView()}}
}

// Run starts the interactive TUI against c, identified as box/addr in the
// status bar. It blocks until the user quits.
func Run(box, addr string, c API) error {
	_, err := tea.NewProgram(NewModel(box, addr, c), tea.WithAltScreen()).Run()
	return err
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.refresh(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// refresh polls the current view's data off the UI thread.
func (m Model) refresh() tea.Cmd {
	c := m.client
	return func() tea.Msg {
		apps, err := c.ListApps()
		if err != nil {
			return errMsg{err}
		}
		return appsLoadedMsg{apps}
	}
}

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
			return m, nil
		case "esc":
			if len(m.stack) > 1 {
				m.stack = m.stack[:len(m.stack)-1]
			}
			return m, nil
		case "r":
			return m, m.refresh()
		}
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case appsLoadedMsg:
		m.loaded, m.down, m.appCount = true, false, len(msg.apps)
	case errMsg:
		m.down = true
	}
	top, cmd := m.stack[len(m.stack)-1].Update(msg)
	m.stack[len(m.stack)-1] = top
	return m, cmd
}

func (m Model) View() string {
	header := lipgloss.NewStyle().Bold(true).Render(" piper ") + "· apps"
	return header + "\n\n" + m.stack[len(m.stack)-1].View() + "\n\n" + m.statusBar()
}

func (m Model) statusBar() string {
	switch {
	case m.down:
		return fmt.Sprintf(" ○ %s · %s · unreachable — retrying…", m.box, m.addr)
	case !m.loaded:
		return fmt.Sprintf(" … %s · %s · connecting…", m.box, m.addr)
	default:
		return fmt.Sprintf(" ● %s · %s · %d apps", m.box, m.addr, m.appCount)
	}
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/tui/ -v`
Expected: PASS. If the `q` key test fails on `tea.KeyMsg` construction, use `tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune{'q'}})` — that is the v1 way to fake a rune key.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/app.go internal/tui/app_test.go
git commit -m "feat(cli): add TUI root model with view stack, poll tick, status bar

Part of #P2

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 9: Dual-mode entry point in `cmd/piper`

**Files:**
- Modify: `cmd/piper/main.go` (imports; the `len(args) == 0` branch in `run()` at line ~126; new `launchTUI` + `isTerminal` vars near `dialClient`; `usage()` at line ~527)
- Test: `cmd/piper/main_test.go`

**Interfaces:**
- Consumes: `tui.Run(box, addr string, c API) error`, `config.LoadClientFile`, `config.LoadClient`, existing `dialClient(remote string, stderr io.Writer) (*client.Client, bool)`.
- Produces: bare `piper` + TTY → TUI; bare + non-TTY → usage exit 2 (unchanged); `piper --remote X` (no subcommand) + TTY → TUI against the relay path.

- [ ] **Step 1: Write the failing tests** — append to `cmd/piper/main_test.go`:

```go
func TestBareInvocationNonTTYPrintsUsage(t *testing.T) {
	old := isTerminal
	isTerminal = func() bool { return false }
	defer func() { isTerminal = old }()

	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("want usage, got: %s", errb.String())
	}
}

func TestBareInvocationTTYLaunchesTUI(t *testing.T) {
	oldT, oldL := isTerminal, launchTUI
	isTerminal = func() bool { return true }
	var gotRemote string
	called := false
	launchTUI = func(remote string, stderr io.Writer) int {
		called, gotRemote = true, remote
		return 0
	}
	defer func() { isTerminal, launchTUI = oldT, oldL }()

	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 0 {
		t.Fatalf("want exit 0, got %d (stderr: %s)", code, errb.String())
	}
	if !called || gotRemote != "" {
		t.Fatalf("want TUI launch with empty remote, called=%v remote=%q", called, gotRemote)
	}

	if code := run([]string{"--remote", "pi4.example.dev"}, &out, &errb); code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if gotRemote != "pi4.example.dev" {
		t.Fatalf("remote not forwarded: %q", gotRemote)
	}
}
```

(`bytes` and `strings` are already imported by `main_test.go`; add them if not.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/piper/ -run BareInvocation -v`
Expected: FAIL — `undefined: isTerminal`, `undefined: launchTUI`.

- [ ] **Step 3: Implement.** In `cmd/piper/main.go`, add imports `"golang.org/x/term"` and `"github.com/getpiper/piper/internal/tui"`. Below `appURL`, add:

```go
// isTerminal reports whether stdout is an interactive terminal; a func var so
// run() tests can force either mode.
var isTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// launchTUI opens the interactive TUI against the current box (or the given
// relay-remote base domain); a func var so run() tests can stub it.
var launchTUI = func(remote string, stderr io.Writer) int {
	c, ok := dialClient(remote, stderr)
	if !ok {
		return 1
	}
	box, addr := "default", ""
	if cf, err := config.LoadClientFile(); err == nil {
		if b, ok := cf.CurrentBox(); ok {
			box = b.Name
		}
	}
	if cc, err := config.LoadClient(); err == nil {
		addr = cc.Addr // env overrides + localhost default applied
	}
	if remote != "" {
		box, addr = remote, "via relay"
	}
	if err := tui.Run(box, addr, c); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}
```

Replace the bare-invocation branch in `run()`:

```go
	args = gfs.Args()
	if len(args) == 0 {
		if isTerminal() {
			return launchTUI(*remote, stderr)
		}
		return usage(stderr)
	}
```

Update `usage()` so the first line documents the dual mode:

```go
	fmt.Fprintln(w, "usage: piper [--remote <base-domain>] [--version] <version|login|connect|create|deploy|list|status|stop|delete|app|github> [args]")
	fmt.Fprintln(w, "       piper                # no subcommand in a terminal: interactive TUI")
```

- [ ] **Step 4: Run the full CLI test suite**

Run: `go test ./cmd/piper/ -v`
Expected: all PASS — including every pre-existing `run()` test (they pass subcommands, or run non-TTY).

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/main.go cmd/piper/main_test.go
git commit -m "feat(cli): bare piper in a terminal opens the TUI

Part of #P2

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 10: Phase 2 verification + PR

**Files:**
- Modify: `PROGRESS.md` (one line)

- [ ] **Step 1: Full gate**

Run: `make verify`
Expected: gofmt clean, vet clean, all tests pass, arm64 cross-compile green.

- [ ] **Step 2: Manual smoke** (real terminal; piperd running locally or use the pi4 box)

```bash
make build && ./bin/piper
```

Expected: alt-screen TUI; apps table populates within 2s; status bar shows `● <box> · <addr> · N apps`; stop piperd → bar flips to `○ … unreachable — retrying…` and recovers when piperd returns; `r` refreshes; `q` and `ctrl+c` exit cleanly; terminal restored. Also confirm `./bin/piper | cat` prints usage (non-TTY guard) and `./bin/piper list` still works.

- [ ] **Step 3: PROGRESS.md** — add: `- TUI skeleton: bare-piper entry, apps dashboard, status bar [#P2]`; commit as `docs: record TUI skeleton in PROGRESS.md` (same trailer).

- [ ] **Step 4: Push and open PR** (after the phase-1 PR merged, rebase onto main first: `git rebase main`)

```bash
git push -u origin ozykhan/tui-skeleton
gh pr create --base main --title "feat(cli): TUI skeleton — bare piper opens a live apps dashboard" \
  --body "Bubble Tea root model with view stack, 2s poll, status bar, read-only apps table; TTY-gated entry so scripts and pipes are untouched. Part of #E. Closes #P2."
```

---

## Out of scope for this plan (next plan docs, per spec phasing)

Phase 3 (drill-down: app detail, deployments, logs+follow — adds `bubbles`, cursor/selection in the apps view, LAST DEPLOY column), phase 4 (actions), phase 5 (boxes view — consumes `ClientFile` directly), phase 6 (wizards). Each gets its own plan written against the then-current skeleton.
