# Release Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `v*` git tag produces a GitHub Release with per-binary, cross-compiled archives (+ `checksums.txt`) for `piperd`, `piper`, and `piper-relay`, version stamped in.

**Architecture:** A `.goreleaser.yaml` (config v2) defines three pure-Go builds and three per-binary archives; a `release.yml` workflow runs goreleaser on tag push; `ci.yml` gains a `goreleaser check` lint on PRs. No release is cut by merging — that is a later, deliberate tag push.

**Tech Stack:** goreleaser v2 (OSS distribution), GitHub Actions, Go 1.26.

## Global Constraints

- **No cgo.** Every build sets `CGO_ENABLED=0` (pure-Go SQLite via `modernc.org/sqlite`).
- **Module path:** `github.com/getpiper/piper`.
- **Version var:** stamp `github.com/getpiper/piper/internal/version.value` (NOT goreleaser's default `main.version`).
- **Targets:** linux/{amd64, arm64, armv7}, darwin/{amd64, arm64}. No Windows.
- **Strip flags:** carry `-s -w` (matches the Makefile release build).
- **Checksum filename:** exactly `checksums.txt` (#46's installer expects it).
- **Conventional commits**, ending with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`. Work is on branch `ozykhan/release-pipeline` (already created).

---

## File Structure

- Create: `.goreleaser.yaml` — build/archive/checksum/release config (Task 1).
- Create: `.github/workflows/release.yml` — tag-triggered release job (Task 2).
- Modify: `.github/workflows/ci.yml` — add `goreleaser check` step + paths-filter entry (Task 3).
- Modify: `PROGRESS.md` — flip the #58 line to built (Task 3).

---

### Task 1: goreleaser config + local verification

**Files:**
- Create: `.goreleaser.yaml`

**Interfaces:**
- Consumes: `cmd/piperd`, `cmd/piper`, `cmd/piper-relay` (existing mains); the version var `github.com/getpiper/piper/internal/version.value`.
- Produces: `dist/` snapshot artifacts — per-binary `.tar.gz` archives named `<binary>_<version>_<os>_<arch>[vN].tar.gz` and `checksums.txt`. Task 2 and Task 3 invoke the same config via `goreleaser release` / `goreleaser check`.

- [ ] **Step 1: Ensure goreleaser is installed, then check with no config (verify it fails)**

Install if missing (macOS): `brew install goreleaser`. Then:

Run: `goreleaser check`
Expected: FAIL — `configuration file not found` (no `.goreleaser.yaml` yet).

- [ ] **Step 2: Write `.goreleaser.yaml`**

```yaml
version: 2

project_name: piper

builds:
  - id: piperd
    main: ./cmd/piperd
    binary: piperd
    env:
      - CGO_ENABLED=0
    goos: [linux, darwin]
    goarch: [amd64, arm64, arm]
    goarm: ["7"]
    ignore:
      - goos: darwin
        goarch: arm
    ldflags:
      - -s -w -X github.com/getpiper/piper/internal/version.value={{ .Version }}
    mod_timestamp: "{{ .CommitTimestamp }}"

  - id: piper
    main: ./cmd/piper
    binary: piper
    env:
      - CGO_ENABLED=0
    goos: [linux, darwin]
    goarch: [amd64, arm64, arm]
    goarm: ["7"]
    ignore:
      - goos: darwin
        goarch: arm
    ldflags:
      - -s -w -X github.com/getpiper/piper/internal/version.value={{ .Version }}
    mod_timestamp: "{{ .CommitTimestamp }}"

  - id: piper-relay
    main: ./cmd/piper-relay
    binary: piper-relay
    env:
      - CGO_ENABLED=0
    goos: [linux, darwin]
    goarch: [amd64, arm64, arm]
    goarm: ["7"]
    ignore:
      - goos: darwin
        goarch: arm
    ldflags:
      - -s -w -X github.com/getpiper/piper/internal/version.value={{ .Version }}
    mod_timestamp: "{{ .CommitTimestamp }}"

archives:
  - id: piperd
    ids: [piperd]
    name_template: "piperd_{{ .Version }}_{{ .Os }}_{{ .Arch }}{{ if .Arm }}v{{ .Arm }}{{ end }}"
  - id: piper
    ids: [piper]
    name_template: "piper_{{ .Version }}_{{ .Os }}_{{ .Arch }}{{ if .Arm }}v{{ .Arm }}{{ end }}"
  - id: piper-relay
    ids: [piper-relay]
    name_template: "piper-relay_{{ .Version }}_{{ .Os }}_{{ .Arch }}{{ if .Arm }}v{{ .Arm }}{{ end }}"

checksum:
  name_template: "checksums.txt"
  algorithm: sha256

snapshot:
  version_template: "{{ incpatch .Version }}-next"

release:
  extra_files:
    - glob: packaging/systemd/piperd.service
    - glob: packaging/systemd/piper-relay.service
    - glob: packaging/systemd/piperd.env.example
```

Note: v2 filters builds into an archive via `ids`. If `goreleaser check` reports `ids` unknown/deprecated for your goreleaser version, the field is `builds` — switch the three `ids:` keys to `builds:` and re-run. Do not change anything else.

- [ ] **Step 3: Check the config (verify it passes)**

Run: `goreleaser check`
Expected: PASS — `1 configuration file(s) validated`. No deprecation warnings.

- [ ] **Step 4: Snapshot build (verify artifacts + version stamping)**

Run: `goreleaser release --snapshot --clean`
Expected: PASS. Then:

Run: `ls dist/*.tar.gz dist/checksums.txt`
Expected: 15 archives (3 binaries × 5 targets) named e.g. `piperd_..._linux_armv7.tar.gz`, `piper_..._darwin_arm64.tar.gz`, plus `checksums.txt`.

Run: `./dist/piper_darwin_arm64/piper version` (adjust dir to your host os/arch; the binary dir under `dist/` is `<id>_<os>_<arch>`)
Expected: prints a version like `0.0.1-next` — NOT `0.0.0-dev` (confirms ldflags stamped the var).

- [ ] **Step 5: Confirm `make cross` and `make test` still green**

Run: `make cross && make test`
Expected: both pass (config-only change must not affect them).

- [ ] **Step 6: Commit**

```bash
git add .goreleaser.yaml
git commit -m "feat(repo): goreleaser config for per-binary release archives (#58)

Part of #58

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Release workflow (tag-triggered)

**Files:**
- Create: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `.goreleaser.yaml` from Task 1.
- Produces: a GitHub Release on `v*` tag push. No downstream task depends on this file.

- [ ] **Step 1: Write `.github/workflows/release.yml`**

```yaml
name: Release

on:
  push:
    tags:
      - 'v*'

permissions:
  contents: write

jobs:
  goreleaser:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"

      - name: Release
        uses: goreleaser/goreleaser-action@v6
        with:
          distribution: goreleaser
          version: "~> v2"
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

- [ ] **Step 2: Verify the workflow is valid YAML / well-formed**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/release.yml')); print('ok')"`
Expected: `ok`.

(There is no way to fully exercise this without pushing a tag; the release itself is a later deliberate step. The config it drives is already proven by Task 1's snapshot build.)

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "feat(repo): release workflow — v* tag to GitHub Release (#58)

Part of #58

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: CI config lint + PROGRESS update

**Files:**
- Modify: `.github/workflows/ci.yml`
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: `.goreleaser.yaml` from Task 1.
- Produces: nothing downstream.

- [ ] **Step 1: Add `.goreleaser.yaml` to the paths-filter**

In `.github/workflows/ci.yml`, the `filters:` block under the "Detect code changes" step currently lists `code:` globs. Add the config file so edits to it trigger the lint. Change:

```yaml
            code:
              - '**.go'
              - 'go.mod'
              - 'go.sum'
              - 'Makefile'
              - '.github/workflows/ci.yml'
```

to:

```yaml
            code:
              - '**.go'
              - 'go.mod'
              - 'go.sum'
              - 'Makefile'
              - '.github/workflows/ci.yml'
              - '.goreleaser.yaml'
```

- [ ] **Step 2: Add the `goreleaser check` step**

Append this step to the end of the `verify` job's `steps:` in `.github/workflows/ci.yml` (after "cross-compile"):

```yaml
      - name: goreleaser check
        if: steps.changes.outputs.code == 'true'
        uses: goreleaser/goreleaser-action@v6
        with:
          distribution: goreleaser
          version: "~> v2"
          args: check
```

- [ ] **Step 3: Verify ci.yml is valid YAML**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml')); print('ok')"`
Expected: `ok`.

- [ ] **Step 4: Flip the PROGRESS.md line**

In `PROGRESS.md`, change the #58 line from stubbed to built. Change:

```
- ⬜ Release pipeline (goreleaser: tag → GitHub Release + cross-compiled binaries/checksums); unblocks installer/image — [#58](https://github.com/getpiper/piper/issues/58)
```

to:

```
- ✅ Release pipeline (goreleaser: tag → GitHub Release + cross-compiled binaries/checksums); unblocks installer/image — [#58](https://github.com/getpiper/piper/issues/58)
```

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/ci.yml PROGRESS.md
git commit -m "ci(repo): lint goreleaser config on PRs; mark #58 built

Part of #58

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Final: open the PR

After all tasks pass, push and open the PR into `main`:

```bash
git push -u origin ozykhan/release-pipeline
gh pr create --base main --title "[repo] Release pipeline: goreleaser tag → GitHub Release" --body "$(cat <<'EOF'
Wires up the goreleaser release pipeline: a `v*` tag produces a GitHub Release with per-binary cross-compiled archives (linux/{amd64,arm64,armv7}, darwin/{amd64,arm64}) + `checksums.txt`, version stamped into the binaries, and the systemd units + env.example attached as release assets.

Merging does not cut a release — the first release is a later, deliberate `v0.1.0` tag push.

Closes #58

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Verify the checksum in the PROGRESS/spec matches: `make test` and `make cross` green, `goreleaser check` green, snapshot build produced the expected 15 archives + `checksums.txt`.
