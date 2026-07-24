# Piper Agent Core (LAN-only) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `piperd` (daemon) + `piper` (CLI) so a developer can run `piper deploy myapp --path .` against a repo with a Dockerfile and have it built, run as a container, health-checked, and served over HTTP on the LAN at `http://myapp.piper.localhost` — all state in SQLite. No relay, no tunnel, no git yet.

**Architecture:** A single Go module produces two binaries. `piperd` exposes a small HTTP control API on `127.0.0.1:8088`. On deploy it: builds an image from an uploaded source tarball via the Docker SDK, runs the container with its port published to an ephemeral host port, polls that host port until healthy, records the deployment in SQLite, then tells a managed **Caddy** subprocess (via its admin API on `127.0.0.1:2019`) to reverse-proxy `<app>.piper.localhost` → `127.0.0.1:<hostport>`, and finally stops the previous container. `piper` is a thin HTTP client to that API.

**Tech Stack:** Go 1.23 · Docker Engine SDK (`github.com/docker/docker`) · pure-Go SQLite (`modernc.org/sqlite`, **no cgo** so we can cross-compile to arm64/armv7 for a Pi) · Caddy 2 as a managed subprocess driven by its JSON admin API · stdlib `net/http` (Go 1.22+ method+path routing, no router dependency) · `go test`.

## Global Constraints

- **Go module path:** `github.com/piperbox/piper` (GitHub org is `getpiper`). Copy verbatim.
- **No cgo.** All builds must pass with `CGO_ENABLED=0`. This forbids cgo-based SQLite drivers — use `modernc.org/sqlite` only. Cross-compile sanity: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...` must succeed.
- **Binary names:** daemon `piperd`, CLI `piper`. Built to `bin/`.
- **Base domain (Plan 1 default):** `piper.localhost`. App hostname = `<app-name>.piper.localhost`. `*.localhost` resolves to `127.0.0.1` on macOS/Linux with no DNS setup.
- **Control API address (default):** `127.0.0.1:8088`. Override via `PIPER_API_ADDR` (daemon) / `PIPER_ADDR` (CLI, full URL e.g. `http://127.0.0.1:8088`).
- **Caddy admin API (default):** `http://127.0.0.1:2019`. Caddy HTTP listener: `:80`.
- **App container port default:** `8080`. The app inside the container must listen on this port (later overridable per app). `PORT` env is injected into the container set to this value.
- **Deployment status values (exact strings):** `"building"`, `"running"`, `"failed"`, `"stopped"`.
- **Commits:** one per task step as indicated; conventional-commit style (`feat:`, `test:`, `chore:`). Follow repo commit conventions for trailers.
- **This is Plan 1 of 3.** Plan 2 = relay + outbound tunnel + DNS-01 TLS + managed wildcard. Plan 3 = GitHub webhook + PR-preview URLs + teardown. Do **not** build those here. Design: `docs/superpowers/specs/2026-07-04-piper-design.md`.

---

## File Structure

```
go.mod                              module github.com/piperbox/piper
go.sum
Makefile                            build/test/cross-compile shortcuts
cmd/piperd/main.go                  daemon entrypoint: wire config→store→docker→caddy→deploy→api
cmd/piper/main.go                   CLI entrypoint: subcommand dispatch (create/deploy/list)
internal/config/config.go           Config struct + Load() from env/flags with defaults
internal/config/config_test.go
internal/store/store.go             SQLite: App, Deployment types + Store methods
internal/store/store_test.go
internal/store/schema.sql           embedded DDL
internal/runtime/runtime.go         Runtime interface + BuildResult/RunResult types
internal/runtime/docker.go          DockerRuntime: real implementation over Docker SDK
internal/runtime/docker_test.go     integration test, skipped when Docker absent
internal/runtime/fake.go            FakeRuntime for deployer unit tests
internal/caddy/client.go            Caddy admin-API client: UpsertRoute/RemoveRoute
internal/caddy/client_test.go       httptest mock of the admin API
internal/caddy/manager.go           start/stop the Caddy subprocess with a base config
internal/deploy/deploy.go           Deployer: orchestrates build→run→health→record→route→stop-old
internal/deploy/deploy_test.go      unit test with FakeRuntime + fake caddy + temp Store
internal/api/api.go                 http.Handler: /v1/apps, /v1/apps/{name}/deploy, ...
internal/api/api_test.go            httptest-driven handler tests
internal/client/client.go          CLI's HTTP client to piperd + source tar helper
internal/client/client_test.go
test/e2e/deploy_test.go             end-to-end: real Docker + Caddy, deploy sample app, curl it
test/e2e/sampleapp/                 tiny Dockerfile app used by e2e
```

**Boundaries:** `store` knows only persistence. `runtime` knows only Docker. `caddy` knows only Caddy's admin API. `deploy` orchestrates the three but talks to them through interfaces (so it unit-tests with fakes). `api` is transport over `deploy`+`store`. `client` is the CLI's view of `api`. Nothing imports "up".

---

### Task 0: Module skeleton + `piper version`

Establishes the toolchain and proves `go test` works before any real logic.

**Files:**
- Create: `go.mod`, `Makefile`, `cmd/piper/main.go`, `internal/version/version.go`, `internal/version/version_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `version.String() string` (returns the version constant, default `"0.0.0-dev"`).

- [ ] **Step 1: Init the module and skeleton**

Run:
```bash
cd /Users/fcanozkan/Documents/workspace/piper
go mod init github.com/piperbox/piper
go mod edit -go=1.23
mkdir -p cmd/piper cmd/piperd internal/version bin
```

- [ ] **Step 2: Write the failing test**

Create `internal/version/version_test.go`:
```go
package version

import "testing"

