# macOS Rootless Toggleable piperd (launchd) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let piperd run rootless and toggleable on a macOS dev box via a per-user launchd LaunchAgent on a high port, controlled by `piper agent up|down|status` — no `sudo`, gone after reboot — while leaving Linux/Pi behavior unchanged.

**Architecture:** Three dependent pieces under one spec. (A) Make piperd's Caddy listen addresses configurable via `PIPER_HTTP_ADDR`/`PIPER_HTTPS_ADDR` (defaults `:80`/`:443`). (B) Ship a rootless macOS LaunchAgent plist + env example whose `/bin/sh -c` wrapper sets high ports and `~/.piper` paths and sources `~/.piper/piperd.env` last. (C) Add a `piper agent` subcommand wrapping `launchctl bootstrap`/`bootout`/`print` in the user domain.

**Tech Stack:** Go 1.x (`CGO_ENABLED=0`), `modernc.org/sqlite`, embedded Caddy, macOS launchd (`launchctl`), Go `testing`.

## Global Constraints

- **No cgo.** All builds must pass with `CGO_ENABLED=0`; `make cross` (linux/arm64) must stay green. No build-tag-split files needed here — gate on `runtime.GOOS` at runtime.
- **Module path:** `github.com/piperbox/piper`.
- **Deployment status strings** unchanged: `"building"`, `"running"`, `"failed"`, `"stopped"`.
- **Defaults unchanged on Linux:** control API `127.0.0.1:8088`, Caddy admin `http://127.0.0.1:2019`, base domain `piper.localhost`, app container port `8080`, and Caddy HTTP/HTTPS listen `:80`/`:443`.
- **Test-first (TDD).** Every change starts with a failing test (or, for static asset/doc files, a contract test that fails until the file exists).
- **Commits:** conventional-commit style, one per task step-group, ending with:
  `Co-Authored-By: Claude {current model} <noreply@anthropic.com>`
- **Before claiming done:** run `make verify` (gofmt → vet → test → cross).
- Branch already created: `ozykhan/macos-launchd-rootless`. Reference issues in commits (`Part of #207` / `#56` / `#208`).

---

## Part A — `[agent]` configurable listen address (issue #207)

### Task 1: Configurable `PIPER_HTTP_ADDR` / `PIPER_HTTPS_ADDR`

**Files:**
- Modify: `internal/config/config.go` (struct + `Load`)
- Modify: `cmd/piperd/main.go` (Caddy start call site, ~lines 208-212)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Config.HTTPAddr string` (default `:80`), `config.Config.HTTPSAddr string` (default `:443`), populated from env `PIPER_HTTP_ADDR` / `PIPER_HTTPS_ADDR`. Consumed by `cmd/piperd/main.go` when starting Caddy.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestLoadListenDefaults(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
	t.Setenv("PIPER_HTTP_ADDR", "")
	t.Setenv("PIPER_HTTPS_ADDR", "")
	c := Load()
	if c.HTTPAddr != ":80" {
		t.Errorf("HTTPAddr = %q, want :80", c.HTTPAddr)
	}
	if c.HTTPSAddr != ":443" {
		t.Errorf("HTTPSAddr = %q, want :443", c.HTTPSAddr)
	}
}

func TestLoadListenOverride(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
	t.Setenv("PIPER_HTTP_ADDR", ":8080")
	t.Setenv("PIPER_HTTPS_ADDR", ":8443")
	c := Load()
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", c.HTTPAddr)
	}
	if c.HTTPSAddr != ":8443" {
		t.Errorf("HTTPSAddr = %q, want :8443", c.HTTPSAddr)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run TestLoadListen -v`
Expected: FAIL — `c.HTTPAddr undefined (type Config has no field or method HTTPAddr)`.

- [ ] **Step 3: Add the struct fields**

In `internal/config/config.go`, add to the `Config` struct (after `CaddyAdmin string`):

```go
	HTTPAddr    string // embedded Caddy HTTP listen address (default :80)
	HTTPSAddr   string // embedded Caddy HTTPS listen address (default :443)
```

- [ ] **Step 4: Populate them in `Load`**

