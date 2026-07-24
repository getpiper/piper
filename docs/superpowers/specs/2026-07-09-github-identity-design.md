# GitHub identity for relay accounts — design

Design for [#99]: switch relay account identity from Google to a
getpiper-owned **GitHub OAuth app** — device flow for `piper login`,
authorization-code flow for the browser (dashboard login) — and remove the
Google flow entirely. Phase 0 dependency of the dashboard roadmap
(getpiper/dashboard, part of #76).

Why GitHub: the audience is GitHub users by definition (git deploys), GitHub
logins map cleanly onto `<hash>-<username>.<apex>` hostnames, and org
membership becomes a natural base for the later org model. Doing it now is
cheap — pre-release, no real accounts to migrate.

This identity app is **distinct from the per-user GitHub App** used for git
deploys (`piper github setup`) — different credentials, different purpose;
deploy-App keys still never leave the box.

## Scope

**In scope:** a `GitHubVerifier` replacing `GoogleVerifier` (device flow, same
server-side broker shape); two new relay endpoints for the browser
authorization-code flow (`GET /v1/login/web`, `GET /v1/login/callback`);
identity/username derivation from the GitHub login; the `accounts` schema
column rename; relay wiring/env changes; removal of the Google verifier and
its `go-oidc` / direct `x/oauth2` dependencies.

**Out of scope:** the dashboard itself (separate repo — it only consumes the
redirect contract defined here); org accounts; any second identity provider
(no dual-provider support pre-release); email capture (nothing uses it; add
the `user:email` scope if and when the dashboard needs it); token/credential
lifecycle changes (mint/hash/authenticate stay exactly as they are).

## Decisions (settled during brainstorming)

1. **Web flow shape: relay-hosted redirect.** The relay owns the whole OAuth
   dance; the dashboard stays a pure static client with no client ID, secret,
   or OAuth code. The credential returns to the dashboard in a URL
   **fragment**, which never reaches server logs.
2. **No email.** No OAuth scopes are requested (public profile only).
   `Identity` carries the GitHub numeric ID and login; the username derives
   from the login, not an email local part.
3. **Schema: rename `google_sub` → `github_id`, no migration code.**
   Pre-release there are no real accounts; the operator drops `accounts` and
   `account_creds` on the hosted relay at deploy (the `agents` table is
   untouched).
4. **Plain HTTP GitHub client, not `x/oauth2`.** GitHub's device flow needs no
   client secret and returns no ID token — identity comes from
   `GET /user` — and GitHub delivers poll errors (`authorization_pending`,
   `slow_down`, …) as fields in **200-OK bodies**, which fights RFC-strict
   library parsing. ~150 lines of explicit protocol code, `httptest`-able via
   configurable base URLs, and both `go-oidc` and direct `x/oauth2` drop out
   of `go.mod`.

## Verifier layer (`internal/relay/`)

`verifier_google.go` and its test are deleted. A new `verifier_github.go`
implements:

```go
type GitHubVerifier struct {
    clientID, clientSecret string
    oauthBase, apiBase     string // https://github.com / https://api.github.com; tests override
    // device flows: handle → *flow, as GoogleVerifier today
}
```

- `Identity` becomes `{Subject string, Login string}` — `Subject` is the
  GitHub numeric user ID rendered as a string (the stable anchor; logins can
  be renamed), `Login` is the GitHub login. `Email` is removed.

**Device flow** keeps today's server-side broker shape (the relay holds any
secrets; the CLI never talks to the IdP):

- `Start` calls `POST {oauthBase}/login/device/code`
  (`Accept: application/json`, `client_id` only, no scopes) and returns an
  opaque random handle plus the existing `DeviceAuth` struct.
- A background goroutine polls
  `POST {oauthBase}/login/oauth/access_token` with
  `grant_type=urn:ietf:params:oauth:grant-type:device_code`, honouring the
  server `interval`, treating `authorization_pending` as keep-waiting,
  `slow_down` as interval +5s (GitHub's documented semantics), and
  `expired_token` / `access_denied` as terminal errors.
- On success it calls `GET {apiBase}/user` with the access token and builds
  `Identity{Subject: strconv(id), Login: login}`. The access token is used
  once and discarded — the relay's own account credential is the durable
  artifact, unchanged.
- `Poll(handle)` reports `ErrAuthPending` / identity / error without
  blocking, exactly as today. The `Verifier` interface is unchanged.

**Web flow** adds two IdP-pure methods on `GitHubVerifier`, grouped in a new
small interface so the API layer stays fake-testable:

```go
type WebVerifier interface {
    AuthCodeURL(state string) string                      // GitHub authorize URL
    Exchange(ctx context.Context, code string) (Identity, error)
}
```

`AuthCodeURL` builds `{oauthBase}/login/oauth/authorize?client_id=…&state=…`.
No `redirect_uri` parameter is sent to GitHub — the OAuth app's single
registered callback URL is used, so the relay needs no callback-URL config.
`Exchange` form-POSTs `client_id`+`client_secret`+`code` to
`/login/oauth/access_token`, then calls `GET /user`.

`FakeVerifier` grows trivial `AuthCodeURL`/`Exchange` implementations so API
tests and the loopback e2e can drive the web flow without GitHub.

## HTTP API (`internal/relay/api.go`)

The device endpoints keep their **exact wire shape** — `POST /v1/login/device`
and `POST /v1/login/poll` return the same JSON fields, so the CLI is
untouched. Two new endpoints:

- **`GET /v1/login/web?redirect_uri=…`** — validates `redirect_uri` against
  the configured allowlist (prefix match); mints a crypto/rand `state`;
  stores `{redirect_uri, expiry: now+10m}` in an in-memory map on the `api`
  struct (state is an HTTP-session concern, not an IdP one); sets a
  `piper_login_state` cookie (`HttpOnly; Secure; SameSite=Lax`, 10-minute
  max-age) binding the flow to the browser (login-CSRF guard); 302s to
  `AuthCodeURL(state)`.
- **`GET /v1/login/callback?code&state`** — requires `state` to match both
  the map entry (consumed on first use) and the cookie; on mismatch or
  expiry, plain `http.Error` 400. Then `Exchange(code)` →
  `UpsertAccount` → `MintAccountCredential` → 302 to
  `{stored redirect_uri}#credential=<cred>&username=<username>`. Exchange or
  store failures are plain `http.Error`s (502 for upstream, 500 for store) —
  the dashboard retries by starting over.

A single relay process serves both endpoints, so the in-memory state map
needs no persistence; states expire in 10 minutes and are single-use.

## Accounts / store (`internal/relay/accounts.go`, `schema.sql`)

- `schema.sql`: `google_sub TEXT NOT NULL UNIQUE` →
  `github_id TEXT NOT NULL UNIQUE`. No ALTER/migration code (decision 3).
- `UpsertAccount(githubID, login string)` — same idempotent
  select-then-insert, keyed on `github_id`.
- `deriveUsername` sanitizes the GitHub **login** instead of an email local
  part: lowercase, runes outside `[a-z0-9-]` → `-`, trim `-`, cap at 30
  chars (GitHub logins are ≤39 chars of `[A-Za-z0-9-]`, so this is nearly a
  passthrough; the cap keeps `<hash>-<username>.<apex>` under DNS's 63-char
  label limit). The collision-suffix loop (`name`, `name-2`, …) stays.

## Wiring / config (`cmd/piper-relay/main.go`)

- `PIPER_RELAY_GOOGLE_CLIENT_ID` / `PIPER_RELAY_GOOGLE_CLIENT_SECRET` →
  `PIPER_RELAY_GITHUB_CLIENT_ID` / `PIPER_RELAY_GITHUB_CLIENT_SECRET`.
- New `PIPER_RELAY_WEB_REDIRECTS`: comma-separated allowed `redirect_uri`
  prefixes (e.g. `https://dash.getpiper.co/`). Empty ⇒ web-login endpoints
  respond 503 "web login not configured".
- Device flow needs only the client ID (GitHub device grant takes no
  secret). Missing secret ⇒ web login 503s; device login still works.
- `PIPER_RELAY_FAKE_APPROVE=1` (test-only auto-approve) and the
  no-client-ID operator-enroll-only mode behave exactly as today.
- Deploy note for the hosted relay: create the GitHub OAuth app (device flow
  **enabled** in its settings; callback URL
  `https://api.public.getpiper.co/v1/login/callback`), set the new env vars,
  and drop the `accounts` / `account_creds` tables.

## CLI (`cmd/piper/relayonboard.go`)

No behavioural change — `piper login` already speaks only the relay's device
endpoints, whose wire shape is preserved. Only comments/strings naming Google
change (e.g. the login hint text).

## Error handling

- Device poll terminal errors (`expired_token`, `access_denied`) surface
  through `Poll` as errors → the existing `/v1/login/poll` 400 path → CLI
  prints the error and exits, as today.
- Unknown/expired/reused web `state`, cookie mismatch, disallowed
  `redirect_uri`: 400 before any GitHub call.
- GitHub 5xx / malformed responses: 502 from the callback; the verifier
  wraps them with enough context to log.

## Testing (TDD, per package)

- **`verifier_github_test.go`** — `httptest` server faking the three GitHub
  endpoints: device happy path (start → pending → approved → identity),
  `slow_down` honoured, `access_denied`/`expired_token` terminal, `/user`
  identity mapping, `AuthCodeURL` contents, `Exchange` happy path and
  upstream-error path.
- **`api_test.go`** — web endpoints driven by `FakeVerifier`: allowlist
  enforcement, state single-use, state expiry, cookie mismatch, and the
  final fragment (`#credential=…&username=…`) on the redirect.
- **`accounts_test.go`** — username derivation from logins (case folding,
  collision suffixes), `UpsertAccount` idempotent by `github_id`.
- **e2e** — unchanged: `PIPER_RELAY_FAKE_APPROVE=1` still drives
  `login → connect → deploy → curl`.

## Acceptance criteria (from #99)

- [ ] `piper login` uses GitHub device flow end to end.
- [ ] A browser can complete an authorization-code flow against the relay
      and obtain the same account credential.
- [ ] Google flow removed (no dual-provider support needed pre-release).

[#99]: https://github.com/piperbox/piper/issues/99