func TestStringNotEmpty(t *testing.T) {
	if String() == "" {
		t.Fatal("version.String() returned empty string")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/version/`
Expected: FAIL — build error, `undefined: String`.

- [ ] **Step 4: Implement**

Create `internal/version/version.go`:
```go
// Package version exposes the build version of Piper binaries.
package version

// value is overridable at build time via -ldflags "-X ...version.value=...".
var value = "0.0.0-dev"

// String returns the current build version.
func String() string { return value }
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/version/`
Expected: PASS (`ok  github.com/piperbox/piper/internal/version`).

- [ ] **Step 6: Add the CLI entrypoint with a `version` subcommand**

Create `cmd/piper/main.go`:
```go
package main

import (
	"fmt"
	"os"

	"github.com/piperbox/piper/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: piper <command> [args]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version.String())
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(2)
	}
}
```

- [ ] **Step 7: Add a Makefile**

Create `Makefile`:
```makefile
.PHONY: build test cross
build:
	CGO_ENABLED=0 go build -o bin/piperd ./cmd/piperd
	CGO_ENABLED=0 go build -o bin/piper  ./cmd/piper
test:
	go test ./...
cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /dev/null ./...
```

- [ ] **Step 8: Verify build, cross-compile, and CLI**

Run:
```bash
CGO_ENABLED=0 go build -o bin/piper ./cmd/piper && ./bin/piper version
make cross
```
Expected: prints `0.0.0-dev`; `make cross` exits 0 (proves no-cgo arm64 build works). Note: `cmd/piperd` is empty until Task 8, so also create a placeholder now.

Create `cmd/piperd/main.go`:
```go
package main

func main() {}
```

- [ ] **Step 9: Commit**

```bash
git add go.mod Makefile cmd internal/version
git commit -m "chore: module skeleton, version pkg, piper CLI stub"
```

---

### Task 1: Config

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Config struct { APIAddr string; DataDir string; BaseDomain string; CaddyAdmin string }`
  - `func Load() Config` — fills from env with the Global-Constraints defaults.

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go`:
```go
package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("PIPER_API_ADDR", "")
	t.Setenv("PIPER_DATA_DIR", "")
	c := Load()
	if c.APIAddr != "127.0.0.1:8088" {
		t.Errorf("APIAddr = %q, want 127.0.0.1:8088", c.APIAddr)
	}
	if c.BaseDomain != "piper.localhost" {
		t.Errorf("BaseDomain = %q, want piper.localhost", c.BaseDomain)
	}
	if c.CaddyAdmin != "http://127.0.0.1:2019" {
		t.Errorf("CaddyAdmin = %q", c.CaddyAdmin)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("PIPER_API_ADDR", "0.0.0.0:9000")
	if got := Load().APIAddr; got != "0.0.0.0:9000" {
		t.Errorf("APIAddr = %q, want 0.0.0.0:9000", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: Load`.

- [ ] **Step 3: Implement**

Create `internal/config/config.go`:
```go
// Package config loads piperd runtime configuration from the environment.
package config

import "os"

type Config struct {
	APIAddr    string // control API listen address
	DataDir    string // directory for the SQLite file
	BaseDomain string // apps served at <name>.<BaseDomain>
	CaddyAdmin string // Caddy admin API base URL
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load builds a Config from env vars, applying defaults.
func Load() Config {
	return Config{
		APIAddr:    env("PIPER_API_ADDR", "127.0.0.1:8088"),
		DataDir:    env("PIPER_DATA_DIR", "./data"),
		BaseDomain: env("PIPER_BASE_DOMAIN", "piper.localhost"),
		CaddyAdmin: env("PIPER_CADDY_ADMIN", "http://127.0.0.1:2019"),
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat: config loading from env with defaults"
```

---

### Task 2: Store (SQLite)

**Files:**
- Create: `internal/store/store.go`, `internal/store/schema.sql`, `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type App struct { Name string; Port int; CreatedAt time.Time }`
  - `type Deployment struct { ID, App, ImageID, ContainerID string; HostPort int; Status string; CreatedAt time.Time }`
  - `func Open(path string) (*Store, error)` — opens/creates the DB and applies schema.
  - `func (*Store) Close() error`
  - `func (*Store) CreateApp(name string, port int) (App, error)` — errors if name exists.
  - `func (*Store) GetApp(name string) (App, error)` — `ErrNotFound` if missing.
  - `func (*Store) ListApps() ([]App, error)`
  - `func (*Store) CreateDeployment(app, imageID, containerID string, hostPort int, status string) (Deployment, error)`
  - `func (*Store) UpdateDeploymentStatus(id, status string) error`
  - `func (*Store) LatestRunning(app string) (Deployment, error)` — most recent deployment with status `running`; `ErrNotFound` if none.
  - `var ErrNotFound = errors.New("not found")`

- [ ] **Step 1: Add the dependency**

Run: `go get modernc.org/sqlite@latest`

- [ ] **Step 2: Write the schema**

Create `internal/store/schema.sql`:
```sql
CREATE TABLE IF NOT EXISTS apps (
    name       TEXT PRIMARY KEY,
    port       INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS deployments (
    id           TEXT PRIMARY KEY,
    app          TEXT NOT NULL REFERENCES apps(name),
    image_id     TEXT NOT NULL,
    container_id TEXT NOT NULL,
    host_port    INTEGER NOT NULL,
    status       TEXT NOT NULL,
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deployments_app ON deployments(app, created_at);
```

- [ ] **Step 3: Write the failing test**

Create `internal/store/store_test.go`:
```go
package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndGetApp(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := s.GetApp("blog")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Name != "blog" || got.Port != 8080 {
		t.Errorf("got %+v", got)
	}
}

func TestGetAppNotFound(t *testing.T) {
	s := openTemp(t)
	if _, err := s.GetApp("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateAppDuplicate(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	if _, err := s.CreateApp("blog", 8080); err == nil {
		t.Error("expected error on duplicate app")
	}
}

func TestLatestRunning(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	if _, err := s.LatestRunning("blog"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty LatestRunning err = %v, want ErrNotFound", err)
	}
	d1, _ := s.CreateDeployment("blog", "img1", "c1", 40001, "running")
	s.CreateDeployment("blog", "img2", "c2", 40002, "failed")
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ID != d1.ID {
		t.Errorf("LatestRunning ID = %s, want %s", got.ID, d1.ID)
	}
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 5: Implement**

Create `internal/store/store.go`:
```go
// Package store persists Piper apps and deployments in SQLite (pure-Go driver).
package store

import (
	_ "embed"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

var ErrNotFound = errors.New("not found")

type App struct {
	Name      string
	Port      int
	CreatedAt time.Time
}

type Deployment struct {
	ID          string
	App         string
	ImageID     string
	ContainerID string
	HostPort    int
	Status      string
	CreatedAt   time.Time
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateApp(name string, port int) (App, error) {
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO apps(name, port, created_at) VALUES(?,?,?)`,
		name, port, now.Format(time.RFC3339Nano))
	if err != nil {
		return App{}, err
	}
	return App{Name: name, Port: port, CreatedAt: now}, nil
}

func (s *Store) GetApp(name string) (App, error) {
	var a App
	var ts string
	err := s.db.QueryRow(`SELECT name, port, created_at FROM apps WHERE name=?`, name).
		Scan(&a.Name, &a.Port, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return a, nil
}

func (s *Store) ListApps() ([]App, error) {
	rows, err := s.db.Query(`SELECT name, port, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		var ts string
		if err := rows.Scan(&a.Name, &a.Port, &ts); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) CreateDeployment(app, imageID, containerID string, hostPort int, status string) (Deployment, error) {
	d := Deployment{
		ID: uuid.NewString(), App: app, ImageID: imageID,
		ContainerID: containerID, HostPort: hostPort, Status: status,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO deployments(id, app, image_id, container_id, host_port, status, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		d.ID, d.App, d.ImageID, d.ContainerID, d.HostPort, d.Status,
		d.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Deployment{}, err
	}
	return d, nil
}

func (s *Store) UpdateDeploymentStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE deployments SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) LatestRunning(app string) (Deployment, error) {
	var d Deployment
	var ts string
	err := s.db.QueryRow(
		`SELECT id, app, image_id, container_id, host_port, status, created_at
		 FROM deployments WHERE app=? AND status='running'
		 ORDER BY created_at DESC LIMIT 1`, app).
		Scan(&d.ID, &d.App, &d.ImageID, &d.ContainerID, &d.HostPort, &d.Status, &ts)
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

- [ ] **Step 6: Add the uuid dependency and run tests**

Run:
```bash
go get github.com/google/uuid@latest
go test ./internal/store/
```
Expected: PASS.

- [ ] **Step 7: Confirm no-cgo still holds**

Run: `make cross`
Expected: exit 0 (modernc sqlite is pure Go).

- [ ] **Step 8: Commit**

```bash
git add internal/store go.mod go.sum
git commit -m "feat: SQLite store for apps and deployments"
```

---

### Task 3: Runtime interface + Docker driver + fake

**Files:**
- Create: `internal/runtime/runtime.go`, `internal/runtime/docker.go`, `internal/runtime/fake.go`, `internal/runtime/docker_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type BuildResult struct { ImageID string }`
  - `type RunResult struct { ContainerID string; HostPort int }`
  - `type Runtime interface {`
    - `Build(ctx context.Context, srcDir, imageTag string) (BuildResult, error)`
    - `Run(ctx context.Context, imageTag string, containerPort int, env map[string]string) (RunResult, error)`
    - `WaitHealthy(ctx context.Context, hostPort int) error` (polls TCP dial to `127.0.0.1:hostPort`, ~30s budget)
    - `Stop(ctx context.Context, containerID string) error`
    - `Logs(ctx context.Context, containerID string) (io.ReadCloser, error)`
    - `}`
  - `func NewDockerRuntime() (*DockerRuntime, error)` — implements `Runtime`.
  - `type FakeRuntime` — in-memory impl for unit tests, records calls; fields `BuildErr, RunResultVal, HealthErr error/val`, `Stopped []string`.

- [ ] **Step 1: Define the interface and shared types**

Create `internal/runtime/runtime.go`:
```go
// Package runtime builds and runs app containers.
package runtime

import (
	"context"
	"io"
)

type BuildResult struct{ ImageID string }
type RunResult struct {
	ContainerID string
	HostPort    int
}

type Runtime interface {
	Build(ctx context.Context, srcDir, imageTag string) (BuildResult, error)
	Run(ctx context.Context, imageTag string, containerPort int, env map[string]string) (RunResult, error)
	WaitHealthy(ctx context.Context, hostPort int) error
	Stop(ctx context.Context, containerID string) error
	Logs(ctx context.Context, containerID string) (io.ReadCloser, error)
}
```

- [ ] **Step 2: Write the fake (used by later tasks' unit tests)**

Create `internal/runtime/fake.go`:
```go
package runtime

import (
	"context"
	"io"
	"strings"
)

// FakeRuntime is an in-memory Runtime for unit tests.
type FakeRuntime struct {
	BuildResultVal BuildResult
	BuildErr       error
	RunResultVal   RunResult
	RunErr         error
	HealthErr      error
	Stopped        []string
}

func (f *FakeRuntime) Build(context.Context, string, string) (BuildResult, error) {
	return f.BuildResultVal, f.BuildErr
}
func (f *FakeRuntime) Run(context.Context, string, int, map[string]string) (RunResult, error) {
	return f.RunResultVal, f.RunErr
}
func (f *FakeRuntime) WaitHealthy(context.Context, int) error { return f.HealthErr }
func (f *FakeRuntime) Stop(_ context.Context, id string) error {
	f.Stopped = append(f.Stopped, id)
	return nil
}
func (f *FakeRuntime) Logs(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("fake logs\n")), nil
}
```

- [ ] **Step 3: Write the Docker driver**

Create `internal/runtime/docker.go`:
```go
package runtime

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/go-connections/nat"
	docker "github.com/docker/docker/client"
)

type DockerRuntime struct{ cli *docker.Client }

func NewDockerRuntime() (*DockerRuntime, error) {
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &DockerRuntime{cli: cli}, nil
}

func (d *DockerRuntime) Build(ctx context.Context, srcDir, imageTag string) (BuildResult, error) {
	tarball, err := archive.TarWithOptions(srcDir, &archive.TarOptions{})
	if err != nil {
		return BuildResult{}, err
	}
	defer tarball.Close()
	resp, err := d.cli.ImageBuild(ctx, tarball, buildOptions(imageTag))
	if err != nil {
		return BuildResult{}, err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil { // drain build log; surfaces build errors
		return BuildResult{}, err
	}
	insp, _, err := d.cli.ImageInspectWithRaw(ctx, imageTag)
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{ImageID: insp.ID}, nil
}

func (d *DockerRuntime) Run(ctx context.Context, imageTag string, containerPort int, env map[string]string) (RunResult, error) {
	port := nat.Port(fmt.Sprintf("%d/tcp", containerPort))
	var envv []string
	for k, v := range env {
		envv = append(envv, k+"="+v)
	}
	created, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image:        imageTag,
			Env:          envv,
			ExposedPorts: nat.PortSet{port: struct{}{}},
		},
		&container.HostConfig{
			PortBindings: nat.PortMap{port: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "0"}}},
		}, nil, nil, "")
	if err != nil {
		return RunResult{}, err
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return RunResult{}, err
	}
	insp, err := d.cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		return RunResult{}, err
	}
	bindings := insp.NetworkSettings.Ports[port]
	if len(bindings) == 0 {
		return RunResult{}, fmt.Errorf("no host port bound for %s", port)
	}
	hp, err := nat.ParsePort(bindings[0].HostPort)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{ContainerID: created.ID, HostPort: hp}, nil
}

func (d *DockerRuntime) WaitHealthy(ctx context.Context, hostPort int) error {
	deadline := time.Now().Add(30 * time.Second)
	addr := fmt.Sprintf("127.0.0.1:%d", hostPort)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("container did not become healthy on %s within 30s", addr)
}

func (d *DockerRuntime) Stop(ctx context.Context, containerID string) error {
	timeout := 10
	_ = d.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	return d.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

func (d *DockerRuntime) Logs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	return d.cli.ContainerLogs(ctx, containerID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "200"})
}

func buildOptions(tag string) buildOpts { return buildOpts{tag} }

// buildOpts adapts to the SDK's ImageBuildOptions without importing it at call sites.
type buildOpts struct{ tag string }

// helper kept minimal; real ImageBuildOptions constructed inline:
var _ = image.BuildResponse{} // ensure image import used across SDK versions

// tarFromDir is unused directly but documents expectation that srcDir contains a Dockerfile.
var _ = tar.Header{}
var _ = filepath.Join
var _ = os.Open
```

> **Note for the implementer:** the exact `ImageBuild` options type is `types.ImageBuildOptions{Tags: []string{tag}, Remove: true, Dockerfile: "Dockerfile"}` from `github.com/docker/docker/api/types`. Replace the `buildOptions`/`buildOpts` placeholder shim above with a direct construction of that struct once you run `go get` and see the resolved SDK version — the SDK's package split shifts across versions, so let the compiler guide the exact import path. This is the one spot to reconcile against the installed SDK.

- [ ] **Step 4: Add Docker SDK deps**

Run:
```bash
go get github.com/docker/docker/client@latest
go get github.com/docker/go-connections/nat@latest
go mod tidy
```
Then fix the `Build` options as noted (use `types.ImageBuildOptions`). Re-run `go build ./internal/runtime/` until it compiles.

- [ ] **Step 5: Write the integration test (skipped without Docker)**

Create `internal/runtime/docker_test.go`:
```go
package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func dockerAvailable(t *testing.T) *DockerRuntime {
	t.Helper()
	r, err := NewDockerRuntime()
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := r.cli.Ping(ctx); err != nil {
		t.Skipf("docker not reachable: %v", err)
	}
	return r
}

func TestDockerBuildRunHealthStop(t *testing.T) {
	r := dockerAvailable(t)
	dir := t.TempDir()
	// Minimal image: netcat that answers on :8080 so WaitHealthy's TCP dial succeeds.
	df := "FROM alpine:3.20\nRUN apk add --no-cache netcat-openbsd\n" +
		"CMD while true; do echo -e 'HTTP/1.1 200 OK\\r\\n\\r\\nhi' | nc -l -p 8080; done\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b, err := r.Build(ctx, dir, "piper-test:latest")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if b.ImageID == "" {
		t.Fatal("empty image id")
	}
	run, err := r.Run(ctx, "piper-test:latest", 8080, map[string]string{"PORT": "8080"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() { r.Stop(context.Background(), run.ContainerID) })
	if err := r.WaitHealthy(ctx, run.HostPort); err != nil {
		t.Fatalf("WaitHealthy: %v", err)
	}
}
```

- [ ] **Step 6: Run tests**

Run: `go test ./internal/runtime/`
Expected: PASS if Docker is running (build+run+health verified); SKIP with a clear message if not.

- [ ] **Step 7: Commit**

```bash
git add internal/runtime go.mod go.sum
git commit -m "feat: docker runtime (build/run/health/stop) + fake"
```

---

### Task 4: Caddy admin client

**Files:**
- Create: `internal/caddy/client.go`, `internal/caddy/client_test.go`, `internal/caddy/manager.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Client struct { ... }`; `func NewClient(adminBase string) *Client`
  - `func (*Client) UpsertRoute(host string, upstreamHostPort int) error` — idempotent; makes Caddy reverse-proxy `host` → `127.0.0.1:<port>` over HTTP. Uses a deterministic route `@id` of `"piper-" + host` so re-deploys replace cleanly.
  - `func (*Client) RemoveRoute(host string) error` — removes the route by id; no error if absent.
  - `func StartManager(ctx, adminBase, httpListen string) (*Manager, error)` + `(*Manager) Stop()` — launches `caddy run` with an initial admin-enabled config (used only by `piperd` main + e2e).

- [ ] **Step 1: Write the failing test (mock admin API)**

Create `internal/caddy/client_test.go`:
```go
package caddy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpsertRoutePutsRouteByID(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if err := c.UpsertRoute("blog.piper.localhost", 40001); err != nil {
		t.Fatalf("UpsertRoute: %v", err)
	}
	if !strings.Contains(gotPath, "piper-blog.piper.localhost") {
		t.Errorf("path = %q, want it to reference the route id", gotPath)
	}
	// Body must be valid JSON containing the upstream.
	var route map[string]any
	if err := json.Unmarshal([]byte(gotBody), &route); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, gotBody)
	}
	if !strings.Contains(gotBody, "127.0.0.1:40001") {
		t.Errorf("body missing upstream: %s", gotBody)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/caddy/`
Expected: FAIL — `undefined: NewClient`.

- [ ] **Step 3: Implement the client**

Create `internal/caddy/client.go`:
```go
// Package caddy drives a running Caddy instance through its JSON admin API.
package caddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

type Client struct {
	base string
	http *http.Client
}

func NewClient(adminBase string) *Client {
	return &Client{base: adminBase, http: &http.Client{}}
}

func routeID(host string) string { return "piper-" + host }

// UpsertRoute makes Caddy reverse-proxy `host` to 127.0.0.1:<port> over HTTP.
// It PUTs a route addressed by a stable @id so repeated calls replace in place.
func (c *Client) UpsertRoute(host string, upstreamHostPort int) error {
	route := map[string]any{
		"@id":   routeID(host),
		"match": []map[string]any{{"host": []string{host}}},
		"handle": []map[string]any{{
			"handler":   "reverse_proxy",
			"upstreams": []map[string]any{{"dial": fmt.Sprintf("127.0.0.1:%d", upstreamHostPort)}},
		}},
	}
	// Remove any existing route with this id (ignore 404), then append fresh.
	_ = c.RemoveRoute(host)
	body, _ := json.Marshal(route)
	// Append into the default server's routes list.
	url := c.base + "/config/apps/http/servers/piper/routes"
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("caddy upsert route: status %d", resp.StatusCode)
	}
	return nil
}

// RemoveRoute deletes the route addressed by the host's stable id.
func (c *Client) RemoveRoute(host string) error {
	url := c.base + "/id/" + routeID(host)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("caddy remove route: status %d", resp.StatusCode)
}
```

> **Note:** `UpsertRoute`'s POST target path assumes the base config already contains an HTTP server named `piper` with a `routes` array (created by `StartManager` in Step 5). The test only asserts the id-addressed PUT/DELETE behavior, so it passes independently.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/caddy/`
Expected: PASS. (The `RemoveRoute` DELETE hits the mock, returns 200; then POST hits it. The test asserts the append body/path.)

- [ ] **Step 5: Implement the manager (subprocess launcher)**

Create `internal/caddy/manager.go`:
```go
package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

type Manager struct{ cmd *exec.Cmd }

// StartManager launches `caddy run` with an admin-enabled base config that has
// one HTTP server named "piper" listening on httpListen with an empty routes list.
func StartManager(ctx context.Context, adminBase, httpListen string) (*Manager, error) {
	adminAddr := strings.TrimPrefix(adminBase, "http://")
	base := map[string]any{
		"admin": map[string]any{"listen": adminAddr},
		"apps": map[string]any{"http": map[string]any{"servers": map[string]any{
			"piper": map[string]any{"listen": []string{httpListen}, "routes": []any{}},
		}}},
	}
	cfg, _ := json.Marshal(base)
	cmd := exec.CommandContext(ctx, "caddy", "run", "--config", "-", "--adapter", "")
	cmd.Stdin = bytes.NewReader(cfg)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start caddy (is it installed?): %w", err)
	}
	m := &Manager{cmd: cmd}
	if err := waitAdmin(adminBase, 10*time.Second); err != nil {
		m.Stop()
		return nil, err
	}
	return m, nil
}

func waitAdmin(base string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/config/")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("caddy admin API not ready at %s", base)
}

func (m *Manager) Stop() {
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		_ = m.cmd.Wait()
	}
}
```

- [ ] **Step 6: Commit**

```bash
git add internal/caddy
git commit -m "feat: caddy admin client + subprocess manager"
```

---

### Task 5: Deployer (orchestrator)

**Files:**
- Create: `internal/deploy/deploy.go`, `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: `store.Store`, `runtime.Runtime`, and a caddy interface `RouteSetter` (satisfied by `*caddy.Client`).
- Produces:
  - `type RouteSetter interface { UpsertRoute(host string, upstreamHostPort int) error; RemoveRoute(host string) error }`
  - `type Deployer struct { ... }`; `func New(s *store.Store, rt runtime.Runtime, rs RouteSetter, baseDomain string) *Deployer`
  - `func (*Deployer) Deploy(ctx context.Context, appName, srcDir string) (store.Deployment, error)` — build→run→health→record(running)→route→stop previous running container. On build/run/health failure records a `failed` deployment and returns the error.
  - `func (*Deployer) hostFor(appName string) string` → `<appName>.<baseDomain>`.

- [ ] **Step 1: Write the failing test (fakes + temp store)**

Create `internal/deploy/deploy_test.go`:
```go
package deploy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/runtime"
	"github.com/piperbox/piper/internal/store"
)

type fakeCaddy struct {
	upserts map[string]int
	removes []string
}

func newFakeCaddy() *fakeCaddy { return &fakeCaddy{upserts: map[string]int{}} }
func (f *fakeCaddy) UpsertRoute(host string, port int) error {
	f.upserts[host] = port
	return nil
}
func (f *fakeCaddy) RemoveRoute(host string) error {
	f.removes = append(f.removes, host)
	return nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDeploySuccessRoutesAndRecords(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	fc := newFakeCaddy()
	d := New(s, rt, fc, "piper.localhost")

	dep, err := d.Deploy(context.Background(), "blog", t.TempDir())
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.Status != "running" {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if fc.upserts["blog.piper.localhost"] != 40001 {
		t.Errorf("route not set correctly: %+v", fc.upserts)
	}
	got, _ := s.LatestRunning("blog")
	if got.ContainerID != "c1" {
		t.Errorf("LatestRunning container = %q", got.ContainerID)
	}
}

func TestDeploySecondStopsPrevious() {
}

func TestDeployHealthFailureRecordsFailed(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      context.DeadlineExceeded,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected error on health failure")
	}
	if _, err := s.LatestRunning("blog"); err == nil {
		t.Fatal("expected no running deployment after health failure")
	}
}
```

> Replace the empty `TestDeploySecondStopsPrevious()` stub with the real body in Step 4 after the type compiles — kept as a named placeholder only so the file lists the intended cases.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/deploy/`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement the deployer**

Create `internal/deploy/deploy.go`:
```go
// Package deploy orchestrates building, running, health-checking, and routing an app.
package deploy

import (
	"context"
	"fmt"
	"time"

	"github.com/piperbox/piper/internal/runtime"
	"github.com/piperbox/piper/internal/store"
)

type RouteSetter interface {
	UpsertRoute(host string, upstreamHostPort int) error
	RemoveRoute(host string) error
}

type Deployer struct {
	store   *store.Store
	rt      runtime.Runtime
	routes  RouteSetter
	baseDom string
}

func New(s *store.Store, rt runtime.Runtime, rs RouteSetter, baseDomain string) *Deployer {
	return &Deployer{store: s, rt: rt, routes: rs, baseDom: baseDomain}
}

func (d *Deployer) hostFor(app string) string {
	return app + "." + d.baseDom
}

func (d *Deployer) Deploy(ctx context.Context, appName, srcDir string) (store.Deployment, error) {
	app, err := d.store.GetApp(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	previous, hadPrevious := d.store.LatestRunning(appName)
	_ = hadPrevious

	tag := fmt.Sprintf("piper/%s:%d", appName, time.Now().Unix())
	build, err := d.rt.Build(ctx, srcDir, tag)
	if err != nil {
		return store.Deployment{}, fmt.Errorf("build: %w", err)
	}
	run, err := d.rt.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		return store.Deployment{}, fmt.Errorf("run: %w", err)
	}
	if err := d.rt.WaitHealthy(ctx, run.HostPort); err != nil {
		_ = d.rt.Stop(ctx, run.ContainerID)
		d.store.CreateDeployment(appName, build.ImageID, run.ContainerID, run.HostPort, "failed")
		return store.Deployment{}, fmt.Errorf("health: %w", err)
	}
	dep, err := d.store.CreateDeployment(appName, build.ImageID, run.ContainerID, run.HostPort, "running")
	if err != nil {
		return store.Deployment{}, err
	}
	if err := d.routes.UpsertRoute(d.hostFor(appName), run.HostPort); err != nil {
		return store.Deployment{}, fmt.Errorf("route: %w", err)
	}
	// Retire the previous running container, if any.
	if previous.ContainerID != "" && previous.ContainerID != run.ContainerID {
		_ = d.rt.Stop(ctx, previous.ContainerID)
		_ = d.store.UpdateDeploymentStatus(previous.ID, "stopped")
	}
	return dep, nil
}
```

> Note: `LatestRunning` returns `ErrNotFound` when there is no previous deployment; the zero-value `Deployment{}.ContainerID == ""` guard above handles that case without special-casing the error.

- [ ] **Step 4: Fill in the second-deploy test**

Replace the `TestDeploySecondStopsPrevious()` stub in `deploy_test.go` with:
```go
func TestDeploySecondStopsPrevious(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	d.Deploy(context.Background(), "blog", t.TempDir())

	rt.RunResultVal = runtime.RunResult{ContainerID: "c2", HostPort: 40002}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("expected c1 stopped, got %v", rt.Stopped)
	}
	got, _ := s.LatestRunning("blog")
	if got.ContainerID != "c2" {
		t.Errorf("LatestRunning = %q, want c2", got.ContainerID)
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/deploy/`
Expected: PASS (all three cases).

- [ ] **Step 6: Commit**

```bash
git add internal/deploy
git commit -m "feat: deployer orchestrates build/run/health/route/retire"
```

---

### Task 6: Control-plane HTTP API

**Files:**
- Create: `internal/api/api.go`, `internal/api/api_test.go`

**Interfaces:**
- Consumes: `*store.Store`, and a `Deployerer` interface `{ Deploy(ctx, app, srcDir string) (store.Deployment, error) }` (satisfied by `*deploy.Deployer`).
- Produces:
  - `func New(s *store.Store, d Deployerer) http.Handler`
  - Routes:
    - `POST /v1/apps` — JSON `{"name":"blog","port":8080}` (port optional, default 8080) → 201 + app JSON. 409 if exists.
    - `GET /v1/apps` — 200 + `[]App` JSON.
    - `GET /v1/apps/{name}` — 200 + app JSON, 404 if missing.
    - `POST /v1/apps/{name}/deploy` — body is a `.tar` of the source (Content-Type `application/x-tar`); handler extracts it to a temp dir, calls `Deploy`, returns 200 + deployment JSON, 500 on failure.

- [ ] **Step 1: Write the failing test**

Create `internal/api/api_test.go`:
```go
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/store"
)

type fakeDeployer struct{ gotApp string }

func (f *fakeDeployer) Deploy(_ context.Context, app, _ string) (store.Deployment, error) {
	f.gotApp = app
	return store.Deployment{ID: "dep1", App: app, Status: "running", HostPort: 40001}, nil
}

func newTestServer(t *testing.T) (http.Handler, *store.Store, *fakeDeployer) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	fd := &fakeDeployer{}
	return New(s, fd), s, fd
}

func TestCreateAndListApp(t *testing.T) {
	h, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"blog","port":8080}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	var apps []store.App
	json.Unmarshal(rec.Body.Bytes(), &apps)
	if len(apps) != 1 || apps[0].Name != "blog" {
		t.Errorf("apps = %+v", apps)
	}
}

func TestCreateDuplicateReturns409(t *testing.T) {
	h, _, _ := newTestServer(t)
	body := `{"name":"blog","port":8080}`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement**

Create `internal/api/api.go`:
```go
// Package api exposes piperd's HTTP control plane.
package api

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/piperbox/piper/internal/store"
)

type Deployerer interface {
	Deploy(ctx context.Context, app, srcDir string) (store.Deployment, error)
}

func New(s *store.Store, d Deployerer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if in.Port == 0 {
			in.Port = 8080
		}
		if _, err := s.GetApp(in.Name); err == nil {
			http.Error(w, "app exists", http.StatusConflict)
			return
		}
		app, err := s.CreateApp(in.Name, in.Port)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, app)
	})

	mux.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		apps, err := s.ListApps()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if apps == nil {
			apps = []store.App{}
		}
		writeJSON(w, http.StatusOK, apps)
	})

	mux.HandleFunc("GET /v1/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		app, err := s.GetApp(r.PathValue("name"))
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, app)
	})

	mux.HandleFunc("POST /v1/apps/{name}/deploy", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, err := s.GetApp(name); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		}
		dir, err := os.MkdirTemp("", "piper-src-*")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.RemoveAll(dir)
		if err := untar(r.Body, dir); err != nil {
			http.Error(w, "bad tar: "+err.Error(), http.StatusBadRequest)
			return
		}
		dep, err := d.Deploy(r.Context(), name, dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, dep)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// untar extracts a tar stream into dir (used for uploaded source).