In `Load`'s returned `Config{...}` literal, after the `CaddyAdmin:` line, add:

```go
		HTTPAddr:    env("PIPER_HTTP_ADDR", ":80"),
		HTTPSAddr:   env("PIPER_HTTPS_ADDR", ":443"),
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/ -run TestLoadListen -v`
Expected: PASS (both).

- [ ] **Step 6: Wire the values into piperd's Caddy start**

In `cmd/piperd/main.go`, replace the Caddy-start block (currently):

```go
		opts := []caddy.Option{}
		if cfg.RelayAddr != "" && !cfg.Terminated {
			opts = append(opts, caddy.WithHTTPS(":443"))
		}
		mgr, err = caddy.StartManager(cfg.CaddyAdmin, ":80", opts...)
```

with:

```go
		opts := []caddy.Option{}
		if cfg.RelayAddr != "" && !cfg.Terminated {
			opts = append(opts, caddy.WithHTTPS(cfg.HTTPSAddr))
		}
		mgr, err = caddy.StartManager(cfg.CaddyAdmin, cfg.HTTPAddr, opts...)
```

- [ ] **Step 7: Verify the whole module still builds and tests pass**

Run: `go build ./... && go test ./internal/config/ ./cmd/piperd/`
Expected: build succeeds; tests PASS. (Defaults match the old literals, so no behavior change.)

- [ ] **Step 8: gofmt + commit**

```bash
gofmt -w internal/config/config.go cmd/piperd/main.go internal/config/config_test.go
git add internal/config/config.go internal/config/config_test.go cmd/piperd/main.go
git commit -m "$(printf 'feat(agent): configurable PIPER_HTTP_ADDR/PIPER_HTTPS_ADDR listen (default :80/:443)\n\nPart of #207\n\nCo-Authored-By: Claude {current model} <noreply@anthropic.com>')"
```

---

## Part B — `[repo]` rootless macOS LaunchAgent (issue #56)

### Task 2: launchd plist + macOS env example + contract tests

**Files:**
- Create: `packaging/launchd/com.piperbox.piperd.plist`
- Create: `packaging/launchd/piperd.env.macos.example`
- Create: `packaging/launchd/piperd_test.go`

**Interfaces:**
- Produces: a LaunchAgent plist at label `com.piperbox.piperd` that runs `/usr/local/bin/piperd` rootless on `:8080`/`:8443` under `~/.piper`, sourcing `~/.piper/piperd.env`. Consumed by the `piper agent` CLI (Task 4), which bootstraps this exact label/path.

- [ ] **Step 1: Write the failing contract tests**

Create `packaging/launchd/piperd_test.go`:

```go
package launchd

import (
	"os"
	"strings"
	"testing"
)

func TestPiperdPlistContract(t *testing.T) {
	b, err := os.ReadFile("com.piperbox.piperd.plist")
	if err != nil {
		t.Fatal(err)
	}
	plist := string(b)
	required := []string{
		"<string>com.piperbox.piperd</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>/bin/sh</string>",
		`PIPER_HTTP_ADDR=":8080"`,
		`PIPER_HTTPS_ADDR=":8443"`,
		`XDG_DATA_HOME="$HOME/.piper/piperd"`,
		`$HOME/.piper/piperd.env`,
		`$HOME/.piper/piper.log`,
		"exec /usr/local/bin/piperd",
	}
	for _, s := range required {
		if !strings.Contains(plist, s) {
			t.Errorf("plist missing %q", s)
		}
	}
}

func TestPiperdEnvMacosExample(t *testing.T) {
	b, err := os.ReadFile("piperd.env.macos.example")
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	for _, s := range []string{"PIPER_API_ADDR", "PIPER_BASE_DOMAIN", "DOCKER_HOST"} {
		if !strings.Contains(env, s) {
			t.Errorf("env example missing %q", s)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./packaging/launchd/ -v`
Expected: FAIL — `open com.piperbox.piperd.plist: no such file or directory`.

- [ ] **Step 3: Create the plist**

