# Graceful SIGTERM shutdown for piperd — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On `SIGTERM`/`SIGINT`, piperd stops accepting work, drains for 15 seconds, cancels remaining deployments, cleans partial containers, releases Caddy/SQLite, and exits within a fixed 20-second application budget.

**Architecture:** Give the webhook handler a cancellable lifecycle context and context-aware wait API. Preserve partial container IDs and clean them with a detached, bounded context after deployment cancellation. Coordinate HTTP drain, forced cancellation, and infrastructure teardown in `cmd/piperd` under separate drain and overall deadlines.

**Tech Stack:** Go, `context`, `net/http`, Docker SDK, embedded Caddy, `modernc.org/sqlite`.

## Global Constraints

- **No cgo** — all builds pass with `CGO_ENABLED=0`; `make cross` (linux/arm64) stays green.
- **Config unchanged** — no new environment variable or config field. `shutdownTimeout` is fixed at `20 * time.Second`; `drainTimeout` is fixed at `15 * time.Second`.
- **Statuses unchanged** — exactly `"building"`, `"running"`, `"failed"`, and `"stopped"`; this work does not introduce a `"building"` write.
- **Layering unchanged** — `webhook` does not import `deploy` or `cmd/piperd`; `deploy` depends only on its existing interfaces.
- **TDD** — add each regression test before its implementation.
- **Commits** — one commit per task, conventional style, with `Part of #48`; do not add a Claude identity footer.

---

### Task 1: Make webhook work cancellable and waitable with a deadline

**Files:**
- Modify: `internal/webhook/webhook.go`
- Test: `internal/webhook/webhook_test.go`

**Interfaces:**
- Consumes: existing `Deployer` methods, all of which already accept `context.Context`.
- Produces: `(*Handler).StopAccepting()`, `(*Handler).WaitContext(context.Context) bool`, `(*Handler).Cancel()`, and the retained `(*Handler).Wait()` test convenience method.

- [ ] **Step 1: Add a blocking deployer and lifecycle tests**

Add `"time"` to `internal/webhook/webhook_test.go`, then add:

```go
type blockingDeployer struct {
	started  chan struct{}
	finished chan struct{}
}

func (d *blockingDeployer) Deploy(ctx context.Context, _, _ string) (store.Deployment, error) {
	close(d.started)
	<-ctx.Done()
	close(d.finished)
	return store.Deployment{}, ctx.Err()
}

func (d *blockingDeployer) DeployPreview(context.Context, string, int, string) (store.Deployment, error) {
	return store.Deployment{}, nil
}

func (d *blockingDeployer) TeardownPreview(context.Context, string, int) error { return nil }

func TestWaitContextTimesOutAndCancelStopsInFlightDeploy(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main", SHA: "s1",
	}}
	d := &blockingDeployer{started: make(chan struct{}), finished: make(chan struct{})}
	h := webhook.New(p, s, d, "piper.localhost")

	if rec := post(h); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	<-d.started

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelWait()
	if h.WaitContext(waitCtx) {
		t.Fatal("WaitContext reported a drain while deploy was blocked")
	}

	h.Cancel()
	if !h.WaitContext(context.Background()) {
		t.Fatal("WaitContext did not report drain after cancellation")
	}
	select {
	case <-d.finished:
	default:
		t.Fatal("deployment context was not cancelled")
	}
}

func TestWaitContextReportsCompletedWork(t *testing.T) {
	h := webhook.New(&fakeProvider{}, newStore(t), &fakeDeployer{}, "piper.localhost")
	if !h.WaitContext(context.Background()) {
		t.Fatal("WaitContext reported timeout with no in-flight work")
	}
}

func TestStopAcceptingRejectsNewWork(t *testing.T) {
	h := webhook.New(&fakeProvider{}, newStore(t), &fakeDeployer{}, "piper.localhost")
	h.StopAccepting()
	if rec := post(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test ./internal/webhook -run 'TestWaitContext' -v`

Expected: FAIL to compile because `WaitContext` and `Cancel` do not exist.

