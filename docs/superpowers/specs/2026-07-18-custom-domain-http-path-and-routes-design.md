# Per-app custom-domain HTTP path + exact-host routes (#228 + #230)

**Issues:** [#228](https://github.com/piperbox/piper/issues/228) · [#230](https://github.com/piperbox/piper/issues/230) · **Date:** 2026-07-18 · **Status:** approved — implemented except the `:80` redirect

> **Landed retroactively 2026-07-24.** This design was written and approved on
> 2026-07-18, but its PR never merged, so the spec sat on an unmerged local
> branch while #228 and #230 themselves shipped in v0.3.0. It is recorded here
> for the design rationale the code no longer carries.
>
> **What shipped:** Leg A (relay `:80` listener, `PIPER_RELAY_HTTP_ADDR`), Leg B
> (`KindHTTP` → box `:80` in every mode), and the HTTPS half of Leg C
> (`deploy.finish` calls `UpsertRouteTLS` for each active app domain).
>
> **What did not:** the `:80` → `308` redirect. `caddy.Client.UpsertRedirect` as
> specified in Leg C was never implemented — `internal/caddy` has no redirect
> route and `deploy.finish` never calls one — and Leg D's redirect assertion is
> absent from the e2e suite. Consequence: `http://<per-app-custom-domain>`
> reaches the box's plaintext Caddy server, matches no route there, and does not
> redirect to HTTPS.
>
> The acceptance criteria below are left exactly as authored, unticked — the
> first one is genuinely unmet.

Part of epic #224 (per-app BYO domains). Combines two child issues that share a
code path: #228 (relay port-80 routing down the tunnel) and #230 (deploy adds
exact-host `:443` routes for app-owned domains). Doing them together yields one
coherent, end-to-end-testable unit: the full `http://myshop.com` → redirect →
`https://myshop.com` → app path.

## Background

