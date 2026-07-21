# Official GitHub App — design

**Date:** 2026-07-20
**Status:** designed, not built
**Supersedes the deferral in:** [`2026-07-05-plan3-git-deploys-design.md`](2026-07-05-plan3-git-deploys-design.md) §"Deliberately not built"

## Problem

A newcomer on the public relay has to cross seven steps and two browser trips before
`git push` deploys anything:

1. install `piperd`
2. `piper login` — GitHub OAuth, browser trip #1
3. `piper enroll`
4. `piper create myapp --port 8080`
5. `piper github setup` — App manifest dance, browser trip #2
6. **manually** install the resulting App on the repo (no deep link — [#152](https://github.com/getpiper/piper/issues/152))
7. `piper app link myapp --repo owner/name --branch main`

Steps 5 and 6 exist only because every box creates its *own* private GitHub App
(`internal/source/github/manifest.go:15`, stored as a singleton row in
`internal/store/schema.sql:21`). That design also forces a publicly resolvable
`hooks.<base>` with a publicly trusted certificate
(`docs/runbooks/git-deploy-e2e.md:58`), which is the only publicly exposed `piperd`
surface.

Separately, the relay authenticates users against a *standalone* GitHub OAuth App
(`internal/relay/verifier_github.go`). So the same human authorizes GitHub twice, for
two different pieces of Piper.

Goal: **one GitHub consent screen, no App-creation dance, no manual install step, no
public DNS or certificate prerequisite** for users of a public relay. The CLI's login is
device flow, so `piper login` still makes two short browser stops — enter the device
code, then the App install page — because device flow cannot install an App; a true
one-browser-trip CLI login is follow-up
[#291](https://github.com/getpiper/piper/issues/291). Web/dashboard onboarding gets the
single install-and-authorize trip today. Everything else in this document exists to
serve that goal.

## Non-goals

- Dashboard UI work. This unblocks the repo picker; it does not build it.
- `piper deploy owner/name` one-shot sugar. Real friction win, separable, easier to
  design once a repo picker exists.
- GitHub Actions provider, raw-webhook provider, cross-fork PRs, relay-side build
  caching, delivery-ID dedupe.
- **Org-owned installations** — *implemented (#290).* An org owner links their Piper org
  to a GitHub org (`PUT /v1/orgs/{slug}/github`, stored on the org account's
  `github_login`; the stable org id is pinned from the first install webhook). An
  org-target install then routes to the *org account* — verified through the installing
  sender's `org_members` membership — so the org's own boxes deploy it. Routing and token
  brokering are unchanged: org boxes are owned directly by the org account. A member's
  *personal* box is out of scope; an org-target install with no linked Piper org falls
  back to the installing user, unchanged.
- Backwards compatibility. Per the pre-1.x policy in `CLAUDE.md`, formats and schemas
  change in place.

## Trust boundary — stated plainly

Today "your GitHub credentials never leave your box" is a headline property of Piper.
This design knowingly gives that up **for users who opt into a relay-held App**: the
relay operator holds a private key that can read source for every repository its users
grant it, and becomes the single point of GitHub trust.

Mitigations narrow the blast radius but do not remove it:

- installation tokens are minted on demand, scoped to a **single repository** with
  minimal permissions, and never persisted;
- user access tokens are used once at login to read `GET /user` and then discarded, so
  no user credential is stored at all;
- an agent can mint a token only for a repository bound to *that agent* — which
  contains accidents and confused-deputy bugs, not an attacker on the box: `bind-repo`
  is itself an agent-issued op, so a compromised box can bind and then mint for any
  repository its account's installation covers. The hard boundary is the account hop —
  an agent can never reach another account's installation.

**BYO stays first-class, permanently.** It is the answer for LAN-only boxes and for
anyone who will not accept the above. It is already built, it sits behind the existing
`source.Provider` seam, and it costs approximately nothing to keep.

## Architecture

Two provider modes behind the existing `source.Provider` seam
(`internal/source/source.go:56`):

- **BYO** — box holds its own App; webhooks arrive publicly at `hooks.<base>`; the agent
  mints its own installation tokens. Unchanged.
- **Brokered** — the *relay* holds a GitHub App. The box holds no GitHub credentials.

**Mode selection:** the relay advertises App support at enroll. A box defaults to
brokered when it sees that. Locally stored BYO credentials are an explicit override and
win.

**Generality:** this is a relay capability, not a getpiper special case. Any
`piper-relay` operator can configure an App; getpiper's hosted relay is simply the
instance that does. With the variables unset a relay serves BYO users only and does not
mount the ingress endpoint.

```
PIPER_RELAY_GITHUB_APP_ID
PIPER_RELAY_GITHUB_APP_KEY         # PEM path
PIPER_RELAY_GITHUB_WEBHOOK_SECRET
PIPER_RELAY_GITHUB_CLIENT_ID       # now the App's, replacing the standalone OAuth App
PIPER_RELAY_GITHUB_CLIENT_SECRET
```

### One App, one identity

The official App replaces the standalone OAuth App outright. A GitHub App has its own
user-to-server OAuth and supports device flow, so it can serve both identity and
deploys. With "Request user authorization during installation" enabled, GitHub shows a
single screen that authorizes *and* selects repositories.

`internal/relay/verifier_github.go` keeps its shape; only the client id/secret change.

### Three flows cross the boundary

1. **Identity + install** — `piper login` sends the user to the App's install URL. The
   redirect carries `code`, `installation_id` and `setup_action`, so one trip yields
   both the identity and the installation.
2. **Webhook ingress** — GitHub delivers to `POST /gh` on the relay. The relay verifies
   the App HMAC, resolves installation → account → bindings → agent, and delivers over
   the existing tunnel.
3. **Token brokering** — the agent asks the relay for a repo-scoped installation
   token over the existing `KindControl` op protocol.

### Agent→relay calls ride the existing control ops

The box already has an authenticated agent→relay channel: `KindControl` streams
carrying `tunnel.ControlRequest{Op: …}`, sent from `internal/agent/tunnelclient.go` and
dispatched in `internal/relay/server.go:225`. The tunnel handshake authenticates the
agent by token, so ops need no second credential and the box needs no relay HTTP base
URL.

Both new agent→relay calls are therefore ops, not HTTP endpoints:

| Op | Request fields | Response |
| --- | --- | --- |
| `bind-repo` | `App`, `Repo`, `Branch` | empty, or `Error` |
| `unbind-repo` | `App` | empty, or `Error` |
| `gh-token` | `Repo` | `Token`, `Expires` |

`ControlRequest` gains `Repo` and `Branch`; `ControlResponse` gains `Token` and
`Expires`. Relay→box HTTP (webhook delivery) still uses the `KindHTTP` stream that
already pipes to the box's Caddy on `:80`.

### Delivery is deliberately boring

The tunnel already carries public HTTP to the box's Caddy, and `webhookStarter` already
installs a `hooks.<base>` Caddy route on Caddy's `:80` server (`cmd/piperd/main.go:621`,
`internal/caddy/client.go:25`). Delivery is therefore: open a `tunnel.KindHTTP` stream —
which the agent already pipes to `:80` in every mode (`cmd/piperd/main.go:191`) — and
speak plain HTTP with `Host: hooks.<base>`. Caddy routes it to `:8089` exactly as a
public request would.

**No new wire protocol and no agent-side receive code.** The only change from today is
that the request arrives through the tunnel rather than from the internet — which is
what removes the public DNS record and certificate as prerequisites, and takes the last
public `piperd` surface off the internet.

## Data model

New relay state (`internal/relay/schema.sql`, edited in place — no migration):

Note the existing key types: `accounts.id` and `accounts.github_id` are `TEXT`
(UUID and decimal-string GitHub id respectively), and `agents` is keyed by
`name TEXT PRIMARY KEY`, which equals the agent's base domain. New tables follow
those types and the file's `IF NOT EXISTS` idiom.

```sql
CREATE TABLE IF NOT EXISTS github_installations (
    installation_id TEXT PRIMARY KEY,
    account_id      TEXT NOT NULL REFERENCES accounts(id),
    target_type     TEXT NOT NULL,   -- 'user' | 'org'
    target_login    TEXT NOT NULL,
    created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS repo_bindings (
    agent_name TEXT NOT NULL REFERENCES agents(name),
    app        TEXT NOT NULL,
    repo       TEXT NOT NULL,        -- 'owner/name', lowercased
    branch     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (agent_name, app)
);
CREATE INDEX IF NOT EXISTS repo_bindings_repo ON repo_bindings(repo);

CREATE TABLE IF NOT EXISTS pending_events (
    agent_name TEXT NOT NULL REFERENCES agents(name),
    app        TEXT NOT NULL,
    ref        TEXT NOT NULL,        -- branch name, or 'pr-<N>'
    payload    BLOB NOT NULL,
    event      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (agent_name, app, ref)
);
```

`agents` gains a `webhook_secret` column — the per-agent HMAC secret minted at enroll.

`pending_events` is a **coalescing slot, not a queue**: a newer event for the same ref
overwrites the older one. Deploys are last-write-wins, so replaying intermediate commits
would be actively wrong. It is additionally capped per agent, evicting oldest, so a
PR-heavy repo cannot grow it without bound.

## Components

### Relay

| File | Responsibility |
| --- | --- |
| `internal/relay/githubapp.go` | App JWT (RS256), repo-scoped installation tokens, `GET /installation/repositories` |
| `internal/relay/ingress.go` | `POST /gh` — verify App HMAC, resolve installation → account → bindings |
| `internal/relay/bindings.go` | Store methods behind the `bind-repo` / `unbind-repo` / `gh-token` control ops |
| `internal/relay/delivery.go` | Deliver over a `KindHTTP` stream; park in `pending_events` on failure; drain on reconnect |
| `internal/relay/server.go` | Existing control-op switch; dispatch the three new ops |
| `internal/relay/api.go` | `GET /v1/github/repos` and the install-redirect handler (account-credential auth) |
| `internal/relay/verifier_github.go` | Existing file; swap the OAuth client to the App's credentials |

### Agent

- `internal/source/github` — extract a `TokenSource` interface. `appTokenSource` wraps
  the existing JWT → installation-token path (`github.go:78-92`) for BYO;
  `relayTokenSource` calls the relay for brokered. `fetch.go` and `report.go` take it as
  a dependency instead of computing tokens themselves.
- `internal/config` — `relay.json` gains `webhook_secret`.
- `cmd/piperd` — in brokered mode, start the webhook listener when the relay advertises
  App support; no local `github_app` row required.
- `internal/store` — the `github_app` table stays, used only by BYO.
- `internal/api` — `POST /v1/apps/{name}/link` (`api.go:208`) additionally pushes the
  updated binding set to the relay.

### CLI

- `piper login` — same surface; now one browser trip covering identity and repo
  selection.
- `piper github setup` — unchanged; becomes the explicit BYO opt-in.
- `piper app link` — unchanged surface; now also registers the binding relay-side.
- `piper github repos` — new; lists installation-accessible repositories. The
  dashboard's repo picker calls the same relay endpoint.

### Repo-list shape ([#308](https://github.com/getpiper/piper/issues/308))

`GET /v1/github/repos` returns one object per repository, not a bare name, so the
picker can render a visibility badge and sort by recency:

```json
{"repos": [
  {"full_name": "owner/name", "visibility": "public", "pushed_at": "2026-07-20T12:34:56Z"}
]}
```

Fields are passed straight through from GitHub's `GET /installation/repositories`
(`visibility` is `public`/`private`/`internal`; `pushed_at` is RFC3339, `""` for a
never-pushed repo). The request sends `per_page=100`; full Link-header pagination
across installations with >100 repos is a follow-up, not built here. `piper github
repos` prints `full_name`, marking non-public repos.

## Flows

### Onboarding — desktop

1. `piper login` → GitHub device flow (enter the code, authorize), then the CLI prints
   the App's install URL and polls until the installation appears. One consent screen
   covers authorize + pick repositories; a redirect-based one-trip CLI login is
   follow-up [#291](https://github.com/getpiper/piper/issues/291).
2. The relay learns the installation from the `installation.created` webhook (sender →
   account by `github_id`) and the CLI's poll sees it appear. Dashboard web logins get
   the same linkage synchronously: their callback carries `code`, `installation_id`,
   `setup_action`, and the relay exchanges the code, reads `GET /user` for `github_id`,
   upserts the account, discards the user token, and links the installation. Both paths
   are idempotent — the webhook and the redirect race in either order.
3. `piper enroll` → base domain, relay token, per-agent `webhook_secret`.
4. `piper create myapp --port 8080` and `piper app link myapp --repo owner/name --branch main`
   → binding registered relay-side.
5. `git push`.

### Onboarding — headless (the Pi case)

Device flow can authorize but cannot install, and no redirect can reach a headless box.
This is honestly **two browser trips**: enter the device code, then open the install URL.
The relay learns the installation from the `installation.created` webhook and the CLI
polls until it appears. Still better than today's manifest flow, which additionally
required a loopback listener the box could not easily offer.

### Push

```
GitHub → POST /gh (relay)
  verify App HMAC
  parse: installation_id, repo full_name, kind
  installation → account
  repo_bindings WHERE repo = ? AND agent.account_id = account
  for each agent:
    re-sign payload with agent.webhook_secret
    KindHTTP stream → HTTP POST, Host: hooks.<base>  (box Caddy :80 → :8089)
    on failure → upsert pending_events (coalesce by ref)
→ agent: existing internal/webhook path, unchanged
→ agent: control op gh-token {Repo} → repo-scoped installation token
→ tarball fetch → build → deploy → deployment status
```

**One authority for branch filtering.** The relay routes on **repository only**; the
agent decides whether the branch matches, exactly as it does today.
`repo_bindings.branch` is display metadata for the dashboard and part of the coalescing
key — it is never a filter. Two components filtering the same condition is how pushes
end up mysteriously deploying nowhere.

### PR previews

No new logic. `pull_request` events route by repository identically, and the agent's
existing `DeployPreview` / `TeardownPreview` path (`internal/deploy/deploy.go:321,366`)
handles them.

### Reconnect

When an agent's tunnel comes up, the relay drains that agent's `pending_events`. A box
that was off overnight deploys the tip commit on boot rather than replaying every push
it missed.

### Installation lifecycle

- `installation.created` — upsert the account by `github_id`, link the installation.
  Written to be idempotent and order-independent: the webhook and the OAuth redirect
  race, and either may land first.
- `installation.deleted` — drop the row. Bindings survive; deploys then fail with an
  explicit "GitHub App is no longer installed for this account" rather than a token
  error.
- `installation_repositories.*` — no action. No repo list is cached, so a revoked
  repository simply fails at token-mint time.

**Ownership check:** an installation links to an account only when
`installation.account.id` matches `accounts.github_id` (user installs) or resolves
through `org_members` (org installs). An event whose installation has no linked account
is dropped with a 202 and a log line — never routed.

## Security and failure modes

**Authorization on token brokering:** an agent may mint a token only for a repository
present in *its own* `repo_bindings`. Agent token → agent → account → installation →
repo bound to that agent. Be precise about what this buys: `bind-repo` is itself an
agent-issued op, so against a *compromised box* the binding check is self-serve and the
effective blast radius is every repository that box's account granted the App. What it
does contain is accidental cross-app minting and confused-deputy bugs. The load-bearing
boundary is the account hop: the installation is always resolved through the asking
agent's own account (a disabled account resolves to nothing), so no box can ever mint
against another tenant's installation.

**Signature handling:** the relay verifies GitHub's `X-Hub-Signature-256` with the App
secret, then drops it and re-signs with the per-agent secret. The agent's verification
(`internal/source/github/parse.go:14`) is unchanged and still mandatory — the tunnel is
not treated as authenticating. Constant-time comparison; a body cap at the relay
mirroring the agent's existing 5 MiB (`internal/webhook/webhook.go:19`).

**Key at rest:** the App private key is a file path, mode-checked at startup, never
logged, never returned by any endpoint. Every relay query is scoped by `account_id`.

| Failure | Behavior |
| --- | --- |
| Box offline | Event parks in `pending_events`, coalesced by ref, drained on reconnect |
| Box connected but delivery fails | Same parking path — GitHub already received its 202, so the relay never leans on GitHub redelivery; the event is re-driven from `pending_events` |
| Relay unreachable at token-mint | Deploy fails fast, status `failed`, message names the relay rather than a generic auth error |
| GitHub API error or rate limit | Relay returns 5xx so GitHub's own delivery retry does the work; no retry loop of our own |
| Installation deleted | Token mint 404s → deploy fails with "GitHub App is no longer installed for this account" |
| Event for an unlinked installation | Dropped, 202, logged; never routed |

## Testing

Test-first per `CLAUDE.md`. Layering holds: `relay` gains GitHub knowledge,
`source/github` gains a `TokenSource` seam, nothing imports up.

- **`internal/source/github`** — existing tests keep passing against `appTokenSource`; a
  fake `TokenSource` covers the brokered path. This proves the seam is real rather than
  nominal.
- **`internal/relay`** — `httptest` fake GitHub for JWT, installation tokens and repo
  listing. Routing resolution as a table test with a negative case per boundary: an
  event for an unlinked installation, a token request for an unbound repo, and an agent
  on account A requesting a repo bound under account B.
- **Delivery** — in-memory yamux pair; assert the re-signed HMAC verifies with the
  agent's secret and that GitHub's original signature is absent.
- **Offline replay** — park two pushes for the same ref, reconnect, assert exactly one
  delivery carrying the newer SHA.
- **e2e** — extend `docs/runbooks/git-deploy-e2e.md` with a brokered-mode run, reusing
  the auto-approve verifier pattern (`internal/relay/verifier.go:31`).

## Outcome

| Today | Brokered |
| --- | --- |
| `piper login` — browser trip #1 | `piper login` — authorize + install, one consent screen (one-trip login: [#291](https://github.com/getpiper/piper/issues/291)) |
| `piper enroll` | `piper enroll` |
| `piper create myapp --port 8080` | `piper create myapp --port 8080` |
| `piper github setup` — browser trip #2 | — |
| *manually* install the App on the repo | — |
| `piper app link …` | `piper app link …` |
| `git push` | `git push` |

Plus: no `hooks.<base>` DNS record, no publicly trusted certificate prerequisite, and no
publicly exposed `piperd` surface. And because the relay holds the installation, a
dashboard can list the user's repositories and offer "deploy this one" — which a
per-box BYO App can never do.
