# One-line installer + CLI-on-PATH Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a single `install.sh` that gets a Linux box to a running `piperd`
service — or any box to a `piper` CLI on `PATH` — from one `curl … | sh`.

**Architecture:** One POSIX `sh` script at the repo root with shared OS/arch
detection, version resolution, and checksum-verified download. Two modes: full
agent install (Linux+systemd) and `--cli-only`. A `--rc` flag installs the latest
pre-release. Tested by a Go offline exec harness that runs the script against an
`httptest` server serving a fake release — no network in `make test`.

**Tech Stack:** POSIX `sh`, goreleaser release assets (`tar.gz` + `checksums.txt`),
Go `testing` + `net/http/httptest` for the harness. No new Go dependencies.

## Global Constraints

- **Module path:** `github.com/piperbox/piper`. Repo: `getpiper/piper`.
- **No new runtime deps in the script:** POSIX `sh` only; `curl` or `wget`;
  `sha256sum` or `shasum`; `tar`; `install`. No `jq`.
- **Release asset naming (goreleaser, verbatim):** archives are
  `piper_{ver}_{os}_{arch}[v{arm}].tar.gz` / `piperd_{ver}_{os}_{arch}…`, where
  `{ver}` is the tag **without** the leading `v` (tag `v0.1.0-rc.1` →
  `piper_0.1.0-rc.1_darwin_arm64.tar.gz`). Checksums file is `checksums.txt`
  (sha256, `hash␣␣filename`). OS tokens `linux`/`darwin`; arch tokens
  `amd64`/`arm64`/`armv7`. Loose assets `piperd.service`, `piperd.env.example`.
- **CLI→daemon env var is `PIPER_ADDR`** (default `http://127.0.0.1:8088`).
- **systemd unit uses `DynamicUser=yes` + `StateDirectory=piper`**, so systemd
  creates the runtime user and `/var/lib/piper`. The installer therefore does
  **not** create a user or data dir — only binaries + unit + env skeleton +
  `daemon-reload` + `enable --now`.
- **`make test` stays hermetic** (no network) and **`make cross`** stays green.

## File Structure

- **Create `install.sh`** (repo root) — the installer. One responsibility:
  resolve → download+verify → install (CLI or agent).
- **Create `packaging/install/install_test.go`** (package `install`) — offline
  exec harness + documentation test. Locates the repo-root `install.sh` by
  walking up to `go.mod`, mirroring `packaging/systemd`'s `repositoryFile` helper.
- **Modify `README.md`** — add an **Install** section with the three one-liners
  and `PIPER_ADDR` CLI guidance; keep the manual block as the source fallback.
- **Modify `.goreleaser.yaml`** — attach `install.sh` to releases via
  `release.extra_files`.
- **Modify `PROGRESS.md`** — flip #46 and #47 to ✅.

---

### Task 1: CLI-only install — detect, download, verify, install

Foundation: arg parsing, OS/arch detection, checksum-verified download+extract,
and the `--cli-only` path with an **explicit** `PIPER_VERSION` (version
*resolution* comes in Task 2). Ships the shared test harness.

**Files:**
- Create: `install.sh`
- Test: `packaging/install/install_test.go`

**Interfaces:**
- Produces (shell functions, later tasks rely on them): `detect_os` →
  `linux|darwin`; `detect_arch` → `amd64|arm64|armv7`; `resolve_version` → tag
  string (stub here: echoes `$PIPER_VERSION`); `download_verify NAME TAG OS ARCH
  DESTDIR` (downloads `NAME_{ver}_{os}_{arch}.tar.gz` + `checksums.txt`, verifies
  sha256, extracts, `install -m 0755` the `NAME` binary into `DESTDIR`);
  `install_cli OS ARCH TAG`.
- Produces (Go test helpers, reused by Tasks 2–4): `scriptPath(t)` →
  repo-root `install.sh`; `hostOSArch()` → (osToken, archToken) for this machine;
  `tarGz(t, name, content)` → `[]byte`; `newReleaseServer(t, assets)` →
  `*httptest.Server` serving `/…/releases/download/{tag}/{file}` and
  auto-computed `checksums.txt`.
