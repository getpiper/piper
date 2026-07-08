# Piper

**An open-source, developer-first PaaS that gives you `git push → live HTTPS URL` on
hardware you own — including a Raspberry Pi at home behind CGNAT.**

Piper (Pi + *pipes traffic home*) runs on a single box you control and, via an optional
self-hostable **cloud relay**, tunnels public HTTPS traffic to it without exposing your
network — solving the NAT / CGNAT / dynamic-IP problem that kills most homelab hosting.

- **Zero-trust relay** — the relay only ever sees ciphertext (L4 SNI passthrough); TLS
  terminates on your box. Route through a relay you don't own, safely.
- **Lean** — built to run on a Raspberry Pi. SQLite state, embedded Caddy for TLS.
- **Developer-first** — CLI-driven, Dockerfile-based builds.

> Status: pre-implementation. Design: [`docs/superpowers/specs/2026-07-04-piper-design.md`](docs/superpowers/specs/2026-07-04-piper-design.md).

## Components

- `piperd` — the agent that runs on your box (control-plane, deployer, tunnel-client).
- `piper-relay` — the optional cloud relay (SNI passthrough + tunnel server). Always self-deployable; a hosted instance is offered purely for convenience and runs this same code.
- `piper` — the CLI.

## Install

One line gets a Linux box to a running `piperd` service:

```bash
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh
```

It detects your OS/arch, downloads the matching release binaries, verifies their
`checksums.txt`, installs `piperd` + `piper` to `/usr/local/bin`, drops the
systemd unit and an `/etc/piper/piperd.env` skeleton (never overwriting an edited
one), and runs `systemctl enable --now piperd`. Re-run any time to upgrade.

Install just the CLI (Linux or macOS) — for driving `piperd` from another
machine on the same network (e.g. your laptop and a Pi on the same LAN):

```bash
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh -s -- --cli-only
```

As root this installs `piper` to `/usr/local/bin`; unprivileged, to
`~/.local/bin`. The control API requires a bearer token, so mint one on the
box and log the CLI in first (running `piperd token create` on the box is
itself the proof you own it — no auth needed for that step). The control API
binds to loopback by default — override `PIPER_API_ADDR` on the box to reach
it from elsewhere on your LAN:

```bash
# on the box:
piperd token create --name laptop              # prints a token once
# on the client:
piper login --token <token> --addr http://your-box:8088
piper list                                     # now authenticated
```

`piper login` verifies the token against the box and saves it (with the
address) to `~/.piper/piper/config.json`, mode `0600`; `PIPER_TOKEN` /
`PIPER_ADDR` override the saved values per command. Manage tokens on the box
with `piperd token list` and `piperd token revoke <name>`.

### Join the public relay (self-service)

On a box running `piperd`, log in and claim the box as your normal user:

```bash
piper login          # opens a Google device-flow login; stores your account credential
piper connect        # enrolls this box on the relay
```

`piper connect` enrolls this box. Where it installs the enrollment depends on
the install:

- **Manual / dev** (piperd reads `~/.piper/piperd`): `connect` writes
  `relay.json` there directly, then just `sudo systemctl restart piperd`.
- **Shipped systemd unit** (piperd runs as a `DynamicUser`, state under
  `/var/lib/piper`): that directory isn't writable by your login user, so
  `connect` instead prints a ready `sudo sh -c … /etc/piper/piperd.env` command
  that stores the enrollment in piperd's root-owned EnvironmentFile (systemd
  injects it into the service at start, so its `DynamicUser` never needs to read
  it). Run it, then `sudo systemctl restart piperd`.

Either way piperd picks up the enrollment at startup and dials the tunnel.

`piper login --relay <url>` targets a self-hosted relay instead of the default
`https://api.public.getpiper.co`. Environment variables (`PIPER_RELAY_ADDR`,
`PIPER_RELAY_TOKEN`, `PIPER_BASE_DOMAIN`) still override `relay.json`.

`piper connect` claims the box in **terminated** mode: piperd holds no cert and
serves apps on `:80`; the relay assigns each app a single-label hostname
`<app-hash>-<username>.public.getpiper.co`, terminates its HTTPS with its
wildcard cert, and forwards plaintext HTTP over the tunnel.

