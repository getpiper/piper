# Design: Plan 3 — Git-driven deploys (GitHub App, push → live URL)

> **Status:** Design approved in brainstorming (2026-07-05). Not yet implemented.
> Builds on Plan 1 (agent core, LAN-only) and Plan 2 (relay + tunnel + on-box TLS).
> Tracked by epic [#11](https://github.com/piperbox/piper/issues/11).
> Parent design: [`2026-07-04-piper-design.md`](2026-07-04-piper-design.md).

## One-liner

`git push → live HTTPS URL`: a **per-user GitHub App** delivers a webhook to the box over the
existing Plan-2 tunnel, the agent fetches the repo at that commit, runs the unchanged
build → run → health → route flow, and reports a **GitHub Deployment** status with the live
URL back to the commit.

## Where this sits

Plan 1 gave us the deployer (`build → run → health → route` via managed Caddy). Plan 2 made
apps reachable at a public HTTPS URL from behind CGNAT (outbound tunnel + relay SNI
passthrough + on-box TLS). **Plan 3 adds a front door and a back channel around that
unchanged core:** how a git event arrives and lands code on the box (front door), and how the
result is reported to GitHub (back channel). The deployer itself does not change.

## Scope

**This spec — first shippable slice:**

- A **provider seam** (`internal/source`) so different git integrations feed one deployer.
- The **GitHub App** provider: per-user App onboarding (manifest flow), webhook
  verification, installation-token code fetch, and GitHub Deployments API reporting.
- **Push-to-tracked-branch → deploy**, with the live URL posted back as a Deployment status.

**Deferred behind the seam (named, not built):**

- **PR-preview URLs + teardown** (`pr-N.<app>.<base>`) — the natural fast-follow; reuses the
  same provider + deployer machinery with an event-kind switch and per-PR container/route
  lifecycle.
- **GitHub Actions** provider — CI workflow calls piper's API to request a deploy (initiation
  inverts: piper receives rather than pulls).
- **Raw signed-webhook + PAT** provider — no App, manual webhook + token.
- **Central getpiper App** — one official App fanning webhooks out to agents (the hosted
  convenience; see below).

## Positioning: per-user App, self-hosted — central App deferred

A GitHub App has **exactly one webhook URL**, fixed at registration. That single fact decides
the model:

- **Per-user App (this spec).** Each user creates *their own* GitHub App via GitHub's
  **manifest flow**; its webhook URL is *their own agent's* public hostname
  (`https://hooks.<agent>.<base>`). No central infra, zero-trust preserved, App secrets never
  leave the box. This is the Coolify/Dokploy model — and it reuses Plan 2's inbound path
  wholesale.
- **Central getpiper App (deferred).** getpiper registers one official App; a central service
  receives all webhooks and fans them out. Slick onboarding, but it puts always-on central
  infra in the hot path of every deploy and breaks the self-hosted story. Per
  [`CLAUDE.md`](../../../CLAUDE.md) the hosted convenience is **separate and later**.

## Architecture — the provider seam

The only new interface. A `source` provider is anything that can drive a deploy from a git
event:

```go
// internal/source
type Kind int // Push | PROpened | PRSynced | PRClosed
type Status int // Pending | Success | Failure

type Event struct {
    Repo string // "alice/blog"
    Ref  string // "refs/heads/main"
    SHA  string // commit to build
    Kind Kind
    PR   int    // 0 for push
}

type Provider interface {
    // Verify signature + parse a raw webhook into a normalized Event.
    Parse(headers http.Header, body []byte) (Event, error)
    // Fetch the repo tree at sha into destDir (installation-token auth).
    Fetch(ctx context.Context, repo, sha, destDir string) error
    // Report a GitHub Deployment status back (pending → success/failure + URL).
    Report(ctx context.Context, ev Event, status Status, url string) error
}
```

`internal/source/github` (the GitHub-App impl) is the **only** concrete provider built.
`githubActions` and `rawWebhook` are named-but-deferred impls behind this same interface —
proving the seam without building them. The deployer never learns which provider fired it.

### Packages / changes

| Package | Role | New? |
|---|---|---|
| `internal/source` | provider interface + `Kind`/`Status`/`Event` types | new |
| `internal/source/github` | App manifest onboarding, installation-token minting, webhook parse (HMAC), tarball fetch, Deployments API report | new |
| `internal/webhook` | HTTP handler: body → `provider.Parse` → look up app by repo → enqueue deploy (async worker, per-app serialization) | new |
| `internal/deploy` | unchanged core; callable with a fetched-source dir + a `report` hook | minor |
| `internal/store` | apps gain `repo`, `branch`, `dockerfile_path`; new table for App credentials | additive |
| `internal/caddy` | route reserved `hooks.<agent>.<base>` → internal webhook handler | small |

**"Nothing imports up" holds:** `source` knows only GitHub + git; `webhook` orchestrates
`source` + `store` + `deploy`; `deploy` stays ignorant of where source came from.

## Data flow

### A. One-time onboarding (create the per-user App)

```
piper app connect alice/blog          # CLI, on dev machine
  └─ piperd generates a GitHub App *manifest*: name, webhook URL =
     https://hooks.<agent>.<base>, permissions contents:read +
     deployments:write + pull_requests:read, events push + pull_request
     (pull_request scope requested up front so the deferred previews slice
      needs no re-onboarding; this slice only acts on push events)
  └─ opens browser → github.com/settings/apps/new?manifest=… → user clicks Create
  └─ GitHub redirects with a temporary code → piperd exchanges it
     (POST /app-manifests/{code}/conversions) → app_id, private_key, webhook_secret
  └─ piperd stores those in the store (App credentials table)
  └─ user installs the App on their repo(s) → installation_id + a webhook fires
```