Create `packaging/launchd/com.piperbox.piperd.plist` (note the XML-escaped `&&`, `>>`, `2>>` inside the wrapper string):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.piperbox.piperd</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>mkdir -p "$HOME/.piper"
export XDG_DATA_HOME="$HOME/.piper/piperd" XDG_CONFIG_HOME="$HOME/.piper/piperd"
export PIPER_HTTP_ADDR=":8080" PIPER_HTTPS_ADDR=":8443"
set -a
[ -f "$HOME/.piper/piperd.env" ] &amp;&amp; . "$HOME/.piper/piperd.env"
set +a
exec &gt;&gt; "$HOME/.piper/piper.log" 2&gt;&gt; "$HOME/.piper/piper.err.log"
exec /usr/local/bin/piperd</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
```

- [ ] **Step 4: Create the macOS env example**

Create `packaging/launchd/piperd.env.macos.example`:

```bash
# piperd environment file (macOS) — copy to ~/.piper/piperd.env.
# Sourced LAST by the launchd agent's wrapper, so anything set here overrides the
# plist's defaults. Every value is optional; defaults shown match piperd's built-ins.
#
# The plist pins XDG_DATA_HOME/XDG_CONFIG_HOME to ~/.piper/piperd so Caddy stores
# its data alongside piperd's. PIPER_DATA_DIR defaults to ~/.piper/piperd — set it
# here only to relocate the SQLite DB.

# --- Control plane (LAN-only) ---
# Control API the piper CLI talks to (loopback by default).
#PIPER_API_ADDR=127.0.0.1:8088
# Apps are served at <name>.<PIPER_BASE_DOMAIN>.
#PIPER_BASE_DOMAIN=piper.localhost
# Caddy admin API base URL (embedded Caddy).
#PIPER_CADDY_ADMIN=http://127.0.0.1:2019

# --- Listen ports (rootless: high ports, no privilege needed) ---
# The plist defaults these to :8080/:8443. Apps are reachable at
# http://<name>.piper.localhost:8080.
#PIPER_HTTP_ADDR=:8080
#PIPER_HTTPS_ADDR=:8443

