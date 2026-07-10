# Custom domains (BYO)

A box serves apps on a base domain. Two ways to configure it:

## Via the control API (dashboard / `curl`) — relay free-tier boxes

    PUT /v1/domain          {"domain":"example.com","dns_provider":"cloudflare","dns_token":"<token>"}
    GET /v1/domain          → status, DNS records to create, dns_ok, cert_not_after
    DELETE /v1/domain       → remove the custom domain

The box issues a wildcard cert via ACME DNS-01 (Let's Encrypt) using the
Cloudflare API token, terminates TLS itself, and asks the relay to splice
`*.example.com` SNI down its tunnel. Your existing shared-domain URLs
(`<hash>-<user>.<apex>`) keep working alongside.

Create the DNS records `GET /v1/domain` lists (wildcard + apex → the relay
host). Issuance starts immediately — records are needed for traffic, not for
the cert. `dns_ok` flips true once the wildcard resolves to the relay.

Secrets never leave the box: the DNS token is write-only (`dns_token_set`
signals presence), and the cert's private key and ACME account key live in
piperd's data dir with 0600 permissions.

## Via environment variables — self-managed boxes

`PIPER_BASE_DOMAIN` + `PIPER_DNS_PROVIDER` (creds via the provider's own env
vars, e.g. `CLOUDFLARE_DNS_API_TOKEN`), or a static `PIPER_TLS_CERT_FILE` /
`PIPER_TLS_KEY_FILE` pair. Unchanged from before.

## Precedence

**env > API > none.** A box whose base domain comes from the environment
(non-terminated relay mode) reports `"source":"env"` on `GET /v1/domain` and
answers `409` to `PUT`/`DELETE` — unset the env config to manage the domain
remotely. LAN-only boxes (no relay) answer `409` to all `/v1/domain` calls.
