# Public relay — self-service onboarding + shared domain — design

Design for the **free tier of a hosted public `piper-relay`**: a stranger with a
Google account installs `piperd`, runs `piper login`, and gets a live HTTPS URL on
a subdomain of the operator's apex — with **zero operator action**. This is the
first concrete, self-service instantiation of the "hosted public relay" that the
[relay control-plane trust model](2026-07-07-relay-control-trust-model-design.md)
already assumes but leaves as a deferred non-goal ("account signup/session
mechanics for the hosted relay").

getpiper will run such a relay at `public.getpiper.co`. Its purposes, in the
owner's words: **attract users** (the one piece that needs a VPS, not just a Pi),
**dogfood** (a real internet-reachable target for online tests / PR previews), and
seed the eventual **dashboard** and **paid tiers**.

This is a **design/trust-model doc, not a UX or wire-format doc**. It fixes the
account model, the naming/TLS posture, the self-service claim flow, and the
guardrails. Exact wire formats, endpoint shapes, and CLI copy belong to the
implementing issues.

## Scope

This is **one slice** of a larger "public relays" concept. The concept decomposes
into independent sub-projects, each with its own spec:

1. **Self-service onboarding + shared domain** — *this spec.*
2. Vercel-style custom domains (relay-terminated BYO-domain) — follow-up spec.
3. Tiers / billing / paywall / real quotas — follow-up spec.
4. Dashboard ([#76](https://github.com/piperbox/piper/issues/76)) — consumes this
   account + control surface.

**In scope here:** Google-OAuth relay accounts, a device-flow self-service box
claim, assigned single-label free subdomains, a relay-terminated wildcard app-TLS
path, and minimal guardrails (per-account caps + operator kill-switch).

**Deferred (each its own spec / issue):** Vercel-style custom domains; an
end-to-end (box-terminated) cert path for the *free* tier; billing, paid tiers,
real quotas, rate limits, and a takedown workflow; the dashboard; a full
abuse/content-moderation model.

## Decisions (settled during brainstorming)

- **Free tier is relay-terminated, always.** The relay holds one wildcard cert
  `*.public.getpiper.co` (its own DNS-01) and **terminates** free-tier app HTTPS,
  forwarding plaintext HTTP to the box over the existing tunnel. This is the
  ngrok/Cloudflare-Tunnel deal, chosen deliberately. See *TLS posture* below for
  the alternative that was rejected and why.
- **Single-label assigned names.** Each app gets an **assigned** hostname
  `<app-hash>-<username>.public.getpiper.co` — a *single* DNS label under the apex,
  so one wildcard covers every user. Names are assigned, not chosen: no
  availability check, no squatting, no reservation flow.
- **Google OAuth is signup.** First `piper login` creates the relay account; the
  Google identity is the account credential. No separate password system.
- **Device flow, not localhost redirect.** `piper login` uses an OAuth **device
  flow** (display URL + code, poll) because the box is frequently headless (a Pi
  over SSH). Same UX as `gh auth login`.
- **Two verbs: `piper login` (identity) and `piper connect` (box claim).** They map
  to the two distinct trust anchors — `login` obtains the caller's **account
  credential** (Google device flow); `connect` uses it to enroll **this box**. The
  split keeps each command single-purpose and each trust anchor testable in
  isolation.
- **Self-service claim replaces operator enrollment.** `piper connect` makes the
  relay mint the enrollment token; nobody runs `piper-relay enroll` for a stranger.
- **Minimal guardrails only.** A hard per-account cap (agents + apps) enforced at
  claim/deploy, plus an operator kill-switch. Real quotas, rate limits, and
  takedown deferred to the tiers spec.

## TLS posture — why relay-terminated, and the invariant carve-out

The trust-model spec states that **app traffic is never decrypted** — it stays
SNI-passthrough, end-to-end from the visitor to Caddy on the box. This spec
**narrows** that invariant:

- **BYO-domain apps stay box-terminated / E2E** (Plan 2, unchanged). The user owns
  the domain, hands `piperd` DNS credentials, the box does DNS-01 and terminates;
  the relay only reads SNI and splices bytes.
- **Free-tier shared-domain apps are relay-terminated by design.** The relay holds
  the wildcard key and sees free-tier plaintext.

### Why not keep E2E for the free tier

Box-termination on `*.public.getpiper.co` would require the relay to **mint certs
for the box's hostnames and deliver them down the tunnel** (the box can't run ACME
itself — it controls neither the DNS nor an inbound port). The clean version is
CSR-on-box so the relay never holds the key. That is buildable, but:

- The **single shared wildcard cannot be handed to every box** (a malicious box
  could then present a valid cert for another user's hostname). Box-termination
  therefore forces either a **leaf cert per app hostname** or a **per-user wildcard**
  (`*.<username>.public.getpiper.co`, which reverts naming to two labels and drops
  the single-wildcard win).
- Either way, every issued cert lives under the registered domain `getpiper.co`,
  which hits **Let's Encrypt's ~50-certs-per-registered-domain-per-week limit**
  almost immediately. At any real scale this needs an LE rate-limit exemption or a
  non-LE CA — a standing operational dependency.

Relay-terminated is **one wildcard, one cert, forever** — no per-user issuance, no
rate limits, no CSR/renewal fan-out. The entire cost is that the relay sees
free-tier plaintext. For a free tier whose whole purpose is *ship fast, attract
users, dogfood*, that trade is accepted. A relay-minted E2E path for the shared
domain remains a clean **future upgrade**, not part of this slice.

### The trust cost, stated plainly

A compromised relay can read (and tamper with) **free-tier** app traffic. It
**cannot** touch BYO-domain traffic, which stays E2E. A free-tier user who won't
extend that trust brings their own domain, or self-hosts the relay. This is the
same, well-understood posture every managed tunnel offers.

## Naming

`<app-hash>-<username>.public.getpiper.co` — one label under the apex.

- `<username>` is **derived from the Google identity** (e.g. email local part),
  sanitized DNS-safe (`[a-z0-9-]`), and made **unique** per account (append a short
  discriminator if the derived form is already taken).
- `<app-hash>` is per-app, so one user's apps never collide and no `<app>.` prefix
  (which would add a second label and break the single wildcard) is needed.
- The whole label must be **≤63 chars**; if `<username>` is long it is truncated —
  `<app-hash>` carries uniqueness regardless.
- The relay **routes by the full hostname** via its routing map; it never parses
  the label back into hash/username, so hyphens in `<username>` create no
  ambiguity.

## Multi-tenant model (instantiating the trust spec)

- **`accounts` table** — the trust spec's deferred account layer, now real. A row
  is created on first Google login: Google subject id + derived unique
  `<username>`. This is the concrete form of the trust spec's "relay account
  credential."
- **`agent → account` binding** — the existing `agents` row (enrollment token
  hash) is now bound to an account at claim time.
- **`hostnames` registry** — `hostname → (account, agent, app)`, assigned at
  deploy; feeds the routing map.
- **Token B provisioning is unchanged.** The control plane still uses relay-held
  Token B per `(account, agent)` exactly as the trust spec defines; this spec adds
  nothing there.

The relay's routing map now carries three kinds of entry: `hostname → session` for
BYO-domain **passthrough** (Plan 2), `hostname → session` for shared-domain
**terminate-and-forward** (new), and `account+agent → session` for the **control
plane** (trust spec).

## Components

### `piper-relay`

- **Google-OAuth accounts** — device-flow auth endpoints; account created/looked-up
  on login by Google subject id; derives the unique `<username>`.
- **Self-service enrollment** — on approve, mints an **enrollment token bound to the
  account** (`agents` row, now account-bound) and returns it to the CLI. Replaces
  the operator `piper-relay enroll` path (which stays available for self-hosters).
- **`hostnames` registry** — assigns `<app-hash>-<username>.public.getpiper.co` on a
  deploy request over the control tunnel; records the mapping; adds it to the
  routing map.
- **Wildcard termination path** — obtains `*.public.getpiper.co` via DNS-01
  (relay-controlled zone). The `:443` listener **branches by SNI**:
  - SNI under `*.public.getpiper.co` → **terminate locally with the wildcard cert,
    forward HTTP over the app's tunnel** (new).
  - Any other SNI → **Plan 2 SNI-passthrough splice, unchanged.**
- **Guardrails** — per-account caps (max agents, max apps) checked at claim/deploy;
  an operator **kill-switch** marking an account or hostname disabled → routing and
  claim refuse.

### `piperd`

- **Accept an enrollment token + dial the tunnel** — receive the enrollment token
  installed by `piper connect` and dial the outbound tunnel exactly as Plan 2 does
  (no new inbound listener). The device flow itself is CLI-side (below); `piperd`
  only receives the resulting token.
- **Hostname registration on deploy** — over the control tunnel, request a hostname
  assignment for the app and store the returned mapping.
- **Accept relay-terminated forwarded HTTP** — receive the relay's forwarded HTTP
  stream for a free-tier hostname and route it to the app container. Caddy already
  serves the app internally over HTTP (Plan 1), so this extends the existing
  tunnel-served path rather than adding a listener.

### `piper` CLI

- **`piper login`** — obtain the caller's **account credential** via the Google
  device flow (print URL + code, poll) and store it in CLI config. This is the
  identity step only; it enrolls no box. Coexists with the existing
  `piper login --token <t> --addr <box>` LAN path (caller → `piperd` bearer);
  transport is chosen purely by the target in CLI config, per the trust spec.
- **`piper connect`** — using the stored account credential, request an enrollment
  token from the relay for **this box** and install it into `piperd` (write config /
  call a `piperd` endpoint), after which `piperd` dials the tunnel. Requires a prior
  `piper login`; if no account credential is present, it errors telling the user to
  run `piper login` first.

## Data flows

### 1. Signup (`piper login`) + box claim (`piper connect`)

```
box: piper login
  └─▶ relay: start device flow ─▶ user approves in browser (Google)
        └─▶ relay: account created/looked-up → return account credential
              └─▶ CLI stores account credential in config

box: piper connect                     (requires the stored account credential)
  └─▶ relay: quota check (under agent cap)
        └─▶ relay: mint enrollment token bound to account
              └─▶ CLI installs token + relay endpoint into piperd
                    └─▶ piperd dials outbound tunnel (Plan 2, unchanged)
```

### 2. Deploy

```
piper deploy
  └─▶ piperd: build + run + health (Plan 1)
        └─▶ piperd ──control tunnel──▶ relay: assign hostname for <app>
              └─▶ relay: app-cap check → assign <app-hash>-<username>.public.getpiper.co
                    → record hostname→(account,agent,app) → add to routing map
                          └─▶ piperd stores mapping → app live
```

### 3. Visitor request (relay-terminated)

```
browser ──HTTPS──▶ relay :443
   SNI = <app-hash>-<username>.public.getpiper.co  (matches wildcard)
     └─▶ relay terminates with *.public.getpiper.co cert
           └─▶ hostname → tunnel lookup
                 └─▶ forward HTTP over tunnel ─▶ box Caddy ─▶ app container ─▶ response
```

### 4. Kill-switch

```
operator disables account|hostname
  └─▶ routing refuses matching requests
  └─▶ enrolled agents of a disabled account are rejected on (re)connect
```

## Error handling & edge cases

- **Username collision** — two Google identities derive the same sanitized
  `<username>`: append a short discriminator so the account's `<username>` is
  unique; `<app-hash>` disambiguates apps within an account.
- **Label > 63 chars** — truncate `<username>`; `<app-hash>` preserves uniqueness.
- **Quota exceeded** — a clear, actionable error at claim (agent cap) or deploy
  (app cap); no partial state.
- **Device-flow failures** — code expiry, user denial, network loss: surfaced by
  the CLI; the box remains unclaimed and unchanged.
- **Wildcard renewal failure** — a relay-level operational concern (monitoring +
  alerting on the relay), out of the request path.
- **Account disabled mid-session** — routing refuses; the box owner still holds the
  ultimate `piperd token revoke` for the control plane (trust spec).

## Testing (TDD, `CGO_ENABLED=0`, arm64 cross)

- **Relay unit** — account creation from a **fake IdP**; enrollment-token → account
  binding; quota enforcement at claim/deploy; hostname assignment + uniqueness +
  63-char handling; the `:443` **SNI branch** (terminate vs passthrough);
  kill-switch refusal.
- **`client`/CLI unit** — `piper login` device-flow client against a **fake relay**
  (code, poll, approve, deny, expiry); `piper connect` requires a stored account
  credential (errors without one) and installs the returned enrollment token.
- **`piperd` unit** — accepts an installed enrollment token and dials the tunnel;
  hostname registration; relay-terminated forwarded-HTTP handling against a fake
  tunnel.
- **Loopback e2e** — extends the Plan 2 loopback relay test with a **fake Google
  IdP**: `piper login` → `piper connect` → `piper deploy` → `curl` the assigned
  hostname through the relay-termination path and assert the app responds.
- Existing gates unchanged: `make test` (Docker/e2e skip cleanly when absent),
  `make cross` (no-cgo arm64 build).

## Issue reconciliation

- The trust spec's deferred "account signup/session mechanics for the hosted relay"
  is **satisfied for the free tier** by this spec (Google-OAuth accounts +
  device-flow claim).
- The trust spec's "app traffic is never decrypted" invariant is **narrowed**: it
  holds for BYO-domain, and is a **documented carve-out** for the relay-terminated
  free tier. Update [#49](https://github.com/piperbox/piper/issues/49) /
  [#73](https://github.com/piperbox/piper/issues/73) notes to reference this
  carve-out so the passthrough-only criterion is understood as scoped to
  BYO-domain + control-plane, with the free tier deliberately terminated.
- File follow-up issues for the deferred slices (custom domains, tiers/billing,
  free-tier E2E upgrade) so the tracker reflects the decomposition.