- Env seams (verbatim, later tasks depend on them): `PIPER_REPO`,
  `PIPER_BASE_URL`, `PIPER_API_URL`, `PIPER_VERSION`, `PIPER_PREFIX`,
  `PIPER_SYSTEMD_DIR`, `PIPER_ENV_DIR`, `PIPER_CLI_ONLY`, `PIPER_RC`; flags
  `--cli-only`, `--rc`, `--no-enable`, `--version[=]`.

- [ ] **Step 1: Write the failing test**

Create `packaging/install/install_test.go`:

```go
package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scriptPath returns the repo-root install.sh, found by walking up to go.mod.
func scriptPath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "install.sh")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test dir")
		}
		dir = parent
	}
}

// hostOSArch maps the running machine to goreleaser's os/arch tokens.
func hostOSArch() (string, string) {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	return runtime.GOOS, arch
}

// tarGz wraps a single named file (mode 0755) in a gzipped tar.
func tarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newReleaseServer serves assets under /{repo}/releases/download/{tag}/{file}
// and synthesises a checksums.txt from them. checksumsOverride, when non-nil,
// replaces the computed checksums.txt body (to simulate corruption).
func newReleaseServer(t *testing.T, assets map[string][]byte, checksumsOverride []byte) *httptest.Server {
	t.Helper()
	var sums strings.Builder
	for name, body := range assets {
		sum := sha256.Sum256(body)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	body := []byte(sums.String())
	if checksumsOverride != nil {
		body = checksumsOverride
	}
	assets["checksums.txt"] = body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := filepath.Base(r.URL.Path)
		if b, ok := assets[base]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// run executes install.sh with the given args and env overlay.
func run(t *testing.T, args []string, env map[string]string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	cmd := exec.Command("sh", append([]string{scriptPath(t)}, args...)...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCLIOnlyInstall(t *testing.T) {
	osTok, archTok := hostOSArch()
	tag := "v9.9.9"
	archive := fmt.Sprintf("piper_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), osTok, archTok)
	assets := map[string][]byte{archive: tarGz(t, "piper", "#!/bin/sh\necho fake-piper\n")}
	srv := newReleaseServer(t, assets, nil)

	prefix := t.TempDir()
	out, err := run(t, []string{"--cli-only", "--no-enable"}, map[string]string{
		"PIPER_REPO":     "getpiper/piper",
		"PIPER_BASE_URL": srv.URL,
		"PIPER_VERSION":  tag,
		"PIPER_PREFIX":   prefix,
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	info, err := os.Stat(filepath.Join(prefix, "piper"))
	if err != nil {
		t.Fatalf("piper not installed: %v\n%s", err, out)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("piper not executable: %v", info.Mode())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./packaging/install/ -run TestCLIOnlyInstall -v`
Expected: FAIL — `install.sh` does not exist (script path stat/exec error).

- [ ] **Step 3: Write minimal implementation**

Create `install.sh`:

