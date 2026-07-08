# Relay-terminated shared domain — design

Design for **Plan 3 of 3** of the public-relay onboarding slice: turning a
self-enrolled free-tier box into a **live HTTPS URL on the operator's shared apex**
with zero further user action. Plan 1 (relay accounts + Google device-flow +
account-bound enrollment) and Plan 2 (`piper login` / `piper connect` writing
`relay.json` → piperd dials the tunnel) have landed. What is still missing is the
last hop: the app is reachable *through the tunnel* but has no public hostname and
no TLS on the shared domain.

This spec is the concrete instantiation of the **relay-terminated wildcard app-TLS
path** that the parent
[public-relay onboarding design](2026-07-07-public-relay-onboarding-design.md)
settled and deferred to "Plan 3". It fixes the naming, the routing, the one new
tunnel primitive, and the termination flow. Exact wire bytes and CLI copy belong
to the implementing plan.

## Scope

**In scope:** per-app single-label hostnames assigned by the relay; an explicit
`hostname → session` routing registry; a **typed-stream** framing on the existing
tunnel; the relay `:443` **SNI branch** (terminate under `*.public.getpiper.co` vs
today's passthrough); relay TLS termination with a static wildcard cert; piperd
free-tier ("terminated") mode + deploy-time hostname registration; per-account
**app cap** and **kill-switch** on the terminated path; and the full
`login → connect → deploy → curl the assigned hostname` loopback e2e.

**Out of scope (each a documented follow-up):** free-tier **PR-preview** hostnames
(they inherit this exact machinery — a thin follow-up); relay-side **DNS-01**
issuance of the wildcard (operator supplies a static PEM first); the #73 **Token-B
caller→box control plane** (a different trust direction, deferred); tiers / billing
/ real quotas; Vercel-style custom domains; a box-terminated (E2E) free-tier cert
path.

## Where Plan 2 leaves off, and the gap Plan 3 closes

Today the tunnel is **pure SNI-passthrough**. The agent dials the relay presenting
`(enrollment token, base_domain)`; the relay registers one session keyed by
`base_domain` (`internal/relay/router.go`), and public `:443`
(`internal/relay/server.go` + `sni.go`) peeks the SNI, aborts the handshake,
replays the recorded ClientHello down a fresh stream, and splices raw ciphertext —
**the box terminates TLS**.

Two facts from the current code force the shape of Plan 3:

1. **The Plan-1/2 free-tier `base_domain` cannot carry per-app names under one
   wildcard.** `internal/deploy/deploy.go` composes an app host as
   `hostFor(app) = app + "." + baseDom`. With a free-tier `base_domain` such as
   `ab12-alice.public.getpiper.co`, that yields `app.ab12-alice.public.getpiper.co`
   — **two labels** under the apex, which `*.public.getpiper.co` does not cover.
   The parent design's naming decision (`<app-hash>-<username>.public.getpiper.co`,
   a *single* label) is therefore not something the box can compose from its
   `base_domain`; **the relay must own the naming** and hand the box the full
   assigned hostname.

2. **Termination is the symmetric twin of passthrough — no HTTP proxy needed.**
   Passthrough peeks SNI, replays the ClientHello, and pipes *ciphertext* to the
   box's `:443`. Termination completes a real `tls.Server` handshake with the
   wildcard cert, then pipes *plaintext* to the box's `:80`, where Caddy already
   routes by `Host` (Plan 1). The relay stays a byte-pump; it never parses HTTP.

## Decisions (settled during brainstorming)

- **Hostname registration rides the agent's already-authenticated tunnel — not the
  #73 control plane.** The #73 Token-B plane is *caller→box*; hostname registration
  is *agent→relay*, and the agent's session is already authenticated by the
  enrollment handshake. So the box opens a **control stream** on its own session and
  asks the relay to assign/register a hostname — no Token B, no account credential,
  no new auth. This keeps Plan 3 self-contained and off the deferred epic.
- **The relay owns naming.** The box sends only the app name; the relay derives and
  returns the full `<app-hash>-<username>.public.getpiper.co`. The box never
  composes the hostname and does not need to know its own username.
