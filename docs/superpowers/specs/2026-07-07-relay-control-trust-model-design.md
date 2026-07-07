# Relay control-plane trust model — design

Trust-model spec for the remote control-plane epic
([#49](https://github.com/getpiper/piper/issues/49)). It reconciles the two token
systems that exist after the authenticated control API
([#77](https://github.com/getpiper/piper/pull/77) / #72) landed, and defines the
end-to-end trust model that the relay control-stream routing
([#73](https://github.com/getpiper/piper/issues/73)), the CLI remote target
([#74](https://github.com/getpiper/piper/issues/74)), and the hosted dashboard
([#76](https://github.com/getpiper/piper/issues/76)) implement against.

This is a **trust-model doc, not a UX doc**. It fixes who authenticates whom at
each hop and what the relay may and may not see. Exact wire formats, cert
issuance mechanics, and CLI flows belong to the implementing issues.

## The problem it reconciles

After #77, two token systems exist and #73's text assumes they can be one:

- **Enrollment token** (`internal/relay/store.go`, `agents` table): minted by the
  relay operator (`piper-relay enroll <name> --domain <base>`), presented by
  `piperd` in the tunnel handshake. Trust direction **agent → relay** — it is the
  box's identity to the relay.
- **Token B, the control bearer** (`internal/store`, `tokens` table): minted on the
  box (`piperd token create`), carried by the caller, validated on **every**
  `/v1/*` request at `piperd`. Trust direction **caller → piperd**.

#73 says *"reuse the existing enrollment token the agent already holds — don't
invent a second trust anchor."* Taken literally that is wrong: the enrollment
token is the **box's** secret to the relay. Making it the caller's credential
would hand every user the box's tunnel-routing secret. So Token B genuinely must
exist as a separate caller-side credential, and #77 was right to build it.

The real reconciliation is not collapsing tokens into one — it is recognizing
there are **three** trust relationships and hanging them all off a single relay
**account**, so the caller still experiences one login.

## The three anchors

| Anchor | Direction | Lives | Status |
| --- | --- | --- | --- |
| **Enrollment token** | agent → relay | box config + relay `agents` hash | Exists. Now also **bound to an account**. |
| **Relay account credential** | caller → relay | caller config + relay `accounts` | **New.** The tenancy + routing-authz anchor. |
| **Token B (control bearer)** | caller/relay → piperd | relay-held + piperd `tokens` hash | Exists (#77). Still **always validated at piperd**. |

They cannot be merged, because they live in different trust domains: the relay
cannot validate Token B (its hash is only on the box), and the box cannot
validate a relay account (it has no account concept). The account is the thing
that ties them together.

## Decisions (settled during brainstorming)

- **Multi-tenant from the start.** getpiper will run a **hosted public relay**
  serving many users, one user owning several boxes. The relay is the tenancy
  authority. Single-user self-host is just "one account." The relay has none of
  this today — only `agents` keyed by enrollment token.
- **Relay-provisioned Token B, single-credential caller (option A).** The caller
  holds only its relay account credential. On box-claim the relay obtains a Token B
  and holds it per `(account, agent)`; per request the relay injects it. Rejected:
  making the user copy each box's Token B by hand (clunky, doesn't scale to a
  hosted product); and a relay-signed single token that makes `piperd` trust the
  relay as an identity provider (a larger, different trust model that contradicts
  #77's "box ownership is the root of trust, credential verified at piperd").
- **Relay terminates TLS on the control plane only.** The relay is an HTTPS
  reverse proxy for control requests: it authenticates the account, checks authz,
  and forwards over the tunnel. Rejected: opaque end-to-end control streams — they
  keep the relay blind but need a stream-open auth handshake and a `piperd` control
  cert, and the relay already provisions Token B, so blindness buys little here.
- **App traffic is never terminated.** This is an invariant, not an incidental:
  website/app traffic stays **SNI-passthrough, end-to-end** from the visitor's
  browser to Caddy/`piperd` on the box (Plan 2, unchanged). The relay reads only
  the SNI hostname to pick a tunnel, then splices raw bytes and never holds the
  keys. **The relay decrypts control traffic only.**

## Multi-tenant relay model

New relay concepts:

- An `accounts` table. Every enrolled agent belongs to an account (enrollment
  binds `agent → account`).
- Callers authenticate to the relay **as an account**.
- **Authz rule:** a caller may reach an agent iff its account owns that agent. The
  map is keyed `account+agent → session` so multi-box-per-account works now and
  explicit cross-account grants can be added later without reshaping it.

The relay's routing map therefore grows from today's single `hostname → session`
(app traffic) to *also* carry `account+agent → session` (control traffic).

## Control-plane data flow

```
piper CLI --HTTPS--> relay: api.<relay-domain>          (relay terminates TLS)
                       │  1. authenticate account credential
                       │  2. authz: does this account own <agent>?  (target via header/path)
                       │  3. inject Token B for (account, agent)
                       └──(existing outbound tunnel: new "control" stream)──▶ piperd 127.0.0.1:8088
                                                                                4. validate Token B (#77)
                                                                                5. run request, return response
```

- The control request rides the agent's **existing outbound tunnel** as another
  multiplexed stream — so there is still **no inbound port on the box**, and it
  works through NAT/CGNAT for free.
- The relay needs exactly one control-plane cert (for `api.<relay-domain>`). This
  is the sole, scoped exception to #73's "relay stays out of the DNS/TLS-key
  business" line — it touches only the control endpoint. **App certs stay DNS-01
  on the box** and never go near the relay.
- `piperd` still `dial`s its own `127.0.0.1:8088` for the control stream exactly as
  it does `127.0.0.1:443` for app traffic (`internal/agent/tunnelclient.go`), so no
  new inbound listener is added to the box.

## Token B provisioning & revocation

- **On box-claim** (agent bound to an account): the relay asks `piperd` — over the
  tunnel — to mint a Token B, and stores it per `(account, agent)`. `piperd token
  create` on the box still exists for the pure-LAN path.
- **Per request:** the caller sends only its account credential; the relay injects
  the stored Token B into the forwarded control request; **`piperd` validates it,
  always** — no transport fast path, the box still independently gates (#77).
- **Revocation:** the box owner runs `piperd token revoke`; the relay's stored copy
  goes dead; the relay re-provisions on the next claim. **The box owner remains the
  ultimate root of trust and can cut the relay off unilaterally.**
- **LAN/loopback path unchanged:** `piper login --token <t>` against
  `127.0.0.1:8088` still works with no relay and no account. Transport is decided
  purely by the target address in the CLI config.

## Trust cost, stated plainly

Because the relay terminates control TLS and stores Token B, a **compromised relay
can drive boxes in the control plane** until the owner revokes. It **cannot** touch
app traffic, which stays end-to-end. This is the deliberate price of a
relay-terminated control plane, and it is bounded by two things: the box owner's
unilateral `piperd token revoke`, and the fact that in the hosted model the caller
has already chosen to log into that relay and let it provision Token B — so the
control plane concedes no trust the user has not already extended. A user who does
not want to extend that trust runs the control plane over LAN/loopback, or
self-hosts the relay.

## What this changes vs. #77's world

- Token B's model is **unchanged**: still minted on the box, still validated at
  `piperd` on every request. The only addition is a second way to mint it — a
  relay-initiated mint over the tunnel — alongside `piperd token create`.
- The loopback/LAN path is **unchanged**: no relay, no account, paste-a-token.
- New surface is entirely on the relay: `accounts`, `agent → account` binding, the
  `account+agent → session` authz map, the `api.<relay-domain>` TLS-terminating
  control listener, and per-`(account, agent)` Token B storage.

## Scope & non-goals

- **In scope:** the trust model above — anchors, tenancy, authz rule, control-plane
  TLS posture, Token B provisioning/revocation, and the invariant that app traffic
  is never decrypted.
- **Deferred:** capability scoping (read-only dashboard token vs deploy-capable CLI
  token) — the `scope` column exists (#77) but enforcement waits for a real
  read-only consumer (#75/#76). Cross-account grants (sharing a box across
  accounts). Account signup/billing/session mechanics for the hosted relay. The
  dashboard-vs-relay API-shape question from #49 (dashboard consumes the same
  account + control surface either way).

## Issue reconciliation

Picking up this model, update the tracker so it stops implying one token:

- **#73** — replace "reuse the enrollment token, don't invent a second anchor" with
  the three-anchor model; note the relay gains an **account layer**, **terminates
  control TLS** (needs an `api.<relay-domain>` cert), and **holds/provisions Token
  B** per `(account, agent)`. Its "relay only routes streams / stays out of TLS"
  criterion is now scoped to *app* traffic; the control plane is deliberately
  terminated.
- **#49** — record that the "reuse enrollment token / session" open note is
  resolved: the caller authenticates by **account**, not by the box's enrollment
  token.
