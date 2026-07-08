# CLI remote target: drive a box through the relay — design

Implements [#74](https://github.com/getpiper/piper/issues/74) (Part of #49), on
top of the relay control plane built in
[`2026-07-08-relay-control-stream-routing-design.md`](2026-07-08-relay-control-stream-routing-design.md).

Two of the issue's three open questions were already answered by shipped work:
auth UX is the existing `piper login` device flow, and the credential's home is
`~/.piper/piper/config.json` (0600) — `RelayAPI` + `AccountCredential` are
already written there. What remains is target selection, and the mechanism is
nearly free: the relay's control plane accepts
`https://api.<apex>/agents/<base-domain>/v1/...` with
`Authorization: Bearer <account-credential>`, strips the prefix, and swaps the
credential for the box's Token B. "Remote" is therefore just a different base
URL and token for the existing `internal/client.Client` — no new HTTP code, no
new client type.

## Target selection: `--remote` flag + `PIPER_REMOTE` env

Stateless and explicit (like `DOCKER_HOST`), not a sticky context (like
`kubectl config use-context`) — you always see where a command lands; a
forgotten context can't deploy to the wrong box. Local/loopback stays the
untouched default.

- One **global flag**, parsed in `run()` before the subcommand switch:
  `piper --remote <base-domain> <command> ...`. The base domain is the agent's
  unique, user-visible identity (what `piper connect` prints).
- The flag's default value is `PIPER_REMOTE`
  (`fs.String("remote", os.Getenv("PIPER_REMOTE"), ...)`), so
  `export PIPER_REMOTE=<base-domain>` makes every control command remote
  without retyping; the flag still overrides the env.
- Parsing globally means one implementation site instead of five per-command
  FlagSets; Go's flag parser stops at the first non-flag argument, so
  `piper --remote box deploy myapp --path .` parses cleanly.

## Which commands are remote-capable

Exactly the five that dial piperd's control API via `dialClient`:

| Command | Remote? | Why |
| --- | --- | --- |
| `create`, `deploy`, `list`, `app link` | Yes | plain control-API calls |
| `github setup` | Yes | the browser/callback dance runs on the laptop either way; only the two piperd calls (manifest, exchange) cross the relay — and the box is typically headless |
| `version` | No | no network |
| `login` | No | establishes the credential remote mode uses; not box-targeted |
| `connect` | No | claims a box and writes the enrollment onto the **local** machine; inherently "run me on the box" |

**Guard rails** for the non-remote commands (`login`, `connect`, `version`):

- The explicit `--remote` **flag** is an error: exit 2,
  `error: --remote does not apply to '<command>'` — never silently ignored
  (worst case otherwise: `piper --remote box connect` and the user believes
  they enrolled the remote box).
- The `PIPER_REMOTE` **env var** is simply not consulted by those commands —
  `export PIPER_REMOTE=...` followed by `piper login` (to refresh an expired
  credential) must keep working, matching how `DOCKER_HOST` coexists with
  `docker login`.

## Client wiring

`dialClient` grows the remote branch. When a remote target is set:

1. Load the client config as today.
2. Require `RelayAPI` and `AccountCredential`; if either is missing:
   `error: remote target requires a relay login; run 'piper login'`.
3. Return `client.New(cc.RelayAPI+"/agents/"+<remote>, cc.AccountCredential)`.

The existing `Client` and every command using it work unchanged — same request
shapes, same response decoding.

## Output and errors

- `deploy` against a remote target prints `deployed <name> (<status>)` with
  **no URL**. In relay-terminated mode the app's public hostname is assigned by
  the relay at deploy time (`<app-hash>-<username>.<apex>`, hash keyed on the
  relay's internal account ID) and is neither derivable by the CLI nor returned
  in the deploy response — printing nothing beats printing a wrong URL. The
  hardcoded `http://<name>.piper.localhost` line stays for local. Returning the
  routed hostname in the deploy response (which also fixes the URL for *local*
  relay-terminated deploys, a pre-existing gap) is a follow-up `[deploy]`
  issue, out of scope here.
- Relay-originated statuses pass through `responseError` verbatim: 401 bad
  credential, 404 not-yours/unknown agent, 503 box offline. No CLI
  special-casing.

## Testing

House style: table-driven, `httptest` fakes, `run()`-level where possible.

- `--remote box list` hits a fake relay at `/agents/box/v1/apps` with
  `Authorization: Bearer <account-credential>`.
- Local `piper list` still hits the fake piperd unchanged (regression guard).
- `PIPER_REMOTE` env selects the target; the flag overrides the env.
- Remote with no relay login → the helpful error above.
- `--remote` flag on `connect`/`login`/`version` → usage error (exit 2);
  `PIPER_REMOTE` set + `piper login` → works.
- Remote `deploy` prints `deployed <name> (<status>)` and no
  `piper.localhost` URL.

## Out of scope (YAGNI)

- Named contexts / `piper use` — layer on later if multi-box demand is real.
- Routed hostname in the deploy response (real app URLs for terminated mode,
  local and remote) — follow-up `[deploy]` issue.
- Agent listing (`piper boxes`) — needs a new relay endpoint; separate issue.
- Keychain/OS credential storage — `config.json` at 0600 stays the documented
  home, same as today.
