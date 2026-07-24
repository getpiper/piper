# Authenticated control API — design

Closes [#72](https://github.com/piperbox/piper/issues/72) — the **gate** for the
remote control-plane epic ([#49](https://github.com/piperbox/piper/issues/49)).
Nothing else in that epic ships until this lands.

## Goal

Give `piperd`'s control API (`127.0.0.1:8088`) real authentication so it can later
be reached through the relay without inheriting today's "you're on the box's
loopback, so you're trusted" assumption. Every `/v1/*` request must carry a bearer
token, **always** — no transport-based fast path. This issue stays LAN-only; it
only makes the API demand a token. Exposing it remotely is a later issue.

## Why "always" — the loopback trap

Today the control API binds `127.0.0.1:8088`. The kernel guarantees only a process
on the same host can connect to a loopback address, so "a connection exists" is a
reliable proxy for "the caller is on this box." That is the entire security model.

The relay work ([#73](https://github.com/piperbox/piper/issues/73)) reuses the
existing **outbound** tunnel to reach the box. Look at how the tunnel moves bytes
today (`internal/agent/tunnelclient.go`, `serveStreams` → `dialLocal`): piperd
holds a yamux session dialed *out* to the relay; when the relay has traffic, piperd
`Accept()`s a stream and calls `dialLocal()`, which opens a **fresh local TCP
connection** and `io.Copy`s both directions. For app traffic that dial is
`127.0.0.1:443`; for a control stream it would be `127.0.0.1:8088`. So a remote
caller's path is:

```
remote piper CLI ──HTTPS──▶ relay ──(tunnel stream)──▶ piperd ──dial 127.0.0.1:8088──▶ control API
```

The API sees an `http.Request` whose `RemoteAddr` is **piperd dialing its own
loopback** — literally `127.0.0.1:<ephemeral>`. It cannot distinguish that from the
operator running `piper` on the box. A reverse tunnel through NAT *deliberately
launders remote connections into local ones* — that is the whole point of it — so
the loopback signal becomes meaningless the moment control traffic can ride the
tunnel. Any rule shaped like "if `RemoteAddr` is loopback, skip auth" would trust
every remote caller. Therefore auth must be a **credential carried end-to-end in
the HTTP request** (opaque bytes that survive the tunnel), enforced **uniformly**,
with no local bypass.

## Decisions (settled during brainstorming)

- **`piper login` / token-paste** is the auth UX, not a silently auto-provisioned
  token. The project is expected to be used mostly via hosted public relays + a
  dashboard, where login is the norm regardless; unifying on one login path is
  simpler than a divergent local fast-path.
- **Enforce always.** On a fresh box the API returns `401` for everything until the
  operator mints a token. One code path, never open. (The rejected alternative —
  "open until first token for LAN-only" — adds a locked/unlocked state to reason
  about and softens the gate.)
- **Scope column now, enforcement later.** Add a `scope` column defaulted to
  `admin` to dodge a future migration, but do **not** build read-only enforcement
  in this issue — there is no read-only consumer until the dashboard
  ([#76](https://github.com/piperbox/piper/issues/76)) / metrics
  ([#75](https://github.com/piperbox/piper/issues/75)).
- **Same box bypasses the relay by construction.** Transport is decided purely by
  the target address in the CLI config: a local context is
  `http://127.0.0.1:8088`, a direct loopback hop with the relay never involved. The
  relay exists only to give a *remote* caller a path to a box behind NAT. There is
  **no co-location auto-detection** — the local context's address simply *is*
  localhost, so it is direct. Auth (a token) is still required on both transports.

## Two homes, two secrets

| | CLI side (`piper`) | Daemon side (`piperd`) |
|---|---|---|
| **Home** | `~/.piper/piper/` | `~/.piper/piperd/` (data dir) |
| **Holds** | plaintext token, active target | token **hash** only |
| **Trust anchor** | you logged in | you can run `piperd` on the box |

Parallel subdirs under `~/.piper/` so the CLI and daemon never collide on a shared
box:

```
~/.piper/
├── piper/      CLI: config.json (token + active target); later CLI logs
└── piperd/     daemon: piper.db (SQLite); later server logs
```

The token's plaintext lives only in `~/.piper/piper/config.json`; only its sha256
hash lives in the daemon's DB, mirroring the existing `internal/relay/store.go`
pattern (`hashToken`, store hash, return plaintext once).

## Components

### 1. Store — `internal/store`

New `tokens` table and methods, following the relay token idiom:

```sql
CREATE TABLE IF NOT EXISTS tokens (
    id         TEXT PRIMARY KEY,
    label      TEXT NOT NULL UNIQUE,
    token_hash TEXT NOT NULL UNIQUE,
    scope      TEXT NOT NULL DEFAULT 'admin',
    created_at TEXT NOT NULL,
    revoked_at TEXT
);
```

- `CreateToken(label, scope) (plaintext string, err error)` — 32 random bytes,
  hex-encoded; store `hashToken(plaintext)`; return plaintext **once**.
- `AuthenticateToken(plaintext) (Token, error)` — hash → lookup; return
  `ErrNotFound`/`ErrBadToken` when unknown or `revoked_at` is set.
- `ListTokens() ([]Token, error)` — never returns plaintext or hash to callers that
  print it; label/scope/created_at/revoked_at only.
- `RevokeToken(label) error` — sets `revoked_at`; `ErrNotFound` if absent.

No `last_used_at`: it would add a write to the auth hot path (lock contention) for
little value here. Additive migration in the existing idempotent
`CREATE IF NOT EXISTS` / `ALTER … ADD COLUMN` style already in `store.go`.

Reuse `hashToken` (sha256 → hex). It currently lives in `internal/relay/store.go`;
duplicating the three-line helper in `internal/store` is fine (no shared crypto
package needed for one function, and `store` must not import `relay`).

### 2. API middleware — `internal/api`

`api.New` already holds `*store.Store`. Wrap the returned mux with an auth checker:
extract `Authorization: Bearer <token>` → `AuthenticateToken` → `401` on
missing/malformed/invalid/revoked, else serve. All existing `/v1/*` handlers sit
behind it unchanged.

- The GitHub OAuth endpoints (`/v1/github/manifest`, `/v1/github/exchange`) are
  called by the authenticated `piper github setup` flow, so they sit behind auth
  too — no unauthenticated carve-out.
- The **webhook listener** is a separate server on `:8089`, already authenticated
  by GitHub's HMAC signature. It is **out of scope** and untouched.
- No health/liveness endpoint is added here (that is #75).

### 3. Token issuance — `piperd` subcommand

`cmd/piperd/main.go` has no argument parsing today; add a small switch so the
default (no args) still runs the daemon, plus:

- `piperd token create --name <label>` — open the store at `PIPER_DATA_DIR`, mint,
  print the plaintext **once** to stdout.
- `piperd token list`
- `piperd token revoke <label>`

Direct-to-DB, no daemon call, no auth — this is the bootstrap root of trust:
running it *is* proof of box ownership (the same trust loopback gave us, made
explicit). Minor: the SQLite file may be open by a running daemon; a brief
write-lock on a rare admin op is acceptable (no WAL change needed).

### 4. CLI — `piper login` + config

- `piper login [--token <t>]` — prompt for (or take) a pasted token, **verify** it
  by reusing the existing `GET /v1/apps` call against the target (no new endpoint),
  then write `~/.piper/piper/config.json` (mode `0600`). Reject and do not save on
  `401`.
- `config.json` (minimal, forward-compatible):
  ```json
  { "addr": "http://127.0.0.1:8088", "token": "<plaintext>" }
  ```
  #74 (remote target) generalizes this into named contexts; the file is introduced
  here with a deliberately small struct.
- `internal/client` reads the token from `~/.piper/piper/config.json` and sets the
  `Authorization` header on every request. `PIPER_TOKEN` env overrides the file
  (for CI/scripting). `internal/config` gains a `~/.piper/piper` path helper;
  `PIPER_DATA_DIR` default moves to `~/.piper/piperd` (env override unchanged;
  service installs set it explicitly, so they are unaffected).

### 5. Daemon default data dir

`internal/config` default for `PIPER_DATA_DIR` changes from `./data` to
`~/.piper/piperd`. Env override is unchanged; systemd/container installs already
set `PIPER_DATA_DIR` explicitly (`/var/lib/piper`, etc.), so this only affects
interactive/dev runs.

## Verification (TDD, failing-test-first)

- **store**: create → plaintext returned once; `AuthenticateToken` valid → token,
  unknown → `ErrBadToken`, revoked → `ErrBadToken`; `RevokeToken` flips it;
  duplicate label → error.
- **api**: httptest — no header → `401`; malformed header → `401`; bad token →
  `401`; valid token → handler runs (`200`/existing behavior). **Existing api
  handler tests are updated to send a valid token** — a deliberate contract change,
  part of this work.
- **client**: attaches the `Authorization` header from config; `PIPER_TOKEN`
  overrides; missing credentials → a clear "run `piper login`" error.
- `make test` and `make cross` green (pure Go, no cgo added).

## Out of scope (later issues)

- Relay control-stream routing + `caller → agent` authz — #73.
- `piper --remote` / named remote contexts — #74.
- Read-only scope **enforcement** and health/metrics surface — #75.
- Hosted dashboard consuming this surface — #76.
- CLI/daemon logs under `~/.piper/*/` — the folders are introduced here; a logging
  subsystem is not built now.
