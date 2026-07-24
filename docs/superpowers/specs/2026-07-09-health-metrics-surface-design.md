# Health/metrics surface over the tunnel — design

Implements [#75](https://github.com/piperbox/piper/issues/75) (Part of #49).
Consumes the authenticated control API (#72) and the relay control-stream
routing (#73): a remote caller — the CLI today, the dashboard (#76) later —
can answer *"is my box up, and what's deployed?"*.

## Scope decision

v1 is **liveness + per-app last-deploy status only** — the issue's own
default. Container CPU/mem waits until the dashboard proves it needs it; it
would drag a runtime-layer dependency into what is otherwise a store read.

Each fact gets one owner:

- **The relay owns "is the box up"** — a connected tunnel session is the
  liveness signal, and the relay already holds it in memory (`Router`).
- **piperd owns "what's deployed"** — deploy status is a store read behind
  the existing token gate.

No new auth machinery anywhere: every surface below sits behind gates that
already exist.

## 1. Per-app deploy status (`[agent]`)

### Store

New query `LatestDeployment(app string) (Deployment, error)`: the newest
deployment row for the app with `pr = 0`, **any** status, ordered by
`created_at DESC`. PR previews (`pr > 0`) are excluded — a failed preview
must not color the app's production status. Returns `ErrNotFound` when the
app has never been deployed. No schema change.

### API

`GET /v1/apps` and `GET /v1/apps/{name}` responses gain a `Status` field per
app: the latest non-preview deployment's status — exactly one of
`"building"`, `"running"`, `"failed"`, `"stopped"` — or `""` when the app has
never been deployed.

The API layer composes this: a small response struct embeds `store.App` and
adds `Status`, filled from `LatestDeployment` per app (`ErrNotFound` → `""`).
The store stays persistence-only; the API stays transport.

Both endpoints already sit behind `RequireToken` (#77), so the *"metrics
endpoints honor the same authn/authz as the rest of the control API"*
criterion is inherited, not re-built. Remotely, the same responses arrive via
the #73 proxy (`/agents/<base>/v1/apps`), Token-B-validated at the box as
always.

## 2. Liveness (`[relay]`)

`GET api.<apex>/agents/<base-domain>` — the bare agent path, no `/v1/` tail —
is answered **by the relay itself**; it never opens a tunnel stream. Handled
in `NewControlProxy` at the spot that path shape currently 404s.

Same gates as the proxy, same order, same semantics:

| Condition | Response |
| --- | --- |
| Missing/unknown/disabled account credential | `401` |
| Agent unknown **or** owned by another account | `404` (no existence leak) |
| Owned agent | `200` `{"agent":"<base>","connected":true\|false}` |

`connected` comes from `router.Lookup(base)` — the *"connected session ⇒ box
up"* signal the acceptance criteria name. Deliberate asymmetry with the
proxy: a proxied `/v1/...` request to an offline box stays `503` (the request
could not be served), but liveness returns `200` with `connected: false` —
offline is the answer, not an error. No last-seen timestamp is stored;
in-memory presence is the whole signal (YAGNI until #76 asks).

## 3. `piper status` (`[cli]`)

New command honoring the global `--remote` flag:

- **Remote** (`piper status --remote <base>`): call `client.Liveness()` — a
  `GET` at the client's base path, which for a remote client is exactly
  `<RelayAPI>/agents/<base>`. Print `box <base>: connected` or
  `box <base>: offline`. If offline, stop — app queries would just 503. If
  connected, `ListApps()` and print one line per app:
  `name\tstatus=running\tport=8080`, with `status=-` for a never-deployed app.
- **Local** (`piper status`): there is no liveness question — the caller is
  talking to the box directly. Print only the app lines.

`internal/client` gains `Liveness()` and the `Status` field in the apps
response it already decodes. `piper list` output stays untouched — `status`
is the new surface.

## Error semantics

All inherited, nothing new: `401`/`404` from the relay gates (§2 table),
`401` from piperd's token gate, `503` only for proxied calls to an offline
box. The CLI reports them through its existing error paths.

## Testing

TDD, house style (fakes; in-memory tunnel pairs for the relay):

- **store**: `LatestDeployment` picks the newest row by `created_at`;
  ignores `pr > 0` rows; `ErrNotFound` for a never-deployed app.
- **api**: apps responses carry the right `Status` per app and `""` when
  undeployed; both endpoints still `401` without a token.
- **relay**: liveness matrix — bad credential `401`; unknown agent `404`;
  other tenant's agent `404`; owned + connected → `{"connected":true}`;
  owned + offline → `{"connected":false}`; liveness opens no tunnel stream.
- **cli/client**: `Liveness()` against a fake server; `piper status` output
  for remote-connected, remote-offline, and local.
- **e2e**: extend the existing self-service relay loop — after
  `login → connect → deploy`, `GET /agents/<base>` reports
  `connected: true` and `/agents/<base>/v1/apps` shows the deployed app
  `running`.

## Out of scope

- Container CPU/mem metrics (revisit with #76).
- Last-seen / heartbeat history persistence on the relay.
- Dashboard consumption (#76); capability scoping (read-only tokens) stays
  deferred per the trust-model spec.