func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.Clean(hdr.Name))
		if !isWithin(dir, target) {
			return errors.New("tar entry escapes destination")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}

func isWithin(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && !startsWithDotDot(rel)
}

func startsWithDotDot(p string) bool {
	return p == ".." || len(p) >= 3 && p[:3] == ".."+string(filepath.Separator)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api/`
Expected: PASS (create 201, list returns the app, duplicate 409).

- [ ] **Step 5: Commit**

```bash
git add internal/api
git commit -m "feat: control-plane HTTP API (apps + deploy upload)"
```

---

### Task 7: CLI client + commands

**Files:**
- Create: `internal/client/client.go`, `internal/client/client_test.go`
- Modify: `cmd/piper/main.go` (add `create`, `deploy`, `list`)

**Interfaces:**
- Consumes: the HTTP API from Task 6.
- Produces:
  - `func New(base string) *Client` (base default from `PIPER_ADDR` or `http://127.0.0.1:8088`).
  - `func (*Client) CreateApp(name string, port int) error`
  - `func (*Client) ListApps() ([]store.App, error)`
  - `func (*Client) Deploy(name, srcDir string) (store.Deployment, error)` — tars `srcDir` and POSTs it.
  - `func TarDir(dir string, w io.Writer) error` — helper (also unit-tested).

- [ ] **Step 1: Write the failing test (round-trip tar + list against httptest)**

Create `internal/client/client_test.go`:
```go
package client

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTarDirRoundTrip(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o644)

	var buf bytes.Buffer
	if err := TarDir(dir, &buf); err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	tr := tar.NewReader(&buf)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(h.Name) == "Dockerfile" {
			found = true
		}
	}
	if !found {
		t.Error("Dockerfile not present in tar")
	}
}

func TestListApps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"Name":"blog","Port":8080}]`))
	}))
	defer srv.Close()
	apps, err := New(srv.URL).ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" {
		t.Errorf("apps = %+v", apps)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/`
Expected: FAIL — `undefined: TarDir`.

- [ ] **Step 3: Implement the client**

Create `internal/client/client.go`:
```go
// Package client is the piper CLI's HTTP client to piperd.
package client

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/piperbox/piper/internal/store"
)

