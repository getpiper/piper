# Progress

Coarse **map** of what's built vs. what's left — by design. Detail for any 🟡/⬜ item lives in its linked issue (`[#N]`), not here; entries stay to one line so they can't drift from the issue. Design lives in [`docs/superpowers/specs/`](docs/superpowers/specs/); plans in [`docs/superpowers/plans/`](docs/superpowers/plans/); how-to-work in [`CLAUDE.md`](CLAUDE.md).

_Last updated: 2026-07-04 — foundation landed (module skeleton, config, CI); `main` protected. Plan 1 tasks next. Live tracker: [issues](https://github.com/getpiper/piper/issues)._

Legend: ✅ done · 🟡 partial / stubbed · ⬜ not started. Issue tag/label conventions: [CLAUDE.md § Issue tracking](CLAUDE.md#issue-tracking--progress).

## Foundation

- ✅ Go module skeleton + `piper version` + Makefile (build/test/cross) — [#12](https://github.com/getpiper/piper/pull/12)
- ✅ Config loading from env with defaults — [#15](https://github.com/getpiper/piper/pull/15)
- ✅ CI `verify` (gofmt/vet/test/cross) gates PRs; no-cgo arm64 cross-compile green — [#13](https://github.com/getpiper/piper/issues/13)

## Plan 1 — Agent core, LAN-only — epic [#9](https://github.com/getpiper/piper/issues/9) ([plan](docs/superpowers/plans/2026-07-04-piper-agent-core.md))

Goal: `piper deploy myapp --path .` → build Dockerfile → run container → health-check → serve at `http://myapp.piper.localhost` via managed Caddy; state in SQLite. No relay, no tunnel, no git.

- ⬜ `store` — SQLite apps + deployments (pure-Go driver) — [#1](https://github.com/getpiper/piper/issues/1)
- ⬜ `runtime` — Docker build/run/health/stop driver + fake — [#2](https://github.com/getpiper/piper/issues/2)
- ⬜ `caddy` — admin-API client (upsert/remove route) + subprocess manager — [#3](https://github.com/getpiper/piper/issues/3)
- ⬜ `deploy` — orchestrator (build → run → health → record → route → retire) — [#4](https://github.com/getpiper/piper/issues/4)
- ⬜ `api` — control-plane HTTP API (`/v1/apps`, `/v1/apps/{name}/deploy`) — [#5](https://github.com/getpiper/piper/issues/5)
- ⬜ `client` + CLI — `piper create` / `deploy` / `list` — [#6](https://github.com/getpiper/piper/issues/6)
- ⬜ `piperd` wiring (config → store → docker → caddy → deploy → api) — [#7](https://github.com/getpiper/piper/issues/7)
- ⬜ e2e — real Docker + Caddy, deploy sample app, curl it — [#8](https://github.com/getpiper/piper/issues/8)

## Plan 2 — Relay + tunnel + TLS — epic [#10](https://github.com/getpiper/piper/issues/10) (not started)

- ⬜ `piper-relay` (zero-trust SNI passthrough + tunnel server)
- ⬜ Outbound tunnel client in `piperd`
- ⬜ DNS-01 wildcard TLS terminated on-box

## Plan 3 — Git-driven deploys — epic [#11](https://github.com/getpiper/piper/issues/11) (not started)

- ⬜ GitHub webhook → build on push
- ⬜ PR-preview URLs + teardown

## Always-green gates

- `make test` (unit; Docker/e2e skip cleanly when absent) · `make cross` (no-cgo arm64 build)