The redirect must land where piperd can catch the `code`. The browser is on the dev machine,
piperd is on the box, so the **CLI runs a localhost callback listener** and hands the `code`
to piperd over the existing LAN control API. App secrets stay on the box, never in getpiper's
hands. (Onboarding UX polish is a plan detail; the ownership model is the design commitment.)

### B. Push → live URL (runtime flow)

```
git push origin main
  → GitHub App fires `push` webhook
  → POST https://hooks.<agent>.<base>/            (GitHub's servers, public internet)
      │  ── rides Plan 2 unchanged ──
      ▼
  relay :443 reads SNI = hooks.<agent>.<base>, passes through (never decrypts)
      ▼ (down the existing outbound tunnel)
  Caddy on-box: terminates TLS (wildcard *.<agent>.<base> cert already covers it),
                routes ONLY this host to piperd's webhook handler
      ▼
  webhook handler:
      1. provider.Parse(headers, body)  → verify HMAC (webhook_secret), normalize Event
      2. 202 Accepted immediately; enqueue deploy (async worker)
      3. store.AppByRepo("alice/blog")  → find app; check Event.Ref == app.branch
      4. provider.Report(ev, pending, "")            → GitHub Deployment "pending"
      5. provider.Fetch(repo, sha, tmp) → mint installation token, download codeload
                                          tarball at sha, extract to tmpdir
      6. deploy.Deploy(app, tmpdir, reportHook)      ← existing build → run → health → route
      7. success → provider.Report(ev, success, "https://blog.<agent>.<base>")
         failure → provider.Report(ev, failure, "")
```

Everything from the relay down is **Plan 2 machinery reused verbatim** — no new inbound path,
no new relay code.

### C. Code fetch — tarball, not `git clone`

`Fetch` mints a short-lived installation token from the App private key, then downloads
`GET /repos/{owner}/{repo}/tarball/{sha}` and extracts it. **Rationale:** no `git` binary
dependency on the Pi (stays true to the minimal-deps / `CGO_ENABLED=0` ethos), no full
history, one HTTP call, and the sha pins exactly what we build. A Dockerfile build only needs
the tree at a commit.

### D. Security boundary

The control API stays loopback-only (`127.0.0.1:8088`). Caddy publicly exposes **only** the
`hooks.` host → **only** the webhook route — never the rest of the control plane. The webhook
handler rejects anything failing HMAC verification before touching the store or Docker. The
one new public surface is a single signature-gated endpoint.

## Error handling

Guiding rule: **a webhook must never hang GitHub, and every outcome is reflected back as a
Deployment status.** The handler verifies HMAC, returns `202` fast, and does the slow
fetch+build on an async worker.

| Failure | Handling |
|---|---|
| Bad/missing HMAC signature | `401`, drop. Never reaches store/Docker. |
| Unknown repo (no app bound, or ref ≠ tracked branch) | `202` no-op. Valid delivery we don't act on — logged, not an error. |
| Fetch fails (token mint, tarball 404/network) | Deployment status → `failure` with reason; no container touched. |
| Build/run/health fails | Existing `deploy` records `"failed"` and keeps the old container serving; add `Report(failure)`. **Last-good stays live.** |
| `Report` fails (GitHub API down) | Log + limited retry; never fail the deploy. Reporting is best-effort, orthogonal to liveness. |
| Concurrent pushes to same app | Serialize per-app: one in-flight deploy per app; newer supersedes queued. |
| Duplicate delivery (GitHub redelivers) | Idempotent on `(repo, sha)` — redelivery for an already-running sha is a no-op. |

The async worker + per-app serialization is the one genuinely new bit of control logic;
everything else is boundary guard-clauses.

## Testing (test-first)

**Unit — the seam, with fakes (no network, no Docker):**

- `source/github`: `Parse` verifies HMAC over recorded GitHub payload fixtures (valid →
  Event; tampered → reject). `Fetch` and `Report` tested against an `httptest.Server`
  standing in for GitHub's API (token mint, tarball download+extract, Deployment status POST).
  Table-driven over event kinds.
- `webhook`: handler wired to a **fake provider** + real store + **fake deployer** — asserts
  routing decisions (bad sig → 401; unknown repo → 202 no-op; good push → deploy called with
  right app + report `pending → success` ordering; concurrent pushes serialize). Most
  logic-coverage lives here and runs anywhere.
- `store`: new app fields + App-credentials round-trip.

**Integration — real GitHub App is impractical in CI, so stub the far side:**

- End-to-end through the *actual* `webhook → source/github → deploy` path, with GitHub
  replaced by an `httptest.Server` and Docker replaced by the existing `runtime` fake. Proves
  wiring without external accounts.

**e2e (opt-in, skips cleanly when absent — matching the existing Docker/Caddy convention):**

- The Plan-2 loopback-relay harness + real Docker/Caddy, driven by a **synthesized signed
  webhook** POSTed at the `hooks.` host, asserting the sample app goes live at its URL. Real
  GitHub is not in the loop; we generate a correctly-HMAC'd payload with a test secret.

**Not tested:** live GitHub App registration / manifest exchange — a manual, documented
onboarding step, not a CI-testable path.

## Deferred (proven-later, behind the seam)

- **PR previews + teardown** — `pr-N.<app>.<base>` ephemeral container + hostname + route,
  torn down on `PRClosed`. Reuses provider + deployer; adds per-PR lifecycle state.
- **GitHub Actions** and **raw-webhook** providers — additional `source.Provider` impls.
- **Central getpiper App** — hosted-convenience fan-out, separate pipeline per `CLAUDE.md`.
