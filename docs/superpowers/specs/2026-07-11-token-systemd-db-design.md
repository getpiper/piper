# `piperd token` targets the service DB on systemd installs

**Issue:** #134 · **Date:** 2026-07-11

## Problem

On a box installed via the shipped systemd unit, `piperd token create` run from an
SSH shell writes to `~/.piper/piperd/piper.db` (the no-env default), while the
running service reads `/var/lib/piper/piper.db` (`Environment=PIPER_DATA_DIR=` in
the unit). The token round-trips nowhere: `piper login` gets `401 invalid token`,
silently. The only workaround is a `sudo -u "#UID" env PIPER_DATA_DIR=…` dance the
user has to reverse-engineer.

The obstacle is ownership, not concurrency: `/var/lib/piper` is a `DynamicUser`
`StateDirectory`, mode 0700. The login user can't read it, and a root-owned write
would leave `piper.db-wal`/`-shm` files the DynamicUser can't reopen. Concurrency
is already handled — `store.Open` sets `busy_timeout(5000)` specifically for a
`token create` running alongside the daemon; the service does not need to stop.

## Design

The fix touches only the `piperd token` subcommand path in `cmd/piperd`; the
daemon startup path is unchanged.

### Path resolution

- `PIPER_DATA_DIR` set → respect it, no magic. Explicit always wins.
- Unset + `config.SystemManaged()` (`/etc/piper` exists) → data dir is
  `/var/lib/piper`.
- Unset + not system-managed → today's default (`~/.piper/piperd`), unchanged.

`/var/lib/piper` becomes a `config` var alongside `SystemEnvDir`
(`config.SystemStateDir`), overridable in tests — the same pattern
`SystemEnvDir` already uses.

### Privilege handling (systemd path only)

- **euid 0:** stat `/var/lib/piper` for its owner uid/gid, then
  `Setgroups([])` + `Setgid` + `Setuid` to that owner *before* `store.Open`.
  Files we create stay owned by the DynamicUser, so the service can reopen
  them. (systemd also re-chowns the StateDirectory on service start, so a
  pre-first-start root owner self-heals.)
- **euid ≠ 0:** fail before touching any DB, with the exact command to run:
  `this box is systemd-managed; run: sudo piperd token create --name <name>`.
  The command never silently falls back to `~/.piper` — that silence is the bug.
- **`/var/lib/piper` missing** (service never started): fail with
  "start the service first: `sudo systemctl start piperd`".

### Structure

Two small helpers in `cmd/piperd`:

- `resolveTokenDataDir()` — the path-resolution rules above; returns the data
  dir or an actionable error.
- `dropToStateDirOwner(dir)` — stat + setgroups/setgid/setuid. The syscalls
  exist on both linux and darwin, so no build-constraint split is expected.

No layering changes; `store` stays ignorant of all of this.

### UX after the fix

```
box$  piperd token create --name laptop        # non-root, systemd box
token: this box is systemd-managed; run: sudo piperd token create --name laptop

box$  sudo piperd token create --name laptop
pt_9f3c…                                        # written to /var/lib/piper/piper.db

laptop$ piper login pt_9f3c… http://box:8088   # accepted
```

Non-systemd boxes (dev laptops, `PIPER_DATA_DIR` users) see no behavior change.

## Testing

TDD, unit-level, with `config.SystemEnvDir`/`config.SystemStateDir` pointed at
temp dirs:

- explicit `PIPER_DATA_DIR` wins even when system-managed;
- system-managed + non-root → error message contains the sudo command, and no
  DB is created under `~/.piper`;
- system-managed + state dir missing → "start the service" error;
- not system-managed → home default, unchanged.

The setuid call itself needs root and is not unit-testable; keep the syscall
wrapper trivial and test the uid-resolution (stat → owner) separately.

## README

The setup step for the systemd install becomes
`sudo piperd token create --name laptop`.

## Out of scope

- `piper connect`'s comparable DynamicUser tension (noted in #134 only as
  "worth a glance").
- A daemon admin socket (single-writer purity isn't needed; the store already
  supports a second writer).
- Lower-friction onboarding flows (pairing codes, first-boot bootstrap token) —
  candidates for follow-up issues, deliberately not bundled here.