```sh
#!/bin/sh
# Piper installer — https://github.com/piperbox/piper
# Installs the piperd agent (+ systemd unit) or just the piper CLI.
set -eu

PIPER_REPO="${PIPER_REPO:-getpiper/piper}"
PIPER_BASE_URL="${PIPER_BASE_URL:-https://github.com}"
PIPER_API_URL="${PIPER_API_URL:-https://api.github.com}"
PIPER_VERSION="${PIPER_VERSION:-}"
PIPER_PREFIX="${PIPER_PREFIX:-}"
PIPER_SYSTEMD_DIR="${PIPER_SYSTEMD_DIR:-/etc/systemd/system}"
PIPER_ENV_DIR="${PIPER_ENV_DIR:-/etc/piper}"
cli_only="${PIPER_CLI_ONLY:-}"
use_rc="${PIPER_RC:-}"
no_enable=""

die() { echo "piper-install: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
	case "$1" in
		--cli-only) cli_only=1 ;;
		--rc) use_rc=1 ;;
		--no-enable) no_enable=1 ;;
		--version) shift; PIPER_VERSION="${1:-}" ;;
		--version=*) PIPER_VERSION="${1#--version=}" ;;
		-h|--help) echo "Usage: install.sh [--cli-only] [--rc] [--version vX.Y.Z]"; exit 0 ;;
		*) die "unknown option: $1" ;;
	esac
	shift
done

detect_os() {
	os="$(uname -s)"
	case "$os" in
		Linux) echo linux ;;
		Darwin) echo darwin ;;
		*) die "unsupported OS: $os" ;;
	esac
}

detect_arch() {
	arch="$(uname -m)"
	case "$arch" in
		x86_64|amd64) echo amd64 ;;
		aarch64|arm64) echo arm64 ;;
		armv7l|armv7) echo armv7 ;;
		*) die "unsupported architecture: $arch" ;;
	esac
}

fetch() { # fetch URL DEST
	if have curl; then curl -fsSL "$1" -o "$2"
	elif have wget; then wget -qO "$2" "$1"
	else die "need curl or wget"; fi
}

fetch_stdout() { # fetch URL -> stdout
	if have curl; then curl -fsSL "$1"
	elif have wget; then wget -qO- "$1"
	else die "need curl or wget"; fi
}

sha256_of() { # sha256_of FILE -> hash
	if have sha256sum; then sha256sum "$1" | awk '{print $1}'
	elif have shasum; then shasum -a 256 "$1" | awk '{print $1}'
	else die "need sha256sum or shasum"; fi
}

# resolve_version echoes the release tag to install (stub: explicit only).
resolve_version() {
	[ -n "$PIPER_VERSION" ] || die "no version set"
	echo "$PIPER_VERSION"
}

# download_verify NAME TAG OS ARCH DESTDIR
download_verify() {
	name="$1"; tag="$2"; os="$3"; arch="$4"; dest="$5"
	ver="${tag#v}"
	archive="${name}_${ver}_${os}_${arch}.tar.gz"
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/$archive" "$tmp/$archive" \
		|| die "download failed: $archive"
	fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/checksums.txt" "$tmp/checksums.txt" \
		|| die "download failed: checksums.txt"
	want="$(grep " ${archive}\$" "$tmp/checksums.txt" | awk '{print $1}')"
	[ -n "$want" ] || die "no checksum for $archive"
	got="$(sha256_of "$tmp/$archive")"
	[ "$want" = "$got" ] || die "checksum mismatch for $archive (want $want got $got)"
	tar xzf "$tmp/$archive" -C "$tmp"
	install -m 0755 "$tmp/$name" "$dest/$name"
	rm -rf "$tmp"
	trap - EXIT
}

cli_prefix() {
	[ -n "$PIPER_PREFIX" ] && { echo "$PIPER_PREFIX"; return; }
	if [ "$(id -u)" -eq 0 ]; then echo /usr/local/bin; else echo "$HOME/.local/bin"; fi
}

install_cli() { # install_cli OS ARCH TAG
	prefix="$(cli_prefix)"
	mkdir -p "$prefix"
	download_verify piper "$3" "$1" "$2" "$prefix"
	echo "installed piper $3 -> $prefix/piper"
	case ":$PATH:" in
		*":$prefix:"*) ;;
		*) echo "note: $prefix is not on your PATH — add it to use piper" ;;
	esac
}

os="$(detect_os)"
arch="$(detect_arch)"
tag="$(resolve_version)"
if [ -n "$cli_only" ]; then
	install_cli "$os" "$arch" "$tag"
else
	die "full agent install not implemented yet"
fi
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./packaging/install/ -run TestCLIOnlyInstall -v`
Expected: PASS.

- [ ] **Step 5: Add the checksum-mismatch test**

Append to `packaging/install/install_test.go`:

