# Linux Rootless-by-Default, Daemonize-on-Demand piperd — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring the Linux agent to parity with the macOS LaunchAgent rootless tier — `curl | sh` (non-root) installs a per-user piperd toggled by `piper agent up/down/status` via `systemctl --user` — and add `sudo piper agent daemonize` to promote it to the existing systemd **system** daemon.

**Architecture:** Three surfaces. (A) A systemd **user** unit (`piperd.user.service`, the twin of the launchd plist) plus an `install.sh` change so non-root installs rootless. (B) A Linux branch in `cmd/piper/agent.go` wrapping `systemctl --user`, plus a `daemonize` subcommand that promotes to the system service. (C) Docs. Rootless is ephemeral (no boot survival); daemonize is the durable, boot-surviving tier.

**Tech Stack:** Go 1.x (`CGO_ENABLED=0`), `go:embed`, POSIX `sh` (`install.sh`), systemd (`systemctl --user` / system), Go `testing`.

## Global Constraints

- **Depends on PR #209.** This work reuses `PIPER_HTTP_ADDR` / `PIPER_HTTPS_ADDR` (added in #209). **Base this plan on `main` after #209 merges**; if it has not merged at execution time, base on branch `ozykhan/macos-launchd-rootless`. The file `cmd/piper/agent.go` (extended by Tasks 2-3) exists only once #209 is in the base.
- **No cgo.** All builds pass with `CGO_ENABLED=0`; `make cross` (linux/arm64) stays green. No build-tag/GOOS-split files — gate on `runtime.GOOS` at runtime via the existing `agentGOOS` var.
- **Module path:** `github.com/getpiper/piper`.
- **Root install unchanged.** `curl | sudo sh` (root) must still install the system daemon exactly as today; the existing `packaging/systemd/piperd.service`, `/var/lib/piper`, and system-path install tests stay green. The only `install.sh` change is replacing the non-root `die` with the rootless path.
- **Rootless is ephemeral.** No `loginctl enable-linger`. Boot survival is what `daemonize` buys.
- **No data migration** on `daemonize` (`~/.piper/piperd` → `/var/lib/piper`): promotion is a fresh durable install.
- **Deployment status strings** unchanged: `"building"`, `"running"`, `"failed"`, `"stopped"`.
- **Commits:** conventional-commit style, one per task step-group, referencing the design and `#56` (agent packaging), ending with:
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`
- **Before claiming done:** run `make verify` (gofmt → vet → test → cross).
- Design: `docs/superpowers/specs/2026-07-14-linux-rootless-toggleable-piperd-design.md`.

---

## Part A — `[repo]` systemd user unit + rootless install

### Task 1: systemd user unit + goreleaser asset + contract test

**Files:**
- Create: `packaging/systemd/piperd.user.service`
- Modify: `.goreleaser.yaml` (`release.extra_files`)
- Modify: `packaging/systemd/piperd_test.go` (add user-unit contract test)

**Interfaces:**
- Produces: a release asset `piperd.user.service` — a rootless systemd **user** unit that runs `%h/.local/bin/piperd` on `:8080`/`:8443` under `~/.piper`, sourcing `~/.piper/piperd.env` last. Consumed by `install.sh` (Task 4, fetched from the release) and referenced by docs (Task 5).

- [ ] **Step 1: Write the failing contract test**

Append to `packaging/systemd/piperd_test.go`:

```go
func TestPiperdUserServiceContract(t *testing.T) {
	b, err := os.ReadFile("piperd.user.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)
	required := []string{
		"ExecStart=%h/.local/bin/piperd",
		"Environment=PIPER_HTTP_ADDR=:8080",
		"Environment=PIPER_HTTPS_ADDR=:8443",
		"Environment=XDG_DATA_HOME=%h/.piper/piperd",
		"Environment=XDG_CONFIG_HOME=%h/.piper/piperd",
		"EnvironmentFile=-%h/.piper/piperd.env",
		"Restart=on-failure",
		"WantedBy=default.target",
	}
	for _, directive := range required {
		if !strings.Contains(unit, directive) {
			t.Errorf("user unit missing %q", directive)
		}
	}
	// Rootless: must NOT carry any system-service privilege/state directives.
	for _, forbidden := range []string{"DynamicUser", "CAP_NET_BIND_SERVICE", "/var/lib/piper"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("user unit must not contain %q", forbidden)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./packaging/systemd/ -run TestPiperdUserServiceContract -v`
Expected: FAIL — `open piperd.user.service: no such file or directory`.

- [ ] **Step 3: Create the user unit**

Create `packaging/systemd/piperd.user.service`:

```ini
[Unit]
Description=Piper agent (rootless dev instance)
After=docker.service network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/piperd
Environment=PIPER_HTTP_ADDR=:8080
Environment=PIPER_HTTPS_ADDR=:8443
Environment=XDG_DATA_HOME=%h/.piper/piperd
Environment=XDG_CONFIG_HOME=%h/.piper/piperd
EnvironmentFile=-%h/.piper/piperd.env
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
```

- [ ] **Step 4: Publish it as a release asset**

In `.goreleaser.yaml`, under `release.extra_files`, add a line after the `piperd.service` glob:

```yaml
    - glob: packaging/systemd/piperd.user.service
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./packaging/systemd/ -run TestPiperdUserServiceContract -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add packaging/systemd/piperd.user.service packaging/systemd/piperd_test.go .goreleaser.yaml
git commit -m "$(printf 'feat(repo): rootless systemd user unit (piperd.user.service)\n\nPart of #56\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Part B — `[cli]` `piper agent` on Linux

### Task 2: `piper agent up|down|status` Linux branch (systemctl --user)

**Files:**
- Modify: `cmd/piper/agent.go` (refactor `agent()` to dispatch by GOOS; add Linux helpers + seams)
- Modify: `cmd/piper/agent_test.go` (replace the non-darwin gate test; add Linux tests)

**Interfaces:**
- Consumes: the user unit name/path from Task 1 (`piperd`, installed as `~/.config/systemd/user/piperd.service`).
- Produces (package-level, used by Task 3): `agentLinux(args []string, stdout, stderr io.Writer) int`, and the test seams `systemctlRun func(args ...string) (string, error)` and `userUnitPath func() (string, error)`.

Context: today `cmd/piper/agent.go`'s `agent()` errors for any non-darwin GOOS. This task splits dispatch into `agentDarwin` (the existing switch, unchanged behavior) and a new `agentLinux`, and gates `agent()` on GOOS. The existing macOS helpers (`agentUp`/`agentDown`/`agentStatus`, `launchctlRun`, `launchdPlistPath`, `guiTarget`) are **untouched**.

- [ ] **Step 1: Write the failing tests**

In `cmd/piper/agent_test.go`, **replace** `TestAgentNonDarwinGate` (linux is now supported) with an unsupported-GOOS test, and add the Linux tests. Ensure the import block has `"path/filepath"`, `"runtime"`, `"strings"`, `"bytes"`, `"os"`, `"testing"`.

```go
func TestAgentUnsupportedGOOS(t *testing.T) {
	agentGOOS = "windows"
	defer func() { agentGOOS = runtime.GOOS }()
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "macOS and Linux only") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestAgentUpLinuxStarts(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()

	dir := t.TempDir()
	unit := filepath.Join(dir, "piperd.service")
	if err := os.WriteFile(unit, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath := userUnitPath
	userUnitPath = func() (string, error) { return unit, nil }
	defer func() { userUnitPath = oldPath }()

	var gotArgs []string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	want := []string{"--user", "start", "piperd"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
	if !strings.Contains(out.String(), "started") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestAgentUpLinuxNotInstalled(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldPath := userUnitPath
	userUnitPath = func() (string, error) { return filepath.Join(t.TempDir(), "absent.service"), nil }
	defer func() { userUnitPath = oldPath }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "not installed") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestAgentDownLinuxStops(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	var gotArgs []string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"down"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	want := []string{"--user", "stop", "piperd"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
}

func TestAgentStatusLinux(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()

	dir := t.TempDir()
	unit := filepath.Join(dir, "piperd.service")
	if err := os.WriteFile(unit, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath := userUnitPath
	userUnitPath = func() (string, error) { return unit, nil }
	defer func() { userUnitPath = oldPath }()

	cases := []struct {
		active string
		err    error
		want   string
	}{
		{"active\n", nil, "piperd: running"},
		{"inactive\n", errFake, "piperd: stopped"},
	}
	for _, c := range cases {
		oldRun := systemctlRun
		systemctlRun = func(args ...string) (string, error) { return c.active, c.err }
		var out, errb bytes.Buffer
		if code := agent([]string{"status"}, &out, &errb); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("active=%q: stdout = %q, want %q", c.active, out.String(), c.want)
		}
		systemctlRun = oldRun
	}
}

func TestAgentStatusLinuxNotInstalled(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldPath := userUnitPath
	userUnitPath = func() (string, error) { return filepath.Join(t.TempDir(), "absent.service"), nil }
	defer func() { userUnitPath = oldPath }()

	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("stdout = %q", out.String())
	}
}

var errFake = fmt.Errorf("exit status 3")
```

(Add `"fmt"` to the test import block for `errFake`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/piper/ -run 'TestAgent(Unsupported|Up|Down|Status)' -v`
Expected: FAIL — `undefined: systemctlRun` / `undefined: userUnitPath`, and `agent` still returns the old non-darwin error.

- [ ] **Step 3: Refactor `agent()` and add the Linux branch**

In `cmd/piper/agent.go`, **replace** the existing `agent(...)` function with the GOOS dispatcher below, and add the Linux seams + helpers. Add `userUnitName` next to `launchdLabel`. Leave `agentUp`/`agentDown`/`agentStatus` (the darwin helpers) unchanged.

```go
const userUnitName = "piperd"

// systemctlRun runs `systemctl <args...>` and returns combined output; a var so
// tests can substitute it without a real systemd.
var systemctlRun = func(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	return string(out), err
}

// userUnitPath returns the installed systemd user-unit path; a var so tests can
// point it at a temp file.
var userUnitPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", userUnitName+".service"), nil
}

// agent dispatches `piper agent ...` to the platform's rootless agent manager.
func agent(args []string, stdout, stderr io.Writer) int {
	switch agentGOOS {
	case "darwin":
		return agentDarwin(args, stdout, stderr)
	case "linux":
		return agentLinux(args, stdout, stderr)
	default:
		fmt.Fprintln(stderr, "error: `piper agent` supports macOS and Linux only")
		return 2
	}
}

func agentDarwin(args []string, stdout, stderr io.Writer) int {
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

func agentLinux(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
	switch args[0] {
	case "up":
		return agentUpLinux(stdout, stderr)
	case "down":
		return agentDownLinux(stdout, stderr)
	case "status":
		return agentStatusLinux(stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
}

func agentUpLinux(stdout, stderr io.Writer) int {
	unit, err := userUnitPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(unit); err != nil {
		fmt.Fprintf(stderr, "error: user service not installed at %s\nsee docs/manual-setup.md (Run the agent on Linux, rootless)\n", unit)
		return 1
	}
	if out, err := systemctlRun("--user", "start", userUnitName); err != nil {
		fmt.Fprintf(stderr, "error: systemctl --user start failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd started")
	return 0
}

func agentDownLinux(stdout, stderr io.Writer) int {
	if out, err := systemctlRun("--user", "stop", userUnitName); err != nil {
		fmt.Fprintf(stderr, "error: systemctl --user stop failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd stopped")
	return 0
}

func agentStatusLinux(stdout, stderr io.Writer) int {
	unit, err := userUnitPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(unit); err != nil {
		fmt.Fprintln(stdout, "piperd: not installed")
		return 0
	}
	out, _ := systemctlRun("--user", "is-active", userUnitName)
	if strings.TrimSpace(out) == "active" {
		fmt.Fprintln(stdout, "piperd: running")
	} else {
		fmt.Fprintln(stdout, "piperd: stopped")
	}
	return 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/piper/ -run 'TestAgent' -v`
Expected: PASS — the new Linux tests, the unsupported-GOOS test, and the existing darwin tests (`TestAgentUpBootstraps`, `TestAgentDownBootsOut`, `TestAgentUsage`) all green.

- [ ] **Step 5: Verify build + full package tests**

Run: `go build ./... && go test ./cmd/piper/`
Expected: build succeeds; PASS.

- [ ] **Step 6: gofmt + commit**

```bash
gofmt -w cmd/piper/agent.go cmd/piper/agent_test.go
git add cmd/piper/agent.go cmd/piper/agent_test.go
git commit -m "$(printf 'feat(cli): piper agent up/down/status on Linux (systemctl --user)\n\nPart of #56\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

### Task 3: `piper agent daemonize` (promote rootless → system daemon)

**Files:**
- Create: `cmd/piper/piperd.service` (embedded copy of `packaging/systemd/piperd.service`)
- Create: `cmd/piper/piperd.env.example` (embedded copy of `packaging/systemd/piperd.env.example`)
- Modify: `cmd/piper/agent.go` (embed + `daemonize` case + `agentDaemonize`)
- Modify: `cmd/piper/agent_test.go` (daemonize tests + embed-guard test)

**Interfaces:**
- Consumes: `agentLinux` dispatch and `systemctlRun` from Task 2; the canonical `packaging/systemd/piperd.service` + `piperd.env.example` (asserted byte-identical to the embedded copies).
- Produces: `agentDaemonize(stdout, stderr io.Writer) int` and the seams `agentEUID func() int`, `userHomeDir func(string) (string, error)`, `systemBinDir`/`systemUnitDir`/`systemEnvDir string`.

Context: `daemonize` is Linux-only (reached only via `agentLinux`) and needs root. `sudo piper agent daemonize` runs as root but the rootless user service belongs to `$SUDO_USER`; tearing it down is best-effort via `systemctl --user --machine=$SUDO_USER@.host`. The system unit + env are `go:embed`ed so promotion works offline; a guard test keeps the embedded copies identical to the canonical `packaging/systemd/` files.

- [ ] **Step 1: Create the embedded copies**

Copy the canonical files into the CLI package so `go:embed` can reach them:

```bash
cp packaging/systemd/piperd.service cmd/piper/piperd.service
cp packaging/systemd/piperd.env.example cmd/piper/piperd.env.example
```

- [ ] **Step 2: Write the failing tests**

Append to `cmd/piper/agent_test.go` (the embed-guard test keeps the copies honest; the daemonize tests drive the promotion via seams):

```go
func TestEmbeddedSystemFilesMatchCanonical(t *testing.T) {
	for _, name := range []string{"piperd.service", "piperd.env.example"} {
		got, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("..", "..", "packaging", "systemd", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("cmd/piper/%s differs from packaging/systemd/%s — re-copy it", name, name)
		}
	}
}

func TestDaemonizeNeedsRoot(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldEUID := agentEUID
	agentEUID = func() int { return 1000 }
	defer func() { agentEUID = oldEUID }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "sudo") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestDaemonizeNeedsSudoUser(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()
	t.Setenv("SUDO_USER", "")

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "SUDO_USER") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestDaemonizePromotes(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()
	t.Setenv("SUDO_USER", "alice")

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".local", "bin", "piperd"), []byte("PIPERD-BIN"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldHome := userHomeDir
	userHomeDir = func(u string) (string, error) { return home, nil }
	defer func() { userHomeDir = oldHome }()

	binDir, unitDir, envDir := t.TempDir(), t.TempDir(), t.TempDir()
	oldBin, oldUnit, oldEnv := systemBinDir, systemUnitDir, systemEnvDir
	systemBinDir, systemUnitDir, systemEnvDir = binDir, unitDir, envDir
	defer func() { systemBinDir, systemUnitDir, systemEnvDir = oldBin, oldUnit, oldEnv }()

	var calls [][]string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { calls = append(calls, args); return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	// user teardown, daemon-reload, enable --now
	joined := ""
	for _, c := range calls {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{
		"--user --machine=alice@.host disable --now piperd",
		"daemon-reload",
		"enable --now piperd",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing systemctl call %q; got:\n%s", want, joined)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(binDir, "piperd")); string(b) != "PIPERD-BIN" {
		t.Errorf("piperd not installed to system bindir; got %q", string(b))
	}
	if _, err := os.Stat(filepath.Join(unitDir, "piperd.service")); err != nil {
		t.Errorf("system unit not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(envDir, "piperd.env")); err != nil {
		t.Errorf("env not seeded: %v", err)
	}
	if !strings.Contains(out.String(), "daemonized") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestDaemonizeDoesNotClobberEnv(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()
	t.Setenv("SUDO_USER", "alice")

	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755)
	os.WriteFile(filepath.Join(home, ".local", "bin", "piperd"), []byte("x"), 0o755)
	oldHome := userHomeDir
	userHomeDir = func(u string) (string, error) { return home, nil }
	defer func() { userHomeDir = oldHome }()

	binDir, unitDir, envDir := t.TempDir(), t.TempDir(), t.TempDir()
	oldBin, oldUnit, oldEnv := systemBinDir, systemUnitDir, systemEnvDir
	systemBinDir, systemUnitDir, systemEnvDir = binDir, unitDir, envDir
	defer func() { systemBinDir, systemUnitDir, systemEnvDir = oldBin, oldUnit, oldEnv }()

	edited := "PIPER_BASE_DOMAIN=example.com\n"
	if err := os.WriteFile(filepath.Join(envDir, "piperd.env"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if b, _ := os.ReadFile(filepath.Join(envDir, "piperd.env")); string(b) != edited {
		t.Errorf("env clobbered: got %q", string(b))
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./cmd/piper/ -run 'TestDaemonize|TestEmbeddedSystemFiles' -v`
Expected: FAIL — `undefined: agentEUID` / `undefined: userHomeDir` / `undefined: systemBinDir` / `agent "daemonize"` hits the usage default (code 2, not the gated codes).

- [ ] **Step 4: Implement `daemonize`**

In `cmd/piper/agent.go`: add `"os/user"` and a **blank** `_ "embed"` to the import block (embedding into a `string` var needs the `embed` package imported, but blank because it isn't referenced by name — a plain `"embed"` import would fail to compile as "imported and not used"); then add the embed vars, seams, the `daemonize` case, `agentDaemonize`, and `copyFile`.

Add near the top of the file (after the imports), the embed directives:

```go
//go:embed piperd.service
var embeddedSystemUnit string

//go:embed piperd.env.example
var embeddedSystemEnv string
```

Add these seams (near `systemctlRun`):

```go
// Overridable system install targets + identity, so daemonize unit-tests
// against temp dirs and stubbed identity.
var (
	agentEUID     = os.Geteuid
	systemBinDir  = "/usr/local/bin"
	systemUnitDir = "/etc/systemd/system"
	systemEnvDir  = "/etc/piper"
)

// userHomeDir resolves a username to its home directory; a var so tests can stub it.
var userHomeDir = func(username string) (string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}
```

In `agentLinux`, add the `daemonize` case and update the usage strings from `<up|down|status>` to `<up|down|status|daemonize>`:

```go
func agentLinux(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status|daemonize>")
		return 2
	}
	switch args[0] {
	case "up":
		return agentUpLinux(stdout, stderr)
	case "down":
		return agentDownLinux(stdout, stderr)
	case "status":
		return agentStatusLinux(stdout, stderr)
	case "daemonize":
		return agentDaemonize(stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status|daemonize>")
		return 2
	}
}
```

Add the implementation:

```go
// agentDaemonize promotes the rootless per-user agent into the systemd system
// daemon (durable, :80/:443, boot-surviving). Linux + root only. It does NOT
// migrate ~/.piper state to /var/lib/piper — a fresh durable install.
func agentDaemonize(stdout, stderr io.Writer) int {
	if agentEUID() != 0 {
		fmt.Fprintln(stderr, "error: `piper agent daemonize` needs root — re-run with sudo")
		return 1
	}
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "" {
		fmt.Fprintln(stderr, "error: SUDO_USER unset — run via `sudo piper agent daemonize`")
		return 1
	}

	// 1. Tear down the invoking user's rootless service (best-effort).
	if out, err := systemctlRun("--user", "--machine="+sudoUser+"@.host", "disable", "--now", userUnitName); err != nil {
		fmt.Fprintf(stderr, "warning: could not stop the rootless service for %s (run `piper agent down` as %s if it lingers): %v\n%s", sudoUser, sudoUser, err, out)
	}

	// 2. Copy piperd from the user's ~/.local/bin into the system bindir.
	home, err := userHomeDir(sudoUser)
	if err != nil {
		fmt.Fprintf(stderr, "error: cannot resolve home for %s: %v\n", sudoUser, err)
		return 1
	}
	if err := os.MkdirAll(systemBinDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := copyFile(filepath.Join(home, ".local", "bin", "piperd"), filepath.Join(systemBinDir, "piperd"), 0o755); err != nil {
		fmt.Fprintf(stderr, "error: installing piperd to %s: %v\n", systemBinDir, err)
		return 1
	}

	// 3. Write the system unit.
	if err := os.MkdirAll(systemUnitDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(systemUnitDir, "piperd.service"), []byte(embeddedSystemUnit), 0o644); err != nil {
		fmt.Fprintf(stderr, "error: writing unit: %v\n", err)
		return 1
	}

	// 4. Seed the env file (skip-if-exists — never clobber operator edits).
	if err := os.MkdirAll(systemEnvDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	envPath := filepath.Join(systemEnvDir, "piperd.env")
	if _, err := os.Stat(envPath); err != nil {
		if err := os.WriteFile(envPath, []byte(embeddedSystemEnv), 0o600); err != nil {
			fmt.Fprintf(stderr, "error: writing env: %v\n", err)
			return 1
		}
	}

	// 5. Enable + start the system service.
	if out, err := systemctlRun("daemon-reload"); err != nil {
		fmt.Fprintf(stderr, "error: systemctl daemon-reload: %v\n%s", err, out)
		return 1
	}
	if out, err := systemctlRun("enable", "--now", "piperd"); err != nil {
		fmt.Fprintf(stderr, "error: systemctl enable --now piperd: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd daemonized — system service on :80/:443, boot-surviving")
	return 0
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./cmd/piper/ -run 'TestDaemonize|TestEmbeddedSystemFiles|TestAgent' -v`
Expected: PASS (all).

- [ ] **Step 6: Verify build + full package tests**

Run: `go build ./... && go test ./cmd/piper/`
Expected: build succeeds; PASS.

- [ ] **Step 7: gofmt + commit**

```bash
gofmt -w cmd/piper/agent.go cmd/piper/agent_test.go
git add cmd/piper/agent.go cmd/piper/agent_test.go cmd/piper/piperd.service cmd/piper/piperd.env.example
git commit -m "$(printf 'feat(cli): piper agent daemonize — promote rootless to systemd system daemon\n\nPart of #56\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Part A (cont.) — `[repo]` rootless installer

### Task 4: `install.sh` rootless default + install test

**Files:**
- Modify: `install.sh` (`install_agent` dispatch; new `install_agent_rootless`; new `PIPER_USER_SYSTEMD_DIR`)
- Modify: `packaging/install/install_test.go` (dedupe `run` env; new rootless test)

**Interfaces:**
- Consumes: the `piperd.user.service` release asset from Task 1; the existing `piperd.env.example` asset.
- Produces: a non-root install path that drops `piperd`+`piper` into `~/.local/bin`, the user unit into `~/.config/systemd/user/piperd.service`, and seeds `~/.piper/piperd.env`.

- [ ] **Step 1: Fix `run`'s env handling, then write the failing rootless test**

The existing `run` helper appends overrides to `os.Environ()`, which does not reliably override intrinsic vars like `HOME` (glibc `getenv` returns the first match). The rootless test overrides `HOME`, so first make overrides win by de-duplicating. In `packaging/install/install_test.go`, replace the body of `run` that builds `cmd.Env`:

```go
	cmd := exec.Command("sh", append([]string{scriptPath(t)}, args...)...)
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			merged[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range env {
		merged[k] = v
	}
	cmd.Env = nil
	for k, v := range merged {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
```

Then append the rootless test:

```go
func TestRootlessAgentInstall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rootless agent path targets Linux/systemd")
	}
	if os.Getuid() == 0 {
		t.Skip("rootless path requires a non-root user")
	}
	osTok, archTok := hostOSArch()
	tag := "v9.9.9"
	ver := strings.TrimPrefix(tag, "v")
	assets := map[string][]byte{
		fmt.Sprintf("piperd_%s_%s_%s.tar.gz", ver, osTok, archTok): tarGz(t, "piperd", "fake-piperd"),
		fmt.Sprintf("piper_%s_%s_%s.tar.gz", ver, osTok, archTok):  tarGz(t, "piper", "fake-piper"),
		"piperd.user.service": []byte("[Service]\nExecStart=%h/.local/bin/piperd\n"),
		"piperd.env.example":  []byte("#PIPER_API_ADDR=127.0.0.1:8088\n"),
	}
	srv := newReleaseServer(t, assets, nil)

	home := t.TempDir()
	unitDir := t.TempDir()
	env := map[string]string{
		"PIPER_BASE_URL":         srv.URL,
		"PIPER_VERSION":          tag,
		"HOME":                   home,
		"PIPER_USER_SYSTEMD_DIR": unitDir,
	}
	// No --cli-only (full agent) and --no-enable (skip systemctl --user shell-out).
	out, err := run(t, []string{"--no-enable"}, env)
	if err != nil {
		t.Fatalf("rootless install failed: %v\n%s", err, out)
	}
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", "piperd"),
		filepath.Join(home, ".local", "bin", "piper"),
		filepath.Join(unitDir, "piperd.service"),
		filepath.Join(home, ".piper", "piperd.env"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v\n%s", p, err, out)
		}
	}

	// Re-run must not clobber an operator-edited env file.
	edited := "PIPER_BASE_DOMAIN=example.com\n"
	if err := os.WriteFile(filepath.Join(home, ".piper", "piperd.env"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, []string{"--no-enable"}, env); err != nil {
		t.Fatalf("re-run failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(home, ".piper", "piperd.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != edited {
		t.Errorf("env file clobbered on re-run: got %q", string(got))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./packaging/install/ -run TestRootlessAgentInstall -v`
Expected: FAIL — the current `install_agent` `die`s for non-root (`the full agent install needs root`), so no files are placed.

- [ ] **Step 3: Add the rootless branch to `install.sh`**

In `install.sh`, add the env-var default near the others (after line 12, `PIPER_ENV_DIR=...`):

```sh
PIPER_USER_SYSTEMD_DIR="${PIPER_USER_SYSTEMD_DIR:-$HOME/.config/systemd/user}"
```

Replace the non-root `die` in `install_agent` (the current lines `if [ -z "$PIPER_PREFIX" ] && [ "$(id -u)" -ne 0 ]; then die ... fi`) with a dispatch into the rootless path:

```sh
	if [ -z "$PIPER_PREFIX" ] && [ "$(id -u)" -ne 0 ]; then
		install_agent_rootless "$os" "$arch" "$tag"
		return
	fi
```

Add the new function (place it just above `install_agent`):

```sh
install_agent_rootless() { # install_agent_rootless OS ARCH TAG
	os="$1"; arch="$2"; tag="$3"
	prefix="$HOME/.local/bin"
	mkdir -p "$prefix"
	download_verify piperd "$tag" "$os" "$arch" "$prefix"
	download_verify piper "$tag" "$os" "$arch" "$prefix"

	mkdir -p "$PIPER_USER_SYSTEMD_DIR"
	fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/piperd.user.service" \
		"$PIPER_USER_SYSTEMD_DIR/piperd.service" || die "download failed: piperd.user.service"

	mkdir -p "$HOME/.piper"
	if [ ! -f "$HOME/.piper/piperd.env" ]; then
		fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/piperd.env.example" \
			"$HOME/.piper/piperd.env" || die "download failed: piperd.env.example"
	fi
	echo "installed rootless piperd + piper $tag -> $prefix"

	if [ -z "$no_enable" ] && have systemctl; then
		systemctl --user daemon-reload
		systemctl --user enable --now piperd
		echo "rootless piperd enabled and started (systemctl --user)"
	else
		echo "note: not started (no systemctl or --no-enable); start with: piper agent up"
	fi
	case ":$PATH:" in
		*":$prefix:"*) ;;
		*) echo "note: $prefix is not on your PATH — add it to use piper/piperd" ;;
	esac
}
```

Also update the macOS guard message in `install_agent` to point at the new toggle (replace the existing `[ "$os" = linux ] || die ...` line):

```sh
	[ "$os" = linux ] || die "on macOS use --cli-only, then follow docs/manual-setup.md (Run the agent on macOS) to run 'piper agent up'"
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./packaging/install/ -run TestRootlessAgentInstall -v`
Expected: PASS.

- [ ] **Step 5: Verify the existing install tests still pass (root path unchanged)**

Run: `go test ./packaging/install/ -v`
Expected: PASS — including `TestAgentInstallDropsUnitAndEnv` (which sets `PIPER_PREFIX`, so it still takes the system branch), `TestCLIOnlyInstall`, `TestChecksumMismatchAborts`.

- [ ] **Step 6: Commit**

```bash
git add install.sh packaging/install/install_test.go
git commit -m "$(printf 'feat(repo): install.sh installs rootless when run non-root\n\nRoot install (curl | sudo sh) is unchanged; non-root now drops a per-user\npiperd + systemd user unit under ~/.piper instead of erroring.\n\nPart of #56\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Part C — `[docs]` docs

### Task 5: Linux rootless docs + doc contract test

**Files:**
- Modify: `docs/getting-started.md` (rootless Linux quick-start + daemonize)
- Modify: `docs/manual-setup.md` (Linux rootless manual section)
- Modify: `docs/runbooks/git-deploy-e2e.md` (Linux rootless verify/teardown note)
- Modify: `packaging/install/install_test.go` (doc contract test)

**Interfaces:**
- Consumes: the install flow (Task 4), `piper agent up/down/status` (Task 2), `piper agent daemonize` (Task 3), and the user unit path (Task 1). This is the doc-coverage gate.

- [ ] **Step 1: Write the failing doc test**

Append to `packaging/install/install_test.go` (`repoRoot` already exists in this file):

```go
func TestRootlessDocumentation(t *testing.T) {
	docs := map[string][]string{
		filepath.Join("docs", "getting-started.md"): {
			"piper agent up",
			"sudo piper agent daemonize",
		},
		filepath.Join("docs", "manual-setup.md"): {
			"packaging/systemd/piperd.user.service",
			"systemctl --user",
		},
		filepath.Join("docs", "runbooks", "git-deploy-e2e.md"): {
			"piper agent status",
			"journalctl --user -u piperd",
		},
	}
	for name, wants := range docs {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), name))
		if err != nil {
			t.Fatal(err)
		}
		content := string(b)
		for _, want := range wants {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./packaging/install/ -run TestRootlessDocumentation -v`
Expected: FAIL — `docs/getting-started.md missing "sudo piper agent daemonize"` (and the others).

- [ ] **Step 3: Add the rootless quick-start to `docs/getting-started.md`**

Open `docs/getting-started.md` and find the Linux install section (the one referencing `install.sh`). Add this subsection immediately after the primary install command block:

````markdown
### Rootless on Linux (dev boxes)

Run the installer **without** `sudo` and you get a rootless dev agent — piperd
runs as **you** on high ports (`:8080`/`:8443`) under `~/.piper`, managed by
`systemctl --user`. No root, and it's gone after a reboot (re-run `piper agent up`).

```bash
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh
piper agent up            # start it (no sudo)
piper agent status        # running / stopped / not installed
piper agent down          # stop it
```

Apps are served at `http://<name>.piper.localhost:8080`. Your user must be able to
reach a Docker socket — be in the `docker` group, or set `DOCKER_HOST`.

**Promote to a real daemon.** When you want a durable, boot-surviving service on
`:80`/`:443` (a Pi, a home server), promote it:

```bash
sudo piper agent daemonize
```

This installs the systemd **system** service (as `curl | sudo sh` would) and stops
the rootless one. It's a fresh durable install — your rootless `~/.piper` apps are
not migrated; redeploy them.
````

- [ ] **Step 4: Add the Linux rootless section to `docs/manual-setup.md`**

Open `docs/manual-setup.md`. Add this section immediately after the systemd "Run the agent as a service" (system) block and before the macOS section:

````markdown
## Run the agent on Linux, rootless (dev box)

For a dev box you can run piperd **rootless** as your user — the systemd twin of
the macOS LaunchAgent. Install the binary and the shipped **user** unit, then
toggle it with `piper agent`:

```bash
install -m 0755 bin/piperd ~/.local/bin/piperd
install -m 0755 bin/piper  ~/.local/bin/piper
mkdir -p ~/.config/systemd/user ~/.piper
install -m 0644 packaging/systemd/piperd.user.service \
  ~/.config/systemd/user/piperd.service
cp packaging/systemd/piperd.env.example ~/.piper/piperd.env   # optional overrides
systemctl --user daemon-reload
piper agent up
```

It serves apps on `http://<name>.piper.localhost:8080`, stores state under
`~/.piper/`, and is **not** boot-surviving (gone after reboot; re-run
`piper agent up`). Your user must reach a Docker socket (`docker` group or
`DOCKER_HOST`). To make it durable on `:80`/`:443`, run `sudo piper agent
daemonize` — see the system-service section above.
````

- [ ] **Step 5: Add the Linux rootless note to `docs/runbooks/git-deploy-e2e.md`**

Open `docs/runbooks/git-deploy-e2e.md`. Add this note near the teardown/verification section (alongside the existing systemd note):

````markdown
### Linux (rootless user agent)

On a dev box the agent can run rootless via `systemctl --user`:

```bash
piper agent status                 # running / stopped / not installed
journalctl --user -u piperd -f     # agent logs
piper agent down                   # stop it
```
````

- [ ] **Step 6: Run the doc test to verify it passes**

Run: `go test ./packaging/install/ -run TestRootlessDocumentation -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add docs/getting-started.md docs/manual-setup.md docs/runbooks/git-deploy-e2e.md packaging/install/install_test.go
git commit -m "$(printf 'docs(repo): Linux rootless install + piper agent daemonize\n\nPart of #56\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 6: Full verification gate + manual Linux smoke

**Files:** none (verification only).

- [ ] **Step 1: Run the full CI-equivalent gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests PASS (incl. `packaging/systemd`, `cmd/piper`, `packaging/install`), `make cross` (linux/arm64) builds. Fix anything that fails, then re-run.

- [ ] **Step 2: Manual Linux smoke test (documented; run on a real Linux box with Docker)**

Not automatable in CI — perform once and record the result in the PR:

```bash
# Rootless (no sudo):
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh   # or run local install.sh
piper agent up            # -> "piperd started", no sudo prompt
piper agent status        # -> "piperd: running"
# deploy a sample app, then confirm it serves:
#   curl http://<name>.piper.localhost:8080
piper agent down          # -> "piperd stopped"

# Promote to system daemon:
sudo piper agent daemonize   # -> "piperd daemonized ...", rootless one stops
systemctl status piperd      # -> active, boot-surviving; serves on :80
```

Expected: rootless piperd runs as your user (no `sudo`), an app is reachable on `:8080`; `daemonize` moves it to the boot-surviving system service on `:80` and tears the rootless one down. Confirm `~/.piper/piperd/` held the rootless SQLite DB.

- [ ] **Step 3: Open the PR**

```bash
git push -u origin ozykhan/linux-rootless-toggleable
gh pr create --base main \
  --title "feat: rootless-by-default piperd on Linux + piper agent daemonize" \
  --body "$(printf 'Brings the Linux agent to parity with the macOS rootless tier: %scurl | sh%s (non-root) installs a per-user piperd toggled by %spiper agent up/down/status%s via systemctl --user, and %ssudo piper agent daemonize%s promotes it to the existing systemd system daemon. Root install (curl | sudo sh) is unchanged.\n\nDesign: docs/superpowers/specs/2026-07-14-linux-rootless-toggleable-piperd-design.md\n\nPart of #56\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)' '`' '`' '`' '`' '`' '`')"
```

---

## Self-Review Notes (traceability)

- Spec §(A) user unit → Task 1; §(A) install.sh → Task 4. §(B) up/down/status → Task 2; §(B) daemonize → Task 3. §(C) docs → Task 5. Success criteria (`make verify` + Linux smoke) → Task 6.
- Type/name consistency: `systemctlRun`, `userUnitPath`, `userUnitName` defined in Task 2 and reused by Task 3's `agentDaemonize`/tests. `agentEUID`, `userHomeDir`, `systemBinDir`/`systemUnitDir`/`systemEnvDir`, `embeddedSystemUnit`/`embeddedSystemEnv`, `copyFile` defined in Task 3 Step 4 and driven by Task 3 Step 2 tests via the same names. `PIPER_USER_SYSTEMD_DIR` defined in Task 4 Step 3 and used by Task 4 Step 1's test. The user-unit contract strings asserted in Task 1 Step 1 match the file written in Step 3.
- Dependency: Tasks 2-3 require `cmd/piper/agent.go` from PR #209 — see Global Constraints. Task 3's embedded copies are kept identical to `packaging/systemd/` by `TestEmbeddedSystemFilesMatchCanonical`.
- The existing `install_test.go` system-path test stays green because the rootless branch triggers only on non-root **and** empty `PIPER_PREFIX`; that test sets `PIPER_PREFIX`.
```