- [ ] **Step 3: Add the handler lifecycle context**

Add fields to `Handler`:

```go
	ctx    context.Context
	cancel context.CancelFunc
	lifecycleMu sync.Mutex
	accepting   bool
```

Initialize them in `New`:

```go
func New(p source.Provider, s *store.Store, d Deployer, baseDomain string) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Handler{
		prov: p, store: s, deploy: d, baseDom: baseDomain,
		locks: map[string]*sync.Mutex{}, lastSHA: map[string]string{},
		ctx: ctx, cancel: cancel, accepting: true,
	}
}
```

At the start of `ServeHTTP`, before reading the request body, register the HTTP
handler through the admission gate:

```go
	h.lifecycleMu.Lock()
	if !h.accepting {
		h.lifecycleMu.Unlock()
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	h.wg.Add(1)
	h.lifecycleMu.Unlock()
	defer h.wg.Done()
```

Keep the existing deployment-goroutine `Add(1)` before the HTTP handler returns,
and replace `h.process(context.Background(), ev)` with `h.process(h.ctx, ev)`.

Replace the old `Wait` comment/method with:

```go
// Wait blocks until all in-flight deploy goroutines finish.
func (h *Handler) Wait() { h.wg.Wait() }

// WaitContext waits for in-flight deploy goroutines until ctx expires. It
// returns true when every goroutine finished and false when ctx expired first.
func (h *Handler) WaitContext(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Cancel asks all in-flight webhook work to stop. It is idempotent.
func (h *Handler) Cancel() { h.cancel() }

// StopAccepting rejects new webhook work and closes the WaitGroup admission
// gate before shutdown starts waiting.
func (h *Handler) StopAccepting() {
	h.lifecycleMu.Lock()
	h.accepting = false
	h.lifecycleMu.Unlock()
}
```

- [ ] **Step 4: Run webhook tests**

Run: `go test ./internal/webhook`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/webhook.go internal/webhook/webhook_test.go
git commit -m "feat(webhook): add cancellable deployment lifecycle" -m "Part of #48"
```

---

### Task 2: Clean partial containers after cancellation

**Files:**
- Modify: `internal/runtime/docker.go`
- Modify: `internal/runtime/fake.go`
- Modify: `internal/deploy/deploy.go`
- Test: `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: `runtime.Runtime.Run` may return a non-empty `RunResult.ContainerID` together with an error.
- Produces: failed run/health paths stop any known container using a live context bounded by `deploymentCleanupTimeout = 5 * time.Second`.

- [ ] **Step 1: Add cancellation-cleanup observability to FakeRuntime**

Add this field to `runtime.FakeRuntime`:

```go
	StopContextErrs []error
```

Update `Stop`:

```go
func (f *FakeRuntime) Stop(ctx context.Context, id string) error {
	f.Stopped = append(f.Stopped, id)
	f.StopContextErrs = append(f.StopContextErrs, ctx.Err())
	return nil
}
```

- [ ] **Step 2: Add the failing partial-container cleanup test**

Add to `internal/deploy/deploy_test.go`:

```go
func TestDeployCancelledRunCleansPartialContainerWithLiveContext(t *testing.T) {
	s, path := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "partial-c"},
		RunErr:         context.Canceled,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(ctx, "blog", t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Deploy error = %v, want context.Canceled", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "partial-c" {
		t.Fatalf("stopped = %v, want [partial-c]", rt.Stopped)
	}
	if len(rt.StopContextErrs) != 1 || rt.StopContextErrs[0] != nil {
		t.Fatalf("stop context errors = %v, want [nil]", rt.StopContextErrs)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Fatalf("failed deployment count = %d, want 1", got)
	}
}
```

- [ ] **Step 3: Run the test and verify it fails**

Run: `go test ./internal/deploy -run TestDeployCancelledRunCleansPartialContainerWithLiveContext -v`

Expected: FAIL because the run-error path does not stop `partial-c`.

- [ ] **Step 4: Preserve Docker container IDs after creation**

