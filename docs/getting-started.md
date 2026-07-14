# Getting started

The full journey, in order: install → drive `piperd` from your laptop →
join the public relay → drive a box remotely → git deploys. Each section
builds on the previous one, but you can stop wherever your setup is complete —
a LAN-only box never needs the relay sections.

## Install

One line gets a Linux box to a running `piperd` service:

```bash
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh
```

It detects your OS/arch, downloads the matching release binaries, verifies their
`checksums.txt`, installs `piperd` + `piper` to `/usr/local/bin`, drops the
systemd unit and an `/etc/piper/piperd.env` skeleton (never overwriting an edited
one), and runs `systemctl enable --now piperd`. Re-run any time to upgrade.
Add `--rc` to install the latest release candidate instead of the latest stable
release.

### Rootless on Linux (dev boxes)

Run the installer **without** `sudo` and you get a rootless dev agent — piperd
runs as **you** on high ports (`:8080`/`:8443`) under `~/.piper`, managed by
`systemctl --user`. No root, and on a headless box it does not survive a reboot
(no login to start the user manager — re-run `piper agent up`).

```bash
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh
piper agent up            # start it (no sudo)
piper agent status        # running / stopped / not installed; when running,
                          # prints the control-API address, app ports, and data dir
piper agent down          # stop it
```

Apps are served at `http://<name>.piper.localhost:8080`. Your user must be able to
reach a Docker socket — be in the `docker` group, or set `DOCKER_HOST`.

**If `piper agent up` reports a crash-loop**, the startup error goes to the
`systemd --user` journal, which minimal distros (e.g. Raspberry Pi OS) don't
persist — so `journalctl --user -u piperd` can be empty. Run piperd
in the foreground with the unit's environment to see the real error:

```bash
piper agent down
XDG_DATA_HOME=~/.piper/piperd XDG_CONFIG_HOME=~/.piper/piperd \
  PIPER_HTTP_ADDR=:8080 PIPER_HTTPS_ADDR=:8443 \
  PIPER_CADDY_ADMIN=http://127.0.0.1:2020 ~/.local/bin/piperd
```

A `listen address … already held` error means another piperd (commonly a
leftover system service) owns the port — stop it with `sudo systemctl stop
piperd`.

**Promote to a real daemon.** When you want a durable, boot-surviving service on
`:80`/`:443` (a Pi, a home server), promote it:

```bash
piper agent daemonize
```

No `sudo` — promotion needs root, so `piper` re-runs itself under `sudo` and
prompts for your password. This installs the systemd **system** service (as
`curl | sudo sh` would), stops the rootless one, and also puts `piper` in
`/usr/local/bin` so later root commands (`sudo piperd token …`) resolve by name.
It's a fresh durable install — your rootless `~/.piper` apps are not migrated;
redeploy them.

Install just the CLI (Linux or macOS) — for driving `piperd` from another
machine, e.g. your laptop and a Pi on the same LAN:

```bash
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh -s -- --cli-only
```

As root this installs `piper` to `/usr/local/bin`; unprivileged, to
`~/.local/bin`.

