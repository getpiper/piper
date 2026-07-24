# App Lifecycle Stop/Delete Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `POST /v1/apps/{name}/stop` and `DELETE /v1/apps/{name}` on the piperd control API, with `piper stop` / `piper delete` CLI parity, per the approved spec `docs/superpowers/specs/2026-07-11-app-lifecycle-stop-delete-design.md` (issue #103).

**Architecture:** Orchestration lives in `internal/deploy` as `Deployer.Stop` / `Deployer.Delete` (mirroring `TeardownPreview`); `internal/store` gains a transactional `DeleteApp`; `internal/api` extends its `Deployerer` interface and adds two thin handlers; `internal/client` + `cmd/piper` add wrappers/commands. New endpoints inherit `RequireToken` and relay-proxy reachability from the existing mux.

**Tech Stack:** Go stdlib (`net/http` mux patterns, `database/sql`), `modernc.org/sqlite` (pure Go — **no cgo, ever**), existing test fakes (`runtime.FakeRuntime`, `fakeCaddy`, `fakeRegistrar`, `fakeDeployer`).

## Global Constraints

- `CGO_ENABLED=0` must hold: run `make verify` (gofmt → vet → test → arm64 cross-build) before claiming done.
- Deployment status strings are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`.
- Branch `ozykhan/app-lifecycle` (already created, spec committed). One commit per task, conventional-commit style, each ending with:

  ```
  Co-Authored-By: Claude {current model} <noreply@anthropic.com>
  ```

- Commits reference the issue: `Part of #103` in the body.
- Semantics locked by the spec: stop is **idempotent** (nothing running → nil); delete tears down **production + previews**; relay hostname recovery (`Register`) and `Deregister` are **best-effort** (any registrar error skips that step, never fails the call); local Caddy `RemoveRoute` failure **is** an error; `store.DeleteApp` runs **last** so failed teardowns stay retryable.

---

### Task 1: `store.DeleteApp`

**Files:**
- Modify: `internal/store/store.go` (add method after `UpdateAppRepo`, ~line 112)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: existing `Store` helpers (`CreateApp`, `CreateDeployment`, `GetApp`, `ListDeployments`), `ErrNotFound`.
- Produces: `func (s *Store) DeleteApp(name string) error` — deletes the app row and all its deployment rows in one transaction; returns `ErrNotFound` when the app doesn't exist. Task 3 calls this.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go` (uses the existing `openTemp` helper):

```go
func TestDeleteAppRemovesAppAndHistory(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "running", "log"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("api", "img2", "c2", 40002, "running", ""); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteApp("blog"); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
	deps, err := s.ListDeployments("blog")
	if err != nil || len(deps) != 0 {
		t.Errorf("deployments after delete = %v (err %v), want none", deps, err)
	}
	// Other apps and their history are untouched.
	if _, err := s.GetApp("api"); err != nil {
		t.Errorf("GetApp(api) after delete: %v", err)
	}
	if deps, _ := s.ListDeployments("api"); len(deps) != 1 {
		t.Errorf("api deployments = %d, want 1", len(deps))
	}
}

func TestDeleteAppUnknownIsNotFound(t *testing.T) {
	s := openTemp(t)
	if err := s.DeleteApp("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteApp(ghost) err = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run TestDeleteApp -v`
Expected: FAIL to compile — `s.DeleteApp undefined`

- [ ] **Step 3: Implement `DeleteApp`**

In `internal/store/store.go`, after `UpdateAppRepo` (line ~112):

```go
// DeleteApp removes the app and its entire deployment history in one
// transaction — the single exception to deployment rows never being deleted
// (they exist as history only while their app does). ErrNotFound when the
// app doesn't exist.
func (s *Store) DeleteApp(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM deployments WHERE app=?`, name); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM apps WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}
```

Also update the comment above `logRetentionPerApp` (line ~147): change

```go
// logRetentionPerApp bounds stored log blobs: only the newest N deployments
// per app keep their logs. Rows themselves are never deleted — they are the
// deployment history.
```

to

```go
// logRetentionPerApp bounds stored log blobs: only the newest N deployments
// per app keep their logs. Rows themselves are never deleted — they are the
// deployment history — except by DeleteApp, which removes the app wholesale.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS (all, including the two new tests)

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): DeleteApp removes an app and its deployment history

