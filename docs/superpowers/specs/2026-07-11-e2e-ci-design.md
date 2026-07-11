# e2e suite in CI — design

**Issue:** [#128](https://github.com/getpiper/piper/issues/128)
**Date:** 2026-07-11

## Problem

The e2e suite (`test/e2e`) skips unless `RUN_E2E=1`, and CI never sets it. The
tests that exercise the real deploy path (Docker build → run → health → Caddy
route → fetch by hostname) have no environment where they are known to pass;
they only run when a developer remembers to run `make e2e` locally, where
stray port squatters break them (#126, #127).

## Decision

Run the e2e suite on **every code-touching PR and every push to `main`**, as a
**separate, non-required** workflow. Fresh CI runners have `:80/:2019/:8088`
free, which sidesteps the local port-squatter flakiness class entirely.

## Workflow: `.github/workflows/e2e.yml`

A new workflow file, not a job in `ci.yml`:

- `ci.yml`'s `verify` job is a **required** status check, so it must always
  run and report — that forces the dorny/paths-filter dance inside the job.
  The e2e check is non-required, so a docs-only PR that skips the whole
  workflow is fine, and native `paths:` trigger filtering is simpler.
- Triggers: `pull_request` and `push: branches: [main]`, both filtered to
  `**.go`, `go.mod`, `go.sum`, `Makefile`, `test/e2e/**` (covers
  `sampleapp/`'s non-Go files), and `.github/workflows/e2e.yml` itself.
- One `e2e` job on `ubuntu-latest` (Docker preinstalled),
  `timeout-minutes: 15` so a hung health-check loop can't burn the 6-hour
  default:
  1. checkout + setup-go 1.26 (matching `verify`)
  2. `sudo sysctl -w net.ipv4.ip_unprivileged_port_start=80` — the runner
     user is non-root and the deploy test binds `:80`
  3. `make e2e`
- **Not** added to branch protection. Visible on every PR; doesn't block
  merge. `make verify` stays fast and Docker-free.

## Docs

One bullet in README's *Contributing* section, next to the existing `verify`
line: e2e runs with `make e2e`, needs Docker and free `:80/:2019/:8088`
(relay tests also use `:8443/:7000`), and CI runs it on every code-touching
PR.

## Verification

Self-verifying: the PR that adds the workflow touches the workflow file and
Makefile-adjacent paths, so the e2e job runs on that PR — a green job is the
acceptance criterion. If runner timing or port binding surprises appear, they
show up on the PR and get fixed there.
