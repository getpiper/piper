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

## Progress & contributing

- **What's built vs. left:** [`PROGRESS.md`](PROGRESS.md) — a coarse map linking each gap to its issue.
- **Tracked work:** [GitHub issues](https://github.com/getpiper/piper/issues). Titles carry an `[area]` prefix (e.g. `[agent]`, `[cli]`, `[relay]`); `epic` issues track whole plans. New here? Look for [`good first issue`](https://github.com/getpiper/piper/labels/good%20first%20issue).
- **How to work in this repo:** [`CLAUDE.md`](CLAUDE.md) — coding principles, branch workflow, and issue conventions.

## Contributing

Trunk-based: `main` is the only long-lived branch. Branch off `main`, open a PR back into it, and squash-merge. See [`CLAUDE.md`](CLAUDE.md) for the full workflow and coding principles.

`main` is protected:

- Changes land only via pull request (no direct pushes); squash-merge only, head branch auto-deleted.
- The CI **`verify`** check (gofmt · `go vet` · `make test` · `make cross`) must pass, and the branch must be up to date, before merging.
- Conversation resolution and linear history required; force-pushes and branch deletion blocked; rules apply to admins too.
- Approving reviews are not yet required (single maintainer) — this bumps to 1 once there's a second reviewer.