Part of #103

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 2: `Deployer.Stop`

**Files:**
- Modify: `internal/deploy/deploy.go` (add methods after `TeardownPreview`, ~line 207)
- Test: `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: `store.LatestRunning/LatestDeployment/UpdateDeploymentStatus/GetDomainConfig`, `runtime.Runtime.Stop`, `RouteSetter.RemoveRoute`, `HostnameRegistrar.Register`. Test fakes already in `deploy_test.go`: `fakeCaddy` (`removed()` returns removed hosts), `fakeRegistrar` (`failing` flag; assigned host is `hash-<app>-alice.public.getpiper.co`), `runtime.FakeRuntime` (`Stopped` records ids).
- Produces:
  - `func (d *Deployer) Stop(ctx context.Context, appName string) error` — Task 4's handler calls this; propagates `store.ErrNotFound` for unknown apps; nil when nothing is running.
  - unexported helpers `primaryHost(appName string) (host string, ok bool)` and `removeCustomDomainRoute(appName string) error` — Task 3 reuses both.

- [ ] **Step 1: Write the failing tests**

Append to `internal/deploy/deploy_test.go`:

```go
func TestStopRetiresRunningAndRemovesRoute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1]", rt.Stopped)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != "blog.piper.localhost" {
		t.Errorf("removed = %v, want [blog.piper.localhost]", routes.removed())
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestRunning after stop err = %v, want ErrNotFound", err)
	}
	// App and history remain: latest deployment is the same row, now stopped.
	dep, err := s.LatestDeployment("blog")
	if err != nil || dep.Status != "stopped" || dep.ContainerID != "c1" {
		t.Errorf("latest = %+v (err %v), want c1 stopped", dep, err)
	}
}

func TestStopNothingRunningIsNoOp(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop no-op err = %v, want nil", err)
	}
	if len(rt.Stopped) != 0 || len(routes.removed()) != 0 {
		t.Errorf("no-op stop touched runtime/routes: %v %v", rt.Stopped, routes.removed())
	}
}

func TestStopUnknownAppIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	d := New(s, &runtime.FakeRuntime{}, newFakeCaddy(), "piper.localhost")
	if err := d.Stop(context.Background(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Stop(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestStopRemovesCustomDomainRoute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDomainStatus("shop.dev", "active", "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := routes.removed()
	want := map[string]bool{"blog.piper.localhost": true, "blog.shop.dev": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Errorf("removed = %v, want both primary and custom-domain hosts", got)
	}
}

func TestStopTerminatedRemovesAssignedHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != "hash-blog-alice.public.getpiper.co" {
		t.Errorf("removed = %v, want the assigned hostname", routes.removed())
	}
	if len(reg.deregs) != 0 {
		t.Errorf("deregs = %v, stop must not deregister (delete-only)", reg.deregs)
	}
}

func TestStopTerminatedRelayDownSkipsRouteBestEffort(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	reg.failing = true // relay unreachable at stop time
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop with relay down err = %v, want nil (best-effort)", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1] despite relay outage", rt.Stopped)
	}
	if len(routes.removed()) != 0 {
		t.Errorf("removed = %v, want none (hostname unknown)", routes.removed())
	}
	dep, err := s.LatestDeployment("blog")
	if err != nil || dep.Status != "stopped" {
		t.Errorf("latest = %+v (err %v), want stopped", dep, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/deploy/ -run TestStop -v`
Expected: FAIL to compile — `d.Stop undefined`

- [ ] **Step 3: Implement `Stop` and helpers**

In `internal/deploy/deploy.go`, after `TeardownPreview`:

```go
// primaryHost resolves the app's routed production host: the relay-assigned
// name in registrar mode (recovered via the idempotent Register), else
// <app>.<baseDom>. ok is false when the relay is unreachable and the
// hostname can't be recovered — callers skip route work best-effort.
func (d *Deployer) primaryHost(appName string) (string, bool) {
	if d.registrar == nil {
		return d.hostFor(appName), true
	}
	host, err := d.registrar.Register(appName)
	return host, err == nil
}

// removeCustomDomainRoute drops <app>.<custom> when a BYO custom domain is
// active; without one it's a no-op (mirrors the upsert in Deploy).
func (d *Deployer) removeCustomDomainRoute(appName string) error {
	dc, err := d.store.GetDomainConfig()
	if err != nil || dc.Status != "active" {
		return nil
	}
	if err := d.routes.RemoveRoute(appName + "." + dc.Domain); err != nil {
		return fmt.Errorf("unroute custom domain: %w", err)
	}
	return nil
}

// Stop retires the app's running production container: stop it, drop its
// routes, mark the deployment "stopped". The app and its history remain.
// Nothing running is a no-op; previews are untouched. The relay keeps the
// app's hostname registration (Deregister is delete-only).
func (d *Deployer) Stop(ctx context.Context, appName string) error {
	if _, err := d.store.GetApp(appName); err != nil {
		return err
	}
	dep, err := d.store.LatestRunning(appName)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = d.runtime.Stop(ctx, dep.ContainerID)
	if host, ok := d.primaryHost(appName); ok {
		if err := d.routes.RemoveRoute(host); err != nil {
			return fmt.Errorf("unroute: %w", err)
		}
	}
	if err := d.removeCustomDomainRoute(appName); err != nil {
		return err
	}
	return d.store.UpdateDeploymentStatus(dep.ID, "stopped")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/deploy/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add internal/deploy/deploy.go internal/deploy/deploy_test.go
git commit -m "feat(deploy): Stop retires the running production container

Part of #103

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 3: `Deployer.Delete`

**Files:**
- Modify: `internal/deploy/deploy.go` (add method after `Stop`)
- Test: `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: Task 1's `store.DeleteApp`; Task 2's `primaryHost` / `removeCustomDomainRoute`; `store.ListDeployments`; `HostnameRegistrar.Deregister`; `hostForPreview`.
- Produces: `func (d *Deployer) Delete(ctx context.Context, appName string) error` — Task 4's handler calls this; propagates `store.ErrNotFound` for unknown apps.

- [ ] **Step 1: Write the failing tests**

First make `fakeCaddy` able to fail: in `internal/deploy/deploy_test.go` add a `removeErr error` field to the `fakeCaddy` struct (line ~17) and change `RemoveRoute` to:

```go
func (f *fakeCaddy) RemoveRoute(host string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removes = append(f.removes, host)
	return nil
}
```

(The zero value keeps every existing test's behavior.) Then append the tests:

```go
func TestDeleteTearsDownProductionAndPreviews(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "main-c", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rt.RunResultVal = runtime.RunResult{ContainerID: "preview-c", HostPort: 40002}
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("DeployPreview: %v", err)
	}

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	stopped := map[string]bool{}
	for _, id := range rt.Stopped {
		stopped[id] = true
	}
	if !stopped["main-c"] || !stopped["preview-c"] {
		t.Errorf("stopped = %v, want main-c and preview-c", rt.Stopped)
	}
	removed := map[string]bool{}
	for _, h := range routes.removed() {
		removed[h] = true
	}
	if !removed["blog.piper.localhost"] || !removed["pr-5-blog.piper.localhost"] {
		t.Errorf("removed = %v, want production and preview hosts", routes.removed())
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
	if deps, _ := s.ListDeployments("blog"); len(deps) != 0 {
		t.Errorf("deployments after delete = %v, want none", deps)
	}
}

func TestDeleteUnknownAppIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	d := New(s, &runtime.FakeRuntime{}, newFakeCaddy(), "piper.localhost")
	if err := d.Delete(context.Background(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Delete(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestDeleteNothingRunningStillDeletesState(t *testing.T) {
	s, _ := newStore(t) // "blog" exists, never deployed
	rt := &runtime.FakeRuntime{}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(rt.Stopped) != 0 {
		t.Errorf("stopped = %v, want none", rt.Stopped)
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
}

func TestDeleteTerminatedDeregistersHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	want := "hash-blog-alice.public.getpiper.co"
	if len(reg.deregs) != 1 || reg.deregs[0] != want {
		t.Errorf("deregs = %v, want [%s]", reg.deregs, want)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != want {
		t.Errorf("removed = %v, want [%s]", routes.removed(), want)
	}
}

func TestDeleteTerminatedRelayDownStillDeletesState(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	reg.failing = true // relay unreachable at delete time
	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete with relay down err = %v, want nil (best-effort)", err)
	}
	if len(reg.deregs) != 0 {
		t.Errorf("deregs = %v, want none (hostname unknown)", reg.deregs)
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
}

func TestDeleteRemovesCustomDomainRoute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDomainStatus("shop.dev", "active", "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	found := false
	for _, h := range routes.removed() {
		found = found || h == "blog.shop.dev"
	}
	if !found {
		t.Errorf("removed = %v, want blog.shop.dev among them", routes.removed())
	}
}

func TestDeleteRouteRemovalFailureLeavesStateIntact(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	routes.removeErr = errors.New("caddy down")
	if err := d.Delete(context.Background(), "blog"); err == nil {
		t.Fatal("Delete with failing unroute must error")
	}
	if _, err := s.GetApp("blog"); err != nil {
		t.Errorf("GetApp = %v, want app still present (delete stays retryable)", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/deploy/ -run TestDelete -v`
Expected: FAIL to compile — `d.Delete undefined`

- [ ] **Step 3: Implement `Delete`**

In `internal/deploy/deploy.go`, after `Stop`:

```go
// Delete tears the app down completely: stops every running deployment
// (production and previews), drops all its routes, releases the relay
// hostname, and deletes the app plus its whole deployment history. Relay
// steps are best-effort; state is deleted last so a failed teardown leaves
// delete retryable.
func (d *Deployer) Delete(ctx context.Context, appName string) error {
	if _, err := d.store.GetApp(appName); err != nil {
		return err
	}
	deps, err := d.store.ListDeployments(appName)
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.Status != "running" {
			continue
		}
		_ = d.runtime.Stop(ctx, dep.ContainerID)
		if dep.PR > 0 {
			if err := d.routes.RemoveRoute(d.hostForPreview(appName, dep.PR)); err != nil {
				return fmt.Errorf("unroute: %w", err)
			}
		}
	}
	if host, ok := d.primaryHost(appName); ok {
		if err := d.routes.RemoveRoute(host); err != nil {
			return fmt.Errorf("unroute: %w", err)
		}
		if d.registrar != nil {
			_ = d.registrar.Deregister(host)
		}
	}
	if err := d.removeCustomDomainRoute(appName); err != nil {
		return err
	}
	return d.store.DeleteApp(appName)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/deploy/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add internal/deploy/deploy.go internal/deploy/deploy_test.go
git commit -m "feat(deploy): Delete tears down containers, routes, hostname, and state

Part of #103

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 4: API endpoints

**Files:**
- Modify: `internal/api/api.go` (extend `Deployerer` at line ~20; add handlers after the `POST /v1/apps/{name}/link` handler, ~line 195)
- Test: `internal/api/api_test.go` (extend `fakeDeployer` at line ~20; add tests)

**Interfaces:**
- Consumes: Tasks 2–3's `Deployer.Stop` / `Deployer.Delete` signatures (`func(ctx context.Context, app string) error`, `store.ErrNotFound` for unknown apps). `*deploy.Deployer` already flows into `api.New` from piperd, so extending the interface needs no wiring change.
- Produces: `POST /v1/apps/{name}/stop` and `DELETE /v1/apps/{name}` — 204 / 404 / 500. Task 5's client calls these.

- [ ] **Step 1: Extend the test fake and write the failing tests**

In `internal/api/api_test.go`, change `fakeDeployer` (line ~20) to:

```go
type fakeDeployer struct {
	gotApp    string
	gotFile   string
	stopped   []string
	deleted   []string
	stopErr   error
	deleteErr error
}
```

and add below its `Deploy` method:

```go
func (f *fakeDeployer) Stop(_ context.Context, app string) error {
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, app)
	return nil
}

func (f *fakeDeployer) Delete(_ context.Context, app string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, app)
	return nil
}
```

Then append the tests:

```go
func TestStopEndpoint(t *testing.T) {
	deployer := &fakeDeployer{}
	h := New(newTestStore(t), deployer, "piper.localhost", "", nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/stop", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(deployer.stopped) != 1 || deployer.stopped[0] != "blog" {
		t.Fatalf("stopped = %v, want [blog]", deployer.stopped)
	}
}

func TestStopEndpointUnknownApp(t *testing.T) {
	h := New(newTestStore(t), &fakeDeployer{stopErr: store.ErrNotFound}, "piper.localhost", "", nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/ghost/stop", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteAppEndpoint(t *testing.T) {
	deployer := &fakeDeployer{}
	h := New(newTestStore(t), deployer, "piper.localhost", "", nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/blog", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(deployer.deleted) != 1 || deployer.deleted[0] != "blog" {
		t.Fatalf("deleted = %v, want [blog]", deployer.deleted)
	}
}

func TestDeleteAppEndpointUnknownApp(t *testing.T) {
	h := New(newTestStore(t), &fakeDeployer{deleteErr: store.ErrNotFound}, "piper.localhost", "", nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/ghost", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run 'TestStopEndpoint|TestDeleteAppEndpoint' -v`
Expected: FAIL — the stop tests get 404 "page not found" (no pattern), the delete tests get 405 (the `GET /v1/apps/{name}` pattern exists but DELETE isn't registered), so none of the assertions hold. (Compilation succeeds: the fake gained the methods but `Deployerer` doesn't require them yet.)

- [ ] **Step 3: Extend the interface and add the handlers**

In `internal/api/api.go`, change `Deployerer` (line ~20) to:

```go
type Deployerer interface {
	Deploy(ctx context.Context, app, srcDir string) (store.Deployment, error)
	Stop(ctx context.Context, app string) error
	Delete(ctx context.Context, app string) error
}
```

Add the handlers inside `New`, after the `POST /v1/apps/{name}/link` handler:

```go
	mux.HandleFunc("POST /v1/apps/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Stop(r.Context(), r.PathValue("name")); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Delete(r.Context(), r.PathValue("name")); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
```

- [ ] **Step 4: Run tests and the full build to verify**

Run: `go test ./internal/api/ -v && go build ./...`
Expected: PASS; whole module compiles (`*deploy.Deployer` satisfies the extended interface — no piperd change needed).

- [ ] **Step 5: Commit**

```bash
git add internal/api/api.go internal/api/api_test.go
git commit -m "feat(agent): POST /v1/apps/{name}/stop and DELETE /v1/apps/{name}

Part of #103

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 5: Client wrappers

**Files:**
- Modify: `internal/client/client.go` (add after `LinkApp`, ~line 136)
- Test: `internal/client/client_test.go`

**Interfaces:**
- Consumes: Task 4's endpoints; existing `do()` / `responseError` helpers.
- Produces: `func (c *Client) StopApp(name string) error` and `func (c *Client) DeleteApp(name string) error` — Task 6's CLI and Task 7's e2e call these.

- [ ] **Step 1: Write the failing tests**

Append to `internal/client/client_test.go`:

```go
func TestStopApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/stop" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := New(srv.URL, "").StopApp("blog"); err != nil {
		t.Fatalf("StopApp: %v", err)
	}
}

func TestStopAppErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown app", http.StatusNotFound)
	}))
	defer srv.Close()
	err := New(srv.URL, "").StopApp("ghost")
	if err == nil || !strings.Contains(err.Error(), "unknown app") {
		t.Fatalf("err = %v, want body in message", err)
	}
}

func TestDeleteApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/apps/blog" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := New(srv.URL, "").DeleteApp("blog"); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/client/ -run 'TestStopApp|TestDeleteApp' -v`
Expected: FAIL to compile — `StopApp`/`DeleteApp` undefined

- [ ] **Step 3: Implement the wrappers**

In `internal/client/client.go`, after `LinkApp`:

```go
func (c *Client) StopApp(name string) error {
	resp, err := c.do(http.MethodPost, "/v1/apps/"+name+"/stop", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("stop", resp)
	}
	return nil
}

func (c *Client) DeleteApp(name string) error {
	resp, err := c.do(http.MethodDelete, "/v1/apps/"+name, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("delete", resp)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/client/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat(cli): client StopApp/DeleteApp wrappers

Part of #103

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 6: CLI `piper stop` / `piper delete`

**Files:**
- Modify: `cmd/piper/main.go` (new `case "stop"` / `case "delete"` in the `run` switch after `case "status"` ~line 238; `confirm` helper; `usage()` line ~396)
- Test: `cmd/piper/main_test.go`

**Interfaces:**
- Consumes: Task 5's `client.StopApp` / `client.DeleteApp`; existing `dialClient(remote, stderr)`; the `t.Setenv("PIPER_ADDR", srv.URL)` test pattern.
- Produces: `piper stop <name>`, `piper delete <name> [--yes]`; package-level `var stdinReader io.Reader = os.Stdin` (test seam, matching the existing `var openBrowserFn = openBrowser` pattern).

- [ ] **Step 1: Write the failing tests**

Append to `cmd/piper/main_test.go`:

```go
func TestRunStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/stop" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"stop", "blog"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stopped blog") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunDeleteWithYesSkipsPrompt(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/apps/blog" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"delete", "blog", "--yes"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !called {
		t.Fatal("DELETE was not sent")
	}
	if !strings.Contains(stdout.String(), "deleted blog") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunDeletePromptConfirms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	old := stdinReader
	stdinReader = strings.NewReader("y\n")
	t.Cleanup(func() { stdinReader = old })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"delete", "blog"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `delete app "blog"`) {
		t.Errorf("stdout = %q, want the confirmation prompt", stdout.String())
	}
	if !strings.Contains(stdout.String(), "deleted blog") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunDeletePromptAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("declined delete must not reach the API")
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	old := stdinReader
	stdinReader = strings.NewReader("n\n")
	t.Cleanup(func() { stdinReader = old })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"delete", "blog"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "aborted") {
		t.Errorf("stdout = %q, want aborted", stdout.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/piper/ -run 'TestRunStop|TestRunDelete' -v`
Expected: FAIL to compile — `stdinReader` undefined (and without it, the commands would just print usage)

- [ ] **Step 3: Implement the commands**

In `cmd/piper/main.go`:

Add `"bufio"` to the imports.

Below `var openBrowserFn = openBrowser` (line ~22):

```go
// stdinReader feeds the delete confirmation prompt; a var so tests can
// substitute input.
var stdinReader io.Reader = os.Stdin
```

In the `run` switch, after `case "status":`'s block (before `case "app":`):

```go
	case "stop":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: piper stop <name>")
			return 2
		}
		c, ok := dialClient(*remote, stderr)
		if !ok {
			return 1
		}
		if err := c.StopApp(args[1]); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "stopped %s\n", args[1])
		return 0
	case "delete":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: piper delete <name> [--yes]")
			return 2
		}
		name := args[1]
		fs := flag.NewFlagSet("delete", flag.ContinueOnError)
		fs.SetOutput(stderr)
		yes := fs.Bool("yes", false, "skip the confirmation prompt")
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper delete <name> [--yes]")
			return 2
		}
		if !*yes && !confirmDelete(stdout, name) {
			fmt.Fprintln(stdout, "aborted")
			return 0
		}
		c, ok := dialClient(*remote, stderr)
		if !ok {
			return 1
		}
		if err := c.DeleteApp(name); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "deleted %s\n", name)
		return 0
