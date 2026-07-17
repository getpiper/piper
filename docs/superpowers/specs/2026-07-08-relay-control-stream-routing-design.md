# Relay control-stream routing + caller→agent authz — design

Implements [#73](https://github.com/getpiper/piper/issues/73) (Part of #49),
against the trust model settled in
[`2026-07-07-relay-control-trust-model-design.md`](2026-07-07-relay-control-trust-model-design.md):
three anchors (enrollment token, relay account credential, Token B), hung off a
relay account. This doc fixes the wire mechanics that spec deliberately left to
the implementing issue — how a control request reaches a box, how the relay gets
its Token B, and how authz is enforced.

## One deviation from the trust-model spec: agent-push, not relay-pull

The trust-model spec says *"the relay asks piperd — over the tunnel — to mint a
Token B."* Taken literally (a relay-initiated pull), that has a bootstrap
problem: `piperd`'s `/v1/tokens` endpoint is itself Token-B-gated
([#77](https://github.com/getpiper/piper/pull/77), no transport bypass), so
minting the *first* Token B would need a special piperd mint-endpoint
authenticated by something other than Token B — reopening exactly the
"does the tunnel count as identity?" question #77 closed.

**Decision: flip the direction.** `piperd` owns its token store; minting is a
local operation needing no one's permission. On first connect after enrollment,
piperd mints a Token B itself and *pushes* it to the relay over the agent→relay
control channel it already uses for hostname registration. There is no mint
endpoint at all — no new inbound auth surface on piperd. The box volunteers a
credential to the relay it already chose to trust (same trust content as the
owner pasting a token by hand, just automated, and in the same outbound
direction as everything else piperd does). The trust cost is exactly what the
spec already concedes ("a compromised relay can drive boxes in the control plane
until the owner revokes") — nothing more. The spec self-scopes to trust model
("exact wire formats belong to the implementing issues"); push strictly
strengthens its invariants, so this is a refinement, not a conflict.

## Data flow

```
piper CLI ──HTTPS──▶ api.<apex>:443 (SNI-dispatched, wildcard cert)
                      │ 1. 401 unless Authorization: Bearer <account-credential> authenticates
                      │ 2. 404 unless that account owns <base-domain>   (authz — never reaches box)
                      │ 3. 503 unless the agent's tunnel session is live
                      │ 4. strip /agents/<base-domain> prefix; swap Authorization → stored Token B
                      └──▶ OpenKind(KindControlAPI) on that session ──▶ piperd dials 127.0.0.1:8088
                                                                          5. piperd validates Token B (#77) — always
```

- The target agent is named by **path prefix**:
  `https://api.<apex>/agents/<base-domain>/v1/...` (GitHub-style
  `/repos/<owner>/<repo>/...`). The base domain is the agent's unique,
  user-visible identity (the `agents` key; what `piper connect` prints).
- Cross-tenant and unknown agents both get **404**, not 403 — existence is not
  leaked. Bad credential is 401. Owned-but-offline agent is 503.
- The caller's account credential is **never** forwarded to the box; the box's
  Token B is **never** sent to the caller. Each secret stays in its own hop.
- piperd still validates Token B on every request — the relay hop grants
  nothing at the box.
- **App traffic is untouched**: `KindPassthrough` (E2E TLS to the box) and
  `KindHTTP` (shared-domain termination) work exactly as before. The relay
  decrypts control traffic only.

## Components

### `internal/tunnel` — protocol additions

- New relay→agent stream kind: `KindControlAPI byte = 'A'` — carries one raw
  HTTP/1.1 request/response; the agent pipes it to the control API, exactly as
  `KindHTTP` pipes to `:80`.
- New agent→relay op on the existing `KindControl` channel:
  `{"op":"provision","token":"<plaintext Token B>"}`. It rides the
  authenticated yamux session, so it can only ever set the token for the
  session's own agent — no extra authz needed.

### `cmd/piperd` + `internal/agent` — provisioning push and stream handling

- `TunnelClient.Run` gains an on-connect hook (the `agent` package stays
  store-blind, per layering; the hook is wired in `cmd/piperd`).
- The hook, on each connect: if **no token labeled `relay:<base-domain>` exists
  in the store — live or revoked** — mint one (`store.CreateToken`) and push it
  via a new `TunnelClient.Provision(token)`. The token row itself is the
  provisioning marker; no new table:
  - `piperd token revoke` is soft (sets `revoked_at`, row persists) — a revoked
    row means "the owner said no": piperd never re-mints for that enrollment.
    The owner's unilateral cutoff holds.
  - Re-running `piper connect` creates a **new enrollment** (new base domain →
    new label) → fresh mint. This is exactly the trust-model spec's
    "re-provisions on the next claim".
- `dialLocal` in `cmd/piperd/main.go` gains one branch:
  `KindControlAPI → net.Dial("tcp", <control API addr>)` (default
  `127.0.0.1:8088`).

### `internal/relay` — storage, proxy, dispatch

- **Storage**: new `agents.control_token` column, plaintext by necessity — the
  relay must present it verbatim (the trust-model spec's stated cost). The
  `provision` op overwrites it for the session's agent.
- **Proxy**: new routes on the existing API handler (`api.go`, alongside
  login/enroll): `/agents/{agent}/v1/...`. After the auth/authz/liveness gates
  above, an `httputil.ReverseProxy` forwards the request:
  - transport dials `sess.OpenKind(KindControlAPI)`; keep-alives disabled — one
    yamux stream per request, so no pooled stream can outlive its session;
  - path rewritten to strip the `/agents/<base-domain>` prefix;
  - `Authorization` header replaced with `Bearer <control_token>`.
- **Wiring**: the API handler needs the live `Router` (session lookup), so the
  `Router` moves out of `Serve` — created in `main`, passed to both `Serve` and
  the API constructor.
- **SNI dispatch**: in `handlePublic`, `SNI == "api."+apex` → terminate with the
  existing wildcard cert (`*.<apex>` covers the single-level `api.<apex>` — zero
  new cert or port) → serve the relay's own API handler in-process. Checked
  *before* app-hostname lookup, so no app registration can shadow the control
  plane. The plaintext `apiAddr` (`:8080`, loopback-warned) keeps serving the
  same handler for dev/e2e. A passthrough-only relay (no wildcard cert) has no
  `:443` control plane — same posture as shared-domain termination today.

## Error semantics (relay-originated responses)

| Condition | Response |
| --- | --- |
| Missing/unknown/disabled account credential | `401` |
| Agent unknown **or** owned by another account | `404` (no existence leak) |
| Agent owned but tunnel not connected | `503` |
| Agent connected, box responds | box's response, verbatim |

If the relay holds no Token B (never provisioned — e.g. an older piperd) or a
revoked one, the forwarded request simply fails piperd's gate and the caller
sees the box's `401` verbatim — accurate, since remote control was never or is
no-longer granted. The relay does not special-case it.

> **Old piperd + new relay is a `502`, not the `401` above.** The no-Token-B
> narrative assumes a piperd new enough to *understand* the `KindControlAPI`
> stream and pipe it to its control API (where the `401` is produced). A piperd
> from before this feature has no `KindControlAPI` branch in `dialLocal`, so the
> forwarded stream falls through to the default (passthrough) handling and is
> piped as **plaintext HTTP into the box's `:443`**, which expects TLS. The box
> never returns a clean `401`; the relay's reverse-proxy transport sees the
> connection break and its `ErrorHandler` returns **`502` (Bad Gateway)**. Both
> outcomes deny access — this is documented only so nobody chases the `401` the
> narrative implies when they meet a `502` against a stale box.

## Testing

- **Unit** (fakes / in-memory tunnel pairs, house style):
  - authz matrix: bad credential → 401; other tenant's agent → 404; unknown
    agent → 404; owned-but-offline → 503; owned + live → proxied.
  - proxy mechanics: prefix stripped, Token B injected, caller's credential not
    forwarded, box response returned verbatim.
  - provision op stores the token for the session's own agent only.
  - marker semantics: no re-mint while a `relay:<base>` row exists (live or
    revoked); fresh mint for a new enrollment.
  - SNI dispatch: `api.<apex>` reaches the API handler; app hostnames still
    terminate/passthrough as before.
- **E2E** (extends the existing self-service loop): `login → connect → deploy`,
  then a control request through the relay (`api.<apex>/agents/<base>/v1/...`)
  reaches the real, token-gated piperd and returns real state; plus an
  authz-denial (second account → 404) end-to-end.

## Out of scope

- CLI `--remote` UX and credential storage — #74.
- Health/metrics surface — #75; dashboard — #76.
- Capability scoping (`scope` column exists; enforcement waits for a read-only
  consumer) and cross-account grants — deferred per the trust-model spec.