# --- Docker ---
# piperd reaches Docker Desktop via /var/run/docker.sock by default. If your
# Docker socket lives elsewhere, point piperd at it:
#DOCKER_HOST=unix:///Users/<you>/.docker/run/docker.sock
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./packaging/launchd/ -v`
Expected: PASS (both).

- [ ] **Step 6: Sanity-check the plist parses as valid XML/plist**

Run: `plutil -lint packaging/launchd/com.piperbox.piperd.plist`
Expected: `packaging/launchd/com.piperbox.piperd.plist: OK`.
(On non-macOS where `plutil` is absent, skip this step — the Go test already guards structure.)

- [ ] **Step 7: Commit**

```bash
git add packaging/launchd/
git commit -m "$(printf 'feat(repo): rootless macOS launchd LaunchAgent + env example\n\nPart of #56\n\nCo-Authored-By: Claude {current model} <noreply@anthropic.com>')"
```

### Task 3: macOS docs + doc contract test

**Files:**
- Modify: `docs/manual-setup.md` (new macOS section)
- Modify: `docs/runbooks/git-deploy-e2e.md` (macOS verify/teardown note)
- Modify: `packaging/launchd/piperd_test.go` (add doc test + `repositoryFile` helper)

**Interfaces:**
- Consumes: the plist/env files from Task 2 (referenced by path in docs) and the `piper agent` commands from Task 4 (named in docs). This is the doc-coverage gate.

- [ ] **Step 1: Write the failing doc test**

Append to `packaging/launchd/piperd_test.go` (add `"path/filepath"` to the import block):

```go
func repositoryFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPiperdLaunchdDocumentation(t *testing.T) {
	manual := repositoryFile(t, "docs", "manual-setup.md")
	for _, s := range []string{
		"packaging/launchd/com.piperbox.piperd.plist",
		"piper agent up",
	} {
		if !strings.Contains(manual, s) {
			t.Errorf("docs/manual-setup.md missing %q", s)
		}
	}

	runbook := repositoryFile(t, "docs", "runbooks", "git-deploy-e2e.md")
	for _, s := range []string{
		"piper agent status",
		"~/.piper/piper.log",
		"piper agent down",
	} {
		if !strings.Contains(runbook, s) {
			t.Errorf("runbook missing %q", s)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./packaging/launchd/ -run Documentation -v`
Expected: FAIL — `docs/manual-setup.md missing "packaging/launchd/com.piperbox.piperd.plist"`.

- [ ] **Step 3: Add the macOS section to `docs/manual-setup.md`**

Insert this section immediately after the systemd "Run the agent as a service" block (before "## Run piperd in Docker (Compose)"):

````markdown
## Run the agent on macOS (dev box)

macOS is a **development** target: instead of a boot-surviving root service, piperd
runs **rootless** as your user on high ports (`:8080`/`:8443`), toggled on and off by
hand. No `sudo`. Install the binary and the shipped LaunchAgent:

```bash
sudo install -m 0755 bin/piperd /usr/local/bin/piperd
install -m 0644 packaging/launchd/com.piperbox.piperd.plist \
  ~/Library/LaunchAgents/com.piperbox.piperd.plist
cp packaging/launchd/piperd.env.macos.example ~/.piper/piperd.env   # optional overrides
piper agent up
```

The agent stores everything under `~/.piper/` (SQLite DB, Caddy data, logs at
`~/.piper/piper{,.err}.log`) and serves apps at `http://<name>.piper.localhost:8080`.
It is **not** a boot service — it's gone after a reboot; re-run `piper agent up`.
Stop it with `piper agent down`; check it with `piper agent status`. This path is
LAN-only; the relay/public-URL flow is Linux/Pi (systemd) only.
````

- [ ] **Step 4: Add the macOS note to `docs/runbooks/git-deploy-e2e.md`**

Add this note near the teardown/verification section of the runbook:

````markdown
### macOS (rootless launchd agent)

On a Mac dev box the agent runs rootless via launchd (see
[manual setup](../manual-setup.md#run-the-agent-on-macos-dev-box)):

```bash
piper agent status          # running / stopped / not installed
tail -f ~/.piper/piper.log  # agent logs (errors in ~/.piper/piper.err.log)
piper agent down            # stop it
```
````

- [ ] **Step 5: Run the doc test to verify it passes**

Run: `go test ./packaging/launchd/ -v`
Expected: PASS (all three tests).

- [ ] **Step 6: Commit**

```bash
git add docs/manual-setup.md docs/runbooks/git-deploy-e2e.md packaging/launchd/piperd_test.go
git commit -m "$(printf 'docs(repo): macOS launchd agent setup + runbook note\n\nPart of #56\n\nCo-Authored-By: Claude {current model} <noreply@anthropic.com>')"
```

---

## Part C — `[cli]` `piper agent up|down|status` (issue #208)

### Task 4: `piper agent` subcommand wrapping launchctl

**Files:**
- Create: `cmd/piper/agent.go`
- Create: `cmd/piper/agent_test.go`
- Modify: `cmd/piper/main.go` (dispatch `case "agent"`, `--remote` reject list, usage string)

**Interfaces:**
- Consumes: the LaunchAgent label/path from Task 2 (`com.piperbox.piperd`, `~/Library/LaunchAgents/com.piperbox.piperd.plist`).
- Produces: `agent(args []string, stdout, stderr io.Writer) int` dispatched from `run`.
- Test seams (package-level vars, overridable in tests): `agentGOOS string`, `launchdPlistPath func() (string, error)`, `launchctlRun func(args ...string) (string, error)`.

- [ ] **Step 1: Write the failing tests**

Create `cmd/piper/agent_test.go`:

```go
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentNonDarwinGate(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "systemctl") {
		t.Errorf("stderr = %q, want systemctl hint", errb.String())
	}
}

func TestAgentUpBootstraps(t *testing.T) {
	agentGOOS = "darwin"
	defer func() { agentGOOS = runtime.GOOS }()

	dir := t.TempDir()
	plist := filepath.Join(dir, "com.piperbox.piperd.plist")
	if err := os.WriteFile(plist, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath := launchdPlistPath
	launchdPlistPath = func() (string, error) { return plist, nil }
	defer func() { launchdPlistPath = oldPath }()

	var gotArgs []string
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if len(gotArgs) < 1 || gotArgs[0] != "bootstrap" {
		t.Errorf("launchctl args = %v, want bootstrap ...", gotArgs)
	}
	if !strings.Contains(out.String(), "started") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestAgentDownBootsOut(t *testing.T) {
	agentGOOS = "darwin"
	defer func() { agentGOOS = runtime.GOOS }()
	var gotArgs []string
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"down"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if len(gotArgs) < 1 || gotArgs[0] != "bootout" {
		t.Errorf("launchctl args = %v, want bootout ...", gotArgs)
	}
}

func TestAgentUsage(t *testing.T) {
	agentGOOS = "darwin"
	defer func() { agentGOOS = runtime.GOOS }()
	var out, errb bytes.Buffer
	if code := agent([]string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: piper agent") {
		t.Errorf("stderr = %q", errb.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/piper/ -run TestAgent -v`
Expected: FAIL — `undefined: agent` / `undefined: agentGOOS` / `undefined: launchctlRun` / `undefined: launchdPlistPath`.

- [ ] **Step 3: Create `cmd/piper/agent.go`**

```go
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const launchdLabel = "com.piperbox.piperd"

// agentGOOS is runtime.GOOS; a var so tests can exercise the non-darwin gate.
var agentGOOS = runtime.GOOS

// launchdPlistPath returns the installed LaunchAgent path; a var so tests can
// point it at a temp file.
var launchdPlistPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// launchctlRun runs `launchctl <args...>` and returns combined output; a var so
// tests can substitute it without shelling out to a real launchd.
var launchctlRun = func(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return string(out), err
}

func guiTarget() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// agent dispatches `piper agent <up|down|status>` — toggling the rootless macOS
// launchd LaunchAgent. macOS only; on other platforms it points at systemd.
func agent(args []string, stdout, stderr io.Writer) int {
	if agentGOOS != "darwin" {
		fmt.Fprintln(stderr, "error: `piper agent` manages the macOS launchd agent; on Linux use `sudo systemctl enable --now piperd`")
		return 2
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
	switch args[0] {
	case "up":
		return agentUp(stdout, stderr)
	case "down":
		return agentDown(stdout, stderr)
	case "status":
		return agentStatus(stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
}

func agentUp(stdout, stderr io.Writer) int {
	plist, err := launchdPlistPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(plist); err != nil {
		fmt.Fprintf(stderr, "error: launchd agent not installed at %s\nsee docs/manual-setup.md (Run the agent on macOS)\n", plist)
		return 1
	}
	out, err := launchctlRun("bootstrap", guiTarget(), plist)
	if err != nil {
		if strings.Contains(out, "already") || strings.Contains(out, "5: Input/output error") {
			fmt.Fprintln(stdout, "piperd already running")
			return 0
		}
		fmt.Fprintf(stderr, "error: launchctl bootstrap failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd started")
	return 0
}

func agentDown(stdout, stderr io.Writer) int {
	out, err := launchctlRun("bootout", guiTarget()+"/"+launchdLabel)
	if err != nil {
		if strings.Contains(out, "No such process") || strings.Contains(out, "not find") {
			fmt.Fprintln(stdout, "piperd already stopped")
			return 0
		}
		fmt.Fprintf(stderr, "error: launchctl bootout failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd stopped")
	return 0
}

func agentStatus(stdout, stderr io.Writer) int {
	plist, err := launchdPlistPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(plist); err != nil {
		fmt.Fprintln(stdout, "piperd: not installed")
		return 0
	}
	out, err := launchctlRun("print", guiTarget()+"/"+launchdLabel)
	if err != nil {
		fmt.Fprintln(stdout, "piperd: stopped")
		return 0
	}
	if strings.Contains(out, "state = running") {
		fmt.Fprintln(stdout, "piperd: running")
	} else {
		fmt.Fprintln(stdout, "piperd: loaded (not running)")
	}
	return 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/piper/ -run TestAgent -v`
Expected: PASS (all four).

- [ ] **Step 5: Dispatch `agent` from `run` and update usage**

In `cmd/piper/main.go`:

(a) Add `"agent"` to the `--remote` reject switch (local-only command):

```go
		switch args[0] {
		case "version", "login", "connect", "agent":
```

(b) Add the dispatch case in the main `switch args[0]` (e.g. after `case "connect":`'s block, before `case "create":`):

```go
	case "agent":
		return agent(args[1:], stdout, stderr)
```

(c) Update the usage string in `usage(w io.Writer)` to include `agent`:

```go
	fmt.Fprintln(w, "usage: piper [--remote <base-domain>] [--version] <version|login|connect|create|deploy|list|status|stop|delete|app|github|agent> [args]")
```

- [ ] **Step 6: Verify build + full CLI package tests**

Run: `go build ./... && go test ./cmd/piper/`
Expected: build succeeds; PASS.

- [ ] **Step 7: gofmt + commit**

```bash
gofmt -w cmd/piper/agent.go cmd/piper/agent_test.go cmd/piper/main.go
git add cmd/piper/agent.go cmd/piper/agent_test.go cmd/piper/main.go
git commit -m "$(printf 'feat(cli): piper agent up/down/status to toggle the macOS launchd agent\n\nPart of #208\n\nCo-Authored-By: Claude {current model} <noreply@anthropic.com>')"
```

---

## Task 5: Full verification gate + manual macOS smoke

**Files:** none (verification only).

- [ ] **Step 1: Run the full CI-equivalent gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests PASS, `make cross` (linux/arm64) builds. Fix anything that fails, then re-run.

- [ ] **Step 2: Manual macOS smoke test (documented; run on a real Mac with Docker Desktop)**

This is not automatable in CI — perform it once on a Mac and record the result in the PR:

```bash
make build
sudo install -m 0755 bin/piperd /usr/local/bin/piperd
install -m 0644 packaging/launchd/com.piperbox.piperd.plist ~/Library/LaunchAgents/
piper agent up            # -> "piperd started", no sudo prompt
piper agent status        # -> "piperd: running"
# deploy a sample app, then confirm it serves:
#   curl http://<name>.piper.localhost:8080
piper agent down          # -> "piperd stopped"
piper agent status        # -> "piperd: loaded (not running)" or "stopped"
```

Expected: piperd runs as your user (no `sudo`), an app is reachable on `:8080`, and `piper agent down` stops it. Confirm `~/.piper/piper.log` has output and `~/.piper/piperd/` holds the SQLite DB.

- [ ] **Step 3: Open the PR**

```bash
git push -u origin ozykhan/macos-launchd-rootless
gh pr create --base main \
  --title "feat: rootless toggleable piperd on macOS (launchd)" \
  --body "$(printf 'Rootless macOS dev-box path for piperd: configurable listen ports, a per-user launchd LaunchAgent, and %spiper agent up/down/status%s. LAN-only; Linux/Pi unchanged.\n\nDesign: docs/superpowers/specs/2026-07-14-macos-launchd-rootless-piperd-design.md\n\nCloses #207\nCloses #56\nCloses #208\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)' '`' '`')"
```

---

## Self-Review Notes (traceability)

- Spec §(A) → Task 1. §(B) plist/env/tests → Task 2; §(B) docs/doc-test → Task 3. §(C) → Task 4. Success criteria (`make verify` + Mac smoke) → Task 5.
- Type/name consistency: `HTTPAddr`/`HTTPSAddr` defined in Task 1 and consumed in Task 1 Step 6. `launchdLabel`, `agentGOOS`, `launchdPlistPath`, `launchctlRun`, `guiTarget` defined in Task 4 Step 3 and used by that task's tests (Step 1) via the same names. Plist strings asserted in Task 2 Step 1 exactly match the file written in Steps 3-4.
- The plist wrapper's XML entities (`&amp;&amp;`, `&gt;&gt;`) are the escaped forms; the Go contract test asserts only on non-escaped substrings (`PIPER_HTTP_ADDR=":8080"`, `$HOME/.piper/piperd.env`, `exec /usr/local/bin/piperd`) that appear literally in the file.