Per-app custom domains are exact hosts (`myshop.com`) attached to one app. The
relay already splices `:443` SNI-passthrough for a registered custom domain to
the owning box (built in #227). Two gaps remain on the HTTP path:

1. **The relay has no `:80` listener.** `http://myshop.com` dies at the relay.
2. **The box has no exact-host `:443` route** for a per-app domain — only the
   shared `<app>.<base>` route and the #102 box-wide `<app>.<custom>` route.

Store support (`app_domains` table, `#225`) and the relay 1:N custom-domain
router + `add-domain`/`domain-active` control ops (`#227`) are already built.

## The end-to-end path this delivers

```
curl http://myshop.com
  → relay :80    (NEW: Host-match → KindHTTP pump)             [#228, Leg A]
  → tunnel → box :80  (NEW: non-terminated KindHTTP→:80)       [#228, Leg B]
  → box Caddy "piper" server: 308 redirect route              [#228, Leg C]
  → https://myshop.com
  → relay :443   (existing SNI passthrough, #227)
  → box :443 exact-host route + cert                           [#230, Leg C]
  → app container
```

## Legs

### Leg A — Relay `:80` listener (`internal/relay`)

- New `handleHTTP(conn, router)`: read the HTTP request line + headers through a
  recording conn (read-deadline-bounded, mirroring `readSNI`'s slowloris guard),
  extract the `Host` header (strip any `:port`, lowercase), `router.Lookup(host)`
  (already matches exact + subdomain), open a `KindHTTP` stream on the owning
  session, replay the buffered header bytes, then byte-pump both directions —
  the same shape as `passthrough` / `terminate`. No router match → close, no
  tunnel. The relay never parses the body; it is a byte pump into the box's `:80`.
- `Serve` gains an `httpAddr string` parameter. Empty ⇒ no `:80` listener (keeps
  passthrough-only relays unchanged). `cmd/piper-relay` reads
  `PIPER_RELAY_HTTP_ADDR` (default `:80`).

### Leg B — Box routes `KindHTTP`→`:80` unconditionally (`cmd/piperd`)

- Drop the `terminated &&` guard in `newDialLocal`'s `KindHTTP` case, so any box
  — terminated or not — pipes a `KindHTTP` stream to `127.0.0.1:80`. Terminated
  boxes are unchanged (they already got KindHTTP→:80). Non-terminated
  (custom-domain) boxes now answer relay `:80` traffic. The semantic of KindHTTP
  ("relay-terminated plaintext HTTP, pipe to :80") is mode-independent.

### Leg C — Box Caddy redirect + exact-host routes (`internal/caddy`, `internal/deploy`) — #230

- `caddy.Client.UpsertRedirect(host)`: adds a `static_response` route to the
  `piper` (plaintext `:80`) server matching `host`, returning `308` with
  `Location: https://{http.request.host}{http.request.uri}`. Stable `@id`
  (reuse `routeID`) so redeploys replace it; `RemoveRoute(host)` already tears it
  down by that id. Needed because Caddy's `automatic_https` is disabled, so no
  redirect is auto-provisioned.
- In `deploy.finish`, after the primary route and alongside the existing #102
  box-wide block: for each `store.ListAppDomains(app)` row with `Status ==
  "active"`, `UpsertRouteTLS(domain, hostPort)` (serves HTTPS on `piper-tls`) and
  `UpsertRedirect(domain)` (`:80` → HTTPS). Same log-and-skip-on-transient-error
  stance as #115 / the #102 block — a Caddy blip must not fail an otherwise-good
  deploy; the domain manager (#229) re-arms routes on renewal/resume.
- Extend `removeCustomDomainRoute(app)` to also drop each app-owned domain's TLS
  route and redirect route (both keyed by the domain host) on stop/delete. The
  existing #102 box-wide removal stays.

### Leg D — E2E (`test/e2e`)

Extends the `TestRelayLoopback` self-signed harness (non-terminated box holding a
static wildcard cert — the exact mode a custom-domain box runs in):

- Bring up the relay with the new `:80` listener.
- Register a custom domain the way the not-yet-built pieces eventually will:
  relay-side via the `add-domain` control op (#227) so the router routes it;
  box-side via `AddAppDomain` + `UpdateAppDomainStatus(..., "active")` (#225) and
  a self-signed cert for the custom host loaded into the box's Caddy. These
  manual steps stand in for #229 (lifecycle manager), #231/#232 (API/CLI), which
  are out of scope here.
- Assert: `curl http://<custom>` through the relay `:80` returns `308` with a
  `https://<custom>` Location; `https://<custom>` through the relay `:443`
  serves the app; the shared-domain URL still works (behavior unchanged).

## Out of scope / deferred (flagged, not silently dropped)

- **`www.`** — the epic's own open question (one domain = apex+`www` vs. N alias
  rows). For now, one exact-host route per `app_domains` row; no implicit `www.`.
- **Box-wide (#102) HTTP redirect** — the same `:80` redirect gap exists for
  box-wide custom domains, but generalizing the box-wide path into the per-domain
  model belongs to #229. Per-app domains only here.
- **Domain lifecycle / issuance (#229, #226 wiring), API (#231), CLI (#232)** —
  unchanged; this PR only adds the routing/redirect plumbing they will drive.

## Testing

TDD, failing-test-first per leg:

- **Leg A:** relay unit test with a fake `tunnel.Session` that accepts a
  `KindHTTP` stream — assert a Host-matched request is pumped down it with the
  request bytes intact, and an unmatched Host opens no stream.
- **Leg B:** `cmd/piperd` unit test on `newDialLocal(terminated=false, ...)` —
  `KindHTTP` dials `:80`.
- **Leg C:** `internal/caddy` test for `UpsertRedirect` (route shape / 308 /
  Location) against the test admin harness; `internal/deploy` test with the
  fakes-based `RouteSetter` asserting active app domains get both routes on
  deploy and lose them on stop/delete, and that inactive domains are skipped.
- **Leg D:** the E2E above, gated behind `RUN_E2E=1`.

## Acceptance criteria (from #228, #230)

- [ ] `curl http://<custom-domain>` through the relay reaches the box and
  redirects to HTTPS.
- [ ] Shared-domain HTTP behavior unchanged.
- [ ] Deploying an app with an active domain serves it at `https://<domain>` and
  the shared URL.
- [ ] Stop/delete removes exactly that app's custom-domain routes; other apps'
  domains untouched.
