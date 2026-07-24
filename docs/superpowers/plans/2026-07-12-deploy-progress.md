# Deploy Progress Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `piper deploy` show live progress instead of a silent multi-minute block, by having piperd create a `building` deployment row up front whose stored log grows during the build, and having the CLI follow it by polling the same endpoints the dashboard uses.

**Architecture:** The production deploy path splits into `Begin` (create the `building` row) + `Finish` (build → run → health → route → finalize). `Finish` tees the Docker build's live output into the row's `logs` column on a ~1s debounce. The deploy POST goes async: it returns the `building` deployment immediately (202) and runs `Finish` in a background goroutine. `piper deploy` polls `deployments` + `deployments/{id}/logs`, printing new log bytes until a terminal status. Previews are untouched.

**Tech Stack:** Go (no cgo), `modernc.org/sqlite`, Docker SDK (`jsonmessage`), standard `net/http`.

## Global Constraints

- **No cgo.** All builds must pass with `CGO_ENABLED=0`; pure-Go SQLite (`modernc.org/sqlite`) only.
- **Module path** `github.com/piperbox/piper`.
- **Deployment status strings** are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`.
- **Layering:** `store` knows only persistence; `runtime` only Docker; `deploy` orchestrates through interfaces; `api` is transport over `deploy`+`store`; `client` is the CLI's view of `api`. Nothing imports "up".
- **TDD:** every task is failing-test-first, then minimal implementation.
- **Commits:** conventional-commit style, ending with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`. Reference `Part of #140`.
- **Before claiming done / pushing:** run `make verify` (gofmt → vet → test → cross).

---

## File Structure

- `internal/store/store.go` — add `UpdateDeploymentLogs`, `FinalizeDeployment`.
- `internal/runtime/runtime.go`, `docker.go`, `fake.go` — add `progress io.Writer` to `Build`.
- `internal/deploy/deploy.go` — `logSink`, split `Deploy` into `Begin`/`Finish`, thread `progress` through `buildRunHealthy`.
- `internal/api/api.go` — async deploy handler, `Deployerer` interface gains `Begin`/`Finish`, drop `DeployResult`.
- `internal/client/client.go` — `Deploy` returns the building deployment; add `Deployments`, `DeploymentLogs`, `App`, `FollowDeploy`.
- `cmd/piper/main.go` — `deploy` command follows the deployment and prints the delta.
- `PROGRESS.md` — flip the deploy-progress line to built.

---

## Task 1: Store — building-row lifecycle methods

**Files:**
- Modify: `internal/store/store.go` (after `UpdateDeploymentStatus`, ~line 211)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces:
  - `func (s *Store) UpdateDeploymentLogs(id, logs string) error`
  - `func (s *Store) FinalizeDeployment(id, imageID, containerID string, hostPort int, status, logs string) error`
- Consumes: existing `CreateDeployment(app, imageID, containerID string, hostPort int, status, logs string) (Deployment, error)` — called with `("app","","",0,"building","")` to open a row.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestBuildingRowLifecycle(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	dep, err := s.CreateDeployment("web", "", "", 0, "building", "")
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if dep.Status != "building" {
		t.Fatalf("status = %q, want building", dep.Status)
	}

	if err := s.UpdateDeploymentLogs(dep.ID, "pulling base image...\n"); err != nil {
		t.Fatalf("UpdateDeploymentLogs: %v", err)
	}
	logs, err := s.DeploymentLogs("web", dep.ID)
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	if logs != "pulling base image...\n" {
		t.Fatalf("logs = %q", logs)
	}

	if err := s.FinalizeDeployment(dep.ID, "img-1", "cid-1", 40001, "running", "done\n"); err != nil {
		t.Fatalf("FinalizeDeployment: %v", err)
	}
	got, err := s.LatestDeployment("web")
	if err != nil {
		t.Fatalf("LatestDeployment: %v", err)
	}
	if got.Status != "running" || got.ImageID != "img-1" || got.ContainerID != "cid-1" || got.HostPort != 40001 {
		t.Fatalf("finalized row = %+v", got)
	}
	logs, _ = s.DeploymentLogs("web", dep.ID)
	if logs != "done\n" {
		t.Fatalf("finalized logs = %q", logs)
	}
}
```

> Check the top of `internal/store/store_test.go` for the existing store-opening helper (it wraps `store.Open(filepath.Join(t.TempDir(), ...))`). Use that helper's real name in place of `openTestStore` if it differs.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestBuildingRowLifecycle -v`
Expected: FAIL — `s.UpdateDeploymentLogs undefined` / `s.FinalizeDeployment undefined`.

- [ ] **Step 3: Write minimal implementation**

In `internal/store/store.go`, after `UpdateDeploymentStatus` (~line 211):

