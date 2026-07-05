# Graceful SIGTERM shutdown for piperd — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On `SIGTERM`/`SIGINT`, piperd stops accepting new work, drains in-flight deploys, releases Caddy/store, and exits `0` in a deterministic order — so systemd/launchd can stop and restart it cleanly.

**Architecture:** Promote the webhook Handler's existing `wg` drain hook (`Wait()`) to real API; give `webhookStarter` a `stop(ctx)` that shuts its HTTP server then drains; add an ordered, time-bounded `shutdown(...)` helper in `cmd/piperd` (mirroring the existing `runRenewLoop` test seam) that `main()` calls on `ctx.Done()`.

**Tech Stack:** Go, `net/http` graceful `Shutdown`, `context.WithTimeout`, embedded Caddy (`caddy.Stop`), `modernc.org/sqlite` store.

## Global Constraints

- **No cgo** — all builds pass with `CGO_ENABLED=0`; `make cross` (linux/arm64) stays green.
- **Config unchanged** — no new env vars or config system. Shutdown timeout is a fixed `20 * time.Second` constant.
- **Module path** `github.com/getpiper/piper`.
- **Deployment status strings** remain exactly `"building"`, `"running"`, `"failed"`, `"stopped"` (this plan writes none of them).
- **Layering** — nothing imports "up"; `webhook` stays unaware of `cmd/piperd`.
- **Commits** — one per task step group, conventional-commit style, ending with the `Co-Authored-By` trailer. Reference `Part of #48`.

---

### Task 1: Promote the webhook drain hook and prove it drains

The webhook Handler already dispatches each deploy in a goroutine tracked by `h.wg`, with `Wait()` blocking until they finish — but `Wait()` is labelled "test-only" and no shutdown path calls it. Promote it to real API and add a test proving it blocks until an in-flight deploy completes (drain, not drop).

**Files:**
- Modify: `internal/webhook/webhook.go:75-76` (the `Wait` doc comment)
- Test: `internal/webhook/webhook_test.go` (add one test + a blocking fake deployer)

**Interfaces:**
- Consumes: `webhook.New(p source.Provider, s *store.Store, d webhook.Deployer, baseDomain string) *webhook.Handler`; `(*Handler).ServeHTTP`; `(*Handler).Wait()`; the `webhook.Deployer` interface (`Deploy`, `DeployPreview`, `TeardownPreview`).
- Produces: `(*Handler).Wait()` as a supported public method that blocks until all in-flight deploy goroutines finish. Task 2 relies on this.

- [ ] **Step 1: Write the failing test**

Add to `internal/webhook/webhook_test.go`. Add `"time"` to that file's imports.

```go
// blockingDeployer holds Deploy open until release is closed, signalling
// started once it is in-flight, so a test can observe drain behaviour.
type blockingDeployer struct {
	release chan struct{}
	started chan struct{}
	calls   int
}

func (d *blockingDeployer) Deploy(context.Context, string, string) (store.Deployment, error) {
	close(d.started)
	<-d.release
	d.calls++
	return store.Deployment{}, nil
}
func (d *blockingDeployer) DeployPreview(context.Context, string, int, string) (store.Deployment, error) {
	return store.Deployment{}, nil
}
func (d *blockingDeployer) TeardownPreview(context.Context, string, int) error { return nil }

func TestWaitDrainsInFlightDeploy(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main", SHA: "s1",
	}}
	d := &blockingDeployer{release: make(chan struct{}), started: make(chan struct{})}
	h := webhook.New(p, s, d, "piper.localhost")

	if rec := post(h); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	<-d.started // deploy goroutine is now in-flight

	done := make(chan struct{})
	go func() { h.Wait(); close(done) }()

	select {
	case <-done:
		t.Fatal("Wait returned before in-flight deploy finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(d.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after deploy finished")
	}
	if d.calls != 1 {
		t.Fatalf("deploy calls = %d", d.calls)
	}
}
```

- [ ] **Step 2: Run the test to verify it passes but the comment is still wrong**

Run: `go test ./internal/webhook/ -run TestWaitDrainsInFlightDeploy -v`
Expected: PASS (the drain behaviour already exists; this test locks it in). If it does not compile, fix the `time` import.

> Note: this behaviour test passes against current code — that is intended. It is the regression guard that lets Task 2 depend on `Wait()`. The only source change in this task is the doc-comment promotion below.

- [ ] **Step 3: Promote the `Wait` doc comment**

In `internal/webhook/webhook.go`, replace:

```go
// Wait blocks until in-flight deploys finish. Test-only.
func (h *Handler) Wait() { h.wg.Wait() }
```

with:

