# TUI GitHub flows — design

**Date:** 2026-07-23
**Scope:** Bring the v0.6.0 relay-brokered GitHub App flows into the TUI: relay web login,
install status/wizard, multi-installation repo picker on link, and the monorepo root-dir
field. CLI subcommands are untouched; the dashboard is untouched.

## Background

v0.6.0 completed the relay-held GitHub App onboarding story for the CLI and dashboard
(#311 #312 #313 #317 #318 #322 #323). The TUI predates it: login is LAN-token only, `g`
runs the old self-held App manifest flow, and the link form is free text with `rootDir`
hardcoded to `""`. Everything needed exists in `internal/relayclient`; the TUI just
doesn't consume it.

## Architecture

### RelayAPI seam

`internal/tui` gains a second interface alongside `API`:

```go
// RelayAPI is the slice of the relay control API the TUI consumes.
// *relayclient.Client satisfies it; tests inject fakes.
type RelayAPI interface {
    CLILoginStart(ctx context.Context) (handle, userCode string, err error)
    CLILoginPoll(ctx context.Context, handle string) (relayclient.Account, error)
    GitHubStatus(ctx context.Context, cred string) (relayclient.Status, error)
    GitHubRepos(ctx context.Context, cred, installationID string) ([]relayclient.Repo, error)
}
```

Injected as a factory mirroring `Dialer` — `RelayDialer func(base string) RelayAPI` —
because the base URL isn't fixed: a fresh user logs in against the default relay, a
configured user against `cc.RelayAPI`. The root model holds the factory and hands it to
the views that need it at construction (`newGithubWizard`, `newLinkForm`). Views read the
account credential from client config at use-time (as `relayonboard.go` does), so a
login completed mid-session is picked up without re-dialing.

### relayclient changes (pre-1.x, in place, no shims)

- `GitHubStatus` returns a `Status` struct instead of `[]Installation`, parsing the two
  fields the relay already sends and the client currently discards:

  ```go
  type Status struct {
      GitHubApp     bool           `json:"github_app"`
      InstallURL    string         `json:"install_url"`
      Installations []Installation `json:"installations"`
  }
  ```

  The relay's HTTP handler and JSON payload are byte-for-byte unchanged; the dashboard
  (which fetches `/v1/github/status` directly, never via this Go client) is unaffected.
  The only Go call sites are `waitForInstall` and `githubRepos` in
  `cmd/piper/relayonboard.go`; both update in the same commit. This also fixes a latent
  CLI gap: the install URL is now available from status, not only from a fresh login
  response.

- `defaultRelayAPI` moves from `cmd/piper` to `relayclient.DefaultAPI` so the TUI can
  offer login with no relay config present.

### Error isolation

Relay errors never feed `errMsg`/`pollResult` — that machinery drives the *box* status
bar ("unreachable — retrying"), and a relay hiccup must not impersonate a box outage.
Wizard and picker messages carry their own `err` fields and banner in-view.

## The `g` wizard

`github.go`'s view is rebuilt as a state machine. The existing manifest flow is renamed
`manifestView` and stays reachable from inside the wizard. `g` anywhere opens the wizard;
breadcrumb stays `github`.

```
loading ──(no account credential)──────────────→ login
   │                                               │ ↵ starts: CLILoginStart →
   │                                               │ show code + verify URL, open
   │                                               │ browser, poll CLILoginPoll per tick
   ├──(cred, status.github_app == false)──→ byo    │
   │     "this relay doesn't broker a              │ success: save RelayAPI + cred
   │      GitHub App — press m for the             │ to client config (off UI thread)
   │      self-held App manifest flow"             ▼
   ├──(cred, no installations)────────────→ install ←──(login returned install_url)
   │     show install URL, open browser        │ poll GitHubStatus per tick
   │     once, poll until an install appears   │
   ▼                                           ▼
installed ←────────────────────────────────────┘
     list installations (target_login + target_type),
     ↵ on one → its repos (read-only browse, live from relay)
```

- **On open**, `refresh` loads client config off the UI thread: credential present →
  `GitHubStatus`; absent → `login` idle. A user who already ran `piper login` lands
  straight on installations.
- **Login is armed, not automatic**: shows "sign in with GitHub via `<relay>`", starts on
  `↵` (no surprise browser launch on `g`). Relay base is `cc.RelayAPI`, else
  `relayclient.DefaultAPI`. Code + verify URL stay on screen throughout for headless/SSH
  boxes where `openBrowser` fails silently.
- **Polling rides the root's 2s tick**: the wizard's `refresh(API)` ignores the box API
  argument and returns the relay poll cmd for its state (`CLILoginPoll` while logging in,
  `GitHubStatus` while awaiting install, nil when settled). One-shot cmds, no blocking
  goroutine, no cancel plumbing — `esc` pops the view and polling stops; an abandoned
  login handle expires on the relay.
- **`ErrAuthPending` is "still waiting"**, not an error. Real errors banner in-view.
- **One-trip carry-over**: on login success with no installation, the login response's
  `install_url` is shown and the browser was already bounced there by the relay; the
  wizard advances to `install` and watches status.
- **Config save** happens inside the success cmd (off the UI thread), then the wizard
  proceeds to status — same precedent as `loginView.submit`.
- **`manifestView`** (the self-held App manifest flow) is pushed by `m` from `byo` or any
  settled state. Root handling of `githubStartMsg`/`githubFormReadyMsg`/`githubDoneMsg`
  and `githubCancel` is unchanged apart from the type rename.
- **Footers** per state, e.g. login `↵ sign in · esc cancel · ? help`; installed
  `↵ repos · m manifest app · esc back · ? help`.

## Link form: repo picker + root-dir

The form keeps its shape and gains:

**Repo combo-box.** The repo text input doubles as a filter:

- On open, with a relay credential present, one cmd fetches `GitHubStatus` → per-
  installation `GitHubRepos` → a flat `{fullName, target}` list in a single
  `reposLoadedMsg`. Any failure is silent; the form stays free-text (LAN/BYO boxes and
  relay outages must never block manual linking).
- With focus on the repo field and a non-empty list, up to ~6 case-insensitive substring
  matches render below, each `owner/name` plus a dim target label *only when the account
  has more than one installation* (#322 disambiguation, shown only when it
  disambiguates).
- No match is selected until `↓` first enters the list; `↓`/`↑` then move the selection
  (typing clears it back to none). `↵` with a selection fills the field and focuses
  branch; `↵` without a selection submits the typed text. Free text is never trapped
  behind the picker. Field cycling moves to `tab` only (today `↓` also cycles).
- No credential → today's behavior plus a dim hint: `press g to connect GitHub`.

**Root-dir field.** Third input after branch, placeholder `(repo root)`, blank = repo
root, submitted as typed — the agent is the authority on subpath validity. `linkAppMsg`
gains `rootDir`; the root passes it to `LinkApp` (replacing the hardcoded `""`).

**Out of scope:** branch auto-fill (relay `Repo` payload has no default branch),
pagination (filter covers big accounts), linking from the wizard's repo browse screen
(linking stays an app-detail action), dashboard changes, relay-side changes.

## Testing

TDD throughout, driving `Update` directly per the package's existing style:

- **relayclient**: `Status` parsing (`github_app`, `install_url`, `installations`)
  against a fake handler; `cmd/piper` tests pass with call-site updates.
- **Wizard** (`github_test.go` rewritten around a scriptable `fakeRelay`): no-cred →
  login idle; ↵ → code + URL rendered; pending poll holds state; success saves config
  and advances; `github_app:false` → byo; install poll flips on first installation;
  relay error banners in-view with no `errMsg` emitted (box bar untouched); `m` pushes
  `manifestView`; esc mid-poll pops cleanly. Manifest tests survive the rename.
- **Link form**: filter/select/fill; multi-installation target labels (and their absence
  for one installation); silent fallback on load failure; no-cred hint; `rootDir`
  carried through `linkAppMsg` to `LinkApp` (asserted in `app_test.go`).
- `make verify` gates the PR; no e2e additions — relay flows are unit-faked here, real
  relay coverage lives in the relay/CLI tests.

## Delivery

- One issue: `[cli] TUI: relay GitHub onboarding wizard + repo picker` — labels `cli`,
  `enhancement`, `P2`, `size/M`.
- Branch `ozykhan/tui-github-flows`, PR `Closes #N`, squash-merge.
- Commits, test-first each:
  1. `feat(relay): GitHubStatus returns Status (github_app, install_url)` + CLI call sites
  2. `feat(cli): TUI RelayAPI seam; rename githubView → manifestView`
  3. `feat(cli): TUI github wizard — login, install, installations`
  4. `feat(cli): TUI link form repo picker + root-dir`
  5. `chore: PROGRESS.md`