In `internal/runtime/docker.go`, after `ContainerCreate` succeeds, retain:

```go
	result := RunResult{ContainerID: created.ID}
```

For every subsequent error from `ContainerStart`, `ContainerInspect`, missing port binding, or `nat.ParsePort`, return `result, err` rather than `RunResult{}, err`. Set `result.HostPort = hp` and return `result, nil` on success.

- [ ] **Step 5: Add bounded detached cleanup in deploy**

Add near the top of `internal/deploy/deploy.go`:

```go
const deploymentCleanupTimeout = 5 * time.Second
```

Add:

```go
func (d *Deployer) stopPartial(ctx context.Context, containerID string) {
	if containerID == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deploymentCleanupTimeout)
	defer cancel()
	_ = d.runtime.Stop(cleanupCtx, containerID)
}
```

Update the run and health failure branches in `buildRunHealthy`:

```go
	run, err := d.runtime.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort)
		return build, run, fmt.Errorf("run: %w", err)
	}
	if err := d.runtime.WaitHealthy(ctx, run.HostPort); err != nil {
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort)
		return build, run, fmt.Errorf("health: %w", err)
	}
```

- [ ] **Step 6: Run deploy and runtime tests**

Run: `go test ./internal/deploy ./internal/runtime`

Expected: PASS; Docker-dependent runtime tests skip cleanly when Docker is unavailable.

- [ ] **Step 7: Commit**

```bash
git add internal/runtime/docker.go internal/runtime/fake.go internal/deploy/deploy.go internal/deploy/deploy_test.go
git commit -m "fix(deploy): clean partial containers after cancellation" -m "Part of #48"
```

---

### Task 3: Coordinate bounded two-phase piperd shutdown

**Files:**
- Modify: `cmd/piperd/main.go`
- Test: `cmd/piperd/main_test.go`

**Interfaces:**
- Consumes: `webhook.Handler.WaitContext(context.Context) bool`, `webhook.Handler.Cancel()`, `http.Server.Shutdown`, `http.Server.Close`, `caddy.Manager.Stop`, and `store.Store.Close`.
- Produces: `shutdown(...)`, `shutdownWithTimeouts(...)`, and lifecycle methods on `webhookStarter` used only in `package main`.

- [ ] **Step 1: Add shutdown recording fakes and normal-order test**

Add `"reflect"` and `"sync"` to `cmd/piperd/main_test.go`. Add package-local fakes matching the production interfaces:

```go
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type recServer struct{ rec *recorder }

func (s *recServer) Shutdown(context.Context) error { s.rec.add("api-shutdown"); return nil }
func (s *recServer) Close() error                   { s.rec.add("api-close"); return nil }

type recWebhook struct {
	rec     *recorder
	drained bool
}

func (w *recWebhook) stop(context.Context)      { w.rec.add("webhook-stop") }
func (w *recWebhook) close()                    { w.rec.add("webhook-close") }
func (w *recWebhook) wait(context.Context) bool { w.rec.add("webhook-wait"); return w.drained }
func (w *recWebhook) cancel()                   { w.rec.add("webhook-cancel") }

type recManager struct{ rec *recorder }
func (m *recManager) Stop() { m.rec.add("caddy") }

type recStore struct{ rec *recorder }
func (s *recStore) Close() error { s.rec.add("store"); return nil }

func TestShutdownDrainsBeforeInfrastructureTeardown(t *testing.T) {
	rec := &recorder{}
	shutdownWithTimeouts(
		&recServer{rec}, &recWebhook{rec: rec, drained: true},
		&recManager{rec}, &recStore{rec}, time.Second, 2*time.Second,
	)
	got := rec.snapshot()
	if len(got) != 6 {
		t.Fatalf("events = %v, want 6 events", got)
	}
	first := map[string]bool{got[0]: true, got[1]: true}
	if !first["api-shutdown"] || !first["webhook-stop"] {
		t.Fatalf("first events = %v, want API and webhook stop", got[:2])
	}
	wantTail := []string{"webhook-wait", "webhook-cancel", "caddy", "store"}
	if !reflect.DeepEqual(got[2:], wantTail) {
		t.Fatalf("tail = %v, want %v", got[2:], wantTail)
	}
}
```

