# Design: Plan 2 — Relay + Outbound Tunnel + On-Box TLS

> **Status:** Design approved in brainstorming (2026-07-04). Not yet implemented.
> Builds on Plan 1 (agent core, LAN-only). Tracked by epic [#10](https://github.com/getpiper/piper/issues/10).
> Parent design: [`2026-07-04-piper-design.md`](2026-07-04-piper-design.md).

## One-liner

Make a live app reachable at a **public HTTPS URL from behind NAT/CGNAT** — the box dials
an **outbound** tunnel to a relay, the relay routes public `:443` traffic down it by SNI,
and TLS terminates **on the box** so the relay only ever sees ciphertext.

## Where this sits

Plan 1 delivered `git-less` LAN deploys: build → run → health → route via managed Caddy on
`http://<app>.piper.localhost`. Plan 2 adds the networking that makes those apps reachable
from the public internet without exposing the box, on the operator's own domain. Plan 3
(git-driven deploys + PR previews) builds on this.

## Positioning: Dokploy-like core, Vercel-like layer deferred

Two models exist for "public HTTPS on a domain":

- **Dokploy / Coolify model** — self-hosted PaaS: the operator brings their **own domain**
  and DNS credentials; the platform solves DNS-01 itself. **This is Plan 2.**
- **Vercel model** — zero-config managed subdomain (`app.you.dev`) where the platform owns
  the apex, runs DNS, and brokers certs centrally. This is the eventual **getpiper-hosted
  relay convenience** and is **deferred** — it layers on top without changing the agent or
  tunnel protocol.

Plan 2 ships the self-hostable foundation. The relay stays tiny and out of the DNS business;
DNS is entirely the agent's concern.

---

## Key decisions (locked in brainstorming)

| Decision | Choice | Why |
|---|---|---|
| Tunnel transport | **Hand-rolled over `hashicorp/yamux`** (one outbound conn, multiplexed streams) | Keeps piper a single pure-Go module (`CGO_ENABLED=0`, arm64). `frp` is a heavy dep; `rathole` is Rust (can't be a Go lib). yamux handles mux/keepalive; our code is just auth + reconnect. |
| TLS / certs | **`go-acme/lego` in-process in `piperd`**, DNS-01; **stock Caddy** loads the PEM | Pure-Go, no `xcaddy` custom builds. DNS-provider code lives in our own binary, not a per-provider custom Caddy. Caddy stays a stock binary that just terminates TLS. |
| Relay trust model | **Per-agent tokens from day one** (enrollment) | One relay can serve many independent agents immediately; the eventual hosted multi-tenant relay needs no protocol change. |
| DNS-01 ownership | **Agent owns the domain + DNS creds** (Dokploy-like) | Relay never touches DNS → stays tiny. Fully self-hostable and locally testable. Relay-brokered managed DNS (Vercel-like) is a later layer. |
| Relay state | **Small SQLite enrollment store on the relay** | Per-agent tokens require persisting `agent → token → allowed base domain`. Zero-trust is intact: the relay holds tokens, never TLS keys or plaintext. A deliberate, documented softening of the parent spec's "stateless relay." |

---

## Architecture

Plan 2 adds one new binary (`piper-relay`), two new agent-side packages (`tunnel`, `certs`),
and extends `caddy`. Layering rule is unchanged: **nothing imports "up"**; `deploy`
orchestrates through interfaces.

### Components

| Unit | Knows only | Depends on | Tested with |
|---|---|---|---|
| `internal/tunnel` | yamux framing + auth handshake (client & server halves) | `yamux`, `net` | in-process loopback (`net.Pipe`) |
| `internal/certs` | ACME DNS-01 via `lego` → PEM + renewal-due logic | `lego`, a `DNSProvider` seam | Pebble (test ACME) + fake DNS solver |
| `cmd/piper-relay` + `internal/relay` | SNI peek, `hostname→session` routing, enrollment store | `tunnel` (server half), SQLite | crafted ClientHello + fake agent |
| `piperd` additions | tunnel-client loop; wire `certs` → `caddy` | `tunnel` (client), `certs` | fakes |
| `internal/caddy` (extend) | add `:443` TLS site + load-PEM admin call | — | admin-API fake |

### Seams

- **`certs.DNSProvider`** — the interface `lego` already exposes; one reference provider
  (Cloudflare) is wired via config, others are drop-in.
- **`internal/relay` store** — its own tiny schema (`agents`: id, token hash, allowed base
  domain), separate from the agent's `store` (different binary, different concern). Reuses
  the `modernc.org/sqlite` + migration *pattern*, not the code.
- **SNI shim** — the relay **peeks then replays**: buffer the TLS ClientHello, parse the SNI
  (via `crypto/tls` ClientHelloInfo capture or a minimal record parser), then stream the
  buffered bytes first down the tunnel. **No decryption ever.**

---

## Data flows

### Runtime — a browser hits a live app
```
browser --TLS--> relay :443  (peek SNI only, no decrypt)
   -> lookup SNI -> agent's live tunnel session
   -> open yamux stream, replay ClientHello + pipe raw bytes both ways
   -> piperd accepts stream -> dials local Caddy :443
   -> Caddy terminates TLS (lego-issued cert) -> app container
```
The relay needs the SNI only to pick the tunnel; Caddy on the box re-reads SNI to route to
the right container. Works behind CGNAT because the tunnel was dialed **outbound**.

### Cert issuance — control-plane, off the hot path
```
piperd: app needs a hostname -> certs pkg runs lego DNS-01
   -> creates _acme-challenge TXT via the agent's own DNS creds
   -> obtains wildcard cert -> stores PEM -> PUTs it to Caddy admin API
   -> renewal loop refreshes before expiry
```
Issuance/renewal happens a handful of times a year per domain and never touches the request
path, so it has no runtime-performance impact; the request path is identical regardless.

### Tunnel protocol
- Agent dials the relay's tunnel port (outbound). Auth handshake: agent presents its
  enrollment **token**; the relay validates it and the base domain that token may claim.
- On success, a **yamux** session multiplexes over that one connection. The agent
  **registers** its hostnames → relay adds them to an in-memory `hostname → session` routing
  map (dropped on disconnect).
- Per public request the relay opens a new yamux stream; the agent dials local Caddy and
  `io.Copy`s both directions. Reconnect uses backoff and **re-registers** on flap.

---

## Config & CLI surface

`piperd` gains config (env, matching Plan 1 style); relay mode is **additive** — with no
`PIPER_RELAY_ADDR`, `piperd` behaves exactly as Plan 1 (LAN-only).

**`piperd`:**
- `PIPER_RELAY_ADDR` — relay tunnel endpoint (empty ⇒ LAN-only)
- `PIPER_RELAY_TOKEN` — enrollment token
- `PIPER_BASE_DOMAIN` — existing; in relay mode it is the real public base (e.g. `alice.example.com`)
- `PIPER_ACME_EMAIL`, `PIPER_ACME_CA` (default Let's Encrypt; Pebble URL in tests)
- `PIPER_DNS_PROVIDER` + provider creds (e.g. `CF_API_TOKEN`) passed to lego

**`piper-relay`:**
- `PIPER_RELAY_TLS_ADDR` (default `:443`), tunnel listen addr, `PIPER_RELAY_DATA_DIR` (SQLite)

**CLI:** the `piper` CLI is unchanged (relay mode is agent-side config, not new dev commands).
The relay gets one admin subcommand: `piper-relay enroll <name> --domain <base>` → prints a token.

---

## Testing strategy

Three tiers keep the "always-green" gate honest.

1. **Unit / in-process (CI, always run):**
   - `tunnel`: loopback session — open stream, echo bytes, auth accept/reject, reconnect re-register.
   - `relay`: crafted TLS ClientHello routes to the right fake session; enrollment/token
     validation; unknown-SNI rejection.
   - `certs`: issuance wiring against **Pebble** + a fake DNS solver (no real DNS/creds);
     renewal-due logic.
   - `caddy`: `:443` site + load-PEM against the admin-API fake.

2. **Loopback e2e (gated, no external creds) — the mechanism proof:** relay + `piperd` +
   Caddy all on localhost with a **self-signed** cert (skip ACME), deploy the sample app, and
   fetch it *through the relay's `:443`* by SNI. Proves tunnel + SNI-demux + passthrough +
   Caddy TLS end-to-end without real DNS or a public host. The Plan-2 analog of Plan 1's e2e.

3. **Real-infra e2e (`RUN_E2E`-gated, docs only, never CI):** real relay host + real domain +
   real DNS creds + Let's Encrypt **staging**. Documented runbook; not automated.

---

## Task breakdown (full detail in the implementation plan)

1. `internal/tunnel` — yamux transport + auth handshake (+ tests)
2. `internal/certs` — lego DNS-01 + renewal (+ Pebble tests)
3. `internal/caddy` — `:443` TLS site + load-PEM
4. `cmd/piper-relay` + `internal/relay` — SNI shim, routing, enrollment store, `enroll` cmd
5. `piperd` tunnel-client loop + wire `certs` → `caddy`; additive config
6. Loopback e2e (tier 2)

Each lands as its own PR/issue under epic #10, TDD-first, with `make test` + `make cross` green.

---

## Non-goals (deferred, behind seams)

- Relay-brokered **managed DNS** (the Vercel-like zero-config path) — later hosted-service layer.
- GitHub webhooks / PR-preview URLs — Plan 3.
- BYO **multi-domain** UX beyond one configured base domain.
- Encrypted Client Hello (ECH) — would hide SNI and break L4 routing; not mainstream, ignored.

## Open questions / risks

- **Relay bandwidth & latency:** all traffic hairpins through the relay — fine for hobbyist
  traffic, a cost/latency factor at scale. Operators with a real public IP can skip the relay.
- **ARM build performance:** unchanged from Plan 1; unrelated to networking.
- **DNS provider coverage:** only one reference provider (Cloudflare) is wired for v1; others
  are drop-in via the lego `DNSProvider` seam but untested until someone needs them.