```go
// Wait blocks until all in-flight deploy goroutines finish. Used by graceful
// shutdown to drain work before the process exits, and by tests.
func (h *Handler) Wait() { h.wg.Wait() }
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/webhook/`
Expected: `ok` (all webhook tests pass).

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/webhook.go internal/webhook/webhook_test.go
git commit -m "$(cat <<'EOF'
feat(webhook): promote Wait() to a supported drain hook

Part of #48

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Ordered, bounded shutdown in piperd

Give `webhookStarter` a `stop(ctx)` that shuts down its HTTP server and drains, then add a `shutdown(...)` helper that tears down API → webhook → Caddy → store in order under a fixed 20s budget, and wire `main()` to call it on `ctx.Done()`.

**Files:**
- Modify: `cmd/piperd/main.go` — `webhookStarter` struct + `run()` (lines 109-149), the Caddy/`mgr` block (lines 54-65), the shutdown tail (lines 101-104), plus the new helper and interfaces.
- Test: `cmd/piperd/main_test.go` — add ordering + nil-tolerance tests for `shutdown`.

**Interfaces:**
- Consumes: `(*webhook.Handler).Wait()` from Task 1; `(*http.Server).Shutdown(context.Context) error`; `(*caddy.Manager).Stop()`; `(*store.Store).Close() error`.
- Produces (all in `package main`):
  - `const shutdownTimeout = 20 * time.Second`
  - `type apiShutdowner interface{ Shutdown(context.Context) error }`
  - `type webhookStopper interface{ stop(context.Context) }`
  - `type listenerStopper interface{ Stop() }`
  - `type storeCloser interface{ Close() error }`
  - `func (w *webhookStarter) stop(ctx context.Context)` — nil-receiver safe; no-op if the webhook never started.
  - `func shutdown(api apiShutdowner, wh webhookStopper, mgr listenerStopper, st storeCloser)` — invokes each non-nil dependency once, in the order api, webhook, caddy, store, under a `shutdownTimeout` context.

- [ ] **Step 1: Write the failing test**

Add to `cmd/piperd/main_test.go`. Add `"context"` (already imported) and `"reflect"` to its imports.

```go
type recShutdowner struct{ rec func(string) }

func (r *recShutdowner) Shutdown(context.Context) error { r.rec("api"); return nil }

type recWebhook struct{ rec func(string) }

func (r *recWebhook) stop(context.Context) { r.rec("webhook") }

type recManager struct{ rec func(string) }

func (r *recManager) Stop() { r.rec("caddy") }

type recCloser struct{ rec func(string) }

func (r *recCloser) Close() error { r.rec("store"); return nil }

func TestShutdownTearsDownInOrder(t *testing.T) {
	var order []string
	rec := func(s string) { order = append(order, s) }
	shutdown(&recShutdowner{rec}, &recWebhook{rec}, &recManager{rec}, &recCloser{rec})
	want := []string{"api", "webhook", "caddy", "store"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestShutdownSkipsAbsentDependencies(t *testing.T) {
	var order []string
	rec := func(s string) { order = append(order, s) }
	// No Caddy (PIPER_SKIP_CADDY) and no webhook (non-relay mode).
	shutdown(&recShutdowner{rec}, nil, nil, &recCloser{rec})
	want := []string{"api", "store"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/piperd/ -run TestShutdown -v`
Expected: FAIL — build error, `undefined: shutdown` (and the interface types).

- [ ] **Step 3: Add the interfaces, constant, and `shutdown` helper**

In `cmd/piperd/main.go`, add near the top of the file (after the imports, before `func main`):

```go
const shutdownTimeout = 20 * time.Second

type apiShutdowner interface{ Shutdown(context.Context) error }
type webhookStopper interface{ stop(context.Context) }
type listenerStopper interface{ Stop() }
type storeCloser interface{ Close() error }

// shutdown tears down piperd's own resources in a deterministic order under a
// single time budget: stop accepting API + webhook requests and drain their
// in-flight work, then release the Caddy listeners (:80/:443) and close the
// store. Absent dependencies (nil) are skipped — Caddy is nil under
// PIPER_SKIP_CADDY, the webhook is nil in non-relay mode.
func shutdown(api apiShutdowner, wh webhookStopper, mgr listenerStopper, st storeCloser) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if api != nil {
		_ = api.Shutdown(ctx)
	}
	if wh != nil {
		wh.stop(ctx)
	}
	if mgr != nil {
		mgr.Stop()
	}
	if st != nil {
		_ = st.Close()
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/piperd/ -run TestShutdown -v`
Expected: PASS for both `TestShutdownTearsDownInOrder` and `TestShutdownSkipsAbsentDependencies`.

