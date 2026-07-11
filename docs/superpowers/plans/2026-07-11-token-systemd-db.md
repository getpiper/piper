# systemd-aware `piperd token` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On a systemd-installed box, `piperd token <create|list|revoke>` targets the running service's DB (`/var/lib/piper/piper.db`) or fails with the exact command to run — never a silent write to `~/.piper/piperd`.

**Architecture:** The `token` subcommand path in `cmd/piperd` gets a data-dir resolver: explicit `PIPER_DATA_DIR` wins; on a systemd-managed box (`config.SystemManaged()`) it targets `config.SystemStateDir` (new var, `/var/lib/piper`), dropping process uid/gid to the state-dir owner when root (so SQLite `-wal`/`-shm` files stay reopenable by the DynamicUser) and erroring with a copy-pasteable `sudo` command when not. Daemon startup is untouched. Spec: `docs/superpowers/specs/2026-07-11-token-systemd-db-design.md`.

**Tech Stack:** Go stdlib only (`os`, `syscall`). No new dependencies.

## Global Constraints

- `CGO_ENABLED=0` must keep passing (`make cross`); stdlib `syscall` only, no cgo.
- Module path `github.com/getpiper/piper`; helpers live in `cmd/piperd`, `store` stays ignorant of paths/privileges (layering rule).
- Concurrent access with the running daemon is fine by design: `store.Open` already sets `busy_timeout(5000)` for exactly this case. Do NOT add service stop/start logic.
- Run `make verify` (gofmt → vet → test → cross) before claiming any task done.
- Commit style: conventional commits, body line `Part of #134`, trailer `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- Branch: `ozykhan/token-systemd-db` (already created off `origin/main`).

---

### Task 1: Owner-drop helpers (`ownerOf`, `dropToStateDirOwner`) + `config.SystemStateDir`

**Files:**
- Create: `cmd/piperd/token.go`
- Modify: `internal/config/config.go` (add `SystemStateDir` var next to `SystemEnvDir`, line ~89)
- Test: `cmd/piperd/token_test.go` (append)

**Interfaces:**
- Consumes: `config.SystemEnvDir` pattern (package var overridable in tests).
- Produces:
  - `var config.SystemStateDir = "/var/lib/piper"` (in `internal/config`)
  - `func ownerOf(path string) (uid, gid int, err error)` (in `package main`, cmd/piperd)
  - `func dropToStateDirOwner(dir string) error` (in `package main`, cmd/piperd) — no-op (nil) when the current euid already owns `dir`. Task 2 calls this.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/piperd/token_test.go` (add `"os"` to its imports):

```go
func TestOwnerOfReturnsCurrentUID(t *testing.T) {
	uid, _, err := ownerOf(t.TempDir())
	if err != nil {
		t.Fatalf("ownerOf: %v", err)
	}
	if uid != os.Getuid() {
		t.Errorf("uid = %d, want %d", uid, os.Getuid())
	}
}

func TestOwnerOfMissingPath(t *testing.T) {
	if _, _, err := ownerOf(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("want error for missing path")
	}
}

func TestDropToStateDirOwnerNoopWhenAlreadyOwner(t *testing.T) {
	// The dir is owned by whoever runs the test, so euid already matches and
	// no setuid is attempted — this covers the decision, not the syscall.
	if err := dropToStateDirOwner(t.TempDir()); err != nil {
		t.Fatalf("want nil for already-owned dir, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/piperd/ -run 'TestOwnerOf|TestDropToStateDirOwner' -v`
Expected: FAIL to build with `undefined: ownerOf` / `undefined: dropToStateDirOwner`

- [ ] **Step 3: Implement**

Add to `internal/config/config.go`, directly after the `SystemEnvDir` var (line ~89):

```go
// SystemStateDir is piperd's DynamicUser StateDirectory under the shipped
// systemd unit (Environment=PIPER_DATA_DIR= in piperd.service). `piperd token`
// targets it on a systemd-managed box so tokens land in the DB the running
// service reads. A var so tests can point it at a scratch directory.
var SystemStateDir = "/var/lib/piper"
```

Create `cmd/piperd/token.go`:

```go
package main

import (
	"fmt"
	"os"
	"syscall"
)

// ownerOf returns the uid/gid owning path.
func ownerOf(path string) (uid, gid int, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("stat %s: no owner info", path)
	}
	return int(st.Uid), int(st.Gid), nil
}

// dropToStateDirOwner switches the process to the uid/gid owning dir, so the
// SQLite side files (-wal/-shm) this command creates stay reopenable by the
// service's DynamicUser. A dir already owned by the current euid — including a
// root-owned one before the service's first start, which systemd re-chowns on
// start — is a no-op.
func dropToStateDirOwner(dir string) error {
	uid, gid, err := ownerOf(dir)
	if err != nil {
		return err
	}
	if uid == os.Geteuid() {
		return nil
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	return nil
}
```

