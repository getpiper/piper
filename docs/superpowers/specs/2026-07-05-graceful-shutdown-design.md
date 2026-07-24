# Graceful SIGTERM shutdown for piperd — design

_Date: 2026-07-05 · Issue: [#48](https://github.com/piperbox/piper/issues/48) · Part of the install-&-run epic ([#43](https://github.com/piperbox/piper/issues/43))_

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

## Approach — drain, then cancel

Shutdown uses a fixed 20-second application budget split into two phases:

1. **Drain (15 seconds).** Stop accepting API and webhook requests, then allow
   active requests and webhook deploys to finish normally.
2. **Cancel and clean up (up to 5 seconds).** Cancel webhook deploy contexts,
   force-close HTTP servers that did not drain, and wait only until the overall
   deadline for deployment cleanup. On the forced-cancellation path, retain
   Caddy and the store until that overall deadline even if tracked webhook work
   returns earlier; this gives cancelled API handlers the same cleanup window.
   Then release Caddy and the store and exit.

This keeps the clean-state benefit of draining for work that finishes promptly,
but makes the application timeout authoritative. A stuck deploy cannot keep
`piperd` alive indefinitely when it is run directly rather than by a supervisor.
The supervisor's `SIGKILL` after `TimeoutStopSec` remains a final backstop, not
the mechanism that normally enforces the application deadline.

## Changes

### `internal/webhook`

Give `Handler` a cancellable lifecycle context. Each deploy goroutine uses that
context instead of `context.Background()`, so shutdown cancellation reaches the
source provider, deploy orchestrator, and Docker runtime operations.

Promote the existing drain hook into lifecycle API with two operations:

- `StopAccepting()` atomically closes the handler's admission gate. Each HTTP
  handler registers with the wait group while holding the same gate, so no
  `WaitGroup.Add` can race with shutdown waiting. Requests arriving after the
  gate closes receive `503 Service Unavailable`.
- `WaitContext(ctx) bool` waits for all tracked goroutines and returns whether
  they drained before `ctx` expired.
- `Cancel()` cancels the lifecycle context and is safe to call more than once.

The wait group tracks both active HTTP handlers and detached deployment
goroutines. A handler adds its deployment goroutine before releasing its own
wait-group reference, so the counter cannot transiently reach zero during
handoff.

`WaitContext` must use a completion channel around `wg.Wait()` and select on the
caller's context. It must never block shutdown after the caller's deadline.

### `cmd/piperd` — `webhookStarter`
Retain the `*http.Server` and `*webhook.Handler` currently created as locals in
`run()` as struct fields. Add:

```go
func (w *webhookStarter) stop(ctx context.Context) {
    if w.srv == nil { return }   // never started (no GitHub App configured)
    w.handler.StopAccepting()    // close admission before any shutdown wait
    _ = w.srv.Shutdown(ctx)      // stop accepting new webhook deliveries
}
```

`stop` is a no-op when the webhook was never started (relay-less mode, or no
stored GitHub App). Draining and cancellation remain explicit operations on the
handler so `cmd/piperd` can coordinate them against the two shutdown phases.

### `internal/deploy` — cancellation cleanup

Deployment already passes its context through build, run, health-check, source,
and reporting operations. Preserve that propagation. When cancellation happens
after a container may have been created, stop that partial container with a
short cleanup context derived with `context.WithoutCancel` and bounded by the
remaining cleanup phase. Do not reuse the already-cancelled deployment context
for cleanup.

Persist a terminal `"failed"` deployment when the existing identifiers permit
it. Do not add a `"building"` state or change the status vocabulary.

### `cmd/piperd` — ordered, bounded shutdown
Extract the teardown into a testable helper (mirroring the existing
`runRenewLoop` seam), invoked from `main()` after `<-ctx.Done()`:

```go
const (
    shutdownTimeout = 20 * time.Second
    drainTimeout    = 15 * time.Second
)

func shutdown(api *http.Server, wh *webhookStarter, mgr *caddy.Manager, st *store.Store) {
    // Establish one overall deadline and a shorter graceful-drain deadline.
    // Stop both HTTP entry points, wait for webhook work during the drain
    // phase, then cancel and wait only for the time remaining overall.
    // Finally release Caddy and SQLite even if a worker did not cooperate.
}
```

`main()` replaces the current trailing `srv.Shutdown(context.Background())` +
`defer mgr.Stop()` / `defer st.Close()` with a single `shutdown(...)` call, then
`os.Exit(0)`. Ordering is now explicit: stop accepting, attempt a graceful
drain, cancel remaining work, then release Caddy and the store. Cooperative
workers finish before teardown; a worker that ignores cancellation cannot delay
process exit beyond the application budget.

`shutdownTimeout` remains a **fixed 20s constant**, comfortably under systemd's
default `TimeoutStopSec=90s`. `drainTimeout` reserves the final 5 seconds for
cancellation and cleanup. **No new env var or config** — honoring the epic's
"config unchanged" constraint.

Callers that started Caddy conditionally (`PIPER_SKIP_CADDY`) or ran without the
webhook still hold valid (possibly nil-guarded) references; `shutdown` tolerates
the absent pieces.

## Testing (TDD, failing test first)

1. **`internal/webhook`** — prove `WaitContext` reports a normal drain, reports
   deadline expiry without blocking, and `Cancel` reaches a blocking fake
   deployer's context.
2. **`internal/deploy`** — cancel during health-check/run and prove a partial
   container is stopped with a live, bounded cleanup context and a terminal
   failure is recorded.
3. **`cmd/piperd`** — unit-test the extracted shutdown coordinator with an
   injectable clock/timeouts: normal work drains before cancellation; stuck
   work is cancelled at the drain deadline; Caddy/store teardown still occurs;
   and the coordinator returns at the overall deadline.

`make test` and `make cross` must stay green.

## Out of scope

- The systemd/launchd unit files and env skeleton (that's #44, which builds on
  this clean-exit behavior).
- Any change to how Docker builds/runs app containers.
- A configurable shutdown timeout (fixed constant is sufficient).