The full service install is Linux + systemd; on macOS use `--cli-only` (a
launchd unit is tracked in [#56](https://github.com/getpiper/piper/issues/56)).
Shell completions and a Homebrew tap are planned follow-ups.

Prefer to build from source, run piperd in Docker via Compose, run the relay as
a service, or wire your own automation? See [`manual-setup.md`](manual-setup.md).

## Drive piperd from another machine on the LAN

The control API requires a bearer token, so mint one on the box and log the CLI
in first. Running `piperd token create` on the box is itself the proof you own
it — no auth needed for that step; on a systemd install it needs `sudo` to reach
the service's data dir and will say so if you forget.

The control API binds to loopback (`127.0.0.1:8088`) by default. To reach it
from another machine on the LAN set `PIPER_API_ADDR=0.0.0.0:8088` on the box —
uncomment it in `/etc/piper/piperd.env` and restart:

```bash
# on the box:
echo 'PIPER_API_ADDR=0.0.0.0:8088' | sudo tee -a /etc/piper/piperd.env
sudo systemctl restart piperd
sudo piperd token create --name laptop         # prints a token once
# on the client — address the box by its IP; mDNS *.local names often
# don't resolve on home LANs (run `hostname -I` on the box to find it):
piper login --token <token> --addr http://192.168.1.50:8088
piper list                                     # now authenticated
```

`piper login` verifies the token against the box and saves it (with the
address) to `~/.piper/piper/config.json`, mode `0600`; `PIPER_TOKEN` /
`PIPER_ADDR` override the saved values per command. Manage tokens on the box
with `sudo piperd token list` and `sudo piperd token revoke <name>`.

## Join the public relay (self-service)

On a box running `piperd`, log in and claim the box as your normal user:

```bash
piper login          # opens a GitHub device-flow login; stores your account credential
piper connect        # enrolls this box on the relay
```

Where `piper connect` installs the enrollment depends on the install:

- **Manual / dev** (piperd reads `~/.piper/piperd`): `connect` writes
  `relay.json` there directly, then just `sudo systemctl restart piperd`.
- **Shipped systemd unit** (piperd runs as a `DynamicUser`, state under
  `/var/lib/piper`): that directory isn't writable by your login user, so
  `connect` instead prints a ready `sudo sh -c … /etc/piper/piperd.env` command
  that stores the enrollment in piperd's root-owned EnvironmentFile (systemd
  injects it into the service at start, so its `DynamicUser` never needs to read
  it). Run it, then `sudo systemctl restart piperd`.

Either way piperd picks up the enrollment at startup and dials the tunnel.

`piper connect` claims the box in **terminated** mode: piperd holds no cert and
serves apps on `:80`; the relay assigns each app a single-label hostname
`<app-hash>-<username>.public.getpiper.dev`, terminates its HTTPS with its
wildcard cert, and forwards plaintext HTTP over the tunnel.

```bash
piper login                  # GitHub device-flow; stores your account credential
piper connect                # claims this box (terminated) and writes relay.json
sudo systemctl restart piperd
piper deploy blog --path .   # → https://<hash>-<you>.public.getpiper.dev
```

`piper login --relay <url>` targets a self-hosted relay instead of the default
`https://api.public.getpiper.dev`. Environment variables (`PIPER_RELAY_ADDR`,
`PIPER_RELAY_TOKEN`, `PIPER_BASE_DOMAIN`) still override `relay.json`.

Bring-your-own-domain apps stay **end-to-end** (the box terminates TLS; the relay
only splices SNI) — set `PIPER_BASE_DOMAIN` + cert/DNS config instead of using
`piper connect`; see [`custom-domains.md`](custom-domains.md). Self-hosters run
the relay passthrough-only by leaving `PIPER_RELAY_TLS_CERT`/`KEY` unset.

## Drive a box remotely

Any control command (`create`, `deploy`, `list`, `status`, `app link`,
`github setup`) can target one of your relay-connected boxes from anywhere, by
the base domain `piper connect` printed:

```bash
piper --remote ab12-alice.public.getpiper.dev list
piper --remote ab12-alice.public.getpiper.dev status  # box up? what's deployed?
export PIPER_REMOTE=ab12-alice.public.getpiper.dev    # or set it once
piper deploy blog --path .
```

Requests travel relay → tunnel → box: the CLI authenticates to the relay with
the account credential `piper login` saved in `~/.piper/piper/config.json`
(mode `0600`), and the relay swaps it for the box's own token — your relay
credential never reaches the box, and the box still enforces its own auth.
The `--remote` flag overrides `PIPER_REMOTE`; `login` and `connect` are
inherently local and reject `--remote`.

## Git deploys

Once your box runs in relay mode, a `git push` can build and publish an app.
Piper uses a **per-user GitHub App** you create yourself — the private key and
webhook secret never leave your box.

```bash
piper create myapp --port 8080                       # register the app (needed before it can be linked)
piper github setup [--org name]                      # create the GitHub App (one-time; use --org for org-owned apps)
# install the App on your repo in GitHub, then:
piper app link myapp --repo owner/name --branch main # bind the repo to an app
```

After that, every push to the tracked branch builds the Dockerfile at the repo
root, health-checks the container, and serves it at `https://myapp.<your-domain>`.
The live URL shows up on GitHub as a Deployment status. Webhooks ride the same
tunnel as your traffic (delivered to `hooks.<your-domain>`); nothing else on the
box is exposed.

Standing this up against a real relay, domain, and GitHub App end-to-end:
[`runbooks/git-deploy-e2e.md`](runbooks/git-deploy-e2e.md).
