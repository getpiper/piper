# Relay custom domains 1:N: pending-until-active lifecycle

**Issue:** [#227](https://github.com/piperbox/piper/issues/227) · **Date:** 2026-07-16 · **Status:** approved

Part of the per-app BYO domains epic ([#224](https://github.com/piperbox/piper/issues/224)). Relay-side prerequisite of the per-domain lifecycle manager (#229).

## Problem

The relay stores one `custom_domain` per agent (a column on `agents`, set by the
`set-domain` control op) and re-derives its router mapping at registration.
Per-app BYO needs many domains per agent. And because TLS-ALPN-01 issuance
requires the relay to route a domain's SNI *before* the box holds a cert —
inverting #102's "box answers before relay routes" ordering — an unproven
mapping must exist, be routable, and yet not be durably squattable.

## Decisions (with rationale)

1. **New `custom_domains` table**, not a JSON list in the existing column and
   not a flag on `hostnames`. Per-row status/expiry falls out naturally, and
   the `domain` primary key closes the FCFS race structurally (today that
   needs a partial unique index). `hostnames` is account-scoped, terminated,
   and derived-named; custom domains are agent-scoped, passthrough, and
   user-named — overloading it would put a mode flag in every query.
2. **Lazy eviction, no sweeper.** Expired pending rows are evicted when a
   rival claims the domain, and filtered at reconnect re-derivation. The
   acceptance criterion is *claimability* — "a pending domain never confirmed
   expires and becomes claimable" — and lazy eviction delivers exactly that
   without the relay's first periodic janitor goroutine. Revisit only if the
   residual routing window (below) proves to matter in practice.
3. **`set-domain` stays as a compat shim** over the new table. The deployed
   public relay serves v0.1.0 boxes whose box-wide BYO re-arms via
   `set-domain` on every reconnect; breaking it is not an option. Shim
   semantics = old semantics: replace all of the agent's rows with one
   `active` row (those boxes proved ownership via DNS-01 before calling).
   A mixed agent holding N per-app rows *and* sending `set-domain` cannot
   occur in shipped combinations — nothing calls the new client ops until
   #229, and #229 removes the `set-domain` caller — so replace-all is safe.
4. **Cap: 5 live domains per agent, relay-operator configurable**, pending
   rows included (they are the squattable kind). Mirrors the existing
   `maxAgents` / per-account app quota pattern. The relay is multi-tenant;
   unbounded rows from one enrollee is an abuse surface.
5. **Pending TTL: 1 hour**, a package constant. Long enough for DNS to be
   pointed and ACME to run; short enough that squatting a name is a nuisance,
   not a hold.

## Data model

New table in `schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS custom_domains (
    domain      TEXT PRIMARY KEY,
    agent_base  TEXT NOT NULL,          -- agents.base_domain
    status      TEXT NOT NULL,          -- 'pending' | 'active'
    created_at  TEXT NOT NULL
);
```

**Migration** (in `Open()`, where the column migrations already live):
`INSERT OR IGNORE` every non-empty `agents.custom_domain` as an `active` row,
**then clear the column**. Clearing is correctness, not tidiness: the copy
re-runs on every `Open()`, so a stale column value would resurrect a domain
the agent has since removed. One-way; the column and its unique index remain
in place, unused.

A row is **live** when `status='active'`, or `status='pending'` and
`created_at` is within the TTL. Expired pending rows are dead weight until
evicted or filtered; they are never re-derived and never block a claim.

## Store API (`internal/relay/store.go`)

- `AddCustomDomain(base, domain) error` — validates with the existing
  `customDomainRE` + `domainClaimable`; enforces the cap
  (`maxDomainsOrDefault()`, default 5, counting the agent's live rows);
  FCFS-rejects (`ErrDomainTaken`) if another agent holds the domain live, but
  **evicts** an expired pending row and claims. Re-adding your own pending
  domain refreshes `created_at` (an operator retrying resets their window);
  re-adding your own active domain is a no-op. New rows start `pending`.
- `ConfirmCustomDomain(base, domain) error` — own row pending→active;
  idempotent on active; error on another agent's row or a missing row.
  Pending age is not checked: eviction (rival claim) is the only thing that
  kills a claim, so a slow issuance (>TTL) still confirms if nobody contested
  the name. The claimability guarantee is unaffected — an expired pending row
  loses any FCFS race that arrives before its confirm.
- `RemoveCustomDomain(base, domain) error` — deletes own row only.
- `CustomDomains(base) ([]string, error)` — the agent's live domains, for
  reconnect re-derivation. Expired pending rows are filtered here.
- `SetCustomDomain(base, domain) (prev string, err error)` — **rewritten as
  the compat shim**: replace all of the agent's rows with one `active` row
  (empty domain = clear all); same signature and returned previous value, so
  `handleControl`'s unregister-previous logic is unchanged.
- `nowFunc func() time.Time` field on `Store` (defaults `time.Now`), so TTL
  tests inject time instead of sleeping. `const pendingTTL = time.Hour`.

## Tunnel protocol & client

`ControlRequest.Op` gains `"add-domain"`, `"remove-domain"`,
`"domain-active"`, all reusing the existing `Domain` field — no new struct
fields. `TunnelClient` gains `AddCustomDomain`, `RemoveCustomDomain`,
`ConfirmCustomDomain`, thin wrappers over `control()` like the four existing
ops. `SetCustomDomain` stays: `internal/domain` still calls it until #229
folds the box-wide path into the per-domain manager.

## Server handling (`internal/relay/server.go`)

Three new `handleControl` cases, each riding the authenticated session (so an
agent can only ever touch its own rows):

- `add-domain` → `store.AddCustomDomain`; on success
  `router.RegisterCustom(domain, sess)` — **routable immediately**, which is
  what lets the ALPN challenge reach the box before any cert exists. If the
  claim evicted an expired-pending squatter, the same `RegisterCustom` call
  overwrites the squatter's mapping (the router is keyed by domain), so its
  routing dies the moment the real owner claims — no extra unregister step.
- `domain-active` → `store.ConfirmCustomDomain`; router untouched (already
  routing).
- `remove-domain` → `store.RemoveCustomDomain` + `router.UnregisterCustom`.

Reconnect (`acceptTunnels`): the single `st.CustomDomain(base)` lookup becomes
a loop over `st.CustomDomains(base)`, registering each. Expired pending rows
are already filtered by the store, so a squat mapping also dies at reconnect.

The router itself is untouched: `RegisterCustom`/`UnregisterCustom` are
per-domain already, and `Lookup`'s exact + subdomain matching applies to each
registered domain as before.

## Security posture

The pending TTL bounds **squatting** (blocking a name), not **takeover**: if
a victim points DNS at the relay *before* claiming the domain, an attacker
who claims first can complete their own TLS-ALPN-01 issuance through the
splice and hold the name until eviction. This is inherent to
first-come-first-served — the epic (#224) explicitly scopes stronger
ownership verification out — and the mitigation is ordering: claim the
domain, then point DNS. The CLI's guided output (#232) instructs exactly that
order. Accepted residual risk, recorded here.

## Testing (test-first, per layer)

- **store** — round-trip CRUD + list; FCFS: live row rejects, expired pending
  evicts (via `nowFunc`); cap at 5 counting pending; re-add refreshes the
  pending window; confirm/remove reject another agent's row; migration copies
  the column then clears it, and a re-`Open` does not resurrect a removed
  domain; `set-domain` shim replaces all rows and returns the previous value.
- **relay server** — the three ops over a loopback tunnel (existing
  `server_test.go` pattern): add → SNI routes while pending; a claim over an
  expired squat unregisters the squatter; reconnect re-derives N domains and
  drops expired pending ones.
- **tunnel/agent** — the three client wrappers round-trip against a stub
  control handler (existing pattern).

## Acceptance criteria (from #227)

- [ ] An agent can hold N custom domains; all route after reconnect.
- [ ] A `pending` domain never confirmed expires and becomes claimable by
      another agent.
- [ ] A domain held by another agent is rejected; the requesting agent sees
      the error.

## Out of scope

- The box-side lifecycle that calls confirm — #229 owns sequencing
  (add → issue → confirm) and the box-wide fold-in.
- Custom-domain port-80 routing (#228).
- API/CLI surfaces (#231, #232).
- Relay-side ownership verification beyond FCFS + TTL (epic non-goal).
