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
`~/.local/bin`. Point it at your box with `PIPER_ADDR` (the daemon's control
API binds to loopback by default — override `PIPER_API_ADDR` on the box to
reach it from elsewhere on your LAN):

```bash
PIPER_ADDR=http://your-box:8088 piper list
```

True remote/internet access — driving a box through the relay tunnel instead
of a directly reachable address, with real API auth — isn't built yet; see
[#49](https://github.com/getpiper/piper/issues/49).

Only pre-release builds exist for now, so add `--rc` to install the latest
release candidate:

```bash
curl -fsSL https://raw.githubusercontent.com/getpiper/piper/main/install.sh | sh -s -- --rc
```

The full service install is Linux + systemd; on macOS use `--cli-only` (a
launchd unit is tracked in [#56](https://github.com/getpiper/piper/issues/56)).
Shell completions and a Homebrew tap are planned follow-ups. Prefer to build from
source, or wire your own automation? The manual steps below still work.

## Run the agent as a service (manual / from source)

**Skip this if you used the one-liner above** — it already does everything here for
you. This is the manual equivalent, for building from source or wiring your own
automation. On the box that runs your apps (a Pi, a VPS, a laptop), install the static
`piperd` binary and the shipped systemd unit so the agent runs headless and comes back
on boot (`install` is the coreutils command — copy-into-place with a mode, needing root
to write these system paths):

```bash
sudo install -m 0755 bin/piperd /usr/local/bin/piperd
sudo install -m 0644 packaging/systemd/piperd.service \
  /etc/systemd/system/piperd.service
sudo install -d -m 0700 /etc/piper
sudo install -m 0600 packaging/systemd/piperd.env.example /etc/piper/piperd.env
sudo systemctl daemon-reload
sudo systemctl enable --now piperd
```

The unit runs as a `DynamicUser` in the `docker` group (so piperd can drive the host
Docker daemon), keeps state under `PIPER_DATA_DIR=/var/lib/piper`, and binds `:80`/`:443`
via `CAP_NET_BIND_SERVICE` — no root. Edit `/etc/piper/piperd.env` to override defaults
or switch on relay mode. See the
[end-to-end runbook](docs/runbooks/git-deploy-e2e.md) for verification, logs, and teardown.

## Run the relay as a service

On a Linux relay host, build or download the static `piper-relay` binary, then install
the binary and the shipped systemd unit:

```bash
sudo install -m 0755 bin/piper-relay /usr/local/bin/piper-relay
sudo install -m 0644 packaging/systemd/piper-relay.service \
  /etc/systemd/system/piper-relay.service
sudo systemctl daemon-reload
```

Enroll the box before starting the service, then enable it at boot:

```bash
sudo systemd-run --pipe --wait --collect \
  --property=DynamicUser=yes \
  --property=StateDirectory=piper-relay \
  --setenv=PIPER_RELAY_DATA_DIR=/var/lib/piper-relay \
  /usr/local/bin/piper-relay enroll <name> --domain <base-domain>
sudo systemctl enable --now piper-relay
```

Open inbound TCP ports `443` and `7000`. See the
[end-to-end runbook](docs/runbooks/git-deploy-e2e.md#part-b--relay) for verification,
address overrides, logs, and teardown.

## Git deploys

Once your box runs in relay mode, a `git push` can build and publish an app. Piper
uses a **per-user GitHub App** you create yourself — the private key and webhook
secret never leave your box.

```
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
