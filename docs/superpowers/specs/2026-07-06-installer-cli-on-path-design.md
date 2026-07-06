# One-line installer + CLI-on-PATH — design

Closes [#46](https://github.com/getpiper/piper/issues/46) (one-line installer) and
[#47](https://github.com/getpiper/piper/issues/47) (standalone `piper` CLI on PATH),
both under the "install & run piperd as a service" epic
([#43](https://github.com/getpiper/piper/issues/43)).

## Goal

Get a box from zero to a running `piperd` service — or a workstation to a `piper`
CLI on `PATH` — with a single `curl … | sh`. One script covers both, because they
share OS/arch detection, artifact download, and checksum verification.

```
# full agent install (Linux, root): binaries + systemd unit + env skeleton
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh

# CLI only (Linux or macOS): just the `piper` client on PATH
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh -s -- --cli-only

# bleeding edge, including pre-releases (release candidates)
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh -s -- --rc
```

## Context (what already exists)

- **Release artifacts** (goreleaser, `.goreleaser.yaml`): per-binary `tar.gz`
  archives named `piper_{version}_{os}_{arch}[v{arm}].tar.gz` and
  `piperd_{version}_{os}_{arch}[v{arm}].tar.gz`, a `checksums.txt` (sha256), and
  the systemd unit + env skeleton attached as loose assets
  (`piperd.service`, `piperd.env.example`). The `version` in a filename strips the
  `v` tag prefix (tag `v0.1.0-rc.1` → `piper_0.1.0-rc.1_darwin_arm64.tar.gz`).
  Arches are `amd64`, `arm64`, `armv7`.
- **Only pre-releases exist so far** (`v0.1.0-rc.1`). The GitHub
  `releases/latest` endpoint returns only non-pre-releases, so a default install
  has nothing to fetch yet — this is exactly why `--rc` exists.
- **CLI → daemon** address is the `PIPER_ADDR` env var (default
  `http://127.0.0.1:8088`, `internal/client/client.go`). The standalone CLI drives
  a remote daemon by setting `PIPER_ADDR`.
- **Manual install today** (README "Run the agent as a service"): `install -m 0755
  bin/piperd /usr/local/bin/piperd`, drop `piperd.service`, create `/etc/piper`,
  drop `piperd.env`, `systemctl daemon-reload`, `systemctl enable --now piperd`.
  The installer automates this exact flow.
- **Packaging test pattern** (`packaging/systemd/piperd_test.go`): a *contract
  test* greps the shipped artifact for required content, and a *documentation
  test* greps README/runbook. We follow the same idiom, plus an offline execution
  harness for the script.

## Script: `install.sh` (repo root)

A single POSIX `sh` script at the repository root, so the canonical URL is short
(`raw.githubusercontent.com/getpiper/piper/main/install.sh`). Also attach it to
releases via goreleaser `extra_files` so a release page links a stable copy.

### Modes

| Invocation | Behaviour |
| --- | --- |
| default (Linux + root) | full agent install |
| `--cli-only` | install only the `piper` client on `PATH` |
| default on macOS | error: full service install unsupported (no launchd yet, #56) → suggest `--cli-only` |
| default as non-root | error: full install needs root → suggest `sudo` or `--cli-only` |

**Full agent install** (Linux, root):
1. Resolve version (below), detect OS/arch.
2. Download + verify `piperd` and `piper` archives; `install -m 0755` both to
   `/usr/local/bin`.
3. Download `piperd.service` → `/etc/systemd/system/piperd.service`.
4. `install -d -m 0700 /etc/piper`; download `piperd.env.example` →
   `/etc/piper/piperd.env` **only if it does not already exist** (never clobber an
   operator-edited env).
5. `systemctl daemon-reload`; unless `--no-enable`, `systemctl enable --now piperd`.

**CLI-only install** (Linux or macOS):
1. Resolve version, detect OS/arch.
2. Download + verify the `piper` archive.
3. Install to `/usr/local/bin` (root) or `~/.local/bin` (non-root); if the target
   dir is not on `PATH`, print a one-line hint to add it.

Both modes are **idempotent / safe to re-run** — a re-run upgrades binaries in
place, re-drops the unit, and `enable --now` / `daemon-reload` are no-ops when
already applied. The env file is preserved across re-runs.

### Version resolution

- **Explicit** — `PIPER_VERSION=v0.1.0` (or `--version v0.1.0`) skips resolution.
- **Default** — latest **stable** via `GET {api}/repos/{repo}/releases/latest`.
  When none exists (today), fail with: *"no stable release yet — re-run with
  --rc to install the latest pre-release."*
- **`--rc`** (or `PIPER_RC=1`) — newest release **including pre-releases**: the
  first `tag_name` in `GET {api}/repos/{repo}/releases` (GitHub returns newest
  first). Resolves `v0.1.0-rc.1` today. Parsed with `grep`/`sed` (no `jq`
  dependency).

### OS / arch detection

- OS: `uname -s` → `Linux`→`linux`, `Darwin`→`darwin`. Anything else → error.
- Arch: `uname -m` → `x86_64|amd64`→`amd64`, `aarch64|arm64`→`arm64`,
  `armv7l|armv7`→`armv7`. Anything else → error naming the unsupported arch.

### Download → verify

- Fetch `{base}/{repo}/releases/download/{tag}/{archive}` and the sibling
  `checksums.txt` with `curl -fsSL` (fall back to `wget` if `curl` is absent).
- Compute sha256 with `sha256sum` (Linux) or `shasum -a 256` (macOS); compare
  against the `checksums.txt` line for the archive. Mismatch → abort and remove
  the temp dir.
- Extract with `tar xzf` into a temp dir; move the binary into place with
  `install`.

### Testability seams (env overrides)

| Var | Default | Purpose |
| --- | --- | --- |
| `PIPER_REPO` | `getpiper/piper` | owner/name for URLs |
| `PIPER_BASE_URL` | `https://github.com` | download host (release assets) |
| `PIPER_API_URL` | `https://api.github.com` | release-resolution host |
| `PIPER_VERSION` | *(resolved)* | explicit tag, skips resolution |
| `PIPER_PREFIX` | *(computed)* | install directory override |
| `PIPER_CLI_ONLY` | unset | same as `--cli-only` |
| `PIPER_RC` | unset | same as `--rc` |
| `--no-enable` | — | skip `systemctl enable --now` (tests / non-systemd) |

Flags override env; env overrides defaults.

### Error handling

- Unsupported OS or arch → message naming the value; exit non-zero.
- Full install on macOS / non-root → message with the `--cli-only` (or `sudo`)
  remedy.
- Download 404 / network failure → message naming the failed URL.
- Checksum mismatch → abort, clean temp, exit non-zero.
- No stable release and no `--rc` → the `--rc` remedy message above.

## Testing — `packaging/install/install_test.go`

Package `install`; locates the repo-root `install.sh` by walking up to `go.mod`
(same helper shape as `packaging/systemd`'s `repositoryFile`).

1. **Offline execution harness.** A Go `httptest.Server` serves a fake release:
   a minimal fake `piper` executable inside a real `.tar.gz`, a matching
   `checksums.txt`, and `releases` / `releases/latest` JSON. The test runs
   `sh install.sh --cli-only --no-enable` with `PIPER_BASE_URL`, `PIPER_API_URL`,
   and `PIPER_PREFIX` pointed at the server + a temp dir, then asserts the `piper`
   binary landed and is executable. Skips cleanly if `sh` or `tar` is unavailable.
2. **Checksum failure.** Same harness with a corrupted `checksums.txt` → the
   script must exit non-zero and install nothing.
3. **`--rc` resolution.** Fake `releases` JSON whose newest entry is a
   pre-release → the script resolves and downloads that tag.
4. **Documentation test.** README contains the `curl … | install.sh` one-liner
   and the `PIPER_ADDR` CLI-on-PATH guidance.

`make test` stays hermetic (no network). Separately, a one-time manual smoke test
runs `sh install.sh --cli-only --rc` against the real `v0.1.0-rc.1` release on
macOS/arm64 to confirm the live path end-to-end (not committed).

## Docs

- **README** gains an **Install** section: the three `curl … | sh` one-liners
  (full, `--cli-only`, `--rc`), a note that the full service install is Linux +
  systemd, and CLI-on-PATH guidance (drive a remote daemon with
  `PIPER_ADDR=http://host:8088`). The existing manual `install -m 0755 …` block
  stays as the "from source / no-curl" fallback.
- Shell **completions** (bash/zsh/fish) and a **Homebrew tap** are explicitly out
  of scope (per #47), noted as follow-ups.

## Out of scope

- launchd support in the installer (macOS full service) — blocked on #56.
- Container/compose install path — #45.
- Signature (cosign/GPG) verification beyond sha256 checksums — checksums.txt is
  the current release guarantee; signing is a later hardening step.

## Verification

- `make test` — the new `packaging/install` tests pass; existing suites unchanged.
- `make cross` — unaffected (no Go build surface changes) but run to stay honest.
- `shellcheck install.sh` — clean.
- Manual: `sh install.sh --cli-only --rc` on macOS/arm64 installs a working
  `piper` from `v0.1.0-rc.1`.