```go
func TestChecksumMismatchAborts(t *testing.T) {
	osTok, archTok := hostOSArch()
	tag := "v9.9.9"
	archive := fmt.Sprintf("piper_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), osTok, archTok)
	assets := map[string][]byte{archive: tarGz(t, "piper", "real-bytes")}
	// checksums.txt claims a bogus hash for the archive.
	bogus := []byte(fmt.Sprintf("%064d  %s\n", 0, archive))
	srv := newReleaseServer(t, assets, bogus)

	prefix := t.TempDir()
	out, err := run(t, []string{"--cli-only", "--no-enable"}, map[string]string{
		"PIPER_BASE_URL": srv.URL,
		"PIPER_VERSION":  tag,
		"PIPER_PREFIX":   prefix,
	})
	if err == nil {
		t.Fatalf("expected non-zero exit on checksum mismatch, got success:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(prefix, "piper")); statErr == nil {
		t.Error("piper was installed despite checksum mismatch")
	}
	if !strings.Contains(out, "checksum mismatch") {
		t.Errorf("expected checksum error message, got:\n%s", out)
	}
}
```

- [ ] **Step 6: Run both tests**

Run: `go test ./packaging/install/ -v`
Expected: PASS (both `TestCLIOnlyInstall` and `TestChecksumMismatchAborts`).

- [ ] **Step 7: Commit**

```bash
chmod +x install.sh
git add install.sh packaging/install/install_test.go
git commit -m "$(cat <<'EOF'
feat(installer): checksum-verified CLI-only install

Part of #47. install.sh with OS/arch detection, checksum-verified
download+extract, and --cli-only mode (explicit PIPER_VERSION). Offline
exec harness runs the script against an httptest fake release.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Version resolution — default stable + `--rc`

Replace the stub `resolve_version` with real resolution: default reads
`releases/latest` (stable only) and errors clearly when none exists; `--rc`
reads `releases` and takes the newest tag (including pre-releases).

**Files:**
- Modify: `install.sh` (replace `resolve_version`)
- Test: `packaging/install/install_test.go` (add resolution tests)

**Interfaces:**
- Consumes: `fetch_stdout`, `PIPER_API_URL`, `PIPER_REPO`, `use_rc`,
  `PIPER_VERSION` (from Task 1).
- Produces: `resolve_version` → tag; honours `PIPER_VERSION` override first,
  then `--rc` (newest of `/releases`), else `/releases/latest`.

- [ ] **Step 1: Write the failing tests**

Append to `packaging/install/install_test.go`:

```go
// newAPIServer serves GitHub-shaped release JSON at /repos/{repo}/releases and
// /repos/{repo}/releases/latest. latestTag may be "" to simulate no stable
// release (404 on /latest). allTags lists newest-first for the /releases list.
func newAPIServer(t *testing.T, latestTag string, allTags []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			if latestTag == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"tag_name": %q}`, latestTag)
		case strings.HasSuffix(r.URL.Path, "/releases"):
			parts := make([]string, len(allTags))
			for i, tg := range allTags {
				parts[i] = fmt.Sprintf(`{"tag_name": %q, "prerelease": true}`, tg)
			}
			fmt.Fprintf(w, "[%s]", strings.Join(parts, ","))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveRCPicksNewestPrerelease(t *testing.T) {
	osTok, archTok := hostOSArch()
	tag := "v0.2.0-rc.1"
	archive := fmt.Sprintf("piper_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), osTok, archTok)
	dl := newReleaseServer(t, map[string][]byte{archive: tarGz(t, "piper", "x")}, nil)
	api := newAPIServer(t, "", []string{tag, "v0.1.0-rc.1"})

	prefix := t.TempDir()
	out, err := run(t, []string{"--cli-only", "--rc", "--no-enable"}, map[string]string{
		"PIPER_BASE_URL": dl.URL,
		"PIPER_API_URL":  api.URL,
		"PIPER_PREFIX":   prefix,
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "piper")); err != nil {
		t.Fatalf("piper (from %s) not installed: %v\n%s", tag, err, out)
	}
}

func TestDefaultNoStableReleaseErrors(t *testing.T) {
	api := newAPIServer(t, "", []string{"v0.1.0-rc.1"}) // no stable
	out, err := run(t, []string{"--cli-only", "--no-enable"}, map[string]string{
		"PIPER_BASE_URL": "http://127.0.0.1:0",
		"PIPER_API_URL":  api.URL,
		"PIPER_PREFIX":   t.TempDir(),
	})
	if err == nil {
		t.Fatalf("expected error when no stable release exists:\n%s", out)
	}
	if !strings.Contains(out, "--rc") {
		t.Errorf("expected message pointing to --rc, got:\n%s", out)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./packaging/install/ -run 'Resolve|NoStable' -v`
