# Release pipeline — goreleaser tag → GitHub Release

**Issue:** [#58](https://github.com/piperbox/piper/issues/58) `[repo]`
**Date:** 2026-07-06
**Status:** Approved design

## Goal

A semver git tag (`v0.1.0`) produces a **GitHub Release** carrying cross-compiled,
per-binary archives for all three commands plus a `checksums.txt`, with the version
stamped into the binaries. This is the artifact path the install epic (#43) consumes
but does not build; it unblocks #45, #46, and the install-from-release half of #47.

Non-goals (own follow-ups): Homebrew tap, published container image (#45), and
artifact signing beyond checksums (cosign/minisign) — noted as a possible later
addition, not required for a first `0.x` release.

## What exists today

- Three commands, all pure-Go (`CGO_ENABLED=0`): `cmd/piperd`, `cmd/piper`,
  `cmd/piper-relay`.
- Version is a single overridable var: `github.com/piperbox/piper/internal/version.value`
  (default `"0.0.0-dev"`), surfaced by `piper version`. goreleaser's **default**
  ldflags target `main.version`, which this repo does not use — so the config must
  set custom ldflags pointing at this var.
- `Makefile` release build already strips symbols with `-s -w`.
- Packaging assets to ship (from #57/#44): `packaging/systemd/piperd.service`,
  `packaging/systemd/piper-relay.service`, `packaging/systemd/piperd.env.example`.
- CI is a single `verify` job in `.github/workflows/ci.yml`, gated by a
  `dorny/paths-filter` `code` filter. No release workflow yet.

## Decisions

- **Per-binary archives.** The three binaries deploy to three different hosts
  (`piperd` on the Pi, `piper-relay` on the cloud relay, `piper` CLI on a dev
  laptop — #47 wants it standalone on PATH). One archive per binary lets the
  installer download only what a host needs.
- **`goreleaser check` on PRs.** A cheap config lint catches a broken
  `.goreleaser.yaml` before a tag push turns it into a failed release. `make cross`
  remains the local pre-push proof; the release workflow is the published-artifact path.
- **Uniform target matrix across all three binaries.** Simpler than per-binary
  target lists; building `piper-relay` for darwin is harmless.

## Design

### `.goreleaser.yaml` (config `version: 2`, distribution `goreleaser` / OSS)

**`builds`** — three entries, one per command, otherwise identical:

- `id` / `binary`: `piperd`, `piper`, `piper-relay`; `main`: `./cmd/<name>`.
- `env: [CGO_ENABLED=0]` — enforces the no-cgo / pure-Go SQLite hard constraint.
- `goos: [linux, darwin]`, `goarch: [amd64, arm64, arm]`, `goarm: ["7"]`.
- `ignore: [{goos: darwin, goarch: arm}]` — drops macOS armv7.
  Net targets per binary: linux/{amd64, arm64, armv7}, darwin/{amd64, arm64}.
- `ldflags: -s -w -X github.com/piperbox/piper/internal/version.value={{.Version}}`
  — carries the strip flags and stamps the real version var.
- `mod_timestamp: "{{.CommitTimestamp}}"` — reproducible builds.

**`archives`** — three entries, each filtered to one build via `ids: [<build id>]`,
with a per-binary `name_template`:
`<name>_{{.Version}}_{{.Os}}_{{.Arch}}{{if .Arm}}v{{.Arm}}{{end}}` → `.tar.gz`
(e.g. `piperd_0.1.0_linux_arm64.tar.gz`, `piper_0.1.0_linux_armv7.tar.gz`).
(v2 uses `ids` to filter builds into an archive; confirm with `goreleaser check`.)

**`checksum`** — `name_template: "checksums.txt"` (default is
`{{.ProjectName}}_{{.Version}}_checksums.txt`); the fixed name is what #46's
installer expects.

**`release.extra_files`** — attaches the three packaging files as standalone
release assets so the installer can fetch them alongside the binaries:
`packaging/systemd/piperd.service`, `packaging/systemd/piper-relay.service`,
`packaging/systemd/piperd.env.example`.

**`snapshot`** — a `version_template` so `goreleaser release --snapshot --clean`
runs locally / in CI without a tag.

### `.github/workflows/release.yml`

- Trigger: `push: { tags: ['v*'] }`.
- `permissions: { contents: write }` (needed to create the Release).
- Checkout with `fetch-depth: 0` (full history + tags so goreleaser derives the version).
- `actions/setup-go@v5` with `go-version: "1.26"`.
- `goreleaser/goreleaser-action@v6` — `distribution: goreleaser`, `version: latest`,
  `args: release --clean`, env `GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}`.

### `ci.yml` change

- Add `.goreleaser.yaml` to the `code` paths-filter so config edits are validated.
- Add a `goreleaser check` step (via `goreleaser/goreleaser-action@v6` with
  `args: check`), gated on `steps.changes.outputs.code == 'true'` like the other steps.

## Verification

- `goreleaser check` passes.
- `goreleaser release --snapshot --clean` produces, under `dist/`:
  - per-binary `.tar.gz` for each of the 5 targets × 3 binaries, and
  - `checksums.txt`.
- A stamped binary reports the version: build via goreleaser snapshot, run
  `piper version`, confirm it is not `0.0.0-dev`.
- `make test` and `make cross` remain green.

## Rollout

Merging this does not cut a release. The first real release is a separate,
deliberate step: push a `v0.1.0` tag once #58's config is on `main`, which the
new workflow turns into the first GitHub Release.
