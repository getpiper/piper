# PR-preview URLs + teardown Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An opened/updated GitHub PR gets an ephemeral live HTTPS URL at `pr-<N>-<app>.<base>`; closing the PR tears it down.

**Architecture:** Rides the existing `source` provider seam. `github/parse.go` maps `pull_request` events; `webhook` translates them into two new `deploy` orchestrator calls (`DeployPreview`/`TeardownPreview`) that route a flattened single-label host covered by the existing `*.<base>` cert. `deploy` never imports `source`; nothing imports "up".

**Tech Stack:** Go (no cgo), `modernc.org/sqlite`, standard `net/http`, `httptest`.

## Global Constraints

- **No cgo.** All builds pass with `CGO_ENABLED=0`; SQLite driver is `modernc.org/sqlite` only.
- **Module path:** `github.com/piperbox/piper`.
- **Deployment status strings** are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`.
- **Preview host format:** `pr-<N>-<app>.<base>` (single label under `<base>` — no cert/DNS/relay change).
- **Layering:** `deploy` imports `runtime`/`store` only (never `source`); `webhook` drives `deploy` through an interface.
- TDD, failing-test-first per step. `make test` and `make cross` green, gofmt clean, `-race` clean.
- Conventional-commit messages ending with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

### Task 1: `source.StatusInactive` enum value

**Files:**
- Modify: `internal/source/source.go` (the `Status` const block)
- Test: `internal/source/source_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `source.StatusInactive` (a `source.Status`), used by `github.Report` (Task 5) and `webhook` (Task 6).

- [ ] **Step 1: Write the failing test**

Add to `internal/source/source_test.go`:

```go
func TestStatusInactiveDistinct(t *testing.T) {
	all := []source.Status{
		source.StatusPending, source.StatusSuccess,
		source.StatusFailure, source.StatusInactive,
	}
	seen := map[source.Status]bool{}
	for _, s := range all {
		if seen[s] {
			t.Fatalf("duplicate status value %d", s)
		}
		seen[s] = true
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/ -run TestStatusInactiveDistinct`
Expected: FAIL — `undefined: source.StatusInactive`.

- [ ] **Step 3: Add the enum value**

In `internal/source/source.go`, extend the `Status` const block:

```go
const (
	StatusPending Status = iota
	StatusSuccess
	StatusFailure
	StatusInactive
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/source/ -run TestStatusInactiveDistinct`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/source/source.go internal/source/source_test.go
git commit -m "$(printf 'feat(source): add StatusInactive for preview teardown\n\nPart of #32.\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 2: store — preview deployments keyed by (app, PR)

**Files:**
- Modify: `internal/store/store.go` (`Deployment` struct, `migrate`, `LatestRunning`; add two methods)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `Deployment.PR int` field.
  - `(*Store).CreatePreviewDeployment(app string, pr int, imageID, containerID string, hostPort int, status string) (Deployment, error)`
  - `(*Store).PreviewRunning(app string, pr int) (Deployment, error)` — returns `ErrNotFound` when none.
  - `LatestRunning` now returns only main (`pr=0`) deployments.

- [ ] **Step 1: Write the failing tests**

Add to `internal/store/store_test.go`:

```go
func TestPreviewDeploymentRoundTrip(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreatePreviewDeployment("blog", 7, "img", "cid", 41000, "running"); err != nil {
		t.Fatalf("CreatePreviewDeployment: %v", err)
	}
	got, err := s.PreviewRunning("blog", 7)
	if err != nil {
		t.Fatalf("PreviewRunning: %v", err)
	}
	if got.PR != 7 || got.ContainerID != "cid" || got.HostPort != 41000 {
		t.Errorf("got %+v", got)
	}
	if _, err := s.PreviewRunning("blog", 8); !errors.Is(err, ErrNotFound) {
		t.Errorf("PreviewRunning(missing) err = %v, want ErrNotFound", err)
	}
}

func TestLatestRunningIgnoresPreviews(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateDeployment("blog", "img", "main-c", 40000, "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePreviewDeployment("blog", 3, "img", "preview-c", 41000, "running"); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ContainerID != "main-c" {
		t.Errorf("LatestRunning returned %q, want main-c", got.ContainerID)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestPreviewDeploymentRoundTrip|TestLatestRunningIgnoresPreviews'`
Expected: FAIL — `s.CreatePreviewDeployment undefined`.

- [ ] **Step 3: Implement store changes**

In `internal/store/store.go`, add `PR` to the `Deployment` struct (after `App`):

```go
type Deployment struct {
	ID          string
	App         string
	PR          int
	ImageID     string
	ContainerID string
	HostPort    int
	Status      string
	CreatedAt   time.Time
}
```

Add the `pr` column to the `migrate` statement slice (after the branch line):

```go
		`ALTER TABLE apps ADD COLUMN branch TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN pr INTEGER NOT NULL DEFAULT 0`,
```