```

Near `usage()` at the bottom, add the helper:

```go
// confirmDelete guards the destructive delete; only "y"/"yes" proceeds.
func confirmDelete(stdout io.Writer, name string) bool {
	fmt.Fprintf(stdout, "delete app %q and all its history? [y/N] ", name)
	sc := bufio.NewScanner(stdinReader)
	if !sc.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(sc.Text()))
	return answer == "y" || answer == "yes"
}
```

Update `usage()`:

```go
func usage(w io.Writer) int {
	fmt.Fprintln(w, "usage: piper [--remote <base-domain>] <version|login|connect|create|deploy|list|status|stop|delete|app|github> [args]")
	return 2
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/piper/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/main.go cmd/piper/main_test.go
git commit -m "feat(cli): piper stop and piper delete commands

Part of #103

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 7: e2e — deploy → stop → delete

**Files:**
- Modify: `test/e2e/deploy_test.go` (extend `TestEndToEndDeploy` after the final fetch, ~line 92)

**Interfaces:**
- Consumes: Task 5's `client.StopApp` / `client.DeleteApp`; the test's existing `c` client, `req` request, and `body` (the served response).

- [ ] **Step 1: Extend the e2e test**

In `test/e2e/deploy_test.go`, after `fmt.Printf("e2e response: %q\n", body)` at the end of `TestEndToEndDeploy`, append:

```go
	// Stop: the hostname must stop serving the app; the app stays listed as
	// "stopped" with its history intact.
	if err := c.StopApp("blog"); err != nil {
		t.Fatalf("StopApp: %v", err)
	}
	afterStop, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch after stop: %v", err)
	}
	stopped, _ := io.ReadAll(afterStop.Body)
	afterStop.Body.Close()
	if string(stopped) == body {
		t.Fatalf("hostname still serves the app after stop: %q", stopped)
	}
	apps, err := c.ListApps()
	if err != nil {
		t.Fatalf("ListApps after stop: %v", err)
	}
	if len(apps) != 1 || apps[0].Status != "stopped" {
		t.Fatalf("apps after stop = %+v, want blog stopped", apps)
	}

	// Delete: app and state gone.
	if err := c.DeleteApp("blog"); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	apps, err = c.ListApps()
	if err != nil {
		t.Fatalf("ListApps after delete: %v", err)
	}
	if len(apps) != 0 {
		t.Fatalf("apps after delete = %+v, want none", apps)
	}
```

(`req` is reusable: `http.NewRequest` with a nil body builds a bodyless GET.)

- [ ] **Step 2: Compile-check; run for real when Docker is available**

Run: `go vet ./test/e2e/`
Expected: clean.

If Docker is running and ports 80/8088/2019 are free, also run: `RUN_E2E=1 go test ./test/e2e/ -run TestEndToEndDeploy -v -timeout 10m`
Expected: PASS. (Skips cleanly otherwise — don't block the task on Docker.)

- [ ] **Step 3: Commit**

```bash
git add test/e2e/deploy_test.go
git commit -m "test(e2e): stop and delete after deploy — hostname stops serving, state gone

Part of #103

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 8: verify, PROGRESS.md, PR

**Files:**
- Modify: `PROGRESS.md` (line ~26, next to the deployment-history entry)

- [ ] **Step 1: Full gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests pass, arm64 cross-build succeeds. Fix anything that fails before proceeding.

- [ ] **Step 2: Update PROGRESS.md**

After the line for #101 (`- ✅ Deployment history + build/deploy logs on the control API — [#101](...)`), add:

```markdown
- ✅ App lifecycle: stop + delete on the control API and CLI — [#103](https://github.com/piperbox/piper/issues/103)
```

- [ ] **Step 3: Commit and push**

```bash
git add PROGRESS.md
git commit -m "docs: mark app stop/delete lifecycle done in PROGRESS

Part of #103

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
git push -u origin ozykhan/app-lifecycle
```

- [ ] **Step 4: Open the PR**

```bash
gh pr create --base main --title "[agent] App lifecycle endpoints: delete and stop" --body "$(cat <<'EOF'
Adds `POST /v1/apps/{name}/stop` and `DELETE /v1/apps/{name}` to the piperd control API, with `piper stop` / `piper delete [--yes]` CLI parity.

- Stop retires the running production container, removes its routes (relay-assigned hostname, `<app>.<baseDom>`, and active custom-domain host), and marks the deployment `stopped` — app and history intact. Idempotent.
- Delete stops every running deployment (production + previews), removes all routes, deregisters the relay hostname (best-effort when the tunnel is down), and deletes the app + its deployment history. State is deleted last so a failed teardown is retryable.
- Both sit behind the existing bearer-token gate and are reachable through the relay proxy.

Design: `docs/superpowers/specs/2026-07-11-app-lifecycle-stop-delete-design.md`

Closes #103

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Then squash-merge per repo convention (after review/CI).
