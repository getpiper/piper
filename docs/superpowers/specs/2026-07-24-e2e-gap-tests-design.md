# e2e gap tests: deploy failure resilience + webhook/PR-preview loop

**Date:** 2026-07-24
**Status:** approved

## Problem

The e2e suite (`test/e2e/`) covers the golden paths of Plans 1 and 2 plus the
custom-domain track, but nothing end-to-end proves the two promises users most
depend on:

1. **A failed deploy never takes down the running version.** The deploy
   orchestrator's retire-only-after-health guarantee is unit-tested with fakes
   only.
2. **`git push → live URL` (Plan 3).** The webhook → checkout → build → deploy
   → PR-preview lifecycle is covered only by `internal/webhook`'s in-process
   integration test; no test exercises it binary-to-binary through the relay.

## Scope

Two new e2e tests, both gated on `RUN_E2E=1` and running under the existing
`make e2e` CI job (15-minute timeout has headroom; each adds roughly one
relay-test's worth of runtime).

Out of scope (deliberately): brokered-App webhook delivery through the relay's
own receiver (#289) — that path needs a relay-side GitHub API seam and stays
covered by unit tests; a follow-up issue may add it later. Also out of scope:
piperd/relay restart-resilience tests.

## Test 1 — `TestDeployFailureAndRedeploy` (`test/e2e/deploy_test.go`)

LAN-only harness, same as `TestEndToEndDeploy`. The ~20 lines of piperd
build/boot are extracted into a shared helper rather than copied.

Sequence:

1. Create `blog`; deploy the existing `sampleapp` (v1); `FollowDeploy` →
   `running`; curl through Caddy `:80` by Host header → exact `"hello piper\n"`
   body.
2. Deploy a **broken** app: a temp dir written by the test whose Dockerfile
   fails to build (`FROM alpine:3.20` + `RUN false`). `FollowDeploy` → status
   `"failed"`; curl still serves the v1 body; the app still lists as
   `running`.
3. Deploy a **v2** app (same netcat single-file server as sampleapp, body
   `"hello piper v2\n"`) → `running`; curl now serves the v2 body, proving the
   swap. Deployment history shows the failed row between the two good ones.

No production-code changes.

## Test 2 — `TestWebhookPushAndPreview` (`test/e2e/webhook_test.go`, new file)

Passthrough-relay harness, same shape as `TestRelayLoopback`: enroll `alice`
with base domain `alice.localhost`, relay on `:8443` (TLS) / `:7000` (tunnel),
self-signed `*.alice.localhost` wildcard cert, on-box TLS termination. BYO
GitHub App mode.

### Production seam (the only one)

A `PIPER_GITHUB_API_BASE` env var, read in `cmd/piperd` and passed into
`github.Config.APIBase` (field already exists, defaults to
`https://api.github.com`) at both provider-construction sites
(`webhookStarter.run` and `newRepoFetcher`). Invisible unless set — same
spirit as the existing `PIPER_TEST_ISSUER` seam.

### Stub GitHub

An in-test loopback HTTP server serving the four endpoints the provider hits:

- `POST .../access_tokens` → a fake installation token,
- `GET /repos/<repo>/tarball/<sha>` → a gzipped tar (GitHub codeload shape:
  single top-level dir) containing a Dockerfile keyed **by SHA**, so the push
  and the PR deliver apps with different response bodies,
- `POST .../deployments` / `GET .../deployments` / `POST .../statuses` → the
  Deployments API happy path.

The stub shape is cribbed from `internal/webhook/integration_test.go`.

### Setup

- Insert the `github_app` row (app_id, generated RSA private key, webhook
  secret) directly into `piper.db` **before** piperd starts — same precedent
  as `insertSecondAccount` writing to `relay.db`; respects the one-writer
  rule.
- Start piperd with the relay env plus `PIPER_GITHUB_API_BASE` pointing at the
  stub.
- `piper create blog --port 8080`, then link the repo `alice/blog` via the
  existing app-link CLI/API.

### The loop

Every webhook delivery TLS-dials relay `:8443` with SNI
`hooks.alice.localhost` — the exact path a real GitHub delivery takes (relay
SNI dispatch → tunnel → box Caddy → loopback webhook listener), HMAC-signed
with the webhook secret (`X-Hub-Signature-256`, `X-GitHub-Event`).

1. **push** (ref `refs/heads/main`, SHA A) → 202 → poll SNI
   `blog.alice.localhost` until the SHA-A body serves. Assert the stub saw a
   deployment-status POST (proves the report-back loop ran).
2. **pull_request opened** (PR 7, head SHA B) → poll
   `pr-7-blog.alice.localhost` until the SHA-B body serves; the main app still
   serves SHA A.
3. **pull_request closed** → poll until the preview hostname stops serving;
   the main app is unaffected.

## Testing

These *are* tests; the verification gate is `make verify` (the new tests skip
without `RUN_E2E=1`) plus a full local `make e2e` run against real Docker
before the PR.