```bash
piper login                  # Google device-flow; stores your account credential
piper connect                # claims this box (terminated) and writes relay.json
sudo systemctl restart piperd
piper deploy blog --path .   # → https://<hash>-<you>.public.getpiper.co
```

Bring-your-own-domain apps stay **end-to-end** (the box terminates TLS; the relay
only splices SNI) — set `PIPER_BASE_DOMAIN` + cert/DNS config instead of using
`piper connect`. Self-hosters run the relay passthrough-only by leaving
`PIPER_RELAY_TLS_CERT`/`KEY` unset.

### Drive a box remotely

Any control command (`create`, `deploy`, `list`, `status`, `app link`, `github setup`)
can target one of your relay-connected boxes from anywhere, by the base
domain `piper connect` printed:

```bash
piper --remote ab12-alice.public.getpiper.co list
piper --remote ab12-alice.public.getpiper.co status  # box up? what's deployed?
export PIPER_REMOTE=ab12-alice.public.getpiper.co   # or set it once
piper deploy blog --path .
```

Requests travel relay → tunnel → box: the CLI authenticates to the relay with
the account credential `piper login` saved in `~/.piper/piper/config.json`
(mode `0600`), and the relay swaps it for the box's own token — your relay
credential never reaches the box, and the box still enforces its own auth.
The `--remote` flag overrides `PIPER_REMOTE`; `login` and `connect` are
inherently local and reject `--remote`.

Only pre-release builds exist for now, so add `--rc` to install the latest
release candidate:

```bash
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh -s -- --rc
```

The full service install is Linux + systemd; on macOS use `--cli-only` (a
launchd unit is tracked in [#56](https://github.com/getpiper/piper/issues/56)).
Shell completions and a Homebrew tap are planned follow-ups.

Prefer to build from source, run piperd in Docker via Compose, run the relay as a
service, or wire your own automation? See [`docs/manual-setup.md`](docs/manual-setup.md).

## Git deploys

Once your box runs in relay mode, a `git push` can build and publish an app. Piper
uses a **per-user GitHub App** you create yourself — the private key and webhook
secret never leave your box.

```
piper create myapp --port 8080                       # register the app (needed before it can be linked)
piper github setup [--org name]                      # create the GitHub App (one-time; use --org for org-owned apps)
# install the App on your repo in GitHub, then:
piper app link myapp --repo owner/name --branch main # bind the repo to an app
```

After that, every push to the tracked branch builds the Dockerfile at the repo root,
health-checks the container, and serves it at `https://myapp.<your-domain>`. The live
URL shows up on GitHub as a Deployment status. Webhooks ride the same tunnel as your
traffic (delivered to `hooks.<your-domain>`); nothing else on the box is exposed.

Standing this up against a real relay, domain, and GitHub App end-to-end:
[`docs/runbooks/git-deploy-e2e.md`](docs/runbooks/git-deploy-e2e.md).

## Contributing

- **What's built vs. left:** [`PROGRESS.md`](PROGRESS.md) — a coarse map linking each gap to its issue.
- **Tracked work:** [GitHub issues](https://github.com/getpiper/piper/issues). Titles carry an `[area]` prefix (e.g. `[agent]`, `[cli]`, `[relay]`); `epic` issues track whole plans. New here? Look for [`good first issue`](https://github.com/getpiper/piper/labels/good%20first%20issue).
- **How to work in this repo:** [`CLAUDE.md`](CLAUDE.md) — coding principles, branch workflow, and issue conventions.

Trunk-based: `main` is the only long-lived branch. Branch off `main`, open a PR back into it, and squash-merge.

`main` is protected:

- Changes land only via pull request (no direct pushes); squash-merge only, head branch auto-deleted.
- The CI **`verify`** check (gofmt · `go vet` · `make test` · `make cross`) must pass, and the branch must be up to date, before merging.
- Conversation resolution and linear history required; force-pushes and branch deletion blocked; rules apply to admins too.
- Approving reviews are not yet required (single maintainer) — this bumps to 1 once there's a second reviewer.