Scope `LatestRunning` to main deployments — change its query's WHERE clause:

```go
		`SELECT id, app, image_id, container_id, host_port, status, created_at
		 FROM deployments WHERE app=? AND status='running' AND pr=0
		 ORDER BY created_at DESC LIMIT 1`, app).
```

Append the two new methods at the end of the file:

```go
func (s *Store) CreatePreviewDeployment(app string, pr int, imageID, containerID string, hostPort int, status string) (Deployment, error) {
	d := Deployment{
		ID: uuid.NewString(), App: app, PR: pr, ImageID: imageID,
		ContainerID: containerID, HostPort: hostPort, Status: status,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO deployments(id, app, image_id, container_id, host_port, status, created_at, pr)
		 VALUES(?,?,?,?,?,?,?,?)`,
		d.ID, d.App, d.ImageID, d.ContainerID, d.HostPort, d.Status,
		d.CreatedAt.Format(time.RFC3339Nano), d.PR)
	if err != nil {
		return Deployment{}, err
	}
	return d, nil
}

func (s *Store) PreviewRunning(app string, pr int) (Deployment, error) {
	var d Deployment
	var ts string
	err := s.db.QueryRow(
		`SELECT id, app, image_id, container_id, host_port, status, created_at, pr
		 FROM deployments WHERE app=? AND pr=? AND status='running'
		 ORDER BY created_at DESC LIMIT 1`, app, pr).
		Scan(&d.ID, &d.App, &d.ImageID, &d.ContainerID, &d.HostPort, &d.Status, &ts, &d.PR)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return d, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -race`
Expected: PASS (all store tests, including pre-existing).

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "$(printf 'feat(store): persist preview deployments keyed by (app, pr)\n\nPart of #32. Adds a pr column, CreatePreviewDeployment and PreviewRunning,\nand scopes LatestRunning to main (pr=0) deployments so preview containers\nare never retired as the main one.\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 3: deploy — `DeployPreview` / `TeardownPreview`

**Files:**
- Modify: `internal/deploy/deploy.go` (extract `buildRunHealthy`; add `hostForPreview`, `DeployPreview`, `TeardownPreview`)
- Test: `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: `store.CreatePreviewDeployment`, `store.PreviewRunning` (Task 2); `runtime.Runtime`; `RouteSetter`.
- Produces:
  - `(*Deployer).DeployPreview(ctx context.Context, appName string, pr int, srcDir string) (store.Deployment, error)`
  - `(*Deployer).TeardownPreview(ctx context.Context, appName string, pr int) error`

- [ ] **Step 1: Write the failing tests**

Add to `internal/deploy/deploy_test.go` (extend `fakeCaddy` to record removes first, then the tests):

```go
func (f *fakeCaddy) removed() []string { return f.removes }
```

At the top, replace the existing `fakeCaddy` type/constructor/`RemoveRoute` with a version that records removes:

```go
type fakeCaddy struct {
	upserts map[string]int
	removes []string
}

func newFakeCaddy() *fakeCaddy {
	return &fakeCaddy{upserts: make(map[string]int)}
}

func (f *fakeCaddy) UpsertRoute(host string, port int) error {
	f.upserts[host] = port
	return nil
}

func (f *fakeCaddy) RemoveRoute(host string) error {
	f.removes = append(f.removes, host)
	return nil
}
```

Then add the tests:

```go
func TestDeployPreviewRoutesFlattenedHostAndKeepsMain(t *testing.T) {
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

	if routes.upserts["pr-5-blog.piper.localhost"] != 40002 {
		t.Errorf("routes = %+v, want pr-5-blog.piper.localhost -> 40002", routes.upserts)
	}
	if len(rt.Stopped) != 0 {
		t.Errorf("stopped = %v, want none (main must survive)", rt.Stopped)
	}
	main, err := s.LatestRunning("blog")
	if err != nil || main.ContainerID != "main-c" {
		t.Errorf("main running = %+v (err %v), want main-c", main, err)
	}
}

func TestDeployPreviewSecondStopsPreviousPreview(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "p1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("first DeployPreview: %v", err)
	}
	rt.RunResultVal = runtime.RunResult{ContainerID: "p2", HostPort: 40002}
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("second DeployPreview: %v", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "p1" {
		t.Errorf("stopped = %v, want [p1]", rt.Stopped)
	}
}

func TestTeardownPreviewStopsAndUnroutes(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "p1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("DeployPreview: %v", err)
	}

	if err := d.TeardownPreview(context.Background(), "blog", 5); err != nil {
		t.Fatalf("TeardownPreview: %v", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "p1" {
		t.Errorf("stopped = %v, want [p1]", rt.Stopped)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != "pr-5-blog.piper.localhost" {
		t.Errorf("removed = %v, want [pr-5-blog.piper.localhost]", routes.removed())
	}
	if _, err := s.PreviewRunning("blog", 5); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PreviewRunning after teardown err = %v, want ErrNotFound", err)
	}
}

func TestTeardownPreviewNoRunningIsNoOp(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if err := d.TeardownPreview(context.Background(), "blog", 99); err != nil {
		t.Fatalf("TeardownPreview no-op err = %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/deploy/ -run 'Preview'`
Expected: FAIL — `d.DeployPreview undefined`.

- [ ] **Step 3: Refactor `Deploy` and add preview methods**

In `internal/deploy/deploy.go`, add the preview host helper next to `hostFor`:

```go
func (d *Deployer) hostForPreview(app string, pr int) string {
	return fmt.Sprintf("pr-%d-%s.%s", pr, app, d.baseDom)
}
```

Extract the shared build/run/health core (add this method):

```go
// buildRunHealthy builds, runs, and health-checks app.  On failure it invokes
// recordFailed with whatever ids are known so the caller persists a "failed"
// record for the right (app, pr) row, then returns a wrapped error.
func (d *Deployer) buildRunHealthy(ctx context.Context, app store.App, srcDir string, recordFailed func(imageID, containerID string, hostPort int)) (runtime.BuildResult, runtime.RunResult, error) {
	tag := fmt.Sprintf("piper/%s:%d", app.Name, time.Now().Unix())
	build, err := d.runtime.Build(ctx, srcDir, tag)
	if err != nil {
		recordFailed(build.ImageID, "", 0)
		return build, runtime.RunResult{}, fmt.Errorf("build: %w", err)
	}
	run, err := d.runtime.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		recordFailed(build.ImageID, run.ContainerID, run.HostPort)
		return build, run, fmt.Errorf("run: %w", err)
	}
	if err := d.runtime.WaitHealthy(ctx, run.HostPort); err != nil {
		_ = d.runtime.Stop(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort)
		return build, run, fmt.Errorf("health: %w", err)
	}
	return build, run, nil
}
```

Replace the body of `Deploy` (from the `tag :=` line through the `WaitHealthy` block) so it uses the helper. The full `Deploy` becomes:

```go
func (d *Deployer) Deploy(ctx context.Context, appName, srcDir string) (store.Deployment, error) {
	app, err := d.store.GetApp(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	previous, err := d.store.LatestRunning(appName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Deployment{}, err
	}

	build, run, err := d.buildRunHealthy(ctx, app, srcDir, func(img, cid string, hp int) {
		_, _ = d.store.CreateDeployment(appName, img, cid, hp, "failed")
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreateDeployment(appName, build.ImageID, run.ContainerID, run.HostPort, "running")
	if err != nil {
		return store.Deployment{}, err
	}
	if err := d.routes.UpsertRoute(d.hostFor(appName), run.HostPort); err != nil {
		return store.Deployment{}, fmt.Errorf("route: %w", err)
	}
	if previous.ContainerID != "" && previous.ContainerID != run.ContainerID {
		_ = d.runtime.Stop(ctx, previous.ContainerID)
		_ = d.store.UpdateDeploymentStatus(previous.ID, "stopped")
	}
	return dep, nil
}
```

Append the two preview methods:

```go
func (d *Deployer) DeployPreview(ctx context.Context, appName string, pr int, srcDir string) (store.Deployment, error) {
	app, err := d.store.GetApp(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	previous, err := d.store.PreviewRunning(appName, pr)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Deployment{}, err
	}

	build, run, err := d.buildRunHealthy(ctx, app, srcDir, func(img, cid string, hp int) {
		_, _ = d.store.CreatePreviewDeployment(appName, pr, img, cid, hp, "failed")
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreatePreviewDeployment(appName, pr, build.ImageID, run.ContainerID, run.HostPort, "running")
	if err != nil {
		return store.Deployment{}, err
	}
	if err := d.routes.UpsertRoute(d.hostForPreview(appName, pr), run.HostPort); err != nil {
		return store.Deployment{}, fmt.Errorf("route: %w", err)
	}
	if previous.ContainerID != "" && previous.ContainerID != run.ContainerID {
		_ = d.runtime.Stop(ctx, previous.ContainerID)
		_ = d.store.UpdateDeploymentStatus(previous.ID, "stopped")
	}
	return dep, nil
}

func (d *Deployer) TeardownPreview(ctx context.Context, appName string, pr int) error {
	dep, err := d.store.PreviewRunning(appName, pr)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = d.runtime.Stop(ctx, dep.ContainerID)
	if err := d.routes.RemoveRoute(d.hostForPreview(appName, pr)); err != nil {
		return fmt.Errorf("unroute: %w", err)
	}
	return d.store.UpdateDeploymentStatus(dep.ID, "stopped")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/deploy/ -race`
Expected: PASS (new preview tests + all pre-existing `Deploy` tests unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/deploy/deploy.go internal/deploy/deploy_test.go
git commit -m "$(printf 'feat(deploy): DeployPreview and TeardownPreview for PR previews\n\nPart of #32. Extracts a shared buildRunHealthy core; previews route the\nflattened pr-N-app.base host and retire only the prior preview for the\nsame (app, pr), never the app main deployment.\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 4: github parse — map `pull_request` events

**Files:**
- Modify: `internal/source/github/parse.go`
- Create: `internal/source/github/testdata/pr_opened.json`, `pr_synchronize.json`, `pr_closed.json`
- Test: `internal/source/github/parse_test.go`

**Interfaces:**
- Consumes: `source.KindPROpened/KindPRSynced/KindPRClosed`, `source.Event.PR` (existing).
- Produces: `Parse` returns PR events with `Kind`, `PR`, `SHA` (head), `Ref` (head ref) populated.

- [ ] **Step 1: Create the fixtures**

`internal/source/github/testdata/pr_opened.json`:

```json
{
  "action": "opened",
  "number": 42,
  "pull_request": { "head": { "ref": "feature-x", "sha": "prsha42" } },
  "repository": { "full_name": "alice/blog" },
  "installation": { "id": 99 }
}
```

`internal/source/github/testdata/pr_synchronize.json`:

```json
{
  "action": "synchronize",
  "number": 42,
  "pull_request": { "head": { "ref": "feature-x", "sha": "prsha43" } },
  "repository": { "full_name": "alice/blog" },
  "installation": { "id": 99 }
}
```

`internal/source/github/testdata/pr_closed.json`:

```json
{
  "action": "closed",
  "number": 42,
  "pull_request": { "head": { "ref": "feature-x", "sha": "prsha43" } },
  "repository": { "full_name": "alice/blog" },
  "installation": { "id": 99 }
}
```

- [ ] **Step 2: Write the failing test**

Add to `internal/source/github/parse_test.go`:

```go
func TestParsePullRequest(t *testing.T) {
	cases := []struct {
		file     string
		wantKind source.Kind
		wantSHA  string
	}{
		{"testdata/pr_opened.json", source.KindPROpened, "prsha42"},
		{"testdata/pr_synchronize.json", source.KindPRSynced, "prsha43"},
		{"testdata/pr_closed.json", source.KindPRClosed, "prsha43"},
	}
	p := newTestProvider(t, "s3cr3t")
	for _, c := range cases {
		body, _ := os.ReadFile(c.file)
		h := http.Header{}
		h.Set("X-GitHub-Event", "pull_request")
		h.Set("X-Hub-Signature-256", sign("s3cr3t", string(body)))
		ev, err := p.Parse(h, body)
		if err != nil {
			t.Fatalf("%s: Parse: %v", c.file, err)
		}
		want := source.Event{
			Repo: "alice/blog", Ref: "feature-x", SHA: c.wantSHA,
			Kind: c.wantKind, PR: 42, InstallationID: 99,
		}
		if ev != want {
			t.Fatalf("%s: got %+v want %+v", c.file, ev, want)
		}
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/source/github/ -run TestParsePullRequest`
Expected: FAIL — event has `Kind: KindOther`, empty `PR`/`SHA`.

- [ ] **Step 4: Implement the parse mapping**

In `internal/source/github/parse.go`, extend the `payload` struct with the PR fields:

```go
	var payload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Action     string `json:"action"`
		Number     int    `json:"number"`
		PullRequest struct {
			Head struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
```

Add a `case "pull_request"` to the event switch (before `default`):

```go
	case "pull_request":
		ev.PR = payload.Number
		ev.Ref = payload.PullRequest.Head.Ref
		ev.SHA = payload.PullRequest.Head.SHA
		switch payload.Action {
		case "opened", "reopened":
			ev.Kind = source.KindPROpened
		case "synchronize":
			ev.Kind = source.KindPRSynced
		case "closed":
			ev.Kind = source.KindPRClosed
		default:
			ev.Kind = source.KindOther
		}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/source/github/ -run 'TestParse' -race`
Expected: PASS (push/ping/bad-signature tests unaffected).

- [ ] **Step 6: Commit**

```bash
git add internal/source/github/parse.go internal/source/github/parse_test.go internal/source/github/testdata/pr_*.json
git commit -m "$(printf 'feat(source/github): parse pull_request webhook events\n\nPart of #32. Maps opened/reopened/synchronize/closed to the PR kinds and\npopulates Event.PR plus head ref/sha.\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 5: github report — per-PR environment + inactive status

**Files:**
- Modify: `internal/source/github/report.go`
- Test: `internal/source/github/report_test.go`

**Interfaces:**
- Consumes: `source.StatusInactive` (Task 1); `Event.PR` (Task 4).
- Produces: `Report` uses `environment: "pr-<N>"` + `transient_environment: true` when `ev.PR > 0`, and posts state `"inactive"` for `StatusInactive`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/source/github/report_test.go`:

```go
func TestReportPendingUsesPREnvironment(t *testing.T) {
	var gotEnv string
	var gotTransient bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case "/repos/alice/blog/deployments":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			gotEnv, _ = body["environment"].(string)
			gotTransient, _ = body["transient_environment"].(bool)
			w.WriteHeader(201)
			io.WriteString(w, `{"id":1}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	ev := source.Event{Repo: "alice/blog", SHA: "sha1", InstallationID: 99, PR: 42}
	if err := p.Report(context.Background(), ev, source.StatusPending, ""); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotEnv != "pr-42" || !gotTransient {
		t.Fatalf("environment=%q transient=%v, want pr-42/true", gotEnv, gotTransient)
	}
}

func TestReportInactivePostsInactiveState(t *testing.T) {
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case r.URL.Path == "/repos/alice/blog/deployments" && r.Method == http.MethodGet:
			io.WriteString(w, `[{"id":555}]`)
		case r.URL.Path == "/repos/alice/blog/deployments/555/statuses":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			gotState, _ = body["state"].(string)
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	ev := source.Event{Repo: "alice/blog", SHA: "sha1", InstallationID: 99, PR: 42}
	if err := p.Report(context.Background(), ev, source.StatusInactive, ""); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotState != "inactive" {
		t.Fatalf("state=%q, want inactive", gotState)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/source/github/ -run 'TestReportPendingUsesPREnvironment|TestReportInactivePostsInactiveState'`
Expected: FAIL — environment is `"production"`; `StatusInactive` posts `"failure"`.

- [ ] **Step 3: Implement report changes**

In `internal/source/github/report.go`, update `createDeployment` to override the environment for PRs (leave production payload otherwise identical):

```go
func (p *Provider) createDeployment(ctx context.Context, token string, ev source.Event) (int64, error) {
	in := map[string]any{
		"ref":               ev.SHA,
		"environment":       "production",
		"auto_merge":        false,
		"required_contexts": []string{},
		"description":       "piper deploy",
	}
	if ev.PR > 0 {
		in["environment"] = fmt.Sprintf("pr-%d", ev.PR)
		in["transient_environment"] = true
	}
	var out struct {
		ID int64 `json:"id"`
	}
	err := p.do(ctx, http.MethodPost, p.apiBase+"/repos/"+ev.Repo+"/deployments", token, in, &out)
	return out.ID, err
}
```

Update `Report` to translate `StatusInactive` to the `"inactive"` state:

```go
	state := "failure"
	switch status {
	case source.StatusSuccess:
		state = "success"
	case source.StatusInactive:
		state = "inactive"
	}
	return p.postStatus(ctx, token, ev.Repo, id, state, url)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/source/github/ -run TestReport -race`
Expected: PASS (existing pending/success report tests still pass — production payload unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/source/github/report.go internal/source/github/report_test.go
git commit -m "$(printf 'feat(source/github): per-PR deploy environment + inactive status\n\nPart of #32. PR deployments use environment pr-N with transient_environment\nso GitHub auto-inactivates superseded previews; StatusInactive posts the\ninactive state on close.\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 6: webhook — route PR events to preview deploy/teardown

**Files:**
- Modify: `internal/webhook/webhook.go` (`Deployer` interface; split `process`)
- Test: `internal/webhook/webhook_test.go` (extend `fakeDeployer`; add PR tests)

**Interfaces:**
- Consumes: `deploy.DeployPreview`/`deploy.TeardownPreview` (Task 3) via the interface; `source.KindPR*`, `source.StatusInactive`; `store.AppByRepo`.
- Produces: PR-opened/synced → `DeployPreview` + success report at `https://pr-<N>-<app>.<base>`; PR-closed → `TeardownPreview` + inactive report.

- [ ] **Step 1: Write the failing tests**

In `internal/webhook/webhook_test.go`, extend `fakeDeployer` with the new methods and counters:

```go
type fakeDeployer struct {
	mu            sync.Mutex
	calls         int
	previewCalls  int
	teardownCalls int
	err           error
}

func (d *fakeDeployer) Deploy(context.Context, string, string) (store.Deployment, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return store.Deployment{}, d.err
}
func (d *fakeDeployer) DeployPreview(context.Context, string, int, string) (store.Deployment, error) {
	d.mu.Lock()
	d.previewCalls++
	d.mu.Unlock()
	return store.Deployment{}, d.err
}
func (d *fakeDeployer) TeardownPreview(context.Context, string, int) error {
	d.mu.Lock()
	d.teardownCalls++
	d.mu.Unlock()
	return d.err
}
func (d *fakeDeployer) count() int         { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }
func (d *fakeDeployer) previews() int      { d.mu.Lock(); defer d.mu.Unlock(); return d.previewCalls }
func (d *fakeDeployer) teardowns() int     { d.mu.Lock(); defer d.mu.Unlock(); return d.teardownCalls }
```

Add the tests:

```go
func TestPROpenedDeploysPreviewAndReports(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPROpened, Repo: "alice/blog", PR: 7, SHA: "s1", Ref: "feature",
	}}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")

	if rec := post(h); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.previews() != 1 || d.count() != 0 {
		t.Fatalf("previews=%d deploys=%d", d.previews(), d.count())
	}
	got := p.statuses()
	if len(got) != 2 || got[0] != source.StatusPending || got[1] != source.StatusSuccess {
		t.Fatalf("statuses = %v", got)
	}
}

func TestPRSyncedIsIdempotentOnSHA(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	ev := source.Event{Kind: source.KindPRSynced, Repo: "alice/blog", PR: 7, SHA: "s1"}
	p := &fakeProvider{ev: ev}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	post(h) // same SHA again
	h.Wait()
	if d.previews() != 1 {
		t.Fatalf("previews = %d, want 1 (dedupe)", d.previews())
	}
}

func TestPRClosedTearsDownAndReportsInactive(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPRClosed, Repo: "alice/blog", PR: 7, SHA: "s1",
	}}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	if d.teardowns() != 1 {
		t.Fatalf("teardowns = %d, want 1", d.teardowns())
	}
	got := p.statuses()
	if len(got) != 1 || got[0] != source.StatusInactive {
		t.Fatalf("statuses = %v, want [inactive]", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/webhook/ -run 'TestPR'`
Expected: FAIL — `fakeDeployer` does not implement the interface / `previews undefined` until impl lands; PR events currently no-op.

- [ ] **Step 3: Implement the webhook routing**

In `internal/webhook/webhook.go`, add `"fmt"` to the imports. Extend the `Deployer` interface:

```go
type Deployer interface {
	Deploy(ctx context.Context, app, srcDir string) (store.Deployment, error)
	DeployPreview(ctx context.Context, app string, pr int, srcDir string) (store.Deployment, error)
	TeardownPreview(ctx context.Context, app string, pr int) error
}
```

Replace `process` (the whole method) with a dispatcher plus three handlers. `processPush` is the old body minus the leading kind check:

```go
func (h *Handler) process(ctx context.Context, ev source.Event) {
	switch ev.Kind {
	case source.KindPush:
		h.processPush(ctx, ev)
	case source.KindPROpened, source.KindPRSynced:
		h.processPreview(ctx, ev)
	case source.KindPRClosed:
		h.processPRClosed(ctx, ev)
	}
}

func (h *Handler) processPush(ctx context.Context, ev source.Event) {
	app, err := h.store.AppByRepo(ev.Repo)
	if errors.Is(err, store.ErrNotFound) {
		log.Printf("webhook: no app bound to %s", ev.Repo)
		return
	}
	if err != nil {
		log.Printf("webhook: lookup %s: %v", ev.Repo, err)
		return
	}
	if ev.Ref != "refs/heads/"+app.Branch {
		log.Printf("webhook: %s ref %s != tracked %s", ev.Repo, ev.Ref, app.Branch)
		return
	}

	lock := h.appLock(app.Name)
	lock.Lock()
	defer lock.Unlock()

	h.mu.Lock()
	dup := h.lastSHA[app.Name] == ev.SHA
	h.mu.Unlock()
	if dup {
		log.Printf("webhook: %s already at %s, skipping", app.Name, ev.SHA)
		return
	}

	_ = h.prov.Report(ctx, ev, source.StatusPending, "")

	dir, err := os.MkdirTemp("", "piper-src-*")
	if err != nil {
		log.Printf("webhook: tmpdir: %v", err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	defer os.RemoveAll(dir)

	if err := h.prov.Fetch(ctx, ev, dir); err != nil {
		log.Printf("webhook: fetch %s@%s: %v", ev.Repo, ev.SHA, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	if _, err := h.deploy.Deploy(ctx, app.Name, dir); err != nil {
		log.Printf("webhook: deploy %s: %v", app.Name, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}

	url := "https://" + app.Name + "." + h.baseDom
	_ = h.prov.Report(ctx, ev, source.StatusSuccess, url)

	h.mu.Lock()
	h.lastSHA[app.Name] = ev.SHA
	h.mu.Unlock()
}

func (h *Handler) processPreview(ctx context.Context, ev source.Event) {
	app, err := h.store.AppByRepo(ev.Repo)
	if errors.Is(err, store.ErrNotFound) {
		log.Printf("webhook: no app bound to %s", ev.Repo)
		return
	}
	if err != nil {
		log.Printf("webhook: lookup %s: %v", ev.Repo, err)
		return
	}

	lock := h.appLock(app.Name)
	lock.Lock()
	defer lock.Unlock()

	key := fmt.Sprintf("%s#%d", app.Name, ev.PR)
	h.mu.Lock()
	dup := h.lastSHA[key] == ev.SHA
	h.mu.Unlock()
	if dup {
		log.Printf("webhook: %s PR %d already at %s, skipping", app.Name, ev.PR, ev.SHA)
		return
	}

	_ = h.prov.Report(ctx, ev, source.StatusPending, "")

	dir, err := os.MkdirTemp("", "piper-src-*")
	if err != nil {
		log.Printf("webhook: tmpdir: %v", err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	defer os.RemoveAll(dir)

	if err := h.prov.Fetch(ctx, ev, dir); err != nil {
		log.Printf("webhook: fetch %s@%s: %v", ev.Repo, ev.SHA, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	if _, err := h.deploy.DeployPreview(ctx, app.Name, ev.PR, dir); err != nil {
		log.Printf("webhook: preview deploy %s PR %d: %v", app.Name, ev.PR, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}

	url := fmt.Sprintf("https://pr-%d-%s.%s", ev.PR, app.Name, h.baseDom)
	_ = h.prov.Report(ctx, ev, source.StatusSuccess, url)

	h.mu.Lock()
	h.lastSHA[key] = ev.SHA
	h.mu.Unlock()
}

func (h *Handler) processPRClosed(ctx context.Context, ev source.Event) {
	app, err := h.store.AppByRepo(ev.Repo)
	if errors.Is(err, store.ErrNotFound) {
		log.Printf("webhook: no app bound to %s", ev.Repo)
		return
	}
	if err != nil {
		log.Printf("webhook: lookup %s: %v", ev.Repo, err)
		return
	}

	lock := h.appLock(app.Name)
	lock.Lock()
	defer lock.Unlock()

	if err := h.deploy.TeardownPreview(ctx, app.Name, ev.PR); err != nil {
		log.Printf("webhook: teardown %s PR %d: %v", app.Name, ev.PR, err)
		return
	}

	h.mu.Lock()
	delete(h.lastSHA, fmt.Sprintf("%s#%d", app.Name, ev.PR))
	h.mu.Unlock()

	_ = h.prov.Report(ctx, ev, source.StatusInactive, "")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/webhook/ -race`
Expected: PASS (new PR tests + all pre-existing push tests).

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/webhook.go internal/webhook/webhook_test.go
git commit -m "$(printf 'feat(webhook): route pull_request events to preview deploy/teardown\n\nPart of #32. PR opened/synced builds a preview at pr-N-app.base (deduped\non head sha); PR closed tears it down and reports the deployment inactive.\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 7: end-to-end integration — open → live → close → gone (no Docker)

**Files:**
- Create: `internal/webhook/integration_test.go`

**Interfaces:**
- Consumes: real `github.Provider`, `store.Store`, `deploy.Deployer` (with `runtime.FakeRuntime`), `webhook.Handler`; an `httptest` GitHub API stub.
- Produces: nothing (test only).

- [ ] **Step 1: Write the failing integration test**

Create `internal/webhook/integration_test.go`:

```go
package webhook_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/piperbox/piper/internal/deploy"
	"github.com/piperbox/piper/internal/runtime"
	"github.com/piperbox/piper/internal/source/github"
	"github.com/piperbox/piper/internal/store"
	"github.com/piperbox/piper/internal/webhook"
)

type recordingCaddy struct {
	mu      sync.Mutex
	routes  map[string]int
	removed map[string]bool
}

func newRecordingCaddy() *recordingCaddy {
	return &recordingCaddy{routes: map[string]int{}, removed: map[string]bool{}}
}
func (c *recordingCaddy) UpsertRoute(host string, port int) error {
	c.mu.Lock()
	c.routes[host] = port
	c.mu.Unlock()
	return nil
}
func (c *recordingCaddy) RemoveRoute(host string) error {
	c.mu.Lock()
	delete(c.routes, host)
	c.removed[host] = true
	c.mu.Unlock()
	return nil
}
func (c *recordingCaddy) has(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.routes[host]
	return ok
}
func (c *recordingCaddy) wasRemoved(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.removed[host]
}

func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

func emptyTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func postEvent(t *testing.T, h http.Handler, event, secret, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%s: code = %d", event, rec.Code)
	}
}

func TestPRPreviewLifecycleEndToEnd(t *testing.T) {
	const secret = "s3cr3t"

	// GitHub API stub: tokens, deployment create/list, statuses, tarball.
	// Status bodies are asserted at unit level (Tasks 5/6); here we only need
	// the API to succeed so the lifecycle runs end to end.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			io.WriteString(w, `{"token":"ghs_x"}`)
		case r.URL.Path == "/repos/alice/blog/deployments" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			io.WriteString(w, `{"id":555}`)
		case r.URL.Path == "/repos/alice/blog/deployments" && r.Method == http.MethodGet:
			io.WriteString(w, `[{"id":555}]`)
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "/tarball/"):
			w.Write(emptyTarball(t))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	prov, err := github.New(github.Config{
		AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: secret, APIBase: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")

	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img"},
		RunResultVal:   runtime.RunResult{ContainerID: "prev-c", HostPort: 40010},
	}
	caddy := newRecordingCaddy()
	dep := deploy.New(s, rt, caddy, "piper.localhost")
	h := webhook.New(prov, s, dep, "piper.localhost")

	const host = "pr-9-blog.piper.localhost"

	openBody := `{"action":"opened","number":9,"pull_request":{"head":{"ref":"feat","sha":"sha9"}},"repository":{"full_name":"alice/blog"},"installation":{"id":99}}`
	postEvent(t, h, "pull_request", secret, openBody)
	h.Wait()
	if !caddy.has(host) {
		t.Fatalf("after open, route %s missing; routes=%v", host, caddy.routes)
	}
	if _, err := s.PreviewRunning("blog", 9); err != nil {
		t.Fatalf("PreviewRunning after open: %v", err)
	}

	closeBody := `{"action":"closed","number":9,"pull_request":{"head":{"ref":"feat","sha":"sha9"}},"repository":{"full_name":"alice/blog"},"installation":{"id":99}}`
	postEvent(t, h, "pull_request", secret, closeBody)
	h.Wait()
	if caddy.has(host) || !caddy.wasRemoved(host) {
		t.Fatalf("after close, route %s should be removed; routes=%v removed=%v", host, caddy.routes, caddy.removed)
	}
	if _, err := s.PreviewRunning("blog", 9); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PreviewRunning after close err = %v, want ErrNotFound", err)
	}
}
```

> Note: `sign`, `testKeyPEM` already exist elsewhere in the repo but in *different packages* (`internal/source/github`), so they are redefined here in `webhook_test`. If a `sign` helper already exists in `webhook_test` (it does not today), reuse it instead of redefining.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webhook/ -run TestPRPreviewLifecycleEndToEnd`
Expected: FAIL first to compile/behaviour until Tasks 1–6 are merged; once they are, it should pass. If earlier tasks are already committed, expect PASS — in that case tighten by temporarily breaking teardown to confirm the assertion bites, then revert.

- [ ] **Step 3: Make it pass / clean up**

No new production code — this test exercises Tasks 1–6. Remove the unused `mu`/`statuses` scaffolding if `go vet` complains; simplify the statuses capture to only what is asserted (route + store state). Ensure `gofmt -l` reports nothing.

- [ ] **Step 4: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/integration_test.go
git commit -m "$(printf 'test(webhook): end-to-end PR preview lifecycle without Docker\n\nPart of #32. Drives a real github.Provider + deploy.Deployer (fake runtime)\nthrough open then close, asserting the route appears then is removed and the\npreview deployment record is retired.\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

### Task 8: green gates + PROGRESS.md

**Files:**
- Modify: `PROGRESS.md` (flip the PR-preview line to done)

**Interfaces:** none.

- [ ] **Step 1: Run the full gates**

Run: `make test`
Expected: PASS (Docker/e2e skip cleanly when absent).

Run: `make cross`
Expected: builds clean (`CGO_ENABLED=0 GOOS=linux GOARCH=arm64`).

Run: `gofmt -l internal/ cmd/`
Expected: no output.

- [ ] **Step 2: Update PROGRESS.md**

In `PROGRESS.md`, change the Plan 3 PR-preview line from:

```
- ⬜ PR-preview URLs + teardown (`pr-N.<app>.<base>`) — deferred behind the seam — [#32](https://github.com/piperbox/piper/issues/32)
```

to:

```
- ✅ PR-preview URLs + teardown (`pr-<N>-<app>.<base>`, flattened for the wildcard cert) — [#32](https://github.com/piperbox/piper/issues/32)
```

And update the `_Last updated_` line to note PR previews landed.

- [ ] **Step 3: Commit**

```bash
git add PROGRESS.md
git commit -m "$(printf 'docs: mark PR previews done in PROGRESS\n\nCloses #32.\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin ozykhan/pr-previews
gh pr create --base main --title "[deploy] PR-preview URLs + teardown" --body "$(printf 'Finishes Plan 3: opened/updated PRs get a live preview at pr-<N>-<app>.<base>; closing tears it down.\n\nPreview host is flattened to a single label so the existing *.<base> cert, DNS, and relay SNI cover it with no Plan-2 change (issue title said pr-N.<app>.<base>).\n\nCloses #32.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)')"
```

---

## Notes for the implementer

- **Cross-fork PRs are out of scope.** `Fetch` pulls the head SHA via the base repo's tarball endpoint, which only resolves for same-repo branch PRs. Do not add fork handling.
- **Do not** widen the cert or touch `internal/certs`, the relay, or DNS. The flattened host is the whole reason no cert change is needed.
- **Layering:** `deploy` must not import `internal/source`. The webhook passes `app string, pr int` — never a `source.Event` — into the deployer.