(`syscall.Setuid`/`Setgid`/`Setgroups` exist on both linux and darwin and apply to all threads since Go 1.16 — no build-constraint split needed.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/piperd/ -run 'TestOwnerOf|TestDropToStateDirOwner' -v`
Expected: 3 × PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go cmd/piperd/token.go cmd/piperd/token_test.go
git commit -m "feat(agent): add state-dir owner drop for piperd token

Part of #134

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: `resolveTokenDataDir` — systemd-aware data-dir resolution

**Files:**
- Modify: `cmd/piperd/token.go` (append function)
- Test: `cmd/piperd/token_test.go` (append)

**Interfaces:**
- Consumes: `dropToStateDirOwner(dir string) error` (Task 1), `config.SystemManaged()`, `config.SystemStateDir`, `config.DefaultDataDir()`.
- Produces: `func resolveTokenDataDir(args []string) (string, error)` — `args` is the token subcommand line (`os.Args[2:]`), echoed in the sudo hint. On the root+systemd path it has the side effect of dropping privileges before returning. Task 3 calls this from `main`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/piperd/token_test.go` (add `"github.com/getpiper/piper/internal/config"` to its imports):

```go
// systemManaged points config at temp dirs simulating a systemd install:
// /etc/piper exists, and the state dir is stateDir. Restores on cleanup.
func systemManaged(t *testing.T, stateDir string) {
	t.Helper()
	oldEnv, oldState := config.SystemEnvDir, config.SystemStateDir
	config.SystemEnvDir = t.TempDir()
	config.SystemStateDir = stateDir
	t.Cleanup(func() { config.SystemEnvDir, config.SystemStateDir = oldEnv, oldState })
}

func TestResolveTokenDataDirEnvWins(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", "/custom/dir")
	systemManaged(t, t.TempDir()) // even on a systemd box

	dir, err := resolveTokenDataDir([]string{"create", "--name", "x"})
	if err != nil {
		t.Fatalf("resolveTokenDataDir: %v", err)
	}
	if dir != "/custom/dir" {
		t.Errorf("dir = %q, want /custom/dir", dir)
	}
}

func TestResolveTokenDataDirDefaultWhenNotManaged(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", "")
	old := config.SystemEnvDir
	config.SystemEnvDir = filepath.Join(t.TempDir(), "absent") // not systemd-managed
	defer func() { config.SystemEnvDir = old }()

	dir, err := resolveTokenDataDir([]string{"list"})
	if err != nil {
		t.Fatalf("resolveTokenDataDir: %v", err)
	}
	if want := config.DefaultDataDir(); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
}

func TestResolveTokenDataDirSystemManagedNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root")
	}
	t.Setenv("PIPER_DATA_DIR", "")
	systemManaged(t, t.TempDir())

	_, err := resolveTokenDataDir([]string{"create", "--name", "laptop"})
	if err == nil {
		t.Fatal("want error for non-root on a systemd-managed box")
	}
	if !strings.Contains(err.Error(), "sudo piperd token create --name laptop") {
		t.Errorf("error %q does not name the sudo command to run", err)
	}
}

func TestResolveTokenDataDirStateDirMissing(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", "")
	systemManaged(t, filepath.Join(t.TempDir(), "absent"))

	_, err := resolveTokenDataDir([]string{"list"})
	if err == nil {
		t.Fatal("want error when the state dir does not exist")
	}
	if !strings.Contains(err.Error(), "systemctl start piperd") {
		t.Errorf("error %q does not say to start the service", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/piperd/ -run TestResolveTokenDataDir -v`
Expected: FAIL to build with `undefined: resolveTokenDataDir`

- [ ] **Step 3: Implement**

Append to `cmd/piperd/token.go` (add `"strings"` and the config import):

```go
// resolveTokenDataDir picks the directory holding the DB `piperd token`
// operates on. Explicit PIPER_DATA_DIR always wins; otherwise, on a
// systemd-managed box, it targets the service's state dir — dropping this
// process to the dir owner's uid/gid when root, and failing with the exact
// sudo command to run when not — so the command can never silently write a
// DB the running service ignores (#134). args is the token subcommand line,
// echoed in that message.
func resolveTokenDataDir(args []string) (string, error) {
	if v := os.Getenv("PIPER_DATA_DIR"); v != "" {
		return v, nil
	}
	if !config.SystemManaged() {
		return config.DefaultDataDir(), nil
	}
	if _, err := os.Stat(config.SystemStateDir); os.IsNotExist(err) {
		return "", fmt.Errorf("service data dir %s does not exist; start the service first: sudo systemctl start piperd", config.SystemStateDir)
	} else if err != nil {
		return "", err
	}
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("this box is systemd-managed and the service data dir %s needs root; run: sudo piperd token %s", config.SystemStateDir, strings.Join(args, " "))
	}
	if err := dropToStateDirOwner(config.SystemStateDir); err != nil {
		return "", err
	}
	return config.SystemStateDir, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/piperd/ -run TestResolveTokenDataDir -v`
Expected: 4 × PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/piperd/token.go cmd/piperd/token_test.go
git commit -m "feat(agent): resolve piperd token data dir on systemd installs

Part of #134

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Wire the resolver into the `token` branch of `main()`

**Files:**
- Modify: `cmd/piperd/main.go:152-167` (the `token` early-exit branch)

**Interfaces:**
- Consumes: `resolveTokenDataDir(args []string) (string, error)` (Task 2).
- Produces: nothing new — behavior change only. `runTokenCmd` and the daemon path are untouched.

- [ ] **Step 1: Replace the data-dir line in the token branch**

In `cmd/piperd/main.go`, the branch currently reads:

```go
	if len(os.Args) > 1 && os.Args[1] == "token" {
		dataDir := config.Load().DataDir
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			log.Fatalf("data dir: %v", err)
		}
```

Change it to:

```go
	if len(os.Args) > 1 && os.Args[1] == "token" {
		dataDir, err := resolveTokenDataDir(os.Args[2:])
		if err != nil {
			log.Fatalf("token: %v", err)
		}
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			log.Fatalf("data dir: %v", err)
		}
```

The following `st, err := store.Open(...)` line still compiles unchanged (`:=` is legal because `st` is newly declared even though `err` now already exists). No other edits in the branch.

- [ ] **Step 2: Build and run the full package tests**

Run: `go test ./cmd/piperd/ -v -count=1`
Expected: all PASS (existing `TestTokenCmd*` unaffected — they call `runTokenCmd` directly; on dev machines without `/etc/piper`, the resolver takes the unchanged home-default path).

- [ ] **Step 3: Run the repo verification gate**

Run: `make verify`
Expected: gofmt clean, vet clean, all tests pass, cross-compile passes.

- [ ] **Step 4: Commit**

```bash
git add cmd/piperd/main.go
git commit -m "fix(agent): point piperd token at the service DB under systemd

Part of #134

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: README — document `sudo piperd token create`

**Files:**
- Modify: `README.md:43-61`

**Interfaces:**
- Consumes: the CLI behavior shipped in Task 3.
- Produces: docs only.

- [ ] **Step 1: Update the token-minting section**

`README.md` lines 43–61 currently document `piperd token create` bare. Replace the paragraph and code block so they read:

````markdown
As root this installs `piper` to `/usr/local/bin`; unprivileged, to
`~/.local/bin`. The control API requires a bearer token, so mint one on the
box and log the CLI in first (running `piperd token create` on the box is
itself the proof you own it — no auth needed for that step; on a systemd
install it needs `sudo` to reach the service's data dir and will say so if
you forget). The control API binds to loopback by default — override
`PIPER_API_ADDR` on the box to reach it from elsewhere on your LAN:

```bash
# on the box:
sudo piperd token create --name laptop         # prints a token once
# on the client:
piper login --token <token> --addr http://your-box:8088
piper list                                     # now authenticated
```

`piper login` verifies the token against the box and saves it (with the
address) to `~/.piper/piper/config.json`, mode `0600`; `PIPER_TOKEN` /
`PIPER_ADDR` override the saved values per command. Manage tokens on the box
with `sudo piperd token list` and `sudo piperd token revoke <name>`.
````

- [ ] **Step 2: Verify nothing else in the README shows the bare command**

Run: `grep -n "piperd token" README.md`
Expected: every runnable example uses `sudo piperd token …`; prose mentions without a command context may stay bare.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document sudo piperd token create for systemd installs

Part of #134

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Final gate + PR

**Files:** none (verification + PR only)

- [ ] **Step 1: Full verification**

Run: `make verify`
Expected: gofmt clean, vet clean, `go test ./...` all pass, `make cross` passes.

- [ ] **Step 2: Push and open the PR**

```bash
git push -u origin ozykhan/token-systemd-db
gh pr create --base main \
  --title "[agent] piperd token targets the service DB on systemd installs" \
  --body "$(cat <<'EOF'
On a systemd-installed box, `piperd token <create|list|revoke>` now targets the running service's DB (`/var/lib/piper/piper.db`): under `sudo` it drops to the state-dir owner uid/gid before opening the store (so `-wal`/`-shm` files stay reopenable by the DynamicUser); without root it fails with the exact `sudo` command to run. Explicit `PIPER_DATA_DIR` still wins, and non-systemd boxes are unchanged. The command can no longer silently write a DB the running service ignores.

Design: `docs/superpowers/specs/2026-07-11-token-systemd-db-design.md`

Closes #134

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Expected: PR opens against `main`; CI `verify` + e2e gates run.
