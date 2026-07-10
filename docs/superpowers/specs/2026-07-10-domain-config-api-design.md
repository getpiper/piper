# Domain-config API: manage BYO base domain + cert issuance remotely

**Issue:** [#102](https://github.com/getpiper/piper/issues/102) · **Date:** 2026-07-10 · **Status:** approved

Phase 2 dependency of the dashboard roadmap (getpiper/dashboard — part of #76).

## Problem

BYO custom domains are configured today via env vars on the box (`PIPER_BASE_DOMAIN` plus lego DNS-provider creds read from the provider's own env vars, e.g. `CLOUDFLARE_DNS_API_TOKEN`), and the whole TLS/tunnel mode — terminated vs BYO, the Caddy `:443` listener, cert issuance — is wired once at piperd startup (`setupRelayTLS`). Remote callers can't see or change any of it, so the dashboard can't offer a Vercel-like "add your domain" flow or show cert issuance status.

## Decisions (with rationale)

1. **Live apply, no restart.** Setting a domain via the API takes effect at runtime: cert issued, Caddy re-wired, relay routing updated — the dashboard watches progress over one connection. piperd does not restart to pick up domain config.
2. **Relay coordination is in scope.** The relay pins each enrollment to its base domain at handshake (`internal/relay/server.go`), so a custom domain requires the relay to learn it. Without this the free-tier → BYO flow — the driving use case — can't work end-to-end.
3. **Shared and custom domains coexist.** Adding a custom domain does not flip the box out of terminated mode. Existing `<hash>-<user>.<apex>` URLs keep working (relay-terminated → `KindHTTP` → `:80`); apps additionally become reachable at `<app>.<custom>` (SNI passthrough → `:443`, box-terminated). No broken window during DNS propagation or cert issuance.
4. **Cloudflare stays the only DNS provider** for now, matching `newDNSProvider` today. The API carries a `dns_provider` field so more can be added without a shape change.
5. **DNS creds are write-only.** The token is stored on-box and never appears in any API response. Cert private key and ACME account key never leave the box; the relay learns only the domain name.
6. **Env stays authoritative** (precedence: env > API-set > none). An env-managed box rejects API writes with 409 rather than silently shadowing them.

## API surface

Bearer-authenticated on the piperd control API, so it works locally and through the relay control stream (`api.<apex>`).

### `PUT /v1/domain`

```json
{"domain": "example.com", "dns_provider": "cloudflare", "dns_token": "…"}
```

Validates domain shape and provider, persists, kicks the issuance state machine, returns current status (the `GET` body). Errors: `400` bad domain/provider; `409` if domain config is env-managed or the box has no relay session ("connect this box to a relay first"). A `PUT` while `failed` retries immediately; a `PUT` with a new domain replaces the old one (tear down, then issue).

### `GET /v1/domain`

```json
{
  "domain": "example.com",
  "dns_provider": "cloudflare",
  "dns_token_set": true,
  "source": "api",
  "status": "issuing",
  "error": "",
  "cert_not_after": "2026-10-08T00:00:00Z",
  "dns_records": [
    {"type": "CNAME", "name": "*.example.com", "value": "public.getpiper.dev"},
    {"type": "CNAME", "name": "example.com", "value": "public.getpiper.dev"}
  ],
  "dns_ok": false
}
```

- `source`: `"api"` or `"env"`.
- `status`: `""` (unconfigured), `"issuing"`, `"active"`, `"failed"`.
- `error`: last issuance/renewal error, empty when healthy.
- `dns_records`: the records a guided setup must tell the user to create — wildcard + apex pointing at the relay host (derived from the configured relay address).
- `dns_ok`: computed on each `GET` — resolve `piper-probe.<domain>` (any label matches the wildcard record) and compare the resulting addresses against those of the relay host. Traffic readiness, independent of issuance (DNS-01 needs only the API token, so issuance starts immediately).

### `DELETE /v1/domain`

Removes the custom domain: relay drops the SNI mapping, Caddy drops the `:443` routes, cert files are deleted, row cleared. Shared-domain URLs are unaffected. `409` if env-managed.

## Data model

New single-row store table `domain_config`: `domain`, `dns_provider`, `dns_token`, `status`, `error`, `cert_not_after`, `updated_at`.

Two on-disk additions under `DataDir` (0600):

- **ACME account key**, persisted instead of today's regenerate-per-boot, so retries and renewals reuse one Let's Encrypt account.
- **Issued cert PEM + key**, so a restart reloads instead of re-issuing (re-obtain-on-every-boot would hit LE's duplicate-certificate rate limit under live-apply).

## The `internal/domain` manager

`domain.Manager` owns the custom-domain lifecycle and orchestrates through four narrow interfaces — persistence (store slice), issuer (`certs`), cert-loader/router (`caddy`), relay notifier (tunnel client) — mirroring how `deploy` orchestrates its collaborators, so it unit-tests entirely with fakes. Layering is unchanged: nothing imports "up".

### State machine (persisted in `domain_config.status`)

```
PUT /v1/domain
   └─> issuing ──obtain *.example.com + example.com──> active
          │                                              │
          └── failed ── auto-retry w/ capped backoff;    │ renewal failure:
                        PUT retries immediately          │ status stays active,
                                                         │ error recorded
```

- **issuing:** build the lego provider from the stored token via `cloudflare.NewDNSProviderConfig` (not env), obtain the wildcard + apex cert.
- **Activation (issuing → active):** persist cert to `DataDir` → load into Caddy → ensure the `:443` TLS server exists (new `caddy` admin-API method; today `:443` exists only if piperd *started* in BYO mode) → backfill `<app>.<custom>` routes on `:443` for every existing app → tell the relay to splice `*.<custom>` SNI to this tunnel. Order matters: the box must be able to answer before the relay routes to it.
- **Renewal:** the existing `runRenewLoop` moves into the manager. Failure sets `error` but keeps `active` (the old cert serves until expiry); success updates `cert_not_after`.
- **Restart resume:** at startup the manager loads the row; `active` + valid cert on disk → reload into Caddy without re-issuance; `issuing`/`failed` → resume attempts. A corrupt or missing cert file degrades to re-issuance, not a crash.
- **Env path folds in:** `setupRelayTLS` is replaced by "manager initialized from env config" — one issuance path total. Env-configured boxes behave exactly as today.

### Coexist routing

New deploys get both hostnames: `deploy` reads the active custom domain from the store per-deploy and adds the extra `:443` route alongside the shared-domain one. The agent's `dialLocal` already default-cases passthrough streams to `127.0.0.1:443` — no tunnel-client change for traffic.

## Relay coordination

One new control message on the existing authenticated `KindControl` stream (same precedent as `RegisterHostname` / `SetControlToken`): **`set-custom-domain {domain}`**, empty domain = clear.

- Relay adds a `custom_domain` column to `agents` with a **uniqueness check** — a domain already held by another agent is rejected; the agent surfaces that as `failed` + error. First-come-first-served; ownership proof is implicit in the DNS-01 cert the box already obtained before asking.
- The router maps `*.<custom>` and `<custom>` → that agent's session as passthrough. On reconnect the handshake is **unchanged** (still claims the enrolled shared base domain); the relay re-derives the custom-domain mapping from the `agents` row at registration. No enrollment mutation, no new auth surface.
- E2E TLS property holds: for custom-domain SNI the relay only splices bytes; termination stays on-box.

## Error handling

Bad domain/provider → `400`; env-managed → `409`; no relay session → `409`. Issuance failures (bad token, ACME errors, relay uniqueness rejection) land in `status`/`error` for the dashboard, with capped-backoff auto-retry.

## Testing (test-first, per layer)

- `store` — `domain_config` CRUD round-trip.
- `domain` — state machine with fakes: happy path to `active`; issuance failure → `failed` + retry; resume-from-disk skips re-issue; renewal failure keeps serving; delete tears down in order.
- `caddy` — ensure-`:443`-server + route add/remove against the admin API (skips without Caddy, like existing tests).
- `api` — PUT/GET/DELETE handler tests: `dns_token` never echoed, env-managed `409`, status shape.
- `relay` — `set-custom-domain` updates row + router; uniqueness rejection; mapping survives reconnect.
- e2e — loopback relay: configure a domain via the API with a fake issuer (test hook, as the TLS e2e does with static certs), curl the app via custom-domain SNI through the relay, assert the shared-domain URL still works.

## Acceptance criteria (from #102)

- [ ] A remote caller can set a box's base domain + DNS creds and read back issuance status.
- [ ] Env vars keep working (config precedence documented).
- [ ] No key material ever leaves the box.

## Out of scope

- DNS providers beyond cloudflare (field is extensible).
- BYO domain without a relay (direct-`:443` boxes); today's code only wires BYO TLS in relay mode.
- Per-app custom domains (this is the box-wide base domain).
- Relay-side domain-ownership verification beyond first-come-first-served + implicit DNS-01 proof.