```go
// UpdateDeploymentLogs overwrites one deployment's captured log. Used to grow
// a building row's log as the build streams.
func (s *Store) UpdateDeploymentLogs(id, logs string) error {
	_, err := s.db.Exec(`UPDATE deployments SET logs=? WHERE id=?`, logs, id)
	return err
}

// FinalizeDeployment fills in a building row's real image/container/port and
// flips its status to running/failed, writing the complete log in one update.
func (s *Store) FinalizeDeployment(id, imageID, containerID string, hostPort int, status, logs string) error {
	_, err := s.db.Exec(
		`UPDATE deployments SET image_id=?, container_id=?, host_port=?, status=?, logs=? WHERE id=?`,
		imageID, containerID, hostPort, status, logs, id)
	return err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestBuildingRowLifecycle -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): building-row log/finalize methods (#140)

Part of #140

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Runtime — live build-output writer

**Files:**
- Modify: `internal/runtime/runtime.go` (interface, line 24), `internal/runtime/docker.go:30-57`, `internal/runtime/fake.go:22-24`
- Test: `internal/runtime/fake_test.go` (create) and `internal/runtime/docker_test.go` (existing Docker-gated tests)

**Interfaces:**
- Produces: `Build(ctx context.Context, srcDir, imageTag string, progress io.Writer) (BuildResult, error)` — when `progress != nil`, the build's plain-text output is written to it live (in addition to `BuildResult.Log`). `progress == nil` preserves prior behavior.
- Consumes: nothing new.

- [ ] **Step 1: Write the failing test**

Create `internal/runtime/fake_test.go`:

```go
package runtime

import (
	"bytes"
	"context"
	"testing"
)

func TestFakeBuildWritesProgress(t *testing.T) {
	f := &FakeRuntime{
		BuildResultVal: BuildResult{ImageID: "img-1"},
		BuildOutput:    "Step 1/2 : FROM alpine\nStep 2/2 : CMD sh\n",
	}
	var progress bytes.Buffer
	got, err := f.Build(context.Background(), "/src", "tag", &progress)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.ImageID != "img-1" {
		t.Fatalf("ImageID = %q", got.ImageID)
	}
	if progress.String() != "Step 1/2 : FROM alpine\nStep 2/2 : CMD sh\n" {
		t.Fatalf("progress = %q", progress.String())
	}
}