Expected: FAIL — stub `resolve_version` dies with "no version set" (no `--rc`
handling, wrong message).

- [ ] **Step 3: Replace `resolve_version`**

In `install.sh`, replace the stub function:

```sh
# resolve_version echoes the release tag to install (stub: explicit only).
resolve_version() {
	[ -n "$PIPER_VERSION" ] || die "no version set"
	echo "$PIPER_VERSION"
}
```

with:

```sh
# first_tag reads a GitHub releases JSON body on stdin and echoes the first
# tag_name. grep -o isolates each match (robust to pretty or compact JSON);
# head -n1 takes the newest (GitHub lists newest first).
first_tag() {
	grep -o '"tag_name": *"[^"]*"' | head -n1 | sed -E 's/.*"([^"]+)"$/\1/'
}

# resolve_version echoes the release tag to install.
resolve_version() {
	[ -n "$PIPER_VERSION" ] && { echo "$PIPER_VERSION"; return; }
	if [ -n "$use_rc" ]; then
		tag="$(fetch_stdout "$PIPER_API_URL/repos/$PIPER_REPO/releases" | first_tag)" || true
		[ -n "${tag:-}" ] || die "could not resolve latest pre-release from GitHub"
		echo "$tag"
	else
		tag="$(fetch_stdout "$PIPER_API_URL/repos/$PIPER_REPO/releases/latest" | first_tag)" || true
		[ -n "${tag:-}" ] || die "no stable release yet — re-run with --rc to install the latest pre-release"
		echo "$tag"
	fi
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./packaging/install/ -run 'Resolve|NoStable' -v`
Expected: PASS.

- [ ] **Step 5: Run the full package**

Run: `go test ./packaging/install/ -v`
Expected: PASS (all tests so far).

- [ ] **Step 6: Commit**