- [ ] **Step 2: Add timeout cancellation test**

Add:

```go
type blockingWebhook struct {
	rec   *recorder
	waits int
}

func (w *blockingWebhook) stop(context.Context) { w.rec.add("webhook-stop") }
func (w *blockingWebhook) close()               { w.rec.add("webhook-close") }
func (w *blockingWebhook) cancel()              { w.rec.add("webhook-cancel") }

func (w *blockingWebhook) wait(ctx context.Context) bool {
	w.waits++
	w.rec.add("webhook-wait")
	if w.waits == 1 {
		<-ctx.Done()
		return false
	}
	return true
}

func TestShutdownCancelsAtDrainDeadlineAndStillTearsDown(t *testing.T) {
	rec := &recorder{}
	started := time.Now()
	shutdownWithTimeouts(
		&recServer{rec}, &blockingWebhook{rec: rec},
		&recManager{rec}, &recStore{rec}, 20*time.Millisecond, 100*time.Millisecond,
	)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown took %v, want below 500ms", elapsed)
	}
	got := rec.snapshot()
	for _, want := range []string{"api-close", "webhook-close", "webhook-cancel", "caddy", "store"} {
		found := false
		for _, event := range got {
			found = found || event == want
		}
		if !found { t.Errorf("events = %v, missing %q", got, want) }
	}
}

func TestShutdownSkipsAbsentDependencies(t *testing.T) {
	rec := &recorder{}
	shutdownWithTimeouts(&recServer{rec}, nil, nil, &recStore{rec}, time.Second, 2*time.Second)
	got := rec.snapshot()
	want := []string{"api-shutdown", "store"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}
```

- [ ] **Step 3: Run shutdown tests and verify they fail**

Run: `go test ./cmd/piperd -run TestShutdown -v`

Expected: FAIL to compile because the shutdown helpers and interfaces do not exist.

- [ ] **Step 4: Add lifecycle interfaces and timeout constants**

Add before `main` in `cmd/piperd/main.go`:

```go
const (
	drainTimeout    = 15 * time.Second
	shutdownTimeout = 20 * time.Second
)

type apiShutdowner interface {
	Shutdown(context.Context) error
	Close() error
}

type webhookLifecycle interface {
	stop(context.Context)
	close()
	wait(context.Context) bool
	cancel()
}

type listenerStopper interface{ Stop() }
type storeCloser interface{ Close() error }
```

- [ ] **Step 5: Track and control the webhook server**

Add `srv *http.Server` and `handler *webhook.Handler` to `webhookStarter`. In `run`, assign both fields instead of local `wh`/`whSrv` variables.

Add:

```go
func (w *webhookStarter) stop(ctx context.Context) {
	if w == nil { return }
	w.once.Do(func() {})
	if w.handler != nil { w.handler.StopAccepting() }
	if w.srv != nil { _ = w.srv.Shutdown(ctx) }
}

func (w *webhookStarter) close() {
	if w != nil && w.srv != nil { _ = w.srv.Close() }
}

func (w *webhookStarter) wait(ctx context.Context) bool {
	return w == nil || w.handler == nil || w.handler.WaitContext(ctx)
}

func (w *webhookStarter) cancel() {
	if w != nil && w.handler != nil { w.handler.Cancel() }
}
```

The `once.Do` barrier prevents shutdown from reading partially published fields during a concurrent lazy start. `StopAccepting` closes the handler admission gate before any `WaitContext` call, preventing concurrent `WaitGroup.Add`/`Wait` misuse.

- [ ] **Step 6: Implement the two-phase coordinator**

Implement `shutdown` as the production wrapper:

```go
func shutdown(api apiShutdowner, wh webhookLifecycle, mgr listenerStopper, st storeCloser) {
	shutdownWithTimeouts(api, wh, mgr, st, drainTimeout, shutdownTimeout)
}
```

