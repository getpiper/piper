# Multi-installation repo picker (relay) — design

**Issue:** #321 — *[relay] repo picker shows only one installation per account and mislabels it with the login username*
**Related:** #320 (multi-account web-login 404 — out of scope here, stays open), #293/#313/#315 (relay-held GitHub App).
**Date:** 2026-07-22

## Problem

The relay serves the web dashboard's new-app wizard through two endpoints, and both collapse an account's GitHub App installations to a single one:

- **Gap 1 — one installation per account, most-recent-wins.** `Store.InstallationForAccount` runs `... ORDER BY created_at DESC LIMIT 1`. `githubRepos` and `githubStatus` each consume that single result, so an account holding several installations (e.g. one on the getpiper org via the installer-fallback in `handleInstallationEvent`, plus one on the personal account) only ever surfaces the newest, and the wizard offers no way to choose.
- **Gap 2 — mislabelled.** `githubStatus` returns `"account": acc.Username` (the logged-in Piper user's login). The wizard renders "installed for `<account>`", but the repos returned belong to the installation's **target** (`target_login`, e.g. `getpiper`). Label and repo list can disagree — which is what surfaced the original confusion.
- **Latent Gap 3 — token minting picks the wrong installation.** `GitHubTokenFor` also resolves through the single most-recent installation. Once an account genuinely holds 2+ installations, a git token for a repo owned by the *older* installation would be minted from the newest one and fail — silently breaking deploys from any non-newest installation. The picker (Gap 1) is what makes multiple installations reachable, so this must be fixed alongside it.

This is relay-side only. The picker UI and the multi-account login-hint live in the separate dashboard repo; #320's 404 is GitHub-side and is not addressed here.

## GitHub invariant this relies on

A GitHub App installation lives on exactly **one** account — a user or an org — and can only ever access repositories **owned by that account**. `GET /installation/repositories` returns only that account's repos. Therefore, for any repo spelled `owner/name`, the installation that can mint a token for it is exactly the one whose `target_login == owner` (GitHub logins are case-insensitive-unique). No stored repo→installation mapping is needed; the owner segment is the key.

## Design

### Store — `internal/relay/installations.go`

Add a value type carrying the installation's own identity, JSON-tagged for pass-through to the API:

```go
type Installation struct {
    ID          string `json:"installation_id"`
    TargetType  string `json:"target_type"`  // "user" | "org"
    TargetLogin string `json:"target_login"`
}
```

Add the enumerating query:

```go
// InstallationsForAccount lists every installation linked to the account,
// newest first. Empty (not an error) when the account has none.
func (s *Store) InstallationsForAccount(accountID string) ([]Installation, error)
```

`SELECT installation_id, target_type, target_login FROM github_installations WHERE account_id=? ORDER BY created_at DESC`. Uses the existing `github_installations_account` index. No schema change.

**Delete `InstallationForAccount`** (the single/most-recent helper). After this work every caller uses either `InstallationsForAccount` or `AccountForInstallation`, so the single-result helper — and its now-stale "the installation an account's agents mint tokens through" doc — is removed rather than left misleading.

### API — `internal/relay/api.go`

**`githubStatus`** — return the array the wizard needs; drop `installed` and `account`:

```json
{
  "github_app": true,
  "installations": [
    {"installation_id": "42", "target_type": "org",  "target_login": "getpiper"},
    {"installation_id": "43", "target_type": "user", "target_login": "faruk"}
  ],
  "install_url": "https://github.com/apps/…/installations/new"
}
```

`github_app:false` still answers 200 (dashboard learns "no App" rather than reading a 503 as an outage). `installations` is `[]` when the App is configured but nothing is installed — the wizard's Connect step reads emptiness, not an error. The label now derives from `target_login`/`target_type`, so it can never disagree with the repos shown (fixes Gap 2). `installed` is dropped — the dashboard derives it from `installations.length > 0` (pre-1.x: break freely, dashboard updates in tandem).

**`githubRepos`** — require `?installation_id=` and authorize it against the caller:

- Missing param → `400`.
- `AccountForInstallation(id)` must resolve and equal `acc.ID`; otherwise `404` "github app not installed for this account" (a not-owned id is reported identically to an unknown one — no existence leak).
- Then `ghApp.Repos(ctx, id)` as today.

Wizard flow: `githubStatus` → user picks from `installations[]` → `githubRepos?installation_id=X`.

### Token minting — `internal/relay/ghtoken.go`

`GitHubTokenFor` keeps the binding authz check first (unchanged — that is the security boundary), then resolves the installation by **repo owner**:

```go
owner, _, _ := strings.Cut(normalizeRepo(repo), "/")
insts, err := s.InstallationsForAccount(accountID)
// pick inst where strings.EqualFold(inst.TargetLogin, owner)
// none → ErrNoInstallation
return app.RepoToken(ctx, inst.ID, repo)
```

`normalizeRepo` already lowercases `owner/name`; the match is case-insensitive against `target_login`. No GitHub round-trips to discover ownership, no schema/wire change.

### `weblogin_cli.go`

The one remaining `InstallationForAccount` caller is an existence check that decides whether to hand the CLI an install URL. It switches to `len(InstallationsForAccount(acc.ID)) == 0`, preserving today's behavior (treat a lookup error as "installed", i.e. no install URL).

## Testing (TDD, failing-first)

**Store (`installations_test.go`)**
- `InstallationsForAccount`: returns all rows newest-first; empty slice (no error) when none.
- Update `TestLinkInstallationBindsToSenderAccount` to assert through `InstallationsForAccount` (single element, correct target).

**Token minting (`ghtoken_test.go`)**
- Two installations — org `getpiper` (id A) and user `alice` (id B). A bound repo `getpiper/app` mints from A; a bound repo `alice/blog` mints from B. Assert the token endpoint hit carries the matching installation id.
- A bound repo whose owner has no linked installation → `ErrNoInstallation`.

**API (`api_test.go`)**
- `githubStatus`: multiple installations → `installations[]` with each target identity; an **org-target install is labeled by the org login, not the username** (the direct Gap-2 regression); no installations → `[]`; `github_app:false` → 200 with `github_app:false`.
- `githubRepos`: valid owned `installation_id` → repos; missing param → 400; `installation_id` not owned by (or unknown to) the account → 404; unauthenticated → 401.
- Update the test `ghStatus` decode struct and `getRepos`/`getStatus` helpers to the new shape; extend `ghAPIStub` to mint access tokens for more than one installation id.

## Out of scope

- Dashboard picker UI and the multi-account login hint (separate repo).
- #320's GitHub-side 404 (stays open).
- Link-header pagination of `/installation/repositories` (#308, unchanged).

## Compatibility

Pre-1.x: the `/v1/github/status` and `/v1/github/repos` response/request shapes change in place — no shim, no version negotiation. The dashboard is updated in tandem. No SQLite schema change.
