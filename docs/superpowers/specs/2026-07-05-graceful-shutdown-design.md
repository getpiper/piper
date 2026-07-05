# Graceful SIGTERM shutdown for piperd — design

_Date: 2026-07-05 · Issue: [#48](https://github.com/getpiper/piper/issues/48) · Part of the install-&-run epic ([#43](https://github.com/getpiper/piper/issues/43))_

## Goal

On `SIGTERM`/`SIGINT`, `piperd` stops accepting new work, drains in-flight
deploys to a consistent recorded state, releases the embedded Caddy listeners
(`:80`/`:443`), the relay tunnel, and the SQLite store, then exits `0` — all
within the service's stop timeout. This lets systemd/launchd (#44) stop and
`restart` the daemon cleanly, with no orphaned listener, tunnel, or locked DB.

```
SIGTERM => stop accepting (API + webhook) → drain in-flight deploys
        → stop Caddy (:80/:443) → close store → exit 0
```

This is purely `piperd`'s own lifecycle. Docker usage for app builds/runs is
unchanged.

## Current state

`cmd/piperd/main.go` already catches the signals via
`signal.NotifyContext(..., SIGINT, SIGTERM)` and, on `ctx.Done()`, calls
`srv.Shutdown(context.Background())` on the API server, with `defer`s for
`mgr.Stop()` (Caddy) and `st.Close()` (store). The tunnel-client goroutine is
tied to `ctx` and already stops. Gaps:

1. **The webhook HTTP server leaks.** `whSrv` is a local created inside
   `webhookStarter.run()` (`main.go:137`) and is never closed. Its listener and
   the `hooks.<base>` route are only reclaimed incidentally when Caddy stops.
2. **Detached webhook deploys are neither cancelled nor drained.** The webhook
   handler dispatches each deploy in a goroutine on `context.Background()`
   (`webhook.go:71`). A `wg`/`Wait()` drain hook already exists but is marked
   "test-only" and never called on shutdown — so a deploy in progress at
   shutdown is simply abandoned.
3. **`srv.Shutdown` is unbounded** — `context.Background()`, no stop-timeout
   budget.
4. **Teardown ordering is LIFO `defer`**, which races the detached deploy
   goroutines (store could close while a deploy still writes).
5. **No test** exercises the shutdown path.

Confirmed non-issues, recorded so they are not re-litigated:

- **No dangling `"building"`.** `deploy.go` never persists a `"building"` row; a
  deployment is written only at a terminal state (`running`/`failed`/`stopped`)
  *after* build+run+health completes. An interrupted deploy leaves no row — the
  DB is consistent by construction. The residual risk is an orphaned *container
  or route* from a deploy killed mid-flight, which draining addresses.
- **`:443` release.** `mgr.Stop()` → `caddy.Stop()` stops all Caddy apps and
  releases the `:80`/`:443` binds synchronously. The fix is to guarantee it runs
  in a deterministic order, not to change it.

## Approach — drain, don't cancel

On signal, **drain** in-flight deploys (let them reach a consistent recorded
state) rather than hard-cancel them mid-container. The webhook path runs on
`context.Background()` and isn't wired for cancellation anyway; draining is both
simpler and produces a cleaner recorded state. Draining is bounded by a single
shutdown budget — if a deploy overruns, the process exits regardless and the
supervisor's `SIGKILL` (past `TimeoutStopSec`) is the backstop.

## Changes

### `internal/webhook`
Promote `Handler.Wait()` from "test-only" to real API — it already blocks on the
`wg` that tracks in-flight deploy goroutines. No behavior change; only the doc
comment and intent change.

### `cmd/piperd` — `webhookStarter`
Retain the `*http.Server` and `*webhook.Handler` currently created as locals in
`run()` as struct fields. Add:

```go
func (w *webhookStarter) stop(ctx context.Context) {
    if w.srv == nil { return }   // never started (no GitHub App configured)
    _ = w.srv.Shutdown(ctx)      // stop accepting new webhook deliveries
    w.handler.Wait()             // drain in-flight deploys
}
```

`stop` is a no-op when the webhook was never started (relay-less mode, or no
stored GitHub App).

### `cmd/piperd` — ordered, bounded shutdown
Extract the teardown into a testable helper (mirroring the existing
`runRenewLoop` seam), invoked from `main()` after `<-ctx.Done()`:

```go
const shutdownTimeout = 20 * time.Second

func shutdown(api *http.Server, wh *webhookStarter, mgr *caddy.Manager, st *store.Store) {
    ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
    defer cancel()
    _ = api.Shutdown(ctx) // stop accepting API requests, drain in-flight
    wh.stop(ctx)          // stop accepting webhooks, drain in-flight deploys
    mgr.Stop()            // release :80/:443
    _ = st.Close()        // close SQLite
}
```

`main()` replaces the current trailing `srv.Shutdown(context.Background())` +
`defer mgr.Stop()` / `defer st.Close()` with a single `shutdown(...)` call, then
`os.Exit(0)`. Ordering is now explicit: accept-stop and deploy-drain happen
before Caddy and the store are torn down, so nothing races a live deploy.

`shutdownTimeout` is a **fixed 20s constant**, comfortably under systemd's
default `TimeoutStopSec=90s`. **No new env var or config** — honoring the epic's
"config unchanged" constraint.

Callers that started Caddy conditionally (`PIPER_SKIP_CADDY`) or ran without the
webhook still hold valid (possibly nil-guarded) references; `shutdown` tolerates
the absent pieces.

## Testing (TDD, failing test first)

1. **`internal/webhook`** — dispatch a deploy backed by a fake `Deployer` that
   blocks on a channel; assert `Wait()` does not return until the deploy
   completes (drain, not drop). Unblock the fake, then confirm `Wait()` returns
   and the deploy ran to its terminal state.
2. **`cmd/piperd`** — unit-test the extracted `shutdown` helper with fakes/stubs
   for the API server, webhook starter, Caddy manager, and store: assert each is
   torn down in order, in-flight work is drained, and the helper returns within
   the budget.

`make test` and `make cross` must stay green.

## Out of scope

- The systemd/launchd unit files and env skeleton (that's #44, which builds on
  this clean-exit behavior).
- Any change to how Docker builds/runs app containers.
- A configurable shutdown timeout (fixed constant is sufficient).
