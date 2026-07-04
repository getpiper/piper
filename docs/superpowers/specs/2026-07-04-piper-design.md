# Design: Piper — Self-Hostable Developer PaaS

> **Name:** **Piper** (Pi + *pipes traffic home*). GitHub org: `getpiper`.
> Daemon: `piperd` · relay: `piper-relay` · CLI binary: `piper`.

> **Status:** Design approved in brainstorming (2026-07-04). Not yet implemented.
> This is a **separate project** from the SEO monorepo — move this file into its own repo.

## One-liner

An open-source, developer-first PaaS that gives you `git push → live HTTPS URL` on
hardware you own — including a **Raspberry Pi at home behind CGNAT** — via an optional,
self-hostable **cloud relay** that tunnels public traffic to your box without exposing it.

Think "Coolify/Vercel, but lean enough to run on a Pi, and it solves the home-networking
problem that kills most homelab hosting."

## Target user

Developers / DIY / homelab crowd. CLI-comfortable, happy to write a Dockerfile.
**Not** a no-code / general-consumer product.

## The core insight (why this is worth building)

The hard part of "host it at home" isn't containers — it's **networking**. Home users
have dynamic IPs, sit behind NAT, and increasingly behind **CGNAT**, where they have *no*
inbound public IP and port-forwarding is impossible. DNS-pointing-at-the-Pi cannot work
for a large slice of the audience.

Solution: the Pi dials an **outbound** persistent connection to a **cloud relay**; the
relay accepts public requests and pushes them *down* that existing connection. Because the
connection is outbound-initiated, NAT / CGNAT / dynamic IP all stop mattering. This is the
feature that unlocks the entire audience — effectively a **self-hostable Cloudflare Tunnel**
wired into a PaaS.

---

## Key decisions (locked)

| Decision | Choice | Why |
|---|---|---|
| First runtime substrate | **Single-host Docker/Podman** on one box (Pi or VPS) | Simplest orchestration; best "runs at home" story. Multi-node/ECS become later plugins behind a common `DeployTarget` interface. |
| Public reachability | **Cloud relay**, outbound tunnel from the Pi | Beats NAT/CGNAT/dynamic IP. Relay is optional + self-hostable. |
| TLS termination | **End-to-end passthrough** — TLS terminates **on the Pi** | Zero-trust: the relay only ever sees ciphertext, so you can safely route through a relay you don't own. |
| Relay routing | **L4 SNI passthrough** (reads only the ClientHello SNI) | Keeps the relay tiny, keyless, stateless — minimal attack surface. |
| Certs | **DNS-01** ACME on the Pi | Pi has no inbound `:80` for HTTP-01; DNS-01 needs no inbound. |
| Hostnames | **Managed wildcard by default** (`app.alice.you.dev`), **BYO domain** opt-in later | Instant "it just works" demo + a real-ownership path. v1 ships only the managed path. |
| Build model | **Build on the Pi from a Dockerfile** | Full git-push magic, simplest to implement, ARM-native, nothing leaves the Pi (preserves zero-trust). Accept slower ARM builds. Nixpacks / offloaded builds are later. |
| Control-plane state | **SQLite on the Pi** | One file, no server, ARM-friendly. |
| Developer surface | **CLI-first**; minimal read-only web dashboard later | Devs-only audience. |

---

## Architecture

Three units with clean boundaries.

### 1. `relay` (cloud — optional, self-hostable)
- **Does:** accepts public `:443`, reads only the **SNI** from the TLS ClientHello, and
  passes raw bytes down the matching tunnel. Also the tunnel *server* the agents dial into.
- **Interface:** `tunnel register(auth, [hostnames])` inbound from agents; public HTTPS
  ingress from browsers.
- **Depends on:** nothing. Stateless, holds no keys, does no HTTP parsing.
- **Build vs. buy:** stand on **frp / rathole** for the NAT-busting tunnel transport
  (reconnection, keepalives, stream mux — weeks of subtle work, don't hand-roll it) +
  a **thin SNI-demux shim** in front. This unit stays deliberately small.

### 2. `agent` (the Pi — all the brains)
- **control-plane + state:** HTTP API + **SQLite** holding apps, deployments, domains,
  tunnel creds.
- **tunnel-client:** maintains the persistent outbound connection to the relay,
  (re)registers hostnames on reconnect.
- **deployer:** `docker build` from the repo Dockerfile → `docker run` → health-check →
  swap; keep the old container until the new one is healthy.
- **ingress + TLS + certs → embedded Caddy:** Caddy natively does automatic HTTPS via
  **DNS-01**, host-based reverse-proxy to the right container, and runs on ARM. This
  **collapses three would-be subsystems (cert-manager, TLS termination, host router) into
  "configure Caddy."**
- **webhook receiver:** git push events arrive **down the same relay tunnel** (a reserved
  control hostname) → triggers the deployer. No new inbound path to invent.

### 3. `cli` (developer's machine)
- **Does:** primary developer surface — `create app`, `deploy`, `logs`, `domains`, `env`.
- **Talks to:** the agent API over LAN or through the tunnel.

---

## Data flows

### Runtime — a browser hits a live app
```
browser --TLS--> relay(:443, reads SNI only) --passthrough--> tunnel
   --> Pi: Caddy (terminates TLS, DNS-01 cert) --> app container
```
Relay never decrypts. Pi owns the cert. Works behind CGNAT because the tunnel was dialed
outbound.

### Deploy — git push → live URL (incl. PR previews)
```
git push -> GitHub webhook -> (down relay tunnel) -> agent
  agent: docker build (Dockerfile, on Pi) -> docker run -> health OK
  agent: register hostname app.alice.you.dev with relay + tell Caddy to route it
  -> live.  PR opened  => same flow with pr-42.app.alice.you.dev
  -> PR closed => agent stops container, drops hostname + Caddy route
```

**What we build vs. lean on:** Caddy handles TLS/certs; a tunnel library handles
NAT-busting. Our actual code is the **control-plane, the deployer, the SNI shim, and the
CLI** — the product, not the plumbing.

---

## v1 slice (proposed — to be confirmed)

The thin vertical that delivers the magic on one substrate:

**In scope**
- `agent` on a single Pi/VPS: control-plane + SQLite, deployer (Dockerfile build+run+health),
  embedded Caddy, tunnel-client.
- `relay`: tunnel server (frp/rathole) + SNI passthrough shim.
- Managed wildcard hostnames + DNS-01 certs.
- `cli`: create app, deploy from a git repo, logs, list.
- GitHub webhook → deploy; **PR preview URLs + teardown**.

**Explicitly deferred (proven-later, behind interfaces)**
- BYO-domain (second cert path + DNS UX).
- Multi-node / Docker Swarm / k3s; AWS ECS/Fargate target.
- Nixpacks / buildpacks auto-detect (no-Dockerfile); offloaded/cloud builds.
- Web dashboard (beyond read-only).
- Teams / multi-tenant auth beyond single-owner.
- Relay-side observability (limited by design — it can't see plaintext).

---

## Open questions / risks
- **Relay bandwidth & latency:** all traffic hairpins through the cloud relay — fine for
  hobbyist traffic, a cost/latency factor at scale. Users with a real public IP can skip
  the relay.
- **ARM build performance:** big images can be slow / OOM on a Pi. Mitigation path =
  offloaded builds (deferred).
- **SNI routing vs. ECH:** Encrypted Client Hello would hide SNI and break L4 routing — not
  mainstream yet; ignore for v1.
- **Managed DNS ops burden:** running the default wildcard apex + DNS-01 delegation is
  real infra the project must operate.
