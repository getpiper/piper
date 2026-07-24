# TLS-ALPN-01 issuance path in internal/certs (tokenless exact-host certs)

**Issue:** [#226](https://github.com/piperbox/piper/issues/226) · **Epic:** [#224](https://github.com/piperbox/piper/issues/224) · **Date:** 2026-07-16 · **Status:** approved

First agent-side building block of per-app BYO domains: issue an exact-host
certificate with **no DNS-provider API token**. The ACME validator connects
with SNI = the domain and ALPN `acme-tls/1`; the relay splices the connection
down the tunnel like any passthrough; the box answers the challenge.

## Problem

`internal/certs` is hardwired to DNS-01 (`SetDNS01Provider`, `certs.go:50`)
because box-wide BYO (#102) needs a wildcard cert. Per-app domains are exact
hosts (apex + `www`), so TLS-ALPN-01 works — but the challenge must be
answered on the box's `:443`, which Caddy owns (the `piper-tls` server), and
which relay passthrough streams reach via a blind pipe to `127.0.0.1:443`
(`cmd/piperd/main.go`).

## Decision: piperd answers, via an ALPN peek on the passthrough path

Considered and rejected:

- **Teach Caddy to answer** (per-challenge admin-API config churn: an
  ALPN-matched `tls_connection_policies` entry + the challenge cert for the
  seconds-long validation window). Racy, deeply coupled to Caddy's config
  shape, and Caddy's HTTP server isn't built to negotiate `acme-tls/1` for a
  connection that speaks no HTTP.
- **Caddy native issuance** (automatic HTTPS obtains the cert itself). Least
  code, but cert state/renewal/errors move inside Caddy where piperd can't
  report them through the `app_domains` status model, and it abandons the
  persisted ACME account key + disk cert layout #102 established. Two
  parallel cert systems is the maintenance trap the epic warns about.

Chosen: piperd runs a tiny in-process TLS-ALPN solver on a loopback port, and
the passthrough path peeks each ClientHello to route `acme-tls/1` connections
to it — everything else pipes to Caddy's `:443` exactly as today. No Caddy
changes, no relay changes, challenge traffic never mixes with app traffic.
Browsers never offer `acme-tls/1`, so no real traffic is misrouted; the cost
is one ClientHello parse per passthrough connection.

## Design

### 1. `internal/certs`: a second challenge mode

`Config` gains an optional `ALPNSolver challenge.Provider` field alongside
`DNSProvider`. `New` requires exactly one of the two to be set (error
otherwise) and calls `SetTLSALPN01Provider` instead of `SetDNS01Provider`
accordingly. `Manager.Obtain`, account-key handling, and `NewCloudflareIssuer`
are untouched — the DNS-01 path keeps working byte-for-byte.

### 2. `certs.ALPNSolver`: lego provider + loopback answerer in one

New type in `internal/certs/alpn.go` (~60–80 lines), both sides of the
challenge:

- `NewALPNSolver(listenAddr string)` starts a TLS listener. Default
  `127.0.0.1:0`; the addr is configurable so the pebble integration test can
  pin the port pebble's validator dials. `Addr()` exposes where it landed;
  `Close()` stops it.
- `Present(domain, token, keyAuth)` builds the challenge cert via lego's
  `tlsalpn01.ChallengeCert` and stores it in a map keyed by domain;
  `CleanUp(domain, …)` removes it. (lego's own `ProviderServer` binds a whole
  fixed port per challenge and can't share; the keyed map serves N concurrent
  domains, which the per-domain lifecycle manager (#229) will need.)
- The listener's `tls.Config` has `NextProtos: ["acme-tls/1"]` and a
  `GetCertificate` that looks up the ClientHello's SNI in the map (unknown SNI
  → handshake error). The validator completes the handshake, inspects the
  cert, and closes. No HTTP is spoken.

### 3. `internal/agent`: ALPN peek on the passthrough path

`dialLocal`'s signature grows the stream:
`func(kind byte, stream net.Conn) (net.Conn, error)` — the callee may read
(peek) bytes from the stream and must replay whatever it consumed into the
local conn before returning; `serveStreams` splices the remainder as today.

In `cmd/piperd/main.go`, the `KindPassthrough` branch peeks the ClientHello —
the same recording-conn + `GetConfigForClient`-abort trick as
`internal/relay/sni.go`, reading `hello.SupportedProtos` this time,
implemented fresh in `internal/agent` (relay code stays untouched):

- ALPN contains `acme-tls/1` → dial the solver's loopback `Addr()`.
- Otherwise → `127.0.0.1:443` (Caddy), as today.
- Replay the recorded hello bytes into the chosen conn either way. A hello
  that can't be parsed falls through to `:443` — Caddy owns rejecting it.

The solver starts with piperd whenever relay mode is up (an idle solver is one
dormant loopback listener holding an empty map). Wiring it into the per-domain
issuance lifecycle is #229; this issue only guarantees the solver is reachable
and the peek routes to it.

### 4. Testing (TDD)

- **Solver unit tests** (CI, no network): `Present` → TLS-dial the solver with
  `NextProtos: ["acme-tls/1"]` and the domain as SNI → assert the negotiated
  protocol and that the presented cert matches `tlsalpn01.ChallengeCert`'s
  digest; after `CleanUp`, the handshake fails; two concurrent domains both
  answer.
- **Peek unit tests** (CI): drive a real `tls.Client` handshake attempt into
  the peek over a pipe, with and without `acme-tls/1` in `NextProtos`; assert
  the routing decision and that the replayed bytes are exactly what the client
  sent. A non-TLS byte stream routes to `:443`.
- **Pebble integration**, env-gated like the existing
  `TestObtainAgainstPebble` (`RUN_ACME=<directory URL>`): `Obtain` an
  exact-host cert with only the ALPN solver configured — no DNS provider.
  This is the issue's acceptance criterion.
- **DNS-01 regression**: existing `internal/certs` and `internal/domain` tests
  keep passing unchanged.

## Out of scope

Later children of #224: relay 1:N pending-domain lifecycle (#227), relay port
80 (#228), the per-domain lifecycle manager and store wiring (#229),
exact-host deploy routes (#230), API (#231), CLI (#232). Direct-`:443` boxes
without a relay remain out of scope for per-app domains entirely.
