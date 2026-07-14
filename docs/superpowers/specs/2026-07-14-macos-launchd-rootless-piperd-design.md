# macOS: rootless, toggleable piperd (launchd) — design

**Date:** 2026-07-14
**Reframes:** issue #56 (`[repo]` launchd plist), split out of #44 / epic #43.

## Problem

Issue #56 asks for a best-effort macOS `launchd` plist that mirrors the shipped
systemd unit (#44): a boot-surviving, root-privileged daemon that binds `:80`/`:443`.

That framing is wrong for macOS. A Mac is a **development box, not a server**. A
dev does not want a headless root daemon that survives reboot — they want to flip
piperd **on when they start working, off when they're done**, with no friction and
no `sudo`. The systemd posture (dedicated user, `CAP_NET_BIND_SERVICE`, boot
service, `/var/lib/piper`) was inherited from a server context that does not apply.

The blocker to a frictionless macOS experience is privilege: piperd's embedded
Caddy binds `:80`/`:443`, which on macOS requires **root** (no
`CAP_NET_BIND_SERVICE`, no unprivileged-port sysctl). Root forces a `system`-domain
launchd job, which forces `sudo` on every toggle — and leaves a Docker-socket
`$HOME` gotcha. The clean fix is to remove the need for root by making the listen
port configurable and pointing macOS at a high port. This is rootless end to end.

Key insight that de-risks the port change: in **relay mode** (the flagship
"git push → public HTTPS behind CGNAT" path) the *relay* owns public `:443` and
tunnels to the agent; the agent serves plaintext on a loopback port. So the port
piperd binds is only ever visible for **LAN mode** (`http://app.piper.localhost`)
and direct-public serving — **never** for the public relay URL. macOS is scoped
LAN-only here, so a high port has no product-URL cost.

## Goals

- Rootless piperd on macOS: no `sudo` to run or toggle.
- App-like on/off: `piper agent up` / `down` / `status`. Off after reboot.
  (`agent` subcommand — top-level `piper status`/`stop` already mean app status/stop.)
- Self-heal while on (crash → relaunch).
- Linux/Pi behavior **byte-for-byte unchanged** (still `:80`/`:443` via systemd).

## Non-goals (explicitly out of scope)

- **Menu-bar GUI app.** A native status-bar toggle is a separate, larger issue:
  it's a macOS-only signed/notarized GUI, needing either Swift/AppKit or a
  cgo-dependent Go lib — and this repo has a hard **no-cgo** constraint
  (`CGO_ENABLED=0`, arm64 Linux cross-compile). The `piper agent up`/`down` CLI is
  its stepping-stone. File separately.
- **Boot survival on macOS.** Deliberately dropped; dev boxes don't want it.
- **Relay / public mode on macOS.** LAN-only for now. Relay mode's tunnel dials a
  hardcoded `127.0.0.1:80`; we don't touch that path. Revisit if a real need appears.

## Decomposition — three issues, one spec

Dependency order:

1. **(A) `[agent]` — configurable listen address.** *Hard prerequisite.* New
   issue.
2. **(B) `[repo]` — launchd LaunchAgent** (the reframed #56). Depends on (A).
3. **(C) `[cli]` — `piper up`/`down`/`status`.** New issue. Depends on (B).

Each issue references this spec and the chain (`Part of #56` etc.).

---

## (A) `[agent]` configurable listen address

Today `cmd/piperd/main.go` hardcodes the HTTP listener to `":80"` (line ~212) and
the HTTPS listener to `":443"` via `WithHTTPS` (line ~210).

- Add `HTTPAddr` and `HTTPSAddr` to `config.Config`, read from `PIPER_HTTP_ADDR`
  and `PIPER_HTTPS_ADDR`, **defaulting to `:80` and `:443`**.
- `main.go`: pass `cfg.HTTPAddr` to `caddy.StartManager` and
  `caddy.WithHTTPS(cfg.HTTPSAddr)` instead of the literals.
- The relay tunnel path (`net.Dial("tcp", "127.0.0.1:80")`) is **not** changed —
  macOS is LAN-only, so relay mode is not a supported macOS configuration here.

**Test (test-first):** config parses the new env vars; defaults are `:80`/`:443`
when unset; overrides are honored. Existing systemd contract/behavior is unaffected
because the defaults match the old literals.

## (B) `[repo]` launchd LaunchAgent

### Files
- `packaging/launchd/com.getpiper.piperd.plist`
- `packaging/launchd/piperd.env.macos.example`
- `packaging/launchd/piperd_test.go`

### Plist shape
Paths reuse piper's existing per-user layout under **`~/.piper/`**:
`internal/config/config.go` already defaults piperd's data dir to `~/.piper/piperd`
(per-user, rootless) and stores the CLI config at `~/.piper/piper/config.json`.

Two facts shape the env handling. **piperd itself reads no config file** — only env
vars (the `env()` helper); the system's `/etc/piper/piperd.env` is a *systemd*
mechanism (`EnvironmentFile=` injects it). And **Caddy's cert/state storage is not
under `~/.piper` by default** — Caddy uses its own AppData location
(`~/Library/Application Support/Caddy` on macOS) unless `XDG_DATA_HOME` is pinned
(there is no explicit Caddy-storage config in Go; the systemd unit pins XDG for
exactly this reason). Decisions: **pin XDG only** (leave `PIPER_DATA_DIR` to its
`~/.piper/piperd` default) so Caddy co-locates under `~/.piper` and
`piper down && rm -rf ~/.piper` wipes everything; and **introduce a new sourced
override file `~/.piper/piperd.env`** to play the role systemd's env file plays.

- **`ProgramArguments`** — a `/bin/sh -c` wrapper. It carries *all* env (launchd's
  plist dict can't expand `$HOME`, and XDG needs it), sets macOS defaults first,
  then sources the user override file **last so user values win**:

  ```sh
  mkdir -p "$HOME/.piper"
  export XDG_DATA_HOME="$HOME/.piper/piperd" XDG_CONFIG_HOME="$HOME/.piper/piperd"
  export PIPER_HTTP_ADDR=":8080" PIPER_HTTPS_ADDR=":8443"
  set -a
  [ -f "$HOME/.piper/piperd.env" ] && . "$HOME/.piper/piperd.env"
  set +a
  exec >> "$HOME/.piper/piper.log" 2>> "$HOME/.piper/piper.err.log"
  exec /usr/local/bin/piperd
  ```

  `PIPER_DATA_DIR` is deliberately unset → piperd's built-in `~/.piper/piperd`
  default applies (and the user can still override it in the env file). XDG is
  pinned to co-locate Caddy. Ports default to `:8080`/`:8443` (rootless). Every
  default is overridable because the env file is sourced last.

- **No `EnvironmentVariables` dict** — all env lives in the wrapper (single source,
  correct precedence, `$HOME`-portable plist).
- **`RunAtLoad`** = `true`, **`KeepAlive`** = `true` — self-heals while loaded.
  Not boot-surviving: the toggle is bootstrap/bootout (see C), and we never
  auto-bootstrap at install/boot.
- **No `StandardOutPath`/`StandardErrorPath`** — launchd won't tilde-expand them;
  the wrapper redirects to `~/.piper/piper{,.err}.log` itself (after `mkdir -p`).
- `Label` = `com.getpiper.piperd`.

No `sudo` and no pre-created system dirs: the wrapper creates `~/.piper` itself, and
piperd creates its own `~/.piper/piperd` subdir.

### Env example (`piperd.env.macos.example`)
macOS-flavored, copied to `~/.piper/piperd.env` (sourced last, so anything set here
overrides the plist wrapper's defaults):
- Header notes it is per-user, no `sudo`; that `XDG_*` is pinned by the plist to
  co-locate Caddy (override only if you know why); and that `PIPER_DATA_DIR` defaults
  to `~/.piper/piperd` and may be set here to relocate the SQLite DB.
- LAN-only control-plane vars (`PIPER_API_ADDR`, `PIPER_BASE_DOMAIN`,
  `PIPER_CADDY_ADMIN`), all commented at defaults.
- Commented port overrides `#PIPER_HTTP_ADDR=:8080` / `#PIPER_HTTPS_ADDR=:8443`.
- Commented `#DOCKER_HOST=` with a note: piperd reaches Docker via the user's
  Docker Desktop socket; set this only if the default socket isn't found.
- Relay vars **omitted** (macOS is LAN-only).

### Tests (mirror `packaging/systemd/piperd_test.go`)
- Plist contract: asserts `Label`, `RunAtLoad`, `KeepAlive`, the `sh -c` wrapper
  presence, `PIPER_HTTP_ADDR=:8080`, `PIPER_HTTPS_ADDR=:8443`, the `$HOME/.piper`
  data/log paths and `$HOME/.piper/piperd.env` sourcing.
- Env example: asserts `PIPER_API_ADDR`, `PIPER_BASE_DOMAIN`, `DOCKER_HOST` present.
- Doc test (via the existing `repositoryFile` helper): `docs/manual-setup.md`
  mentions `packaging/launchd/com.getpiper.piperd.plist` and `piper agent up`;
  runbook mentions the macOS verify/teardown snippet.

## (C) `[cli]` `piper agent up` / `down` / `status`

A new `agent` subcommand in `cmd/piper` (top-level `status`/`stop` already mean app
status/stop), macOS-only, no `sudo`:

| Command | Action |
|---|---|
| `piper agent up` | `launchctl bootstrap gui/<uid> ~/Library/LaunchAgents/com.getpiper.piperd.plist` |
| `piper agent down` | `launchctl bootout gui/<uid>/com.getpiper.piperd` |
| `piper agent status` | `launchctl print gui/<uid>/com.getpiper.piperd` (summarized: running / stopped / not installed) |

- `uid` from `os.Getuid()`.
- On non-macOS (`runtime.GOOS != "darwin"`): print a clear message pointing at the
  systemd path (`sudo systemctl enable --now piperd`) and exit non-zero. No crash.
- Handle "plist not installed" and "already up/down" with friendly messages, not
  raw `launchctl` errors.

**Test:** OS gate (non-darwin prints the systemd hint); subcommand dispatch calls
the right `launchctl` verb (via an injectable runner). Actual `launchctl`
invocation is not unit-tested (needs a real session) — covered by the manual runbook.

## Docs

- `docs/manual-setup.md`: new **"Run the agent on macOS (dev box)"** section beside
  the systemd block — install `piperd` to `/usr/local/bin`, `install` the plist to
  `~/Library/LaunchAgents/`, copy the env example to `~/.piper/piperd.env`,
  then `piper agent up`. Note it's rootless, LAN-only, high-port, and gone after
  reboot (re-run `piper agent up`).
- `docs/runbooks/git-deploy-e2e.md`: macOS verify/logs/teardown note —
  `piper agent status`, `~/.piper/piper*.log`, `piper agent down`.

## Success criteria

- `make verify` green (gofmt/vet/test/cross), including new (A)/(B)/(C) tests.
- On a Mac with Docker Desktop: copy env example, `piper agent up` → piperd runs as
  the user (no `sudo`), an app deploys and is reachable at
  `http://<app>.piper.localhost:8080`. `piper agent down` stops it. Reboot → off.
- On Linux/Pi: no behavior change; systemd unit and `:80`/`:443` defaults intact.
