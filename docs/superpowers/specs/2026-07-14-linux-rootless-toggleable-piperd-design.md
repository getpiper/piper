# Linux: rootless-by-default, daemonize-on-demand piperd — design

**Date:** 2026-07-14
**Extends:** the macOS rootless work (#207/#56/#208, PR #209). Brings the Linux
agent to parity with the macOS LaunchAgent for the *rootless* tier, and adds an
explicit path to promote it to the existing systemd **system** daemon.

## Problem

The macOS work (PR #209) gave developers a frictionless, rootless, toggleable
agent: `piper agent up/down/status`, high ports, `~/.piper`, no `sudo`, gone after
reboot. Linux has no such tier. Today `install.sh` is binary: run it as **root**
and you get the full systemd **system** daemon (`DynamicUser`, `:80`/`:443`,
`/var/lib/piper`, boot-surviving); run it non-root and it **dies** with *"the full
agent install needs root — re-run with sudo, or use --cli-only"*. A developer who
just wants to try piperd on their Linux laptop has to either `sudo`-install a
boot-surviving root daemon or install the CLI alone with nothing to talk to.

Docker solved this same problem with its **rootless** mode: `curl | sh` installs a
per-user daemon managed by `systemctl --user`, and you opt into the rootful system
service deliberately. We adopt that shape. The Linux analog of a macOS launchd
LaunchAgent is a **systemd user service** (`systemctl --user`), so the same
`piper agent up/down/status` verb works identically on both platforms.

## Goals

- **Rootless by default on Linux:** `curl | sh` (non-root) installs a per-user
  piperd on high ports (`:8080`/`:8443`) under `~/.piper`, no `sudo`.
- **Symmetric toggle:** `piper agent up/down/status` behaves identically on Linux
  (`systemctl --user`) and macOS (`launchctl`).
- **Ephemeral, like macOS:** the rootless agent does **not** survive reboot (no
  `enable-linger`). Re-run `piper agent up`.
- **Deliberate promotion:** `sudo piper agent daemonize` converts a rootless
  install into the existing systemd **system** daemon (durable, `:80`/`:443`,
  boot-surviving).
- **Root install unchanged:** `curl | sudo sh` still installs the system daemon
  exactly as today. Existing systemd unit, `/var/lib/piper`, and Pi/prod flow are
  byte-for-byte unchanged.

## Non-goals (explicitly out of scope)

- **Container-level rootless isolation.** "Rootless" here means *piperd runs as
  your user* — not Docker's user-namespace container isolation. piperd still shells
  out to Docker; your user must reach a Docker socket (be in the `docker` group, or
  set `DOCKER_HOST`).
- **Boot survival for the rootless tier.** No `loginctl enable-linger`. Boot
  survival is exactly what daemonizing buys you.
- **Data migration on promotion.** `piper agent daemonize` does **not** copy
  `~/.piper/piperd` state into `/var/lib/piper`. It is a *fresh* durable install;
  redeploy your apps. (The system service uses `DynamicUser` + `StateDirectory`, so
  pre-seeding its state is awkward and not worth the complexity.)
- **A PID-file supervisor.** We delegate supervision to systemd, as macOS delegates
  to launchd. No `systemctl --user` (minimal/non-systemd distro) → the rootless
  install fails with a clear message; it does not fall back to a hand-rolled
  supervisor.
- **macOS `daemonize`.** macOS is a dev target with no system-daemon tier;
  `daemonize` is Linux-only and errors on macOS.

## The two-tier model

| | Rootless (default) | Daemonized (opt-in) |
|---|---|---|
| Install | `curl \| sh` (non-root) | `sudo piper agent daemonize` |
| Runs as | your user | system (systemd `DynamicUser`) |
| Ports | `:8080`/`:8443` | `:80`/`:443` |
| Data dir | `~/.piper/piperd` | `/var/lib/piper` |
| Supervisor | `systemctl --user` (Linux) · launchd (macOS) | `systemctl` (system) |
| Boot-surviving | no (ephemeral) | yes |
| Toggle | `piper agent up/down/status` | `systemctl` |

The two tiers both bind the control API on `127.0.0.1:8088`, so they **cannot run
simultaneously**. That is why `daemonize` tears the rootless service down as part
of promotion.

## Dependency on PR #209

This work reuses the `PIPER_HTTP_ADDR` / `PIPER_HTTPS_ADDR` config introduced in
PR #209 to point the rootless Linux agent at `:8080`/`:8443`. The implementation
plan bases on `main` **after #209 merges** (or stacks on that branch); until then
the config fields are only present on the `ozykhan/macos-launchd-rootless` branch.

## Decomposition — three surfaces, one spec

- **(A) `[repo]` systemd user unit + install.sh rootless default** (issue: new)
- **(B) `[cli]` `piper agent` Linux branch + `daemonize`** (extends #208)
- **(C) `[docs]` getting-started + manual-setup + runbook** (extends #56 docs)

## (A) `[repo]` systemd user unit + rootless install

### Files
- Create: `packaging/systemd/piperd.user.service` — the rootless user unit.
- Modify: `install.sh` — non-root default now installs rootless.
- Modify: `packaging/systemd/piperd_test.go` — contract test for the user unit.
- Modify: `packaging/install/install_test.go` — rootless-path test.

### User unit shape (`piperd.user.service`)

The Linux twin of the launchd plist. It runs as the invoking user, on high ports,
under `~/.piper`, and self-heals — but does **not** use `DynamicUser`,
`CAP_NET_BIND_SERVICE`, `StateDirectory`, or `WantedBy=multi-user.target` (those
are system-service concerns). Reuses the `PIPER_HTTP_ADDR`/`PIPER_HTTPS_ADDR`
config from #209:

```ini
[Unit]
Description=Piper agent (rootless dev instance)
After=docker.service network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/piperd
Environment=PIPER_HTTP_ADDR=:8080
Environment=PIPER_HTTPS_ADDR=:8443
Environment=XDG_DATA_HOME=%h/.piper/piperd
Environment=XDG_CONFIG_HOME=%h/.piper/piperd
EnvironmentFile=-%h/.piper/piperd.env
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
```

(`%h` is systemd's user-home specifier. The env file is sourced last via
`EnvironmentFile=-…` so operator overrides win, mirroring the plist and the system
unit.)

### `install.sh` change

Privilege-detected, minimal. **Root behavior is unchanged.** The existing system
path already branches on `[ -z "$PIPER_PREFIX" ] && [ "$(id -u)" -ne 0 ]` and
`die`s there. Replace **only that `die`** with the rootless install — the
discriminator is unchanged, so every existing test (which sets `PIPER_PREFIX`)
still takes the system branch:

- **Root, or `PIPER_PREFIX` set:** system install — unchanged
  (`/etc/systemd/system`, `/etc/piper`, `systemctl enable --now`).
- **Non-root and empty `PIPER_PREFIX`** (plain `curl | sh`): rootless install —
  1. download `piperd`+`piper` to `~/.local/bin`,
  2. install `piperd.user.service` → `~/.config/systemd/user/piperd.service`
     (override dir: `PIPER_USER_SYSTEMD_DIR`),
  3. seed `~/.piper/piperd.env` from the shipped example, **skip-if-exists**,
  4. unless `--no-enable` and if `systemctl --user` is available:
     `systemctl --user daemon-reload && systemctl --user enable --now piperd`,
  5. if `systemctl --user` is absent, print a clear message pointing at
     `--cli-only` or `sudo` system install — do **not** hand-roll a supervisor.
- `--cli-only` unchanged.

### Tests
- `packaging/systemd/piperd_test.go`: contract test asserting the user unit
  contains `PIPER_HTTP_ADDR=:8080`, `PIPER_HTTPS_ADDR=:8443`,
  `WantedBy=default.target`, `ExecStart=%h/.local/bin/piperd`, `Restart=on-failure`,
  and **no** `DynamicUser` / `CAP_NET_BIND_SERVICE`.
- `packaging/install/install_test.go`: rootless-path test — non-root, **no
  `PIPER_PREFIX`**, `HOME` set to a temp dir (so `~/.local/bin`,
  `~/.config/systemd/user`, `~/.piper` resolve under it), a stub `systemctl` on
  `PATH`, and `PIPER_USER_SYSTEMD_DIR` overridden; assert `piperd`+`piper` land in
  `$HOME/.local/bin`, the user unit lands in the override dir, and
  `~/.piper/piperd.env` is seeded (and not clobbered on re-run).

## (B) `[cli]` `piper agent` Linux branch + `daemonize`

Extends the existing OS-gated dispatcher in `cmd/piper/agent.go`. Today
`agentGOOS != "darwin"` errors; add a `linux` branch. Introduce a `systemctlRun`
seam (a package-level `var`, twin of the existing `launchctlRun`) so the Linux
paths unit-test without a real systemd.

### Files
- Modify: `cmd/piper/agent.go` — `linux` branch + `daemonize`.
- Modify: `cmd/piper/agent_test.go` — Linux + daemonize tests.
- Modify: `cmd/piper/main.go` — no change to dispatch (already routes `agent`);
  `daemonize` is a fourth subcommand inside `agent`.
- Embed: `packaging/systemd/piperd.service` + `piperd.env.example` into the `piper`
  binary via `go:embed` for `daemonize` to write offline.

### `up` / `down` / `status` (Linux)
- `up` → `systemctl --user start piperd`
- `down` → `systemctl --user stop piperd`
- `status` → `systemctl --user is-active piperd` (+ unit-file presence under
  `~/.config/systemd/user/`) → the same `running` / `stopped` / `not installed`
  vocabulary the macOS branch prints.

### `daemonize` (Linux only, requires root)
1. Gate: macOS → error (`"macOS is a dev target — no system daemon"`); non-root →
   error (`"re-run with sudo"`).
2. Tear down the rootless user service. `sudo piper agent daemonize` runs as root
   but the user service belongs to `$SUDO_USER`, so this is best-effort via
   `systemctl --user --machine=$SUDO_USER@.host disable --now piperd`, with a clear
   fallback message if that fails (*"run `piper agent down` as <user> first"*).
   **This is the highest-risk unit** — its tests must pin the `$SUDO_USER`
   handling.
3. `install -m0755` `piperd` from `~/.local/bin` (of `$SUDO_USER`) → `/usr/local/bin`.
4. Write the embedded `/etc/systemd/system/piperd.service`; seed
   `/etc/piper/piperd.env` from the embedded example, **skip-if-exists**.
5. `systemctl daemon-reload && systemctl enable --now piperd`.

### Tests
- `up`/`down`/`status`: `agentGOOS="linux"` + `systemctlRun` seam; assert command
  shapes (`--user start/stop`, `is-active` parsing → status vocabulary).
- `daemonize`: gating (macOS error, non-root error); with `agentGOOS="linux"`,
  `getuid`/`SUDO_USER` seams, and a stubbed `systemctlRun`, assert the promotion
  sequence (user teardown → binary install → unit write → enable), and that a
  pre-existing `/etc/piper/piperd.env` is not clobbered.

## (C) `[docs]` docs

- `docs/getting-started.md`: rootless `curl | sh` as the default Linux quick-start;
  `piper agent up/down/status`; `sudo piper agent daemonize` to promote; the Docker
  `docker`-group note; the "gone after reboot" ephemerality.
- `docs/manual-setup.md`: a Linux rootless section mirroring the macOS one
  (systemd user unit install by hand, `~/.piper` layout), alongside the existing
  system-service section.
- `docs/runbooks/git-deploy-e2e.md`: Linux rootless verify/teardown note
  (`systemctl --user status piperd`, `journalctl --user -u piperd`, `piper agent down`).
- Doc contract test extends `packaging/install/install_test.go`'s
  `TestInstallDocumentation` to assert the rootless Linux flow + `piper agent daemonize`.

## Success criteria

- `make verify` GREEN (gofmt, vet, all tests incl. new contract/CLI tests, cross
  linux/arm64).
- On a real Linux box: `curl | sh` (non-root) → `piper agent up` (no sudo) →
  `piper agent status` = running → deploy a sample app reachable on `:8080` →
  `sudo piper agent daemonize` → the app is served by the boot-surviving system
  service on `:80` and the rootless user service is gone.
- `curl | sudo sh` still produces the current system daemon, unchanged.