- **Relay terminates with a static wildcard PEM first.** The operator supplies the
  `*.public.getpiper.co` cert/key via config; the relay loads it. Relay-side DNS-01
  issuance is a follow-up, so the plan carries no ACME/network dependency and is
  fully unit-testable with a self-signed wildcard.
- **One cohesive plan.** The typed-stream framing threads through both `tunnel` and
  `agent` and both the relay and box sides of routing; splitting would leave an
  awkward half-working intermediate. Tasks remain individually shippable/committed.
- **`terminated` is a mode marker in `relay.json`.** A box claimed by `piper
  connect` is always relay-terminated; the marker lets piperd skip box TLS, serve
  apps on `:80`, and register hostnames on deploy — without inferring mode from the
  absence of cert config.

## The one new primitive: typed tunnel streams

Every stream over the yamux session now opens with a **1-byte kind**. The relay and
agent ship together in one pre-1.0 module, so this is a clean protocol bump, not a
compatibility concern.

| Kind | Direction | Meaning | Box action |
| --- | --- | --- | --- |
| `T` | relay → agent | passthrough; the replayed ClientHello follows | dial `127.0.0.1:443`, splice ciphertext (today's path, now tagged) |
| `H` | relay → agent | relay-terminated plaintext HTTP | dial `127.0.0.1:80` (Caddy HTTP, routes by `Host`) |
| `C` | agent → relay | control request | serve a length-prefixed JSON register/deregister |

Both ends grow a small Accept loop that dispatches on the kind byte (the agent
opens only `C`; the relay opens only `T`/`H`, but each side reads the byte
explicitly so the framing is direction-agnostic and future-proof). Control frames
reuse the existing `writeFrame`/`readFrame` length-prefix helpers in
`internal/tunnel/tunnel.go`.

## Components

### `internal/tunnel`

- Add the kind byte to stream open/accept: a helper to open a stream and write its
  kind, and to accept a stream and read its kind. Keep `writeFrame`/`readFrame` for
  the control payloads.

### `internal/relay`

- **Routing registry** — add `byHost map[string]*Session` beside today's `byBase`.
  Populated by the `C`-stream `register`, torn down by `deregister` and on session
  close. Lookup order at `:443`: exact `byHost` match → **terminate**; else
  `byBase` suffix match → **passthrough** (unchanged).
- **Control-stream handler** — after registering a session, accept the agent's `C`
  streams. `register{app}` → app-cap + kill-switch check → derive/record hostname →
  add to `byHost` → return `{hostname}`. `deregister{hostname}` → remove.
- **`hostnames` table** — `hostname → (account, agent, app)`, backing the per-account
  **app cap** and records; a `disabled` flag participates in the kill-switch.
- **Naming** — `hostname = short(hash(app)) + "-" + <username> + "." + <apex>`,
  idempotent per `(account, app)`. Label capped at 63 chars (truncate `<username>`;
  the hash preserves uniqueness). `<username>` and uniqueness come from Plan 1.
- **`:443` terminate branch** — on an exact `byHost` SNI, complete a `tls.Server`
  handshake with the wildcard cert, feeding the buffered ClientHello back through a
  prefix-conn (first drain the recorded bytes, then read the live conn), then open
  an `H` stream and pipe plaintext ↔ box `:80`. Disabled account/hostname → refuse.
- **Wildcard cert** — load cert+key from configured paths (`PIPER_RELAY_TLS_CERT` /
  `PIPER_RELAY_TLS_KEY`). No ACME in this plan.

### `piperd` / `internal/agent` / `internal/deploy`

- **Terminated mode** — `relay.json` gains `terminated: true` (written by `piper
  connect`). In terminated mode piperd **skips** `setupRelayTLS` and
  `caddy.WithHTTPS(":443")` (the box holds no cert) and Caddy serves apps on `:80`.
- **Typed accept** — the agent's stream-accept loop reads the kind byte: `T` → dial
  `:443` (existing), `H` → dial `:80`.
- **Deploy-time registration** — in terminated mode the deploy orchestrator opens a
  `C` stream, `register{app}`, receives the assigned hostname, and adds a Caddy
  route `hostname → app upstream`. `deregister` on teardown. This replaces the
  `app.baseDom` host composition for the terminated path.

### `piper` CLI / `internal/config`

- `piper connect` writes `terminated: true` into `relay.json`. `config.RelayFile`
  and `Load()` carry the flag (env override preserved, per Plan 2 precedence).

## Data flows

### Deploy (terminated mode)

```
piper deploy
  └─▶ piperd: build + run + health (Plan 1)
        └─▶ piperd ──C stream──▶ relay: register{app}
              └─▶ relay: app-cap + kill-switch → derive <app-hash>-<username>.<apex>
                    → record hostname→(account,agent,app) → add to byHost
                          └─▶ relay returns {hostname}
                                └─▶ piperd: Caddy route hostname→app on :80 → live
```

### Visitor request (relay-terminated)

```
browser ──HTTPS──▶ relay :443
   SNI = <app-hash>-<username>.public.getpiper.co  (exact byHost hit)
     └─▶ relay completes TLS with the *.public.getpiper.co wildcard cert
           └─▶ open H stream to the app's session
                 └─▶ pipe plaintext ↔ box :80 (Caddy routes by Host) ─▶ app ─▶ response
```

### Passthrough (BYO domain, unchanged)

```
browser ──HTTPS──▶ relay :443
   SNI not in byHost, suffix-matches a byBase base_domain
     └─▶ replay ClientHello down a T stream ─▶ box :443 terminates ─▶ app
```

### Kill-switch

```
operator disables account|hostname
  └─▶ register refuses; byHost lookup refuses; existing sessions of a disabled
      account are rejected on (re)connect (Plan 1 Authenticate, unchanged)
```

## Error handling & edge cases

- **App cap exceeded** — clear, actionable error at deploy; no Caddy route added,
  no partial state.
- **Label > 63 chars** — truncate `<username>`; `<app-hash>` preserves uniqueness.
- **Re-deploy** — hostname derivation is idempotent per `(account, app)`; the same
  hostname is returned and the route upserted.
- **Deregister / teardown** — removes the `byHost` entry and the Caddy route; a
  missing entry is a no-op.
- **Session loss** — on session close the relay drops that session's `byHost`
  entries; the box re-registers its live apps on reconnect + next deploy.
- **Account disabled mid-session** — routing and registration refuse; the box owner
  still holds `piperd token revoke` for the control plane (trust spec).
- **Wildcard cert config** — if cert/key paths are set but the files are
  missing/invalid, the relay fails fast at startup with a clear error. If no cert
  paths are configured, the relay runs **passthrough-only** (the terminate branch is
  never armed and a `byHost` hit with no cert is refused) — self-hosters who only do
  BYO-domain need no wildcard.

## Testing (TDD, `CGO_ENABLED=0`, arm64 cross)

- **tunnel unit** — kind-byte round-trip; `T`/`H`/`C` open/accept dispatch.
- **relay unit** — control-stream register/deregister; hostname derivation +
  idempotence + uniqueness + 63-char truncation; app-cap enforcement; kill-switch
  refusal; the `:443` **terminate-vs-passthrough** branch driven with a self-signed
  wildcard cert.
- **agent/piperd unit** — kind-byte dial selection (`H`→`:80`, `T`→`:443`);
  deploy-time registration against a fake relay control stream; `relay.json`
  `terminated` round-trip + env override.
- **Loopback e2e (Docker-gated)** — extend the Plan-2 loopback relay test with a
  **fake Google IdP**: `piper login` → `piper connect` → `piper deploy` → **`curl`
  the assigned hostname through the relay-termination path** and assert the app
  responds. Skips cleanly when Docker is absent.
- Existing gates unchanged: `make test`, `make cross`.

## Issue reconciliation

- Completes the parent onboarding slice: with this plan a self-enrolled free-tier
  box is served end-to-end on `*.public.getpiper.co`, closing the "not yet served
  end-to-end" caveat noted in the Plan-2 CLI plan and epic
  [#49](https://github.com/getpiper/piper/issues/49) /
  [#10](https://github.com/getpiper/piper/issues/10).
- Confirms the parent design's **narrowed invariant**: app traffic stays E2E for
  BYO-domain (passthrough) and control-plane; the free-tier shared-domain path is
  the deliberate, documented **relay-terminated** carve-out.
- File follow-up issues for the deferred slices surfaced here: free-tier PR-preview
  hostnames, relay-side DNS-01 wildcard issuance.
