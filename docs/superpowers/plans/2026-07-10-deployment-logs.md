# Deployment History + Build/Deploy Logs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Capture build + deploy output per deployment and expose deployment history and per-deployment logs over the authenticated control API (issue #101).

**Architecture:** `runtime` stops discarding the Docker build stream and returns it (tail-capped) in `BuildResult`; `deploy` assembles one log blob per deployment (build log + container output on run/health failure) and passes it to `store`; `store` persists it in a new `logs` column on `deployments` with 20-per-app retention; `api` adds two token-gated read endpoints. Spec: `docs/superpowers/specs/2026-07-10-deployment-logs-design.md`.

**Tech Stack:** Go, `modernc.org/sqlite` (pure Go), Docker Engine SDK (`pkg/jsonmessage`, `pkg/stdcopy` — already in the module graph via `github.com/docker/docker`).

## Global Constraints

- **No cgo** — everything must build with `CGO_ENABLED=0` (`make cross` proves it).
- Deployment status strings are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`.
- Layering: `store` = persistence only, `runtime` = Docker only, `deploy` orchestrates via interfaces, `api` = transport. Nothing imports "up".
- Run `make verify` (gofmt → vet → test → cross) before claiming any task done. Docker-gated tests skip without Docker; run them when Docker is available.
- One commit per task step-cycle, conventional-commit style, body includes `Part of #101`, and every commit message ends with:
  `Co-Authored-By: Claude {current model} <noreply@anthropic.com>`
- Branch: `ozykhan/deploy-logs` (already exists, carries the spec commit).

---

### Task 1: Store — `logs` column, history queries, retention

**Files:**
- Modify: `internal/store/schema.sql` (deployments CREATE TABLE)
- Modify: `internal/store/store.go` (`migrate`, `CreateDeployment`, `CreatePreviewDeployment`, new `ListDeployments`, `DeploymentLogs`, `pruneDeploymentLogs`)
- Test: `internal/store/store_test.go`
- Modify (mechanical, keep tree compiling): `internal/deploy/deploy.go:99,105,137,143`, `internal/api/api_test.go:288,316`, `internal/store/store_test.go:117,118,131,142,159,162,272,273,286,288` — every `CreateDeployment`/`CreatePreviewDeployment` call gains a trailing `""` logs argument.

**Interfaces:**
- Consumes: existing `store.Deployment`, `ErrNotFound`, `openTemp(t)` test helper (`store_test.go:9`).
- Produces (later tasks rely on these exact signatures):
  - `func (s *Store) CreateDeployment(app, imageID, containerID string, hostPort int, status, logs string) (Deployment, error)`
  - `func (s *Store) CreatePreviewDeployment(app string, pr int, imageID, containerID string, hostPort int, status, logs string) (Deployment, error)`
  - `func (s *Store) ListDeployments(app string) ([]Deployment, error)` — newest first
  - `func (s *Store) DeploymentLogs(app, id string) (string, error)` — `ErrNotFound` if no such deployment under that app; `""` if pruned

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go` (the file is `package store`, so tests may touch `s.db`). Add `"database/sql"` and `"strings"` to its imports.

```go
func TestDeploymentLogsRoundTripAndScoping(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed", "step 1/2\nboom\n")
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	logs, err := s.DeploymentLogs("blog", d.ID)
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	if !strings.Contains(logs, "boom") {
		t.Errorf("logs = %q, want build output", logs)
	}
	// Same id under another app must not resolve.
	if _, err := s.DeploymentLogs("api", d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-app lookup err = %v, want ErrNotFound", err)
	}
	if _, err := s.DeploymentLogs("blog", "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id err = %v, want ErrNotFound", err)
	}
}

func TestListDeploymentsNewestFirst(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img2", "c2", 40002, "running", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePreviewDeployment("blog", 5, "img3", "c3", 40003, "running", ""); err != nil {
		t.Fatal(err)
	}

	deps, err := s.ListDeployments("blog")
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("len = %d, want 3 (previews included)", len(deps))
	}
	if deps[0].ImageID != "img3" || deps[2].ImageID != "img1" {
		t.Errorf("order = [%s %s %s], want newest first", deps[0].ImageID, deps[1].ImageID, deps[2].ImageID)
	}
	if deps[0].PR != 5 {
		t.Errorf("deps[0].PR = %d, want 5", deps[0].PR)
	}
	if empty, err := s.ListDeployments("never-deployed"); err != nil || len(empty) != 0 {
		t.Errorf("unknown app = %v (err %v), want empty, nil", empty, err)
	}
}