- [ ] **Step 5: Give `webhookStarter` a tracked server + `stop`**

In `cmd/piperd/main.go`, replace the struct (lines 109-114):

```go
type webhookStarter struct {
	cfg  config.Config
	st   *store.Store
	rt   *runtime.DockerRuntime
	once sync.Once
}
```

with:

```go
type webhookStarter struct {
	cfg     config.Config
	st      *store.Store
	rt      *runtime.DockerRuntime
	once    sync.Once
	srv     *http.Server
	handler *webhook.Handler
}
```

In `run()` (lines 135-142), replace the local `wh`/`whSrv` with the fields:

```go
	wdep := deploy.New(w.st, w.rt, caddy.NewClient(w.cfg.CaddyAdmin), w.cfg.BaseDomain)
	w.handler = webhook.New(prov, w.st, wdep, w.cfg.BaseDomain)
	w.srv = &http.Server{Addr: w.cfg.WebhookAddr, Handler: w.handler}
	go func() {
		if err := w.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("webhook serve: %v", err)
		}
	}()
```

Then add the `stop` method (place it right after `run()`):

```go
// stop shuts the webhook HTTP server and drains in-flight deploys. It is safe
// on a nil *webhookStarter (non-relay mode) and when the webhook never started
// (no GitHub App configured). The once.Do barrier ensures any concurrent
// start() has fully published w.srv before we read it.
func (w *webhookStarter) stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.once.Do(func() {})
	if w.srv == nil {
		return
	}
	_ = w.srv.Shutdown(ctx)
	w.handler.Wait()
}
```

- [ ] **Step 6: Hoist `mgr` and wire the shutdown tail in `main()`**

In `main()`, change the Caddy block (lines 54-65) so `mgr` is visible after the block and drop its `defer`:

```go
	// Unless PIPER_SKIP_CADDY is set (e.g. a caddy is already running), manage one.
	var mgr *caddy.Manager
	if os.Getenv("PIPER_SKIP_CADDY") == "" {
		opts := []caddy.Option{}
		if cfg.RelayAddr != "" {
			opts = append(opts, caddy.WithHTTPS(":443"))
		}
		mgr, err = caddy.StartManager(cfg.CaddyAdmin, ":80", opts...)
		if err != nil {
			log.Fatalf("caddy: %v", err)
		}
	}
```

Remove the now-redundant `defer st.Close()` at line 44 (shutdown closes the store; early fatal paths use `log.Fatalf`, which calls `os.Exit` and skips defers anyway).

Replace the shutdown tail (lines 101-104):

```go
	<-ctx.Done()
```

with:

```go
	<-ctx.Done()
	log.Println("shutting down")

	// Only pass dependencies that exist; a typed-nil wrapped in an interface
	// would read as non-nil, so assign the interface only when the concrete
	// value is present.
	var mgrStop listenerStopper
	if mgr != nil {
		mgrStop = mgr
	}
	var whStop webhookStopper
	if wh != nil {
		whStop = wh
	}
	shutdown(srv, whStop, mgrStop, st)
	os.Exit(0)
```

- [ ] **Step 7: Verify the whole package builds and tests pass**

Run: `go build ./cmd/piperd/ && go test ./cmd/piperd/ ./internal/webhook/`
Expected: build succeeds; both packages report `ok`.

- [ ] **Step 8: Full suite + cross-compile**

Run: `make test && make cross`
Expected: all tests pass (Docker-dependent tests skip cleanly if Docker is absent); `make cross` builds linux/arm64 without error.

- [ ] **Step 9: Commit**

```bash
git add cmd/piperd/main.go cmd/piperd/main_test.go
git commit -m "$(cat <<'EOF'
feat(agent): graceful, ordered SIGTERM shutdown for piperd

Drain the API server and webhook deploys, then release the Caddy
listeners and close the store, in a deterministic order under a fixed
20s budget. Fixes the leaked webhook HTTP server and undrained deploy
goroutines on stop/restart.

Part of #48

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Notes for the implementer

- **Shutdown budget is best-effort by design.** `handler.Wait()` waits on webhook deploys that run on `context.Background()` and cannot be cancelled; a build in progress may outlive the 20s budget. That is accepted (see the spec): systemd's `SIGKILL` past `TimeoutStopSec` is the backstop, and because `deploy.go` writes only terminal rows, a killed deploy leaves the DB consistent (no `"building"` row).
- **Do not** add a configurable timeout, cancel in-flight deploys, or touch how Docker builds/runs apps — all out of scope for #48.
- The systemd/launchd unit files that consume this clean-exit behaviour are #44, a separate follow-up.