```bash
git add install.sh packaging/install/install_test.go
git commit -m "$(cat <<'EOF'
feat(installer): resolve latest stable, or --rc pre-release

Part of #46. Default resolves releases/latest (stable) and errors with
an --rc hint when none exists yet; --rc installs the newest release
including pre-releases. Only pre-releases exist today (v0.1.0-rc.x).

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Full agent install — binaries + systemd unit + env skeleton

Implement the default (non-`--cli-only`) path: install both binaries, drop the
systemd unit, create the env skeleton **only if absent**, and `daemon-reload` +
`enable --now` when systemd is present. The exec test runs on Linux only (it
exercises real file drops with dir overrides); it skips on macOS.

**Files:**
- Modify: `install.sh` (add `install_agent`, dispatch to it)
- Test: `packaging/install/install_test.go` (add Linux-gated agent test)

**Interfaces:**
- Consumes: `download_verify`, `fetch`, `have`, `PIPER_SYSTEMD_DIR`,
  `PIPER_ENV_DIR`, `PIPER_PREFIX`, `no_enable` (from Task 1).
- Produces: `install_agent OS ARCH TAG` — installs `piperd`+`piper`, drops
  `piperd.service` to `$PIPER_SYSTEMD_DIR`, writes `$PIPER_ENV_DIR/piperd.env`
  from `piperd.env.example` only when missing, then `systemctl daemon-reload` +
  `enable --now piperd` unless `--no-enable` or systemd absent.

- [ ] **Step 1: Write the failing test**

Append to `packaging/install/install_test.go`:

```go
func TestAgentInstallDropsUnitAndEnv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("agent install path targets Linux/systemd")
	}
	osTok, archTok := hostOSArch()
	tag := "v9.9.9"
	ver := strings.TrimPrefix(tag, "v")
	assets := map[string][]byte{
		fmt.Sprintf("piperd_%s_%s_%s.tar.gz", ver, osTok, archTok): tarGz(t, "piperd", "fake-piperd"),
		fmt.Sprintf("piper_%s_%s_%s.tar.gz", ver, osTok, archTok):  tarGz(t, "piper", "fake-piper"),
		"piperd.service":     []byte("[Service]\nExecStart=/usr/local/bin/piperd\n"),
		"piperd.env.example": []byte("#PIPER_API_ADDR=127.0.0.1:8088\n"),
	}
	srv := newReleaseServer(t, assets, nil)

	prefix := t.TempDir()
	unitDir := t.TempDir()
	envDir := t.TempDir()
	env := map[string]string{
		"PIPER_BASE_URL":    srv.URL,
		"PIPER_VERSION":     tag,
		"PIPER_PREFIX":      prefix,
		"PIPER_SYSTEMD_DIR": unitDir,
		"PIPER_ENV_DIR":     envDir,
	}
	out, err := run(t, []string{"--no-enable"}, env)
	if err != nil {
		t.Fatalf("agent install failed: %v\n%s", err, out)
	}
	for _, p := range []string{
		filepath.Join(prefix, "piperd"),
		filepath.Join(prefix, "piper"),
		filepath.Join(unitDir, "piperd.service"),
		filepath.Join(envDir, "piperd.env"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v\n%s", p, err, out)
		}
	}

	// Re-run must not clobber an operator-edited env file.
	edited := "PIPER_BASE_DOMAIN=example.com\n"
	if err := os.WriteFile(filepath.Join(envDir, "piperd.env"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, []string{"--no-enable"}, env); err != nil {
		t.Fatalf("re-run failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(envDir, "piperd.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != edited {
		t.Errorf("env file was clobbered on re-run: got %q", string(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./packaging/install/ -run TestAgentInstall -v`
Expected: FAIL on Linux — script dies "full agent install not implemented yet".
(On macOS it SKIPs; run on a Linux CI/box to see the failure.)

- [ ] **Step 3: Implement `install_agent` and dispatch**

In `install.sh`, add this function after `install_cli`:

```sh
install_agent() { # install_agent OS ARCH TAG
	os="$1"; arch="$2"; tag="$3"
	[ "$os" = linux ] || die "the full agent install needs Linux + systemd; on macOS use --cli-only (launchd support tracked in #56)"
	prefix="${PIPER_PREFIX:-/usr/local/bin}"
	if [ -z "$PIPER_PREFIX" ] && [ "$(id -u)" -ne 0 ]; then
		die "the full agent install needs root — re-run with sudo, or use --cli-only"
	fi
	mkdir -p "$prefix"
	download_verify piperd "$tag" "$os" "$arch" "$prefix"
	download_verify piper "$tag" "$os" "$arch" "$prefix"

	mkdir -p "$PIPER_SYSTEMD_DIR"
	fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/piperd.service" \
		"$PIPER_SYSTEMD_DIR/piperd.service" || die "download failed: piperd.service"

	mkdir -p "$PIPER_ENV_DIR"
	chmod 0700 "$PIPER_ENV_DIR" 2>/dev/null || true
	if [ ! -f "$PIPER_ENV_DIR/piperd.env" ]; then
		fetch "$PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/piperd.env.example" \
			"$PIPER_ENV_DIR/piperd.env" || die "download failed: piperd.env.example"
		chmod 0600 "$PIPER_ENV_DIR/piperd.env" 2>/dev/null || true
	fi
	echo "installed piperd + piper $tag -> $prefix"

	if [ -z "$no_enable" ] && have systemctl; then
		systemctl daemon-reload
		systemctl enable --now piperd
		echo "piperd service enabled and started"
	else
		echo "note: service not enabled (no systemctl or --no-enable); start with: systemctl enable --now piperd"
	fi
}
```

Then replace the dispatch tail:

```sh
if [ -n "$cli_only" ]; then
	install_cli "$os" "$arch" "$tag"
else
	die "full agent install not implemented yet"
fi
```

with:

```sh
if [ -n "$cli_only" ]; then
	install_cli "$os" "$arch" "$tag"
else
	install_agent "$os" "$arch" "$tag"
fi
```

- [ ] **Step 4: Run test to verify it passes**

Run (on Linux): `go test ./packaging/install/ -run TestAgentInstall -v`
Expected: PASS. On macOS: SKIP (expected — verified on CI/Linux).

- [ ] **Step 5: Run the full package**

Run: `go test ./packaging/install/ -v`
Expected: PASS (agent test PASS on Linux / SKIP on macOS; all others PASS).

- [ ] **Step 6: Commit**

```bash
git add install.sh packaging/install/install_test.go
git commit -m "$(cat <<'EOF'
feat(installer): full agent install (binaries + unit + env)

Closes #46. Default mode installs piperd+piper, drops the systemd unit,
writes the env skeleton only when absent (never clobbers an edited env),
and daemon-reload + enable --now when systemd is present. Linux/root
only; macOS is directed to --cli-only (launchd is #56).

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Docs, release wiring, and progress

Add the README **Install** section (with a documentation test), attach
`install.sh` to releases, and flip `PROGRESS.md`.

**Files:**
- Modify: `README.md`
- Modify: `.goreleaser.yaml`
- Modify: `PROGRESS.md`
- Test: `packaging/install/install_test.go` (documentation test)

**Interfaces:**
- Consumes: `scriptPath`/repo-root walk (from Task 1).
- Produces: `TestInstallDocumentation` — asserts README teaches the curl
  one-liner and `PIPER_ADDR`.

- [ ] **Step 1: Write the failing documentation test**

Append to `packaging/install/install_test.go`:

```go
func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(scriptPath(t))
}

func TestInstallDocumentation(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	for _, want := range []string{
		"raw.githubusercontent.com/piperbox/piper/main/install.sh",
		"--cli-only",
		"--rc",
		"PIPER_ADDR",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README missing %q", want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./packaging/install/ -run TestInstallDocumentation -v`
Expected: FAIL — README has none of these strings yet.

- [ ] **Step 3: Add the README Install section**

In `README.md`, insert this section immediately **before** the existing
`## Run the agent as a service` heading:

````markdown
## Install

One line gets a Linux box to a running `piperd` service:

```bash
curl -fsSL https://raw.githubusercontent.com/piperbox/piper/main/install.sh | sh
```

It detects your OS/arch, downloads the matching release binaries, verifies their
`checksums.txt`, installs `piperd` + `piper` to `/usr/local/bin`, drops the
systemd unit and an `/etc/piper/piperd.env` skeleton (never overwriting an edited
one), and runs `systemctl enable --now piperd`. Re-run any time to upgrade.

Install just the CLI (Linux or macOS) — for driving a remote daemon from your
workstation:

```bash
curl -fsSL https://raw.githubusercontent.com/piperbox/piper/main/install.sh | sh -s -- --cli-only
```

As root this installs `piper` to `/usr/local/bin`; unprivileged, to
`~/.local/bin`. Point it at your box with `PIPER_ADDR`:

```bash
PIPER_ADDR=http://your-box:8088 piper list
```

Only pre-release builds exist for now, so add `--rc` to install the latest
release candidate:

```bash
curl -fsSL https://raw.githubusercontent.com/piperbox/piper/main/install.sh | sh -s -- --rc
```

The full service install is Linux + systemd; on macOS use `--cli-only` (a
launchd unit is tracked in [#56](https://github.com/piperbox/piper/issues/56)).
Shell completions and a Homebrew tap are planned follow-ups. Prefer to build from
source, or wire your own automation? The manual steps below still work.
````

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./packaging/install/ -run TestInstallDocumentation -v`
Expected: PASS.

- [ ] **Step 5: Attach `install.sh` to releases**

In `.goreleaser.yaml`, under `release.extra_files`, add the script alongside the
existing entries:

```yaml
  extra_files:
    - glob: packaging/systemd/piperd.service
    - glob: packaging/systemd/piper-relay.service
    - glob: packaging/systemd/piperd.env.example
    - glob: install.sh
```

- [ ] **Step 6: Update PROGRESS.md**

In `PROGRESS.md`, under the `## Install & run piperd as a service` section,
replace these two lines:

```markdown
- ⬜ One-line `curl … | sh` installer (blocked on Release artifacts) — [#46](https://github.com/piperbox/piper/issues/46)
- ⬜ Standalone `piper` CLI on PATH — [#47](https://github.com/piperbox/piper/issues/47)
```

with:

```markdown
- ✅ One-line `curl … | sh` installer (OS/arch detect, checksum-verified, `--cli-only`/`--rc`) — [#46](https://github.com/piperbox/piper/issues/46)
- ✅ Standalone `piper` CLI on PATH (`--cli-only`; drives a remote daemon via `PIPER_ADDR`) — [#47](https://github.com/piperbox/piper/issues/47)
```

- [ ] **Step 7: Verify everything green**

Run: `make test && make cross`
Expected: all pass, including `./packaging/install/`. If `shellcheck` is
installed, also run `shellcheck install.sh` and expect no findings.

- [ ] **Step 8: Commit**

```bash
git add README.md .goreleaser.yaml PROGRESS.md packaging/install/install_test.go
git commit -m "$(cat <<'EOF'
docs(installer): README install section + release wiring

Closes #46, closes #47. README Install section (full / --cli-only / --rc,
PIPER_ADDR remote guidance) with a documentation test; attach install.sh
to releases; flip PROGRESS.md.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Real smoke test + PR

Verify the live path against the real `v0.1.0-rc.1` release, then open the PR.

- [ ] **Step 1: Smoke-test the CLI install against the real release**

Run:
```bash
mkdir -p /tmp/piper-smoke
PIPER_PREFIX=/tmp/piper-smoke sh install.sh --cli-only --rc --no-enable
/tmp/piper-smoke/piper version
```
Expected: downloads `piper_0.1.0-rc.1_<os>_<arch>.tar.gz`, verifies checksum,
and `piper version` prints a version. Clean up: `rm -rf /tmp/piper-smoke`.

- [ ] **Step 2: Push and open the PR**

```bash
git push -u origin ozykhan/installer-cli-on-path
gh pr create --base main --title "[repo] One-line installer + piper CLI on PATH" --body "$(cat <<'EOF'
One `install.sh`: full agent install (Linux+systemd) and `--cli-only`; `--rc`
installs the latest pre-release. Checksum-verified downloads from GitHub
Releases; idempotent; never clobbers an edited env file. Tested by an offline
exec harness (httptest fake release) — `make test` stays network-free.

Part of #43. Closes #46. Closes #47.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 3: Confirm CI is green** on the PR before requesting review.

---

## Notes for the implementer

- **POSIX `sh`, not bash.** No arrays, no `[[ ]]`, no `local`. Test with `sh`
  (dash on Linux) as the harness does. Run `shellcheck` if available.
- **`set -eu`** is on: guard optional expansions (`${x:-}`) and don't rely on
  unset vars.
- **The agent-install exec test only runs on Linux.** On the macOS dev box it
  skips; CI (Linux) gives the real coverage. That's expected, not a gap.
- **No network in `make test`** — every download is served by the in-process
  `httptest` server. The only real-network step is the manual smoke test (Task 5).
