# Relay Hardening Design

**Status:** Approved 2026-07-04

## Goal

Complete the bounded Plan 2 follow-ups in issue #27 without changing the relay
protocol or adding speculative provider infrastructure. The pre-authentication
read deadlines identified by the issue are already implemented on `main` and
remain unchanged.

## Changes

### Certificate replacement

Initial TLS setup continues to load the wildcard certificate into Caddy. A
renewal replaces the complete `tls.certificates.load_pem` list with the renewed
certificate/key pair instead of appending another entry. The Caddy client owns
the admin-API request shape; `renewLoop` only decides when to renew and supplies
the new PEM data.

An admin-API fake verifies the replacement method, path, and one-element array
body. A renewal-loop test uses a controllable renewal seam rather than waiting
for the production ticker.

### Unique relay base domains

The relay schema creates a unique index on `agents(base_domain)`. Using a named
`CREATE UNIQUE INDEX IF NOT EXISTS` statement applies the invariant both to new
databases and existing databases when they are reopened. If an existing
database contains duplicate domains, startup fails while applying the schema;
the relay must not silently choose one agent.

A store test enrolls two differently named agents with the same base domain and
expects the second enrollment to fail.

### DNS provider selection

`PIPER_DNS_PROVIDER` remains the extension point promised by the Plan 2 design.
An empty value selects `cloudflare` for backward compatibility. The explicit
value `cloudflare` is accepted. Any other value returns a clear startup error
naming the unsupported provider; future providers can be added to the same
small selector without changing configuration or certificate-manager APIs.

Provider selection is isolated behind a function that returns lego's existing
DNS provider interface. Tests cover the default, explicit Cloudflare selection,
and rejection path without requiring real credentials for the unsupported
case.

### Cancellation and test hygiene

`serveStreams` watches the supplied context and closes the yamux session when
the context is cancelled. Closing the session unblocks `Accept`, allowing the
function and `RunTunnelClient` to exit promptly. The watcher also exits when the
session closes normally, so it does not leak a goroutine.

`TestLoadRelayFields` uses `t.Setenv` for automatic, panic-safe restoration.

## Error handling

- Caddy replacement propagates transport and non-2xx errors.
- Duplicate domains return the SQLite constraint error from `Enroll`.
- Unsupported DNS providers fail relay TLS setup before ACME work begins.
- Session close errors during context cancellation are intentionally ignored;
  cancellation is already the controlling event.

## Verification

Each behavior is implemented test-first with focused package tests. Completion
requires the repository's full sequence: `gofmt -l .` with no output,
`go vet ./...`, `make test`, and `make cross`.
