# Progress

Coarse **map** of what's built vs. what's left — by design. Detail for any 🟡/⬜ item lives in its linked issue (`[#N]`), not here; entries stay to one line so they can't drift from the issue. Design lives in [`docs/superpowers/specs/`](docs/superpowers/specs/); plans in [`docs/superpowers/plans/`](docs/superpowers/plans/); how-to-work in [`CLAUDE.md`](CLAUDE.md).

_Last updated: 2026-07-07 — epic #43 (install & run piperd as a service) closed; registry publish and macOS launchd ([#56](https://github.com/getpiper/piper/issues/56)) tracked as standalone follow-ups. Plan 3 complete: push-to-deploy plus PR-preview URLs + teardown (`pr-<N>-<app>.<base>`, flattened for the wildcard cert). Live tracker: [issues](https://github.com/getpiper/piper/issues)._

Legend: ✅ done · 🟡 partial / stubbed · ⬜ not started. Issue tag/label conventions: [CLAUDE.md § Issue tracking](CLAUDE.md#issue-tracking--progress).

## Foundation

- ✅ Go module skeleton + `piper version` + Makefile (build/test/cross) — [#12](https://github.com/getpiper/piper/pull/12)
- ✅ Config loading from env with defaults — [#15](https://github.com/getpiper/piper/pull/15)
- ✅ CI `verify` (gofmt/vet/test/cross) gates PRs; no-cgo arm64 cross-compile green — [#13](https://github.com/getpiper/piper/issues/13)
- ✅ Release pipeline (goreleaser: tag → GitHub Release + cross-compiled binaries/checksums); unblocks installer/image — [#58](https://github.com/getpiper/piper/issues/58)

## Plan 1 — Agent core, LAN-only — epic [#9](https://github.com/getpiper/piper/issues/9) ([plan](docs/superpowers/plans/2026-07-04-piper-agent-core.md))

Goal: `piper deploy myapp --path .` → build Dockerfile → run container → health-check → serve at `http://myapp.piper.localhost` via managed Caddy; state in SQLite. No relay, no tunnel, no git.

- ✅ `store` — SQLite apps + deployments (pure-Go driver) — [#17](https://github.com/getpiper/piper/pull/17)
- ✅ `runtime` — Docker build/run/health/stop driver + fake — [#19](https://github.com/getpiper/piper/pull/19)
- ✅ `caddy` — admin-API client (upsert/remove route) + in-process manager (Caddy embedded as a library) — [#3](https://github.com/getpiper/piper/issues/3), [#39](https://github.com/getpiper/piper/issues/39)
- ✅ `deploy` — orchestrator (build → run → health → record → route → retire) — [#22](https://github.com/getpiper/piper/pull/22)
- ✅ `api` — control-plane HTTP API (`/v1/apps`, `/v1/apps/{name}/deploy`) — [#23](https://github.com/getpiper/piper/pull/23)
- ✅ `client` + CLI — `piper create` / `deploy` / `list` — [#24](https://github.com/getpiper/piper/pull/24)
- ✅ `piperd` wiring (config → store → docker → caddy → deploy → api) — [#7](https://github.com/getpiper/piper/issues/7)
- ✅ e2e — real Docker + Caddy, deploy sample app, curl it — [#8](https://github.com/getpiper/piper/issues/8)

## Plan 2 — Relay + tunnel + TLS — epic [#10](https://github.com/getpiper/piper/issues/10) ([plan](docs/superpowers/plans/2026-07-04-piper-relay-tunnel-tls.md))

Goal: public HTTPS from behind NAT/CGNAT — `piperd` dials an outbound yamux tunnel to `piper-relay`, which routes public `:443` by SNI (never decrypts); TLS terminates on-box with a lego-issued wildcard cert. Agent owns the domain + DNS creds (Dokploy-like).

- ✅ `tunnel` — yamux transport + token/base-domain handshake — [#10](https://github.com/getpiper/piper/issues/10)
- ✅ `certs` — lego DNS-01 wildcard issuance + renewal — [#10](https://github.com/getpiper/piper/issues/10)
- ✅ `caddy` — `:443` TLS listener + load-PEM — [#10](https://github.com/getpiper/piper/issues/10)
- ✅ `piper-relay` — enrollment (per-agent tokens), SNI passthrough, tunnel server — [#10](https://github.com/getpiper/piper/issues/10)
- ✅ `piper-relay` managed systemd service + operator docs — [#38](https://github.com/getpiper/piper/issues/38)
- ✅ `piperd` — outbound tunnel client + cert wiring (additive; LAN-only unchanged) — [#10](https://github.com/getpiper/piper/issues/10)
- ✅ e2e — loopback relay path (tunnel + SNI + on-box TLS) — [#10](https://github.com/getpiper/piper/issues/10)

## Plan 3 — Git-driven deploys — epic [#11](https://github.com/getpiper/piper/issues/11) ([plan](docs/superpowers/plans/2026-07-05-plan3-git-deploys.md))

Goal: `git push → live HTTPS URL` via a per-user GitHub App; webhook rides the Plan-2 tunnel to `hooks.<base>`; status reported to GitHub.

- ✅ `source` — provider seam (Event/Kind/Status + Provider interface) — [#31](https://github.com/getpiper/piper/pull/31)
- ✅ `source/github` — App JWT + installation token, webhook parse (HMAC), tarball fetch, Deployments API, manifest onboarding — [#31](https://github.com/getpiper/piper/pull/31)
- ✅ `webhook` — signed webhook → app lookup → deploy, per-app serialization — [#31](https://github.com/getpiper/piper/pull/31)
- ✅ `api`/`cli` — `github setup`, `app link`, onboarding endpoints — [#31](https://github.com/getpiper/piper/pull/31)
- ✅ `piperd` — webhook served over the tunnel in relay mode — [#31](https://github.com/getpiper/piper/pull/31)
- ✅ PR-preview URLs + teardown (`pr-<N>-<app>.<base>`, flattened for the wildcard cert) — [#50](https://github.com/getpiper/piper/pull/50)

## Install & run piperd as a service — epic [#43](https://github.com/getpiper/piper/issues/43) ✅ closed

Goal: piperd installable and self-sustaining on the box (Pi/VPS/laptop) — service unit, container image, one-line installer — without changing how it uses Docker for apps.

- ✅ Graceful `SIGTERM` shutdown (clean service stop/restart) — [#48](https://github.com/getpiper/piper/issues/48)
- ✅ Native systemd unit (`DynamicUser`+`docker` group, `CAP_NET_BIND_SERVICE`, `StateDirectory`) — [#44](https://github.com/getpiper/piper/issues/44)
- ✅ Container image + compose (host `docker.sock`; registry publish tracked separately) — [#45](https://github.com/getpiper/piper/issues/45)
- ✅ One-line `curl … | sh` installer (OS/arch detect, checksum-verified, `--cli-only`/`--rc`) — [#46](https://github.com/getpiper/piper/issues/46)
- ✅ Standalone `piper` CLI on PATH (`--cli-only`; drives a `piperd` on another host on the same network via `PIPER_ADDR`) — [#47](https://github.com/getpiper/piper/issues/47)

Descoped from the epic, tracked standalone:
- ⬜ launchd plist (best-effort macOS) — [#56](https://github.com/getpiper/piper/issues/56)

## Always-green gates

- `make test` (unit; Docker/e2e skip cleanly when absent) · `make cross` (no-cgo arm64 build)