func TestDeploymentLogRetentionPrunesTo20PerApp(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("api", "img", "c", 40000, "running", "other-app log"); err != nil {
		t.Fatal(err)
	}
	var last Deployment
	for i := 0; i < 22; i++ {
		var err error
		last, err = s.CreateDeployment("blog", "img", "c", 40001, "running", "log body")
		if err != nil {
			t.Fatal(err)
		}
	}

	var withLogs int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM deployments WHERE app='blog' AND logs != ''`).Scan(&withLogs); err != nil {
		t.Fatal(err)
	}
	if withLogs != 20 {
		t.Errorf("blog rows with logs = %d, want 20", withLogs)
	}
	var rows int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM deployments WHERE app='blog'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 22 {
		t.Errorf("blog rows = %d, want 22 (rows are history; only logs are pruned)", rows)
	}
	// The newest deployment's log survives; the other app is untouched.
	if logs, err := s.DeploymentLogs("blog", last.ID); err != nil || logs != "log body" {
		t.Errorf("newest logs = %q (err %v), want kept", logs, err)
	}
	var otherWithLogs int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM deployments WHERE app='api' AND logs != ''`).Scan(&otherWithLogs); err != nil {
		t.Fatal(err)
	}
	if otherWithLogs != 1 {
		t.Errorf("api rows with logs = %d, want 1", otherWithLogs)
	}
}

func TestMigrateAddsLogsColumnToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// A pre-#101 database: deployments table without the logs column.
	if _, err := db.Exec(`
		CREATE TABLE apps (name TEXT PRIMARY KEY, port INTEGER NOT NULL,
			repo TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
		CREATE TABLE deployments (id TEXT PRIMARY KEY, app TEXT NOT NULL REFERENCES apps(name),
			image_id TEXT NOT NULL, container_id TEXT NOT NULL, host_port INTEGER NOT NULL,
			status TEXT NOT NULL, created_at TEXT NOT NULL, pr INTEGER NOT NULL DEFAULT 0);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open over old db: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDeployment("blog", "img", "c", 40001, "failed", "migrated log")
	if err != nil {
		t.Fatalf("CreateDeployment on migrated db: %v", err)
	}
	if logs, err := s.DeploymentLogs("blog", d.ID); err != nil || logs != "migrated log" {
		t.Errorf("logs = %q (err %v), want migrated log", logs, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestDeploymentLogs|TestListDeployments|TestDeploymentLogRetention|TestMigrateAddsLogs' -v`
Expected: compile error — `too many arguments in call to s.CreateDeployment` / `s.DeploymentLogs undefined` (a compile failure is the failing state here).

- [ ] **Step 3: Implement**

`internal/store/schema.sql` — add `logs` to the deployments baseline (matches how `apps.repo`/`apps.branch` appear both in schema and migrate):

```sql
CREATE TABLE IF NOT EXISTS deployments (
    id           TEXT PRIMARY KEY,
    app          TEXT NOT NULL REFERENCES apps(name),
    image_id     TEXT NOT NULL,
    container_id TEXT NOT NULL,
    host_port    INTEGER NOT NULL,
    status       TEXT NOT NULL,
    logs         TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL
);
```

`internal/store/store.go` — add the migrate line:

```go
		`ALTER TABLE deployments ADD COLUMN pr INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deployments ADD COLUMN logs TEXT NOT NULL DEFAULT ''`,
```

Replace `CreateDeployment` and `CreatePreviewDeployment`, and add the retention + read functions:

```go
// logRetentionPerApp bounds stored log blobs: only the newest N deployments
// per app keep their logs. Rows themselves are never deleted — they are the
// deployment history.
const logRetentionPerApp = 20

func (s *Store) CreateDeployment(app, imageID, containerID string, hostPort int, status, logs string) (Deployment, error) {
	d := Deployment{
		ID: uuid.NewString(), App: app, ImageID: imageID,
		ContainerID: containerID, HostPort: hostPort, Status: status,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO deployments(id, app, image_id, container_id, host_port, status, logs, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		d.ID, d.App, d.ImageID, d.ContainerID, d.HostPort, d.Status, logs,
		d.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Deployment{}, err
	}
	return d, s.pruneDeploymentLogs(app)
}

func (s *Store) CreatePreviewDeployment(app string, pr int, imageID, containerID string, hostPort int, status, logs string) (Deployment, error) {
	d := Deployment{
		ID: uuid.NewString(), App: app, PR: pr, ImageID: imageID,
		ContainerID: containerID, HostPort: hostPort, Status: status,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO deployments(id, app, image_id, container_id, host_port, status, logs, created_at, pr)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		d.ID, d.App, d.ImageID, d.ContainerID, d.HostPort, d.Status, logs,
		d.CreatedAt.Format(time.RFC3339Nano), d.PR)
	if err != nil {
		return Deployment{}, err
	}
	return d, s.pruneDeploymentLogs(app)
}

func (s *Store) pruneDeploymentLogs(app string) error {
	_, err := s.db.Exec(
		`UPDATE deployments SET logs='' WHERE app=? AND logs != '' AND id NOT IN (
		   SELECT id FROM deployments WHERE app=? ORDER BY created_at DESC LIMIT ?)`,
		app, app, logRetentionPerApp)
	return err
}

// ListDeployments returns every deployment for app (previews included),
// newest first.
func (s *Store) ListDeployments(app string) ([]Deployment, error) {
	rows, err := s.db.Query(
		`SELECT id, app, pr, image_id, container_id, host_port, status, created_at
		 FROM deployments WHERE app=? ORDER BY created_at DESC`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var ts string
		if err := rows.Scan(&d.ID, &d.App, &d.PR, &d.ImageID, &d.ContainerID, &d.HostPort, &d.Status, &ts); err != nil {
			return nil, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeploymentLogs returns the captured log for one deployment, scoped by app
// so an id from another app is ErrNotFound. Empty string when the log was
// pruned by retention.
func (s *Store) DeploymentLogs(app, id string) (string, error) {
	var logs string
	err := s.db.QueryRow(
		`SELECT logs FROM deployments WHERE app=? AND id=?`, app, id).Scan(&logs)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return logs, err
}
```

Mechanically update every existing call site listed under **Files** to pass `""` as the new final `logs` argument (deploy.go gets real logs in Task 3), e.g. `internal/deploy/deploy.go:99` becomes:

```go
		_, _ = d.store.CreateDeployment(appName, img, cid, hp, "failed", "")
```

and `internal/store/store_test.go:117` becomes:

```go
	d1, _ := s.CreateDeployment("blog", "img1", "c1", 40001, "running", "")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ ./internal/deploy/ ./internal/api/ -v`
Expected: PASS (all packages — call sites compile again).

- [ ] **Step 5: Commit**

```bash
git add internal/store internal/deploy/deploy.go internal/api/api_test.go
git commit -m "feat(store): deployment logs column, history queries, 20-per-app retention

Part of #101

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 2: Runtime — capture the build log, demux container logs

**Files:**
- Create: `internal/runtime/log.go` (TailBuffer)
- Create: `internal/runtime/log_test.go`
- Modify: `internal/runtime/runtime.go` (BuildResult)
- Modify: `internal/runtime/docker.go` (Build, Logs)
- Modify: `internal/runtime/fake.go` (Log fields)
- Test: `internal/runtime/docker_test.go` (Docker-gated failing-build test)

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces (Task 3 relies on these):
  - `BuildResult` gains `Log string`, populated **even when Build returns an error**.
  - `type TailBuffer struct{ ... }` — exported `io.Writer` keeping the last `LogCap` (1 MiB) bytes; `String()` prefixes `"[log truncated]\n"` when earlier output was dropped.
  - `const LogCap = 1 << 20`
  - `FakeRuntime` gains `LogsVal string`, `LogsErr error`; `Logs` returns them (replaces the hardcoded `"fake logs\n"`, which nothing consumes today).
  - `DockerRuntime.Logs` now yields demuxed plain text (stdcopy), not the raw multiplexed stream.

- [ ] **Step 1: Write the failing TailBuffer test**

Create `internal/runtime/log_test.go`:

```go
package runtime

import (
	"strings"
	"testing"
)

func TestTailBufferPassthroughUnderCap(t *testing.T) {
	var b TailBuffer
	if _, err := b.Write([]byte("hello\nworld\n")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "hello\nworld\n" {
		t.Errorf("String() = %q, want passthrough", got)
	}
}

func TestTailBufferKeepsTailAndMarksTruncation(t *testing.T) {
	var b TailBuffer
	if _, err := b.Write([]byte(strings.Repeat("x", LogCap))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("THE END")); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if !strings.HasPrefix(got, "[log truncated]\n") {
		t.Errorf("missing truncation marker: %q...", got[:32])
	}
	if !strings.HasSuffix(got, "THE END") {
		t.Error("tail was not kept")
	}
	if len(got) > LogCap+len("[log truncated]\n") {
		t.Errorf("len = %d, want <= cap+marker", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestTailBuffer -v`
Expected: compile error — `undefined: TailBuffer`.

- [ ] **Step 3: Implement TailBuffer**

Create `internal/runtime/log.go`:

```go
package runtime

// LogCap bounds every captured deployment log blob so a pathological build
// can't balloon memory on a Pi-class box.
const LogCap = 1 << 20 // 1 MiB

// TailBuffer is an io.Writer that keeps only the last LogCap bytes written.
// Errors live at the end of a log, so the tail is the part worth keeping.
type TailBuffer struct {
	buf       []byte
	truncated bool
}

func (t *TailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > LogCap {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-LogCap:]...)
		t.truncated = true
	}
	return len(p), nil
}

// String returns the captured tail, prefixed with a truncation marker when
// earlier output was dropped.
func (t *TailBuffer) String() string {
	if t.truncated {
		return "[log truncated]\n" + string(t.buf)
	}
	return string(t.buf)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/runtime/ -run TestTailBuffer -v`
Expected: PASS.

- [ ] **Step 5: Write the failing Docker-gated build-log test**

Append to `internal/runtime/docker_test.go`:

```go
func TestDockerBuildFailureReturnsLog(t *testing.T) {
	r := dockerAvailable(t)
	dir := t.TempDir()
	df := "FROM busybox:1.36\nRUN exit 7\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := r.Build(context.Background(), dir, "piper-runtime-failtest:latest")
	if err == nil {
		t.Fatal("expected build error")
	}
	if b.Log == "" {
		t.Fatal("expected non-empty build log on failure")
	}
	if !strings.Contains(b.Log, "exit 7") {
		t.Errorf("log should show the failing step, got:\n%s", b.Log)
	}
}
```

Add `"strings"` to the test file's imports.

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestDockerBuildFailureReturnsLog -v`
Expected: compile error — `b.Log undefined` (or SKIP without Docker; with Docker present it must be run and fail before Step 7).

- [ ] **Step 7: Implement Build capture, Logs demux, fake fields**

`internal/runtime/runtime.go`:

```go
// BuildResult carries the built image id and the build's plain-text log.
// Log is populated even when Build returns an error — that failing output is
// the whole point of capturing it.
type BuildResult struct {
	ImageID string
	Log     string
}
```

`internal/runtime/docker.go` — replace the drain in `Build` (imports gain `"github.com/docker/docker/pkg/jsonmessage"` and `"github.com/docker/docker/pkg/stdcopy"`):

```go
	defer resp.Body.Close()
	// Decode the build's JSON progress stream into a tail-capped plain-text
	// log. A failing build step arrives on the stream and surfaces as err
	// here, log in hand.
	var log TailBuffer
	if err := jsonmessage.DisplayJSONMessagesStream(resp.Body, &log, 0, false, nil); err != nil {
		return BuildResult{Log: log.String()}, err
	}
	insp, err := d.cli.ImageInspect(ctx, imageTag)
	if err != nil {
		return BuildResult{Log: log.String()}, fmt.Errorf("inspect built image (build may have failed): %w", err)
	}
	return BuildResult{ImageID: insp.ID, Log: log.String()}, nil
```

and replace `Logs` (containers run without a TTY, so the stream is stdcopy-multiplexed; demux so callers see plain text):

```go
func (d *DockerRuntime) Logs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	raw, err := d.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: "200",
	})
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, raw)
		raw.Close()
		pw.CloseWithError(err)
	}()
	return pr, nil
}
```

`internal/runtime/fake.go` — add fields and replace `Logs`:

```go
type FakeRuntime struct {
	BuildResultVal  BuildResult
	BuildErr        error
	RunResultVal    RunResult
	RunErr          error
	HealthErr       error
	LogsVal         string
	LogsErr         error
	Stopped         []string
	StopContextErrs []error
}

func (f *FakeRuntime) Logs(context.Context, string) (io.ReadCloser, error) {
	if f.LogsErr != nil {
		return nil, f.LogsErr
	}
	return io.NopCloser(strings.NewReader(f.LogsVal)), nil
}
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./internal/runtime/ -v`
Expected: PASS (Docker tests run for real if Docker is up; otherwise SKIP — if skipped, note it and rely on CI/e2e).

- [ ] **Step 9: Commit**

```bash
git add internal/runtime
git commit -m "feat(runtime): capture build log in BuildResult, demux container logs

Part of #101

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 3: Deploy — assemble and persist the per-deployment log

**Files:**
- Modify: `internal/deploy/deploy.go` (`buildRunHealthy`, `Deploy`, `DeployPreview`, new `appendContainerOutput`)
- Test: `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: `runtime.TailBuffer`, `runtime.BuildResult.Log`, `FakeRuntime.LogsVal` (Task 2); `store.CreateDeployment(..., logs string)`, `store.DeploymentLogs`, `store.ListDeployments` (Task 1).
- Produces: `Deploy`/`DeployPreview` signatures unchanged — callers (`api`, `webhook`) are untouched. `buildRunHealthy` becomes internal-only shape: `(runtime.BuildResult, runtime.RunResult, string, error)` with `recordFailed func(imageID, containerID string, hostPort int, logs string)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/deploy/deploy_test.go` (imports gain `"strings"`):

```go
func deploymentLog(t *testing.T, s *store.Store, app string) string {
	t.Helper()
	deps, err := s.ListDeployments(app)
	if err != nil || len(deps) == 0 {
		t.Fatalf("ListDeployments: %v (%d rows)", err, len(deps))
	}
	logs, err := s.DeploymentLogs(app, deps[0].ID)
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	return logs
}

func TestDeployBuildFailurePersistsBuildLog(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{Log: "Step 1/2 : FROM busybox\nboom\n"},
		BuildErr:       errors.New("build failed"),
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected build error")
	}
	logs := deploymentLog(t, s, "blog")
	if !strings.Contains(logs, "boom") {
		t.Errorf("failed deployment logs = %q, want build output", logs)
	}
}

func TestDeployHealthFailureAppendsContainerOutput(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1", Log: "build ok\n"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      errors.New("unhealthy"),
		LogsVal:        "panic: kaboom\n",
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected health error")
	}
	logs := deploymentLog(t, s, "blog")
	for _, want := range []string{"build ok", "--- container output ---", "panic: kaboom"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	if strings.Index(logs, "build ok") > strings.Index(logs, "container output") {
		t.Error("build log must precede container output")
	}
}

func TestDeploySuccessPersistsBuildLog(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1", Log: "build ok\n"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	logs := deploymentLog(t, s, "blog")
	if logs != "build ok\n" {
		t.Errorf("logs = %q, want build log only (no container output on success)", logs)
	}
}

func TestDeployCombinedLogIsTailCapped(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1", Log: strings.Repeat("b", runtime.LogCap)},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      errors.New("unhealthy"),
		LogsVal:        "THE END",
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected health error")
	}
	logs := deploymentLog(t, s, "blog")
	if !strings.HasPrefix(logs, "[log truncated]\n") {
		t.Error("combined log over cap must carry the truncation marker")
	}
	if !strings.HasSuffix(logs, "THE END") {
		t.Error("tail (container output) must be kept")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/deploy/ -run 'TestDeployBuildFailurePersists|TestDeployHealthFailureAppends|TestDeploySuccessPersists|TestDeployCombinedLog' -v`
Expected: FAIL — logs come back `""` (Task 1 left `""` placeholders at the call sites).

- [ ] **Step 3: Implement**

`internal/deploy/deploy.go` — imports gain `"io"`. Replace `buildRunHealthy` and add `appendContainerOutput`:

```go
// buildRunHealthy builds, runs, and health-checks app, capturing one
// tail-capped log blob (build output, plus container output when the run or
// health check fails). On failure it invokes recordFailed with whatever ids
// and log are known so the caller persists a "failed" record for the right
// (app, pr) row, then returns a wrapped error.
func (d *Deployer) buildRunHealthy(ctx context.Context, app store.App, srcDir string, recordFailed func(imageID, containerID string, hostPort int, logs string)) (runtime.BuildResult, runtime.RunResult, string, error) {
	tag := fmt.Sprintf("piper/%s:%d", app.Name, time.Now().Unix())
	var log runtime.TailBuffer
	build, err := d.runtime.Build(ctx, srcDir, tag)
	_, _ = io.WriteString(&log, build.Log)
	if err != nil {
		recordFailed(build.ImageID, "", 0, log.String())
		return build, runtime.RunResult{}, log.String(), fmt.Errorf("build: %w", err)
	}
	run, err := d.runtime.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		d.appendContainerOutput(ctx, &log, run.ContainerID)
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort, log.String())
		return build, run, log.String(), fmt.Errorf("run: %w", err)
	}
	if err := d.runtime.WaitHealthy(ctx, run.HostPort); err != nil {
		d.appendContainerOutput(ctx, &log, run.ContainerID)
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort, log.String())
		return build, run, log.String(), fmt.Errorf("health: %w", err)
	}
	return build, run, log.String(), nil
}

// appendContainerOutput best-effort appends the container's stdout/stderr to
// log; it must run before stopPartial removes the container. A fetch failure
// never masks the deploy error. Detached context so a cancelled deploy can
// still capture (same rationale as stopPartial).
func (d *Deployer) appendContainerOutput(ctx context.Context, log io.Writer, containerID string) {
	if containerID == "" {
		return
	}
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deploymentCleanupTimeout)
	defer cancel()
	rc, err := d.runtime.Logs(logCtx, containerID)
	if err != nil {
		return
	}
	defer rc.Close()
	_, _ = io.WriteString(log, "\n--- container output ---\n")
	_, _ = io.Copy(log, rc)
}
```

In `Deploy`, replace the call and both `CreateDeployment` lines:

```go
	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, func(img, cid string, hp int, logs string) {
		_, _ = d.store.CreateDeployment(appName, img, cid, hp, "failed", logs)
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreateDeployment(appName, build.ImageID, run.ContainerID, run.HostPort, "running", logs)
```

In `DeployPreview`, the same shape:

```go
	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, func(img, cid string, hp int, logs string) {
		_, _ = d.store.CreatePreviewDeployment(appName, pr, img, cid, hp, "failed", logs)
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreatePreviewDeployment(appName, pr, build.ImageID, run.ContainerID, run.HostPort, "running", logs)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/deploy/ -v`
Expected: PASS — new tests and all pre-existing ones (stop ordering, failed-row recording, preview flows).

- [ ] **Step 5: Commit**

```bash
git add internal/deploy
git commit -m "feat(deploy): persist build + container output per deployment

Part of #101

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 4: API — history and logs endpoints

**Files:**
- Modify: `internal/api/api.go` (two handlers on the mux in `New`)
- Test: `internal/api/api_test.go`, `internal/api/auth_test.go`

**Interfaces:**
- Consumes: `store.ListDeployments`, `store.DeploymentLogs`, `store.ErrNotFound` (Task 1); existing `writeJSON`, `RequireToken`, `newTestStore`/`fakeDeployer` test helpers.
- Produces: `GET /v1/apps/{name}/deployments` (JSON `[]store.Deployment`, newest first, 404 unknown app) and `GET /v1/apps/{name}/deployments/{id}/logs` (`text/plain`, 404 unknown deployment, 200 empty when pruned). Same `RequireToken` gate — piperd wraps the whole mux, so nothing to wire.

- [ ] **Step 1: Write the failing tests**

Append to `internal/api/api_test.go`:

```go
func TestListDeploymentsEndpoint(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{}, "piper.localhost", "", nil)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed", "boom"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img2", "c2", 40002, "running", "ok"); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/deployments", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var deps []store.Deployment
	if err := json.NewDecoder(rr.Body).Decode(&deps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(deps) != 2 || deps[0].ImageID != "img2" || deps[1].Status != "failed" {
		t.Errorf("deps = %+v, want [img2 running, img1 failed]", deps)
	}

	missing := httptest.NewRecorder()
	h.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/apps/nope/deployments", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("unknown app status = %d, want 404", missing.Code)
	}
}

func TestDeploymentLogsEndpoint(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{}, "piper.localhost", "", nil)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	dep, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed", "Step 1/2\nboom\n")
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/deployments/"+dep.ID+"/logs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if !strings.Contains(rr.Body.String(), "boom") {
		t.Errorf("body = %q, want build output", rr.Body.String())
	}

	// The same deployment id under a different app must 404.
	crossApp := httptest.NewRecorder()
	h.ServeHTTP(crossApp, httptest.NewRequest(http.MethodGet, "/v1/apps/api/deployments/"+dep.ID+"/logs", nil))
	if crossApp.Code != http.StatusNotFound {
		t.Errorf("cross-app status = %d, want 404", crossApp.Code)
	}
	unknown := httptest.NewRecorder()
	h.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/deployments/no-such-id/logs", nil))
	if unknown.Code != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", unknown.Code)
	}
}
```

Append to `internal/api/auth_test.go` (match its existing style for building the wrapped handler; the assertion is what matters):

```go
func TestDeploymentEndpointsRequireToken(t *testing.T) {
	s := newTestStore(t)
	h := RequireToken(s, New(s, &fakeDeployer{}, "piper.localhost", "", nil))
	for _, path := range []string{
		"/v1/apps/blog/deployments",
		"/v1/apps/blog/deployments/dep1/logs",
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", path, rr.Code)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run 'TestListDeploymentsEndpoint|TestDeploymentLogsEndpoint|TestDeploymentEndpointsRequireToken' -v`
Expected: FAIL — 404 from the mux (routes don't exist yet); the auth test may already pass (RequireToken 401s unknown paths too) — that's fine.

- [ ] **Step 3: Implement the handlers**

In `internal/api/api.go`, inside `New` after the `GET /v1/apps/{name}` handler:

```go
	mux.HandleFunc("GET /v1/apps/{name}/deployments", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, err := s.GetApp(name); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		deps, err := s.ListDeployments(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if deps == nil {
			deps = []store.Deployment{}
		}
		writeJSON(w, http.StatusOK, deps)
	})
	mux.HandleFunc("GET /v1/apps/{name}/deployments/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		logs, err := s.DeploymentLogs(r.PathValue("name"), r.PathValue("id"))
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown deployment", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, logs)
	})
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api
git commit -m "feat(api): deployment history and per-deployment logs endpoints

Part of #101

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 5: Verify, PROGRESS.md, PR

**Files:**
- Modify: `PROGRESS.md` (one line, links #101)

- [ ] **Step 1: Full gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests pass (Docker-gated ones run if Docker is up), `make cross` builds. Fix anything it flags before proceeding.

- [ ] **Step 2: Update PROGRESS.md**

Read `PROGRESS.md`, find the agent/control-API section, and add one line following the existing one-line-plus-issue-link convention, e.g.:

```markdown
- Deployment history + build/deploy logs on the control API [#101]
```

- [ ] **Step 3: Commit and push**

```bash
git add PROGRESS.md
git commit -m "docs: note deployment history + logs (#101) in PROGRESS

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
git push -u origin ozykhan/deploy-logs
```

- [ ] **Step 4: Open the PR**

```bash
gh pr create --base main --title "feat(agent): deployment history + build/deploy logs on the control API" --body "$(cat <<'EOF'
Captures build + deploy output per deployment and exposes it remotely:

- `runtime`: Docker build stream decoded (jsonmessage) into a 1 MiB tail-capped log returned in `BuildResult` — even on failure; container logs demuxed (stdcopy).
- `deploy`: one log blob per deployment — build output, plus container stdout/stderr when run/health fails (fetched before the container is removed).
- `store`: `logs` column (guarded ALTER migration), `ListDeployments`, `DeploymentLogs`; logs kept for the newest 20 deployments per app, rows kept forever.
- `api`: `GET /v1/apps/{name}/deployments` and `GET /v1/apps/{name}/deployments/{id}/logs`, behind the existing token gate, reachable over the relay proxy.

Design: docs/superpowers/specs/2026-07-10-deployment-logs-design.md

Closes #101

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Expected: PR URL printed; squash-merge after review per branch workflow.