func TestFakeBuildNilProgressIsSafe(t *testing.T) {
	f := &FakeRuntime{BuildResultVal: BuildResult{ImageID: "img-1"}, BuildOutput: "x"}
	if _, err := f.Build(context.Background(), "/src", "tag", nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestFakeBuild -v`
Expected: FAIL — `too many arguments in call to f.Build` and `unknown field BuildOutput`.

- [ ] **Step 3: Write minimal implementation**

In `internal/runtime/runtime.go`, change the interface method (line 24):

```go
	Build(ctx context.Context, srcDir, imageTag string, progress io.Writer) (BuildResult, error)
```

In `internal/runtime/docker.go`, change `Build` (lines 30, 49):

```go
func (d *DockerRuntime) Build(ctx context.Context, srcDir, imageTag string, progress io.Writer) (BuildResult, error) {
```

and replace the decode line (49):

```go
	var log TailBuffer
	out := io.Writer(&log)
	if progress != nil {
		out = io.MultiWriter(&log, progress)
	}
	if err := jsonmessage.DisplayJSONMessagesStream(resp.Body, out, 0, false, nil); err != nil {
		return BuildResult{Log: log.String()}, err
	}
```

Add `"io"` to `docker.go`'s imports if not already present.

In `internal/runtime/fake.go`, add the field and rewrite `Build` (lines 11-24):

```go
type FakeRuntime struct {
	BuildResultVal  BuildResult
	BuildOutput     string // written to progress on Build, simulating live output
	BuildErr        error
	RunResultVal    RunResult
	RunErr          error
	HealthErr       error
	LogsVal         string
	LogsErr         error
	Stopped         []string
	StopContextErrs []error
}

func (f *FakeRuntime) Build(_ context.Context, _, _ string, progress io.Writer) (BuildResult, error) {
	if progress != nil && f.BuildOutput != "" {
		_, _ = io.WriteString(progress, f.BuildOutput)
	}
	return f.BuildResultVal, f.BuildErr
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/runtime/ -run TestFakeBuild -v`
Expected: PASS.

- [ ] **Step 5: Update the Docker-gated build test to pass a progress buffer**

In `internal/runtime/docker_test.go`, find the existing test(s) that call `rt.Build(ctx, dir, tag)` and add a progress buffer argument, asserting it received output. Example edit — locate the real-build test and change its call:

```go
	var progress bytes.Buffer
	res, err := rt.Build(ctx, dir, tag, &progress)
	// ...existing assertions on res / err...
	if progress.Len() == 0 {
		t.Fatalf("expected live build output on progress writer")
	}
```

Add `"bytes"` to the test imports if needed. Any other `Build(` call sites in this file that don't care about progress: pass `nil`.

Run: `go test ./internal/runtime/ -v` (Docker tests skip cleanly without Docker).
Expected: PASS or SKIP (no compile errors).

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/
git commit -m "feat(runtime): stream live build output to a progress writer (#140)

Part of #140

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Deploy — Begin/Finish split with a store-backed log sink

**Files:**
- Modify: `internal/deploy/deploy.go` (`buildRunHealthy` 71-97, `Deploy` 118-166; add `logSink`, `Begin`, `Finish`)
- Test: `internal/deploy/deploy_test.go`

**Interfaces:**
- Produces:
  - `func (d *Deployer) Begin(appName string) (store.Deployment, error)` — GetApp guard, then create a `building` row; returns it.
  - `func (d *Deployer) Finish(ctx context.Context, dep store.Deployment, srcDir string) error` — build→run→health→route→finalize; finalizes the row `failed` on build/run/health error.
  - `func (d *Deployer) Deploy(ctx context.Context, appName, srcDir string) (store.Deployment, error)` — unchanged signature; now `Begin`+`Finish`, returning the finalized deployment.
- Consumes: `store.CreateDeployment`, `store.UpdateDeploymentLogs`, `store.FinalizeDeployment`, `store.LatestDeployment`, `runtime.Build(...progress)`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/deploy/deploy_test.go`. These use the existing `newStore(t)` helper (creates app `blog`), `newFakeCaddy()`, and `runtime.FakeRuntime`:

```go
func TestBeginCreatesBuildingRow(t *testing.T) {
	s, _ := newStore(t)
	d := New(s, &runtime.FakeRuntime{}, newFakeCaddy(), "piper.localhost")
	dep, err := d.Begin("blog")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if dep.Status != "building" {
		t.Fatalf("status = %q, want building", dep.Status)
	}
	// LatestRunning must ignore a building row.
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LatestRunning on building row = %v, want ErrNotFound", err)
	}
}

func TestFinishSucceedsAndPersistsLog(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img-1", Log: "built ok\n"},
		BuildOutput:    "pulling...\nbuilt ok\n",
		RunResultVal:   runtime.RunResult{ContainerID: "cid-1", HostPort: 40001},
	}
	caddy := newFakeCaddy()
	d := New(s, rt, caddy, "piper.localhost")

	dep, err := d.Begin("blog")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := d.Finish(context.Background(), dep, t.TempDir()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, err := s.LatestDeployment("blog")
	if err != nil {
		t.Fatalf("LatestDeployment: %v", err)
	}
	if got.Status != "running" || got.ContainerID != "cid-1" || got.HostPort != 40001 {
		t.Fatalf("finalized = %+v", got)
	}
	if caddy.upserts["blog.piper.localhost"] != 40001 {
		t.Fatalf("route not set: %+v", caddy.upserts)
	}
	logs, _ := s.DeploymentLogs("blog", dep.ID)
	if !strings.Contains(logs, "built ok") {
		t.Fatalf("logs missing build output: %q", logs)
	}
}

func TestFinishBuildFailureFinalizesSameRowFailed(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{Log: "boom\n"},
		BuildErr:       errors.New("build blew up"),
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	dep, err := d.Begin("blog")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := d.Finish(context.Background(), dep, t.TempDir()); err == nil {
		t.Fatal("Finish: expected error")
	}
	// Same row, now failed — no second row created.
	all, _ := s.ListDeployments("blog")
	if len(all) != 1 {
		t.Fatalf("want 1 deployment row, got %d", len(all))
	}
	if all[0].ID != dep.ID || all[0].Status != "failed" {
		t.Fatalf("row = %+v", all[0])
	}
	logs, _ := s.DeploymentLogs("blog", dep.ID)
	if !strings.Contains(logs, "boom") {
		t.Fatalf("failed log missing build output: %q", logs)
	}
}

func TestDeployWrapperEqualsBeginFinish(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img-1"},
		RunResultVal:   runtime.RunResult{ContainerID: "cid-1", HostPort: 40002},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	dep, err := d.Deploy(context.Background(), "blog", t.TempDir())
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.Status != "running" || dep.HostPort != 40002 {
		t.Fatalf("deploy result = %+v", dep)
	}
}

func TestLogSinkFlushesOnInterval(t *testing.T) {
	s, _ := newStore(t)
	dep, err := s.CreateDeployment("blog", "", "", 0, "building", "")
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	sink := &logSink{store: s, id: dep.ID}
	// First write: lastFlush is zero → flushes immediately.
	_, _ = sink.Write([]byte("chunk-a\n"))
	if logs, _ := s.DeploymentLogs("blog", dep.ID); logs != "chunk-a\n" {
		t.Fatalf("after first write logs = %q", logs)
	}
	// Second write within the interval: buffered, not yet flushed.
	sink.lastFlush = time.Now()
	_, _ = sink.Write([]byte("chunk-b\n"))
	if logs, _ := s.DeploymentLogs("blog", dep.ID); logs != "chunk-a\n" {
		t.Fatalf("debounced logs = %q, want unchanged", logs)
	}
	// Interval elapsed: next write flushes the whole buffer.
	sink.lastFlush = time.Now().Add(-2 * logFlushInterval)
	_, _ = sink.Write([]byte("chunk-c\n"))
	if logs, _ := s.DeploymentLogs("blog", dep.ID); logs != "chunk-a\nchunk-b\nchunk-c\n" {
		t.Fatalf("after interval logs = %q", logs)
	}
}
```

Ensure the test file imports `context`, `errors`, `strings`, `time`, `internal/runtime`, `internal/store` (most are already imported — see the file header at lines 1-14).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/deploy/ -run 'TestBegin|TestFinish|TestDeployWrapper|TestLogSink' -v`
Expected: FAIL — `d.Begin undefined`, `d.Finish undefined`, `logSink` / `logFlushInterval` undefined, `BuildOutput` unknown (only if Task 2 not yet merged — Task 2 must land first).

- [ ] **Step 3: Add the log sink**

At the top of `internal/deploy/deploy.go` (after the `deploymentCleanupTimeout` const, ~line 15), add:

```go
// logFlushInterval bounds how often a running build's growing log is persisted
// to its deployment row, so a slow build's output reaches pollers (the CLI and
// dashboard) without a store write per line.
const logFlushInterval = time.Second

// logSink is the progress io.Writer handed to the build: it accumulates output
// in a tail-capped buffer and flushes the whole buffer to the deployment's log
// column at most once per logFlushInterval. Written from a single goroutine
// (the deploy), so it needs no locking. The authoritative final log is written
// separately by FinalizeDeployment.
type logSink struct {
	buf       runtime.TailBuffer
	store     *store.Store
	id        string
	lastFlush time.Time
}

func (ls *logSink) Write(p []byte) (int, error) {
	n, err := ls.buf.Write(p)
	if time.Since(ls.lastFlush) >= logFlushInterval {
		_ = ls.store.UpdateDeploymentLogs(ls.id, ls.buf.String())
		ls.lastFlush = time.Now()
	}
	return n, err
}
```

- [ ] **Step 4: Thread progress + stage lines through `buildRunHealthy`**

Replace `buildRunHealthy` (lines 71-97) with (changes: `progress io.Writer` param, `out` multiwriter, stage lines, `Build(..., progress)`):

```go
func (d *Deployer) buildRunHealthy(ctx context.Context, app store.App, srcDir string, progress io.Writer, recordFailed func(imageID, containerID string, hostPort int, logs string)) (runtime.BuildResult, runtime.RunResult, string, error) {
	tag := fmt.Sprintf("piper/%s:%d", app.Name, time.Now().Unix())
	var log runtime.TailBuffer
	out := io.Writer(&log)
	if progress != nil {
		out = io.MultiWriter(&log, progress)
	}
	_, _ = io.WriteString(out, "→ building image\n")
	build, err := d.runtime.Build(ctx, srcDir, tag, progress)
	_, _ = io.WriteString(&log, build.Log)
	if err != nil {
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		recordFailed(build.ImageID, "", 0, log.String())
		return build, runtime.RunResult{}, log.String(), fmt.Errorf("build: %w", err)
	}
	_, _ = io.WriteString(out, "→ starting container\n")
	run, err := d.runtime.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		d.appendContainerOutput(ctx, &log, run.ContainerID)
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort, log.String())
		return build, run, log.String(), fmt.Errorf("run: %w", err)
	}
	_, _ = io.WriteString(out, "→ health-checking\n")
	if err := d.runtime.WaitHealthy(ctx, run.HostPort); err != nil {
		d.appendContainerOutput(ctx, &log, run.ContainerID)
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort, log.String())
		return build, run, log.String(), fmt.Errorf("health: %w", err)
	}
	return build, run, log.String(), nil
}
```

> Note: build output reaches `log` via `build.Log` and reaches `progress` (the sink) live via `d.runtime.Build(..., progress)`. Stage lines go to `out` (both). Container output/error on failure go to `log` only, then `recordFailed` writes the authoritative failed log immediately — the sink is never written after, so it cannot overwrite it.

- [ ] **Step 5: Replace `Deploy` with `Begin` + `Finish` + wrapper**

Replace `Deploy` (lines 118-166) with:

```go
// Begin opens a building deployment row for appName and returns it; its id is
// what an async caller (the deploy API) hands back before the build finishes.
func (d *Deployer) Begin(appName string) (store.Deployment, error) {
	if _, err := d.store.GetApp(appName); err != nil {
		return store.Deployment{}, err
	}
	return d.store.CreateDeployment(appName, "", "", 0, "building", "")
}

// Finish builds, runs, health-checks, and routes dep's app from srcDir,
// streaming the build's output into dep's log and finalizing the row
// running/failed. On build/run/health failure it finalizes the same row failed.
func (d *Deployer) Finish(ctx context.Context, dep store.Deployment, srcDir string) error {
	app, err := d.store.GetApp(dep.App)
	if err != nil {
		return err
	}
	previous, err := d.store.LatestRunning(dep.App)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	sink := &logSink{store: d.store, id: dep.ID}
	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, sink, func(img, cid string, hp int, logs string) {
		_ = d.store.FinalizeDeployment(dep.ID, img, cid, hp, "failed", logs)
	})
	if err != nil {
		return err
	}

	if err := d.store.FinalizeDeployment(dep.ID, build.ImageID, run.ContainerID, run.HostPort, "running", logs); err != nil {
		return err
	}
	host := d.hostFor(dep.App)
	if d.registrar != nil {
		host, err = d.registrar.Register(dep.App)
		if err != nil {
			return fmt.Errorf("register hostname: %w", err)
		}
	}
	if err := d.routes.UpsertRoute(host, run.HostPort); err != nil {
		return fmt.Errorf("route: %w", err)
	}
	if err := d.store.SetAppHostname(dep.App, host); err != nil {
		return fmt.Errorf("record hostname: %w", err)
	}
	if dc, err := d.store.GetDomainConfig(); err == nil && dc.Status == "active" {
		if err := d.routes.UpsertRouteTLS(dep.App+"."+dc.Domain, run.HostPort); err != nil {
			return fmt.Errorf("route custom domain: %w", err)
		}
	}
	if previous.ContainerID != "" && previous.ContainerID != run.ContainerID {
		_ = d.runtime.Stop(ctx, previous.ContainerID)
		_ = d.store.UpdateDeploymentStatus(previous.ID, "stopped")
	}
	return nil
}

// Deploy is the synchronous Begin+Finish used by the webhook path; it returns
// the finalized deployment.
func (d *Deployer) Deploy(ctx context.Context, appName, srcDir string) (store.Deployment, error) {
	dep, err := d.Begin(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	if err := d.Finish(ctx, dep, srcDir); err != nil {
		return store.Deployment{}, err
	}
	return d.store.LatestDeployment(appName)
}
```

- [ ] **Step 6: Update the preview call site**

`DeployPreview` (still at ~line 168 area after the edits) calls `buildRunHealthy`. Update that one call to pass `nil` progress (previews stay on the old path). Find:

```go
	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, func(img, cid string, hp int, logs string) {
```

and change to:

```go
	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, nil, func(img, cid string, hp int, logs string) {
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./internal/deploy/ -v`
Expected: PASS (new tests + all existing deploy tests, which exercise `Deploy` unchanged).

- [ ] **Step 8: Commit**

```bash
git add internal/deploy/deploy.go internal/deploy/deploy_test.go
git commit -m "feat(deploy): split Deploy into Begin/Finish with a live log sink (#140)

Part of #140

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: API — async deploy handler

**Files:**
- Modify: `internal/api/api.go` (`Deployerer` 20-24, `DeployResult` 34-40 remove, deploy handler 162-192)
- Test: `internal/api/api_test.go` (`fakeDeployer` 20-53; deploy tests 156-271)

**Interfaces:**
- Produces: `POST /v1/apps/{name}/deploy` returns **202** with a `store.Deployment` (`Status:"building"`); runs `Finish` in a background goroutine.
- Consumes: `Deployerer.Begin(app string) (store.Deployment, error)`, `Deployerer.Finish(ctx, dep, srcDir) error`.

- [ ] **Step 1: Rewrite the deploy tests (failing)**

In `internal/api/api_test.go`, replace the `fakeDeployer` type and its `Deploy` method (lines 20-53) with a store-backed fake implementing `Begin`/`Finish`:

```go
type fakeDeployer struct {
	store     *store.Store
	gotApp    string
	gotFile   string
	stopped   []string
	deleted   []string
	stopErr   error
	deleteErr error
}

func (f *fakeDeployer) Begin(app string) (store.Deployment, error) {
	f.gotApp = app
	return f.store.CreateDeployment(app, "", "", 0, "building", "")
}

func (f *fakeDeployer) Finish(_ context.Context, dep store.Deployment, srcDir string) error {
	contents, err := os.ReadFile(filepath.Join(srcDir, "Dockerfile"))
	if err != nil {
		_ = f.store.FinalizeDeployment(dep.ID, "", "", 0, "failed", err.Error())
		return err
	}
	f.gotFile = string(contents)
	return f.store.FinalizeDeployment(dep.ID, "img1", "cid1", 40001, "running", "built ok")
}

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

Update the test constructors so the fake shares the handler's store. Change `newTestHandler` (lines 65-68) and any test that builds a handler it will POST a deploy to. The simplest pattern — build the store once and hand it to both:

```go
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	s := newTestStore(t)
	return New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
}
```

For tests that construct `New(s, &fakeDeployer{}, ...)` directly with a named store `s`, change to `&fakeDeployer{store: s}`.

Now replace the two deploy tests. Replace `TestDeployUploadExtractsAndCallsDeployer` (starts line 156) and `TestDeployResponseIncludesHostname` (starts line 231) with:

```go
func TestDeployIsAsyncAndDrivesRowToRunning(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deployer := &fakeDeployer{store: s}
	h := New(s, deployer, "piper.localhost", "", nil, nil)

	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	body := []byte("FROM scratch\n")
	_ = tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/apps/web/deploy", &tarball)
	req.Header.Set("Content-Type", "application/x-tar")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var dep store.Deployment
	if err := json.Unmarshal(rr.Body.Bytes(), &dep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dep.ID == "" || dep.Status != "building" {
		t.Fatalf("202 body = %+v, want building row with id", dep)
	}

	// The goroutine finalizes the row; poll the store until it does.
	waitForStatus(t, s, "web", "running")
	if deployer.gotFile != "FROM scratch\n" {
		t.Fatalf("Finish saw Dockerfile %q", deployer.gotFile)
	}
}

func TestDeployUnknownAppIs404(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/apps/ghost/deploy", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-tar")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func waitForStatus(t *testing.T, s *store.Store, app, want string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		d, err := s.LatestDeployment(app)
		if err == nil && d.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("deployment for %s never reached %q", app, want)
}
```

Add `"time"` to the test imports. Delete any now-unused helper referenced only by the removed tests.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestDeploy -v`
Expected: FAIL — interface mismatch (`*fakeDeployer` missing `Deploy` / has `Begin`,`Finish` the interface doesn't declare) and 200-vs-202.

- [ ] **Step 3: Update the `Deployerer` interface**

In `internal/api/api.go`, replace lines 20-24:

```go
type Deployerer interface {
	Begin(app string) (store.Deployment, error)
	Finish(ctx context.Context, dep store.Deployment, srcDir string) error
	Stop(ctx context.Context, app string) error
	Delete(ctx context.Context, app string) error
}
```

- [ ] **Step 4: Remove the now-unused `DeployResult`**

Delete the `DeployResult` type (lines 34-40) and its doc comment.

- [ ] **Step 5: Rewrite the deploy handler**

Replace the deploy handler (lines 162-192) with:

```go
	mux.HandleFunc("POST /v1/apps/{name}/deploy", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, err := s.GetApp(name); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dir, err := os.MkdirTemp("", "piper-src-*")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := untar(r.Body, dir); err != nil {
			os.RemoveAll(dir)
			http.Error(w, "bad tar: "+err.Error(), http.StatusBadRequest)
			return
		}
		dep, err := d.Begin(name)
		if err != nil {
			os.RemoveAll(dir)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Deploy runs past this request: own the temp dir and use a background
		// context, since r.Context() is cancelled once the 202 is written. The
		// build outcome is observed by polling the deployment's status + logs.
		go func() {
			defer os.RemoveAll(dir)
			_ = d.Finish(context.Background(), dep, dir)
		}()
		writeJSON(w, http.StatusAccepted, dep)
	})
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/api/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/api/api.go internal/api/api_test.go
git commit -m "feat(api): async deploy POST returns a building deployment (#140)

Part of #140

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Client — follow a deployment by polling

**Files:**
- Modify: `internal/client/client.go` (`Deploy` 104-122; add methods; imports)
- Test: `internal/client/client_test.go`

**Interfaces:**
- Produces:
  - `func (c *Client) Deploy(name, srcDir string) (store.Deployment, error)` — POSTs the tar, returns the 202 building deployment.
  - `func (c *Client) Deployments(name string) ([]store.Deployment, error)`
  - `func (c *Client) DeploymentLogs(name, id string) (string, error)`
  - `func (c *Client) App(name string) (api.App, error)`
  - `func (c *Client) FollowDeploy(name, id string, progress io.Writer) (store.Deployment, error)` — polls logs+status, writing new log bytes to progress, until a terminal status.
  - Field `pollInterval time.Duration` on `Client` (defaults to 1s in `New`; tests set it small).
- Consumes: Task 4's endpoints.

- [ ] **Step 1: Write the failing test**

Add to `internal/client/client_test.go` (use `httptest` like existing tests; check the file for the existing server-setup pattern and reuse it):

```go
func TestFollowDeployStreamsThenReportsTerminal(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/apps/web/deployments/dep1/logs":
			n := atomic.AddInt32(&polls, 1)
			w.Header().Set("Content-Type", "text/plain")
			if n < 3 {
				io.WriteString(w, "line1\n")
			} else {
				io.WriteString(w, "line1\nline2\n")
			}
		case r.URL.Path == "/v1/apps/web/deployments":
			status := "building"
			if atomic.LoadInt32(&polls) >= 3 {
				status = "running"
			}
			writeJSONTest(w, []store.Deployment{{ID: "dep1", App: "web", Status: status}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.pollInterval = time.Millisecond
	var progress bytes.Buffer
	dep, err := c.FollowDeploy("web", "dep1", &progress)
	if err != nil {
		t.Fatalf("FollowDeploy: %v", err)
	}
	if dep.Status != "running" {
		t.Fatalf("status = %q, want running", dep.Status)
	}
	if progress.String() != "line1\nline2\n" {
		t.Fatalf("progress = %q, want the full log printed once (no dupes)", progress.String())
	}
}
```

> `writeJSONTest` is a one-line helper: `func writeJSONTest(w http.ResponseWriter, v any) { w.Header().Set("Content-Type","application/json"); json.NewEncoder(w).Encode(v) }` — add it to the test file if there's no existing JSON-writing helper. Add imports: `bytes`, `io`, `sync/atomic`, `time`, `encoding/json`, `internal/store`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run TestFollowDeploy -v`
Expected: FAIL — `c.pollInterval undefined`, `c.FollowDeploy undefined`.

- [ ] **Step 3: Add the poll field and methods**

In `internal/client/client.go`: add `"io"`, `"time"`, and `"github.com/piperbox/piper/internal/store"` to imports (io is already imported). Add `pollInterval time.Duration` to the `Client` struct and set it in `New`:

```go
type Client struct {
	base         string
	token        string
	http         *http.Client
	pollInterval time.Duration
}

func New(base, token string) *Client {
	if base == "" {
		base = "http://127.0.0.1:8088"
	}
	return &Client{base: base, token: token, http: &http.Client{}, pollInterval: time.Second}
}
```

Change `Deploy` (104-122) to return the 202 building deployment:

```go
func (c *Client) Deploy(name, srcDir string) (store.Deployment, error) {
	var body bytes.Buffer
	if err := TarDir(srcDir, &body); err != nil {
		return store.Deployment{}, err
	}
	resp, err := c.do(http.MethodPost, "/v1/apps/"+name+"/deploy", "application/x-tar", &body)
	if err != nil {
		return store.Deployment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return store.Deployment{}, responseError("deploy", resp)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		return store.Deployment{}, err
	}
	return dep, nil
}

func (c *Client) Deployments(name string) ([]store.Deployment, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps/"+name+"/deployments", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, responseError("deployments", resp)
	}
	var deps []store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&deps); err != nil {
		return nil, err
	}
	return deps, nil
}

func (c *Client) DeploymentLogs(name, id string) (string, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps/"+name+"/deployments/"+id+"/logs", "", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return "", responseError("deployment logs", resp)
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func (c *Client) App(name string) (api.App, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps/"+name, "", nil)
	if err != nil {
		return api.App{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return api.App{}, responseError("app", resp)
	}
	var a api.App
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return api.App{}, err
	}
	return a, nil
}

// FollowDeploy polls until the deployment reaches a terminal status, writing
// new log bytes to progress as the stored log grows. Returns the terminal
// deployment.
func (c *Client) FollowDeploy(name, id string, progress io.Writer) (store.Deployment, error) {
	printed := 0
	for {
		logs, err := c.DeploymentLogs(name, id)
		if err != nil {
			return store.Deployment{}, err
		}
		if len(logs) >= printed {
			_, _ = io.WriteString(progress, logs[printed:])
		} else {
			// Tail-cap dropped the front (log exceeded the cap): reprint whole.
			_, _ = io.WriteString(progress, logs)
		}
		printed = len(logs)

		deps, err := c.Deployments(name)
		if err != nil {
			return store.Deployment{}, err
		}
		for _, d := range deps {
			if d.ID != id {
				continue
			}
			switch d.Status {
			case "running", "failed", "stopped":
				return d, nil
			}
		}
		time.Sleep(c.pollInterval)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/client/ -run TestFollowDeploy -v`
Expected: PASS.

- [ ] **Step 5: Fix any existing client test broken by the `Deploy` return-type change**

Existing `Deploy` tests referenced `api.DeployResult`. Run `go test ./internal/client/ -v`; for any failure, update the assertion to decode/inspect a `store.Deployment` with `Status:"building"` (the 202 body) instead of the old `DeployResult`.

Run: `go test ./internal/client/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/client/
git commit -m "feat(cli): client can follow a deployment by polling (#140)

Part of #140

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: CLI — `piper deploy` streams progress

**Files:**
- Modify: `cmd/piper/main.go:182-208` (the `deploy` case)
- Test: `cmd/piper/main_test.go`

**Interfaces:**
- Consumes: `client.Deploy`, `client.FollowDeploy`, `client.App`, `appURL`.

- [ ] **Step 1: Write the failing test**

Add to `cmd/piper/main_test.go` a test that stands up a fake piperd with the async contract and runs the `deploy` command. Follow the file's existing pattern for invoking `run(...)` against an `httptest` server (search the file for how other subcommands are tested and mirror the harness — arg slice, stdout/stderr buffers, config pointing at the server). Core assertions:

```go
func TestDeployStreamsProgressAndReportsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps/web/deploy":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "web", Status: "building"})
		case r.URL.Path == "/v1/apps/web/deployments/dep1/logs":
			io.WriteString(w, "pulling base image...\nbuilt ok\n")
		case r.URL.Path == "/v1/apps/web/deployments":
			json.NewEncoder(w).Encode([]store.Deployment{{ID: "dep1", App: "web", Status: "running"}})
		case r.URL.Path == "/v1/apps/web":
			json.NewEncoder(w).Encode(api.App{App: store.App{Name: "web", Hostname: "web.piper.localhost"}, Status: "running"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	// ...point the CLI config/client at srv.URL per the file's existing harness...
	// ...create a temp source dir with a Dockerfile...
	var stdout, stderr bytes.Buffer
	code := runDeployForTest(t, srv.URL, srcDir, &stdout, &stderr) // adapt to the file's actual entrypoint

	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "pulling base image") {
		t.Fatalf("progress not streamed to stderr: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "deployed web: http://web.piper.localhost (running)") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
```

> Adapt the harness call (`runDeployForTest`) to however `cmd/piper/main_test.go` already drives commands — reuse that; don't invent a new one. To keep the poll fast, the test harness should let you set a short interval, or accept the 1s default for a single tick (the fake returns `running` on the first status poll, so at most one `time.Sleep(1s)` may elapse — acceptable, or expose a test seam if the file already has one).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/piper/ -run TestDeployStreamsProgress -v`
Expected: FAIL — stdout/stderr assertions unmet (old code prints one line, no streaming).

- [ ] **Step 3: Rewrite the `deploy` case**

Replace the body of `case "deploy":` (lines 182-208) — keep the arg parsing (182-201) and replace from `dep, err := c.Deploy(...)` onward:

```go
		dep, err := c.Deploy(name, *path)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		final, err := c.FollowDeploy(name, dep.ID, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if final.Status != "running" {
			fmt.Fprintf(stderr, "deploy failed: %s (%s)\n", name, final.Status)
			return 1
		}
		app, err := c.App(name)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "deployed %s: %s (%s)\n", name, appURL(app.Hostname, *remote != ""), final.Status)
		return 0
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/piper/ -run TestDeployStreamsProgress -v`
Expected: PASS.

- [ ] **Step 5: Run the whole CLI package**

Run: `go test ./cmd/piper/ -v`
Expected: PASS. Fix any other test that asserted the old single-line deploy behavior.

- [ ] **Step 6: Commit**

```bash
git add cmd/piper/
git commit -m "feat(cli): piper deploy streams build progress and reports the URL (#140)

Closes #140

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: Docs + full verify

**Files:**
- Modify: `PROGRESS.md`

- [ ] **Step 1: Update PROGRESS.md**

Open `PROGRESS.md`, find the deploy/CLI line covering deploy output (search for `deploy`), and update it to note that `piper deploy` now streams live build progress, keeping the `[#140]` reference. Match the file's existing one-line-per-item terse style; do not restate the issue.

- [ ] **Step 2: Run the full verify gate**

Run: `make verify`
Expected: PASS — gofmt clean, `go vet` clean, all tests pass, `make cross` (linux/arm64) builds.

If gofmt flags files: `make fmt`, then re-run `make verify`.

- [ ] **Step 3: Commit**

```bash
git add PROGRESS.md
git commit -m "docs: mark deploy progress streaming built (#140)

Part of #140

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin ozykhan/deploy-progress
gh pr create --base main --title "feat: stream piper deploy progress (#140)" --body "$(cat <<'EOF'
## What

`piper deploy` no longer blocks silently through a multi-minute first build.
piperd now opens a `building` deployment row up front and grows its stored log
as the build streams; the deploy POST returns that row immediately (202) and
runs the build in the background; the CLI follows by polling the deployments +
logs endpoints (the same surface the dashboard uses), printing new log bytes
until the deploy reaches `running`/`failed`.

Previews are unchanged. See `docs/superpowers/specs/2026-07-12-deploy-progress-design.md`.

Closes #140

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

---

## Self-Review

**Spec coverage:**
- §1 Store (`UpdateDeploymentLogs`, `FinalizeDeployment`, building-row create) → Task 1. ✅
- §2 Deploy (`Begin`/`Finish`/`Deploy` wrapper, store-backed sink, stage lines, `buildRunHealthy` progress; previews pass `nil`) → Task 3; runtime `Build` progress → Task 2. ✅
- §3 API (async 202, background goroutine owns temp dir + context, pre-`Begin` 404/400 synchronous, interface `Begin`/`Finish`) → Task 4. ✅
- §4 Client/CLI (202 id, poll logs+status, print delta to stderr, hostname via `GET /v1/apps/{name}`, exit codes) → Tasks 5–6. ✅
- §5 Testing (store round-trip/finalize; deploy incremental+finalize+failure+wrapper; runtime live output; api 202+drives-to-terminal+404; client/cmd poll) → per-task tests. ✅
- Known-limitation (crash strands a `building` row) — documented as out of scope; no task, intentional. ✅

**Placeholder scan:** No TBD/TODO. The one adaptation note is in Task 6 Step 1, where the harness call must match `cmd/piper/main_test.go`'s existing test entrypoint (the file's harness is authoritative over an invented `runDeployForTest`).

**Type consistency:** `Begin(app string) (store.Deployment, error)` and `Finish(ctx, dep store.Deployment, srcDir string) error` match across deploy (Task 3), the api `Deployerer` interface + fake (Task 4). `FinalizeDeployment(id, imageID, containerID string, hostPort int, status, logs string)` matches store (Task 1) and its callers in deploy/api-fake. `logSink{store, id, buf, lastFlush}` + `logFlushInterval` match between the impl (Task 3 Step 3) and its test (Task 3 Step 1). `Client.pollInterval` + `FollowDeploy(name, id string, progress io.Writer)` match across Tasks 5–6.