Add:

```go
func shutdownWithTimeouts(api apiShutdowner, wh webhookLifecycle, mgr listenerStopper, st storeCloser, drain, overall time.Duration) {
	overallCtx, cancelOverall := context.WithTimeout(context.Background(), overall)
	defer cancelOverall()
	drainCtx, cancelDrain := context.WithTimeout(overallCtx, drain)
	defer cancelDrain()

	var calls sync.WaitGroup
	if api != nil {
		calls.Add(1)
		go func() { defer calls.Done(); _ = api.Shutdown(drainCtx) }()
	}
	if wh != nil {
		calls.Add(1)
		go func() { defer calls.Done(); wh.stop(drainCtx) }()
	}
	entryDone := make(chan struct{})
	go func() { calls.Wait(); close(entryDone) }()

	entryDrained := false
	select {
	case <-entryDone:
		entryDrained = true
	case <-drainCtx.Done():
	}

	workDrained := entryDrained
	if wh != nil && entryDrained {
		workDrained = wh.wait(drainCtx)
	}
	if !workDrained {
		if api != nil { _ = api.Close() }
		if wh != nil { wh.close() }
	}
	if wh != nil {
		wh.cancel()
		if !workDrained { _ = wh.wait(overallCtx) }
	}
	if !workDrained {
		// API handlers are cancelled by Close but are not tracked separately.
		// Keep shared infrastructure alive for their reserved cleanup window.
		<-overallCtx.Done()
	}
	if mgr != nil { mgr.Stop() }
	if st != nil { _ = st.Close() }
}
```

Do not wait on `entryDone` after the drain deadline. Forced `Close` plus context cancellation is best effort. On that path, deliberately retain Caddy/store until `overallCtx` expires so cancelled API handlers also receive the reserved cleanup window; process exit is the final bound.

- [ ] **Step 7: Wire main to explicit shutdown ownership**

Hoist `var mgr *caddy.Manager` outside the conditional Caddy block and remove `defer mgr.Stop()`. Remove `defer st.Close()`.

After `<-ctx.Done()`:

```go
	log.Println("shutting down")
	var mgrStop listenerStopper
	if mgr != nil { mgrStop = mgr }
	var whLifecycle webhookLifecycle
	if wh != nil { whLifecycle = wh }
	shutdown(srv, whLifecycle, mgrStop, st)
	os.Exit(0)
```

- [ ] **Step 8: Run focused tests**

Run: `go test ./cmd/piperd ./internal/webhook ./internal/deploy ./internal/runtime`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add cmd/piperd/main.go cmd/piperd/main_test.go
git commit -m "feat(agent): add bounded two-phase shutdown" -m "Part of #48"
```

---

### Task 4: Full verification

**Files:** No source changes expected.

**Interfaces:** Verifies the complete repository contract.

- [ ] **Step 1: Check formatting**

Run: `gofmt -l .`

Expected: no output.

- [ ] **Step 2: Run vet**

Run: `go vet ./...`

Expected: exit 0.

- [ ] **Step 3: Run all tests**

Run: `make test`

Expected: exit 0; Docker integration/e2e tests skip cleanly if Docker is unavailable.

- [ ] **Step 4: Cross-compile**

Run: `make cross`

Expected: exit 0 for `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...`.

- [ ] **Step 5: Inspect final scope**

Run: `git status --short && git diff origin/main...HEAD --stat`

Expected: clean working tree; changes limited to the graceful-shutdown design/plan and files named in Tasks 1–3.

## Implementation Notes

- `http.Server.Shutdown` must not be called sequentially for API and webhook: that delays stopping the second listener.
- `WaitContext` is valid only after webhook HTTP shutdown/close prevents future handler calls from adding to the wait group.
- Cancellation is cooperative. The overall deadline bounds waiting even if an SDK call ignores context.
- Cleanup uses `context.WithoutCancel` only to detach from the cancelled deployment; it immediately adds a 5-second timeout.
- The service-unit files remain out of scope for #48.