type Client struct {
	base string
	http *http.Client
}

func New(base string) *Client {
	if base == "" {
		base = "http://127.0.0.1:8088"
	}
	return &Client{base: base, http: &http.Client{}}
}

func (c *Client) CreateApp(name string, port int) error {
	body, _ := json.Marshal(map[string]any{"name": name, "port": port})
	resp, err := c.http.Post(c.base+"/v1/apps", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create app: %s: %s", resp.Status, b)
	}
	return nil
}

func (c *Client) ListApps() ([]store.App, error) {
	resp, err := c.http.Get(c.base + "/v1/apps")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var apps []store.App
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, err
	}
	return apps, nil
}

func (c *Client) Deploy(name, srcDir string) (store.Deployment, error) {
	var buf bytes.Buffer
	if err := TarDir(srcDir, &buf); err != nil {
		return store.Deployment{}, err
	}
	resp, err := c.http.Post(c.base+"/v1/apps/"+name+"/deploy", "application/x-tar", &buf)
	if err != nil {
		return store.Deployment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return store.Deployment{}, fmt.Errorf("deploy: %s: %s", resp.Status, b)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		return store.Deployment{}, err
	}
	return dep, nil
}

// TarDir writes a tar of dir (relative paths) to w.
func TarDir(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/client/`
Expected: PASS.

- [ ] **Step 5: Wire the CLI subcommands**

Replace `cmd/piper/main.go` with:
```go
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/piperbox/piper/internal/client"
	"github.com/piperbox/piper/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	c := client.New(os.Getenv("PIPER_ADDR"))
	switch os.Args[1] {
	case "version":
		fmt.Println(version.String())
	case "create":
		fs := flag.NewFlagSet("create", flag.ExitOnError)
		port := fs.Int("port", 8080, "container port the app listens on")
		fs.Parse(os.Args[2:])
		if fs.NArg() < 1 {
			fatal("usage: piper create <name> [--port N]")
		}
		must(c.CreateApp(fs.Arg(0), *port))
		fmt.Printf("created app %q (port %d)\n", fs.Arg(0), *port)
	case "deploy":
		fs := flag.NewFlagSet("deploy", flag.ExitOnError)
		path := fs.String("path", ".", "source directory containing a Dockerfile")
		fs.Parse(os.Args[2:])
		if fs.NArg() < 1 {
			fatal("usage: piper deploy <name> [--path DIR]")
		}
		dep, err := c.Deploy(fs.Arg(0), *path)
		must(err)
		fmt.Printf("deployed %s: http://%s.piper.localhost  (%s)\n", fs.Arg(0), fs.Arg(0), dep.Status)
	case "list":
		apps, err := c.ListApps()
		must(err)
		for _, a := range apps {
			fmt.Printf("%s\tport=%d\n", a.Name, a.Port)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: piper <version|create|deploy|list> [args]")
	os.Exit(2)
}
func fatal(msg string) { fmt.Fprintln(os.Stderr, msg); os.Exit(2) }
func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 6: Build and run the whole test suite**

Run:
```bash
make build
go test ./...
```
Expected: build produces `bin/piper` and `bin/piperd`; all non-e2e tests PASS (Docker-gated ones SKIP if no Docker).

- [ ] **Step 7: Commit**

```bash
git add internal/client cmd/piper
git commit -m "feat: piper CLI (create/deploy/list) over HTTP client"
```

---

### Task 8: `piperd` wire-up + end-to-end test

**Files:**
- Modify: `cmd/piperd/main.go`
- Create: `test/e2e/deploy_test.go`, `test/e2e/sampleapp/Dockerfile`, `test/e2e/sampleapp/main.go` (or a shell app)

**Interfaces:**
- Consumes: everything above.
- Produces: a running `piperd` process; no exported Go API. The e2e test is the acceptance gate for Plan 1.

- [ ] **Step 1: Implement `piperd` main**

Replace `cmd/piperd/main.go` with:
```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/caddy"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/deploy"
	"github.com/piperbox/piper/internal/runtime"
	"github.com/piperbox/piper/internal/store"
)

func main() {
	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "piper.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	rt, err := runtime.NewDockerRuntime()
	if err != nil {
		log.Fatalf("docker: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Unless PIPER_SKIP_CADDY is set (e.g. a caddy is already running), manage one.
	if os.Getenv("PIPER_SKIP_CADDY") == "" {
		mgr, err := caddy.StartManager(ctx, cfg.CaddyAdmin, ":80")
		if err != nil {
			log.Fatalf("caddy: %v", err)
		}
		defer mgr.Stop()
	}

	dep := deploy.New(st, rt, caddy.NewClient(cfg.CaddyAdmin), cfg.BaseDomain)
	handler := api.New(st, dep)

	srv := &http.Server{Addr: cfg.APIAddr, Handler: handler}
	go func() {
		log.Printf("piperd listening on %s (apps at *.%s)", cfg.APIAddr, cfg.BaseDomain)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	srv.Shutdown(context.Background())
}
```

- [ ] **Step 2: Create the sample app for e2e**

Create `test/e2e/sampleapp/Dockerfile`:
```dockerfile
FROM alpine:3.20
RUN apk add --no-cache netcat-openbsd
EXPOSE 8080
CMD while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: 12\r\n\r\nhello piper\n' | nc -l -p 8080; done
```

- [ ] **Step 3: Write the end-to-end test**

Create `test/e2e/deploy_test.go`:
```go
package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/client"
)

// TestEndToEndDeploy builds piperd, runs it against real Docker + Caddy, deploys
// the sample app, and fetches it through Caddy's :80 by Host header.
// Skips unless RUN_E2E=1 and Docker + caddy are available.
func TestEndToEndDeploy(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker + caddy on PATH + free :80/:8088/:2019)")
	}
	repoRoot, _ := filepath.Abs("../..")
	dataDir := t.TempDir()

	// Build piperd.
	bin := filepath.Join(t.TempDir(), "piperd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/piperd")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build piperd: %v\n%s", err, out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+dataDir,
		"PIPER_API_ADDR=127.0.0.1:8088",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	defer cmd.Process.Kill()

	waitPort(t, "127.0.0.1:8088", 15*time.Second)

	c := client.New("http://127.0.0.1:8088")
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if _, err := c.Deploy("blog", filepath.Join(repoRoot, "test/e2e/sampleapp")); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Fetch through Caddy on :80 with Host: blog.piper.localhost.
	req, _ := http.NewRequest("GET", "http://127.0.0.1:80/", nil)
	req.Host = "blog.piper.localhost"
	var body string
	for i := 0; i < 20; i++ {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("no response through Caddy")
	}
	fmt.Printf("e2e response: %q\n", body)
}

func waitPort(t *testing.T, addr string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			conn.Close()
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("port %s never opened", addr)
}
```

- [ ] **Step 4: Run the e2e test**

Run (requires Docker running and `caddy` on PATH; may need sudo/root for `:80`, or set the base server listen to `:8081` via a config tweak if `:80` is privileged in your env):
```bash
RUN_E2E=1 go test ./test/e2e/ -run TestEndToEndDeploy -v
```
Expected: PASS, printing `e2e response: "hello piper\n"`. If it can't bind `:80`, temporarily change `StartManager(ctx, cfg.CaddyAdmin, ":80")` to `":8081"` and fetch `http://127.0.0.1:8081/`.

- [ ] **Step 5: Manual smoke test (optional but recommended)**

Run in three terminals:
```bash
# 1. a caddy is managed by piperd automatically; just run piperd:
make build && ./bin/piperd
# 2. deploy the sample app:
./bin/piper create blog --port 8080
./bin/piper deploy blog --path test/e2e/sampleapp
# 3. hit it:
curl -H 'Host: blog.piper.localhost' http://127.0.0.1/
# or, since *.localhost resolves to 127.0.0.1:
curl http://blog.piper.localhost/
```
Expected: `hello piper`.

- [ ] **Step 6: Commit**

```bash
git add cmd/piperd test/e2e
git commit -m "feat: piperd wire-up + end-to-end deploy test"
```

- [ ] **Step 7: Final verification**

Run:
```bash
go vet ./...
go test ./...
make cross
```
Expected: vet clean; all unit tests PASS (e2e + docker SKIP unless enabled); arm64 cross-build succeeds. Plan 1 is done: `piper deploy` a Dockerfile repo → live on the LAN.

---

## Plan-of-record notes (self-review)

- **Spec coverage (Plan 1 scope):** single-host Docker deploy ✔ (Tasks 3,5,8), SQLite state ✔ (Task 2), Caddy host-routing ✔ (Tasks 4,5), CLI-first surface ✔ (Task 7), Dockerfile build on the box ✔ (Task 3), no-cgo/ARM ✔ (Global Constraints + `make cross` gates). **Deferred to Plan 2/3 and intentionally absent:** relay, outbound tunnel, DNS-01 TLS, managed wildcard, git webhooks, PR previews. TLS is explicitly out — Plan 1 serves HTTP on the LAN.
- **Known reconciliation point:** Task 3 Step 3/4 — the Docker SDK's `ImageBuildOptions` import path/type has shifted across SDK versions; the plan flags the exact struct (`types.ImageBuildOptions{Tags, Remove, Dockerfile}`) and tells the implementer to let the compiler confirm the import after `go get`. This is the single place the plan defers to the installed dependency version rather than hardcoding, by necessity.
- **Type consistency:** `Runtime`, `RouteSetter`/`caddy.Client`, `Deployerer`/`deploy.Deployer`, and `store.App`/`store.Deployment` names/signatures are consistent across Tasks 2–8.
