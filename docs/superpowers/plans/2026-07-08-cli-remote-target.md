# CLI Remote Target (`piper --remote`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Teach the `piper` CLI to drive a relay-connected box through the relay's control plane via a global `--remote <base-domain>` flag (default from `PIPER_REMOTE`), leaving local/loopback usage untouched.

**Architecture:** The relay control plane (merged in #91) already accepts `https://api.<apex>/agents/<base-domain>/v1/...` with `Authorization: Bearer <account-credential>`, strips the prefix, and swaps the credential for the box's Token B. So "remote" is only a different base URL + token for the existing `internal/client.Client`: `dialClient` grows one branch, a global flag selects the target, and guard rails reject `--remote` on the three commands that are inherently local (`version`, `login`, `connect`).

**Tech Stack:** Go stdlib only (`flag`, `net/http/httptest` for tests). No new dependencies.

**Spec:** `docs/superpowers/specs/2026-07-08-cli-remote-target-design.md`

## Global Constraints

- `CGO_ENABLED=0` must keep building (no new deps, so automatic — but `make verify` proves it).
- Module path `github.com/getpiper/piper`.
- Local defaults unchanged: control API `http://127.0.0.1:8088`, config at `~/.piper/piper/config.json` (0600).
- Commits: conventional-commit style, one per task, ending with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- Branch: `faruk/cli-remote-target` (already created off `main`). PR body: `Closes #74`.
- Test style: table-less house style — one focused `TestXxx` per behavior, `httptest.NewServer` fakes, `t.Setenv("HOME", t.TempDir())` to isolate the config file, assertions inside the fake handler with `t.Errorf`.
- Error output style: `fmt.Fprintln(stderr, "error:", ...)`, exit codes: 1 = operational failure, 2 = usage error.

---

### Task 1: Global `--remote` flag + guard rails for local-only commands

`run()` in `cmd/piper/main.go` currently dispatches on `args[0]` with no global flags. Add a global FlagSet parsed before dispatch, registering `--remote` with its default from `PIPER_REMOTE`. Reject the **explicit flag** on `version`, `login`, `connect` (exit 2); the **env var** is simply not consulted by those commands (so `export PIPER_REMOTE=...` + `piper login` to refresh a credential keeps working — same coexistence model as `DOCKER_HOST` and `docker login`).

**Files:**
- Modify: `cmd/piper/main.go` (`run()` at ~line 66, `usage()` at ~line 322)
- Create: `cmd/piper/remote_test.go`

**Interfaces:**
- Consumes: existing `run(args []string, stdout, stderr io.Writer) int`, `usage(w io.Writer) int`.
- Produces: `run` accepts `--remote <base-domain>` before the subcommand; the flag is registered but its value not yet bound to a variable (Task 2 binds it as `remote := gfs.String(...)` and threads it into `dialClient`).

- [ ] **Step 1: Write the failing tests**

Create `cmd/piper/remote_test.go`:

```go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRemoteFlagRejectedForLocalOnlyCommands(t *testing.T) {
	for _, cmd := range []string{"version", "login", "connect"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--remote", "box.example.com", cmd}, &stdout, &stderr); code != 2 {
			t.Errorf("%s: code = %d, want 2", cmd, code)
		}
		if got := stderr.String(); !strings.Contains(got, "--remote does not apply") {
			t.Errorf("%s: stderr = %q", cmd, got)
		}
	}
}

// Pins the env-vs-flag guard-rail asymmetry: PIPER_REMOTE must NOT affect
// local-only commands (it passes trivially today; it guards against Task 2
// and later work wiring the env into these commands by accident).
func TestRunVersionIgnoresPiperRemoteEnv(t *testing.T) {
	t.Setenv("PIPER_REMOTE", "box.example.com")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify the first one fails**

Run: `go test ./cmd/piper/ -run 'TestRunRemote|TestRunVersionIgnores' -v`
Expected: `TestRunRemoteFlagRejectedForLocalOnlyCommands` FAILS — today `--remote` isn't a known token, so `run` falls through to `usage` (exit 2 but stderr says `usage: piper ...`, not `--remote does not apply`). `TestRunVersionIgnoresPiperRemoteEnv` passes (it's a pin, see comment).

- [ ] **Step 3: Implement the global flag parse + guard**

In `cmd/piper/main.go`, replace the top of `run`:

```go
func run(args []string, stdout, stderr io.Writer) int {
	gfs := flag.NewFlagSet("piper", flag.ContinueOnError)
	gfs.SetOutput(stderr)
	gfs.String("remote", os.Getenv("PIPER_REMOTE"), "base domain of a relay-connected box to drive through the relay")
	if err := gfs.Parse(args); err != nil {
		return 2
	}
	args = gfs.Args()
	if len(args) == 0 {
		return usage(stderr)
	}
	remoteFlagSet := false
	gfs.Visit(func(f *flag.Flag) {
		if f.Name == "remote" {
			remoteFlagSet = true
		}
	})
	if remoteFlagSet {
		switch args[0] {
		case "version", "login", "connect":
			fmt.Fprintf(stderr, "error: --remote does not apply to %q\n", args[0])
			return 2
		}
	}
	switch args[0] {
	// ... existing switch body unchanged ...
```

(The `gfs.String` return value is deliberately not bound yet — Task 2 binds it. `flag`'s parser stops at the first non-flag argument, so `piper --remote box deploy myapp --path .` leaves `deploy myapp --path .` in `gfs.Args()`, and plain `piper create blog` is untouched.)

Update `usage`:

```go
func usage(w io.Writer) int {
	fmt.Fprintln(w, "usage: piper [--remote <base-domain>] <version|login|connect|create|deploy|list|app|github> [args]")
	return 2
}
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./cmd/piper/ -v`
Expected: all PASS (including the pre-existing local-path tests — the regression guard the spec requires).

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/main.go cmd/piper/remote_test.go
git commit -m "feat(cli): global --remote flag, rejected on local-only commands

Part of #74.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: `dialClient` remote branch — route control commands through the relay

When a remote target is set, `dialClient` must point the existing `client.Client` at `<RelayAPI>/agents/<base-domain>` with the account credential from `piper login` (device flow) — the relay does the rest. All five `dialClient` callers thread the value through: `create`, `deploy`, `list` inline in `run`, plus `cmdApp` and `cmdGithub`/`githubSetup`.

**Files:**
- Modify: `cmd/piper/main.go` (`dialClient` at ~line 24; call sites in the `create`/`deploy`/`list` cases; `cmdApp`, `cmdGithub`, `githubSetup` signatures)
- Test: `cmd/piper/remote_test.go`

**Interfaces:**
- Consumes: `config.LoadClient() (config.ClientConfig, error)` — fields `Addr`, `Token`, `RelayAPI`, `AccountCredential`; `client.New(base, token string) *client.Client`; the `--remote` flag registered in Task 1.
- Produces: `dialClient(remote string, stderr io.Writer) (*client.Client, bool)`; `cmdApp(remote string, args []string, stdout, stderr io.Writer) int`; `cmdGithub(remote string, args []string, stdout, stderr io.Writer) int`; `githubSetup(remote, org string, stdout, stderr io.Writer) int`. In `run`, the flag value is bound as `remote := gfs.String("remote", os.Getenv("PIPER_REMOTE"), ...)` and passed as `*remote`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/piper/remote_test.go` (add imports `encoding/json`, `net/http`, `net/http/httptest`, `github.com/getpiper/piper/internal/config`, `github.com/getpiper/piper/internal/store` to the existing block):

```go
func TestRunRemoteListRoutesThroughRelay(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/ab12-alice.public.getpiper.co/v1/apps" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cred-xyz" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]store.App{{Name: "api", Port: 3000}})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "ab12-alice.public.getpiper.co", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "api\tport=3000\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunRemoteEnvSelectsTarget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	t.Setenv("PIPER_REMOTE", "env-box.public.getpiper.co")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/env-box.public.getpiper.co/v1/apps" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]store.App{})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}

func TestRunRemoteFlagOverridesEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	t.Setenv("PIPER_REMOTE", "wrong-box.public.getpiper.co")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/right-box.public.getpiper.co/v1/apps" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]store.App{})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "right-box.public.getpiper.co", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}

func TestRunRemoteRequiresRelayLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // empty config: no RelayAPI/AccountCredential
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "box.example.com", "list"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %s", code, stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "piper login") {
		t.Errorf("stderr = %q, want a pointer to `piper login`", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/piper/ -run TestRunRemote -v`
Expected: the four new tests FAIL — `--remote`'s value is parsed but ignored, so `list` dials the default local address (connection refused → exit 1, or wrong path assertions).

- [ ] **Step 3: Implement the remote branch and thread it through**

In `cmd/piper/main.go`:

Bind the flag in `run` (replacing Task 1's unbound registration):

```go
	remote := gfs.String("remote", os.Getenv("PIPER_REMOTE"), "base domain of a relay-connected box to drive through the relay")
```

Replace `dialClient`:

```go
// dialClient returns a client for piperd's control API: loopback by default,
// or — when remote is a relay-connected box's base domain — through the
// relay's control plane at <RelayAPI>/agents/<base-domain>, authenticated by
// the account credential from `piper login`. The relay strips the prefix and
// swaps the credential for the box's own token, so the same Client works for
// both.
func dialClient(remote string, stderr io.Writer) (*client.Client, bool) {
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, false
	}
	if remote != "" {
		if cc.RelayAPI == "" || cc.AccountCredential == "" {
			fmt.Fprintln(stderr, "error: remote target requires a relay login; run `piper login`")
			return nil, false
		}
		return client.New(strings.TrimRight(cc.RelayAPI, "/")+"/agents/"+remote, cc.AccountCredential), true
	}
	return client.New(cc.Addr, cc.Token), true
}
```

(`strings` is already imported in main.go.)

Update every call site:

- `create` case: `c, ok := dialClient(*remote, stderr)`
- `deploy` case: `c, ok := dialClient(*remote, stderr)`
- `list` case: `c, ok := dialClient(*remote, stderr)`
- dispatch: `case "app": return cmdApp(*remote, args[1:], stdout, stderr)` and `case "github": return cmdGithub(*remote, args[1:], stdout, stderr)`
- `func cmdApp(remote string, args []string, stdout, stderr io.Writer) int` — inside: `c, ok := dialClient(remote, stderr)`
- `func cmdGithub(remote string, args []string, stdout, stderr io.Writer) int` — inside: `return githubSetup(remote, *org, stdout, stderr)`
- `func githubSetup(remote, org string, stdout, stderr io.Writer) int` — inside: `c, ok := dialClient(remote, stderr)`

- [ ] **Step 4: Run the package tests**

Run: `go test ./cmd/piper/ -v`
Expected: all PASS — the four new remote tests plus every pre-existing local test (`TestRunList`, `TestRunCreate...`, `TestRunDeploy...`, `TestRunGithubSetup...` use `PIPER_ADDR` and take the local branch unchanged).

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/main.go cmd/piper/remote_test.go
git commit -m "feat(cli): route control commands through the relay with --remote/PIPER_REMOTE

Part of #74.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Remote `deploy` output — no fabricated URL

`deploy` hardcodes `http://<name>.piper.localhost` in its success line. Against a remote box the real hostname is relay-assigned and unknown to the CLI (see spec / follow-up [#93](https://github.com/getpiper/piper/issues/93)), so print `deployed <name> (<status>)` with no URL. Local output is unchanged.

**Files:**
- Modify: `cmd/piper/main.go` (`deploy` case, the `fmt.Fprintf(stdout, "deployed %s: http://%s.piper.localhost (%s)\n", ...)` line at ~line 146)
- Test: `cmd/piper/remote_test.go`

**Interfaces:**
- Consumes: `*remote` from Task 2; `client.Deploy(name, srcDir string) (store.Deployment, error)`.
- Produces: nothing new — output change only.

- [ ] **Step 1: Write the failing test**

Append to `cmd/piper/remote_test.go` (add imports `os`, `path/filepath`):

```go
func TestRunRemoteDeployPrintsNoLocalURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/ab12-alice.public.getpiper.co/v1/apps/blog/deploy" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "running"})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "ab12-alice.public.getpiper.co", "deploy", "blog", "--path", srcDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "deployed blog (running)\n" {
		t.Errorf("stdout = %q, want %q", got, "deployed blog (running)\n")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/piper/ -run TestRunRemoteDeployPrintsNoLocalURL -v`
Expected: FAIL — stdout is `deployed blog: http://blog.piper.localhost (running)\n` (the fabricated local URL).

- [ ] **Step 3: Implement the conditional output**

In the `deploy` case of `run`, replace:

```go
		fmt.Fprintf(stdout, "deployed %s: http://%s.piper.localhost (%s)\n", name, name, dep.Status)
```

with:

```go
		if *remote != "" {
			// The app's public hostname is relay-assigned at deploy time and
			// not in the response; print no URL rather than a wrong one.
			fmt.Fprintf(stdout, "deployed %s (%s)\n", name, dep.Status)
		} else {
			fmt.Fprintf(stdout, "deployed %s: http://%s.piper.localhost (%s)\n", name, name, dep.Status)
		}
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./cmd/piper/ -v`
Expected: all PASS (including `TestRunDeploySupportsNameFirstFlags`, which pins the local output).

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/main.go cmd/piper/remote_test.go
git commit -m "feat(cli): remote deploy prints no fabricated localhost URL

Part of #74.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Document remote usage + PROGRESS.md + full verify

The issue's acceptance criteria require the token's home to be documented. README already documents `~/.piper/piper/config.json` for login; add the remote-target section. Update `PROGRESS.md` (one line + issue link, per house rules — check its existing format first and match it).

**Files:**
- Modify: `README.md` (after the "Join the public relay (self-service)" section's final paragraph about BYO domains, ~line 105)
- Modify: `PROGRESS.md`

**Interfaces:** none — docs only.

- [ ] **Step 1: Add the README section**

Insert after the BYO-domain paragraph (the one ending `PIPER_RELAY_TLS_CERT`/`KEY` unset., ~line 105):

```markdown
### Drive a box remotely

Any control command (`create`, `deploy`, `list`, `app link`, `github setup`)
can target one of your relay-connected boxes from anywhere, by the base
domain `piper connect` printed:

```bash
piper --remote ab12-alice.public.getpiper.co list
export PIPER_REMOTE=ab12-alice.public.getpiper.co   # or set it once
piper deploy blog --path .
```

Requests travel relay → tunnel → box: the CLI authenticates to the relay with
the account credential `piper login` saved in `~/.piper/piper/config.json`
(mode `0600`), and the relay swaps it for the box's own token — your relay
credential never reaches the box, and the box still enforces its own auth.
The `--remote` flag overrides `PIPER_REMOTE`; `login` and `connect` are
inherently local and reject `--remote`.
```

- [ ] **Step 2: Update PROGRESS.md**

Two edits, matching the existing entry format:

1. Add a ✅ line next to the other #49-track entries (near the `piper login` / `piper connect` line at ~line 42):

```markdown
  - ✅ remote CLI target — `piper --remote <base-domain>` / `PIPER_REMOTE` drives a box through the relay control plane — [#74](https://github.com/getpiper/piper/issues/74)
```

2. In the "Epic #49 remains open" line (~line 48), move `[#74]` from the not-built list to the done list at the end of the sentence (alongside #72, #90, #73).

- [ ] **Step 3: Run the full verify gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests pass (Docker-dependent tests may SKIP), cross-compile OK.

- [ ] **Step 4: Commit**

```bash
git add README.md PROGRESS.md
git commit -m "docs: remote CLI usage via --remote/PIPER_REMOTE

Part of #74.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Completion

After all tasks: use superpowers:finishing-a-development-branch — push `faruk/cli-remote-target`, open a PR into `main` with `Closes #74` in the body (and the standard `🤖 Generated with [Claude Code](https://claude.com/claude-code)` footer), squash-merge policy per repo rules.
