# Health/Metrics Surface Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A remote caller can answer "is my box up, and what's deployed?" — relay-answered liveness from the tunnel session, per-app last-deploy status on the authenticated control API, and a `piper status` CLI consumer.

**Architecture:** The relay owns "is the box up" (its in-memory `Router` session map already knows) via a new `GET /agents/<base-domain>` route answered without opening a tunnel stream. piperd owns "what's deployed": `GET /v1/apps[/name]` responses gain a `Status` field fed by a new `store.LatestDeployment` query (latest `pr=0` deployment, any status). `piper status` consumes both. No new auth anywhere — every surface sits behind gates that already exist (relay account credential + ownership; piperd `RequireToken`).

**Tech Stack:** Go stdlib (`net/http`, `httptest`), `modernc.org/sqlite` via `internal/store`, in-memory yamux tunnel pairs for relay tests.

**Spec:** `docs/superpowers/specs/2026-07-09-health-metrics-surface-design.md` — read it first.

## Global Constraints

- **No cgo**: everything builds with `CGO_ENABLED=0` (`make cross` must pass).
- Deployment status strings are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`; `Status` is `""` when an app has never been deployed.
- Layering: `store` persistence-only; `api` transport-only; `client` is the CLI's view of `api`; nothing imports "up".
- Liveness offline is **200 with `connected:false`**, not an error; proxied `/v1/...` to an offline box stays 503.
- Relay gate order & codes unchanged: bad credential 401; unknown or cross-tenant agent 404 (no existence leak).
- Branch: `faruk/health-metrics-surface`. One commit per task, conventional style, body references `Part of #75`, trailer:
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`
- Run `make verify` (gofmt → vet → test → cross) before claiming done.

---

### Task 1: `store.LatestDeployment`

**Files:**
- Modify: `internal/store/store.go` (add method after `LatestRunning`, ~line 210)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: existing `Deployment` type, `ErrNotFound`, test helper `openTemp(t)`.
- Produces: `func (s *Store) LatestDeployment(app string) (Deployment, error)` — newest `pr=0` deployment for `app` regardless of status, ordered by `created_at DESC`; `ErrNotFound` when the app has never been (non-preview-)deployed. Task 2 calls this.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go` (near `TestLatestRunning`):

```go
func TestLatestDeployment(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	if _, err := s.LatestDeployment("blog"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty LatestDeployment err = %v, want ErrNotFound", err)
	}
	s.CreateDeployment("blog", "img1", "c1", 40001, "running")
	d2, _ := s.CreateDeployment("blog", "img2", "c2", 40002, "failed")
	got, err := s.LatestDeployment("blog")
	if err != nil {
		t.Fatalf("LatestDeployment: %v", err)
	}
	if got.ID != d2.ID || got.Status != "failed" {
		t.Errorf("LatestDeployment = %+v, want id %s status failed", got, d2.ID)
	}
}

func TestLatestDeploymentIgnoresPreviews(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	s.CreateDeployment("blog", "img", "main-c", 40000, "running")
	// Created later, so it would win on created_at if pr>0 rows weren't excluded.
	s.CreatePreviewDeployment("blog", 3, "img", "preview-c", 41000, "failed")
	got, err := s.LatestDeployment("blog")
	if err != nil {
		t.Fatalf("LatestDeployment: %v", err)
	}
	if got.ContainerID != "main-c" || got.Status != "running" {
		t.Errorf("LatestDeployment = %+v, want main-c/running", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestLatestDeployment' -v`
Expected: FAIL to compile — `s.LatestDeployment undefined`.

- [ ] **Step 3: Implement**

Add to `internal/store/store.go`, directly after `LatestRunning` (mirror its shape):

```go
// LatestDeployment returns the newest non-preview deployment for app,
// whatever its status — the app's production deploy state. PR previews
// (pr > 0) never color it. ErrNotFound when the app was never deployed.
func (s *Store) LatestDeployment(app string) (Deployment, error) {
	var d Deployment
	var ts string
	err := s.db.QueryRow(
		`SELECT id, app, image_id, container_id, host_port, status, created_at
		 FROM deployments WHERE app=? AND pr=0
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

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS (all, including the two new tests).

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): LatestDeployment — newest non-preview deploy per app

Part of #75.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Deploy status on piperd's apps API

**Files:**
- Modify: `internal/api/api.go` (the `POST /v1/apps`, `GET /v1/apps`, `GET /v1/apps/{name}` handlers; new type + helper)
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `store.LatestDeployment(app) (store.Deployment, error)` from Task 1; test helpers `newTestStore(t)`, `fakeDeployer`.
- Produces: exported wire type `api.App` — `struct { store.App; Status string }` — returned (as JSON, flattened by embedding: `Name`, `Port`, `Repo`, `Branch`, `CreatedAt`, `Status`) by all three app endpoints. Tasks 4–5 decode it.

- [ ] **Step 1: Write the failing tests**

Append to `internal/api/api_test.go`:

```go
func TestListAppsIncludesDeployStatus(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{}, "piper.localhost", "", nil)
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "running"); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var apps []App
	if err := json.NewDecoder(rr.Body).Decode(&apps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// ListApps orders by name: api, blog.
	if len(apps) != 2 || apps[0].Name != "api" || apps[0].Status != "" {
		t.Errorf("apps[0] = %+v, want api with empty status", apps)
	}
	if apps[1].Name != "blog" || apps[1].Status != "running" {
		t.Errorf("apps[1] = %+v, want blog running", apps)
	}
}

func TestGetAppIncludesDeployStatus(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{}, "piper.localhost", "", nil)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed"); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/apps/blog", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var app App
	if err := json.NewDecoder(rr.Body).Decode(&app); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if app.Name != "blog" || app.Status != "failed" {
		t.Errorf("app = %+v, want blog failed", app)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run 'DeployStatus' -v`
Expected: FAIL to compile — `undefined: App`.

- [ ] **Step 3: Implement**

In `internal/api/api.go`:

Add above `func New(...)`:

```go
// App is the wire shape of an app in API responses: the stored app plus the
// status of its latest non-preview deployment — exactly one of "building",
// "running", "failed", "stopped" — or "" when never deployed.
type App struct {
	store.App
	Status string
}

// latestStatus resolves the App.Status for one app; never-deployed is "".
func latestStatus(s *store.Store, app string) (string, error) {
	d, err := s.LatestDeployment(app)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return d.Status, nil
}
```

In the `POST /v1/apps` handler, change the final write (a just-created app has never deployed, so the shape stays uniform):

```go
	writeJSON(w, http.StatusCreated, App{App: app})
```

Replace the `GET /v1/apps` handler body after `ListApps`:

```go
	mux.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		apps, err := s.ListApps()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]App, 0, len(apps))
		for _, a := range apps {
			status, err := latestStatus(s, a.Name)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			out = append(out, App{App: a, Status: status})
		}
		writeJSON(w, http.StatusOK, out)
	})
```

(The old `if apps == nil { apps = []store.App{} }` disappears — `make([]App, 0, …)` already marshals as `[]`.)

In the `GET /v1/apps/{name}` handler, replace the final write:

```go
		status, err := latestStatus(s, app.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, App{App: app, Status: status})
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -v`
Expected: PASS — new tests and all existing ones (existing tests decode into `store.App`, which ignores the extra `Status` key; `TestListAppsEmptyReturnsArray` still sees `[]`). The existing `auth_test.go` already proves every route 401s without a token under `RequireToken` — the enriched responses inherit that with no new code.

- [ ] **Step 5: Commit**

```bash
git add internal/api/api.go internal/api/api_test.go
git commit -m "feat(agent): per-app deploy status on apps API responses

Part of #75.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Relay liveness endpoint

**Files:**
- Modify: `internal/relay/proxy.go:33-45` (path parsing / gate section of `NewControlProxy`)
- Test: `internal/relay/proxy_test.go`

**Interfaces:**
- Consumes: existing `st.AgentAccount(base) (accountID, username string, err error)`, `router.Lookup(base) (*tunnel.Session, bool)`, package-local `writeJSON(w, code, body)` from `internal/relay/api.go`; test fixtures `proxyFixture(t)`, `pipeSession(t, base)`, `proxyGet(t, api, path, cred)`.
- Produces: `GET /agents/<base-domain>` (bare path, no `/v1/` tail) → relay-answered `200 {"agent":"<base>","connected":true|false}` after the same 401/404 gates as the proxy; non-GET on the bare path → 405. Tasks 4–6 consume this route.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/proxy_test.go`:

```go
func TestControlProxyLiveness(t *testing.T) {
	api, _, router, aliceCred, malloryCred, base := proxyFixture(t)

	// Same gates as the proxy: no/bad credential → 401.
	if rr := proxyGet(t, api, "/agents/"+base, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no cred: %d, want 401", rr.Code)
	}
	// Cross-tenant and unknown agents → 404, indistinguishable.
	if rr := proxyGet(t, api, "/agents/"+base, malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant: %d, want 404", rr.Code)
	}
	if rr := proxyGet(t, api, "/agents/nope.public.getpiper.co", aliceCred); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d, want 404", rr.Code)
	}

	// Owned but no live session: offline is an answer, not an error.
	rr := proxyGet(t, api, "/agents/"+base, aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("offline liveness: %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var live struct {
		Agent     string `json:"agent"`
		Connected bool   `json:"connected"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&live); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if live.Agent != base || live.Connected {
		t.Errorf("offline liveness = %+v, want agent=%s connected=false", live, base)
	}

	// Connected session ⇒ box up. No fakeBox is serving streams: if the
	// handler opened a tunnel stream, this request would hang — liveness
	// must be answered from the router's in-memory map alone.
	relaySess, _ := pipeSession(t, base)
	router.Register(relaySess)
	rr = proxyGet(t, api, "/agents/"+base, aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("connected liveness: %d, want 200", rr.Code)
	}
	if err := json.NewDecoder(rr.Body).Decode(&live); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !live.Connected {
		t.Errorf("connected liveness = %+v, want connected=true", live)
	}

	// Bare agent path is a GET-only resource.
	req := httptest.NewRequest(http.MethodPost, "/agents/"+base, nil)
	req.Header.Set("Authorization", "Bearer "+aliceCred)
	pr := httptest.NewRecorder()
	api.ServeHTTP(pr, req)
	if pr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST liveness: %d, want 405", pr.Code)
	}
}
```

Add `"encoding/json"` to the test file's imports if not present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestControlProxyLiveness -v`
Expected: FAIL — `offline liveness: 404, want 200` (bare path currently 404s on shape).

- [ ] **Step 3: Implement**

In `internal/relay/proxy.go`, replace the path-parse + authz block (currently lines 33–45):

```go
		// Path shape: /agents/<base-domain>[/v1/...]
		rest := strings.TrimPrefix(r.URL.Path, "/agents/")
		base, tail, _ := strings.Cut(rest, "/")
		if base == "" {
			http.NotFound(w, r)
			return
		}

		ownerID, _, err := st.AgentAccount(base)
		if err != nil || ownerID != acc.ID {
			http.NotFound(w, r)
			return
		}

		if tail == "" {
			// Liveness: answered by the relay itself from its in-memory
			// session map — never opens a tunnel stream. Offline is an
			// answer, not an error: 200 with connected:false.
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			_, connected := router.Lookup(base)
			writeJSON(w, http.StatusOK, map[string]any{"agent": base, "connected": connected})
			return
		}
		if !strings.HasPrefix(tail, "v1/") {
			http.NotFound(w, r)
			return
		}
```

Everything from `sess, ok := router.Lookup(base)` down is unchanged. (The ownership check now runs before the `/v1/` shape check; a non-`v1` tail still 404s, as the existing `TestControlProxyAuthz` "Path without /v1/" case pins.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS — the new test and the whole existing relay suite (authz matrix, provision, SNI dispatch).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/proxy.go internal/relay/proxy_test.go
git commit -m "feat(relay): liveness — GET /agents/<base> answers from the tunnel session map

Part of #75.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Client — `Liveness()` and `Status` in apps responses

**Files:**
- Modify: `internal/client/client.go` (`ListApps` return type; new `Liveness` type + method)
- Test: `internal/client/client_test.go`

**Interfaces:**
- Consumes: `api.App` from Task 2 (`import "github.com/piperbox/piper/internal/api"` — downward per layering: client is the CLI's view of api); relay liveness JSON from Task 3.
- Produces: `func (c *Client) ListApps() ([]api.App, error)`; `type Liveness struct { Agent string; Connected bool }` (json tags `agent`, `connected`); `func (c *Client) Liveness() (Liveness, error)` — a GET at the client's base path, which for a remote client is exactly `<RelayAPI>/agents/<base>`. Task 5 consumes both.

- [ ] **Step 1: Write the failing tests**

In `internal/client/client_test.go`, replace `TestListApps` and add `TestLiveness`:

```go
func TestListApps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]api.App{
			{App: store.App{Name: "blog", Port: 8080}, Status: "running"},
		})
	}))
	defer srv.Close()

	apps, err := New(srv.URL, "").ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" || apps[0].Port != 8080 || apps[0].Status != "running" {
		t.Errorf("apps = %+v", apps)
	}
}

func TestLiveness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The liveness resource is the client's base path itself.
		if r.Method != http.MethodGet || r.URL.Path != "/agents/box.public.getpiper.co" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cred" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent": "box.public.getpiper.co", "connected": true,
		})
	}))
	defer srv.Close()

	live, err := New(srv.URL+"/agents/box.public.getpiper.co", "cred").Liveness()
	if err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if live.Agent != "box.public.getpiper.co" || !live.Connected {
		t.Errorf("live = %+v", live)
	}
}
```

Add `"github.com/piperbox/piper/internal/api"` to the test file's imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/client/ -run 'TestListApps|TestLiveness' -v`
Expected: FAIL to compile — `undefined: api` / `c.Liveness undefined` (and `apps[0].Status` undefined on `store.App`).

- [ ] **Step 3: Implement**

In `internal/client/client.go`:

Add the import:

```go
	"github.com/piperbox/piper/internal/api"
```

Change `ListApps` to decode the enriched wire type:

```go
func (c *Client) ListApps() ([]api.App, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, responseError("list apps", resp)
	}
	var apps []api.App
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, err
	}
	return apps, nil
}
```

Add after `ListApps`:

```go
// Liveness reports the relay's view of the box: whether its tunnel session is
// currently connected. It GETs the client's base path itself, which on a
// remote client is the relay's /agents/<base-domain> resource — it has no
// meaning against a local piperd address.
type Liveness struct {
	Agent     string `json:"agent"`
	Connected bool   `json:"connected"`
}

func (c *Client) Liveness() (Liveness, error) {
	resp, err := c.do(http.MethodGet, "", "", nil)
	if err != nil {
		return Liveness{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return Liveness{}, responseError("liveness", resp)
	}
	var l Liveness
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		return Liveness{}, err
	}
	return l, nil
}
```

- [ ] **Step 4: Run tests and build to verify**

Run: `go test ./internal/client/ -v && go build ./...`
Expected: client tests PASS; whole module still builds (`cmd/piper` uses only promoted fields `app.Name`/`app.Port`, which `api.App` keeps).

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat(cli): client Liveness() and deploy status in apps responses

Part of #75.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: `piper status` command

**Files:**
- Modify: `cmd/piper/main.go` (new `case "status"` in `run`'s command switch, after `case "list"`; `usage` line)
- Test: Create `cmd/piper/status_test.go`

**Interfaces:**
- Consumes: `dialClient(remote, stderr)`, `c.Liveness() (client.Liveness, error)`, `c.ListApps() ([]api.App, error)`; test conventions from `cmd/piper/remote_test.go` (`t.Setenv("HOME", …)` + `config.SaveClient`).
- Produces: `piper status` — remote: `box <base>: connected|offline` then (when connected) one line per app `name\tstatus=<s>\tport=<p>` with `-` for never-deployed; local: app lines only.

- [ ] **Step 1: Write the failing tests**

Create `cmd/piper/status_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/store"
)

func TestRunStatusLocal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/apps" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]api.App{
			{App: store.App{Name: "api", Port: 3000}},
			{App: store.App{Name: "blog", Port: 8080}, Status: "running"},
		})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{Addr: srv.URL, Token: "tok"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	want := "api\tstatus=-\tport=3000\nblog\tstatus=running\tport=8080\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunStatusRemoteConnected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents/box.public.getpiper.co":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agent": "box.public.getpiper.co", "connected": true,
			})
		case "/agents/box.public.getpiper.co/v1/apps":
			_ = json.NewEncoder(w).Encode([]api.App{
				{App: store.App{Name: "blog", Port: 8080}, Status: "running"},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "box.public.getpiper.co", "status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	want := "box box.public.getpiper.co: connected\nblog\tstatus=running\tport=8080\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunStatusRemoteOffline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/box.public.getpiper.co" {
			// Offline must short-circuit: no app listing request.
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent": "box.public.getpiper.co", "connected": false,
		})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "box.public.getpiper.co", "status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "box box.public.getpiper.co: offline\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunStatusRejectsArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "extra"}, &stdout, &stderr); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/piper/ -run TestRunStatus -v`
Expected: FAIL — `status` falls through to `usage`, so exit code 2 and empty stdout (`TestRunStatusRejectsArgs` passes trivially; the other three fail).

- [ ] **Step 3: Implement**

In `cmd/piper/main.go`, add after `case "list":`'s block:

```go
	case "status":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: piper status")
			return 2
		}
		c, ok := dialClient(*remote, stderr)
		if !ok {
			return 1
		}
		if *remote != "" {
			live, err := c.Liveness()
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
			if !live.Connected {
				fmt.Fprintf(stdout, "box %s: offline\n", *remote)
				return 0
			}
			fmt.Fprintf(stdout, "box %s: connected\n", *remote)
		}
		apps, err := c.ListApps()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		for _, app := range apps {
			status := app.Status
			if status == "" {
				status = "-"
			}
			fmt.Fprintf(stdout, "%s\tstatus=%s\tport=%d\n", app.Name, status, app.Port)
		}
		return 0
```

Update `usage`:

```go
	fmt.Fprintln(w, "usage: piper [--remote <base-domain>] <version|login|connect|create|deploy|list|status|app|github> [args]")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/piper/ -v`
Expected: PASS — the four new tests plus the existing suite (login, remote, onboarding).

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/main.go cmd/piper/status_test.go
git commit -m "feat(cli): piper status — box liveness + per-app deploy status

Part of #75.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: E2E — liveness + deploy status through the relay

**Files:**
- Modify: `test/e2e/relay_terminated_test.go` (extend `TestRelayTerminatedSelfService` after the cross-tenant 404 block, and the existing `/v1/apps` assertion)

**Interfaces:**
- Consumes: existing helpers `controlRequest(t, sni, addr, path, cred, wantStatus, within) string`, `agentBaseDomain`, `accountCredential`; the `apps` response string already captured in the test; routes from Tasks 2–3.
- Produces: end-to-end proof for #75's acceptance criteria.

- [ ] **Step 1: Extend the e2e test**

In `TestRelayTerminatedSelfService`, right after the `insertSecondAccount` / 404 block (currently the end of the function body), add:

```go
	// ---- Health/metrics surface (#75) ----
	// Liveness: relay-answered from the live tunnel session, no box round-trip.
	live := controlRequest(t, "api."+apex, "127.0.0.1:8443", "/agents/"+base, cred, http.StatusOK, 10*time.Second)
	if !strings.Contains(live, `"connected":true`) {
		t.Fatalf("liveness = %q, want connected:true", live)
	}
	// Same gates as the proxy: another tenant gets 404, not an existence leak.
	controlRequest(t, "api."+apex, "127.0.0.1:8443", "/agents/"+base, mcred, http.StatusNotFound, 10*time.Second)
```

And strengthen the existing `/v1/apps` assertion (the `apps := controlRequest(...)` block) so it also pins deploy status — replace:

```go
	if !strings.Contains(apps, "blog") {
		t.Fatalf("control response missing deployed app: %q", apps)
	}
```

with:

```go
	if !strings.Contains(apps, "blog") || !strings.Contains(apps, `"Status":"running"`) {
		t.Fatalf("control response missing deployed app with running status: %q", apps)
	}
```

(Note: the `mcred` variable already exists a few lines up; the liveness block must stay after it.)

- [ ] **Step 2: Run the e2e test**

Run: `go test ./test/e2e/ -run TestRelayTerminatedSelfService -v -timeout 10m`
Expected: PASS on a machine with real Docker + Caddy; SKIP (cleanly) without them. If it skips locally, say so explicitly in the task report — do not claim it passed.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/relay_terminated_test.go
git commit -m "test(e2e): liveness + deploy status through the relay

Part of #75.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Docs, PROGRESS, full verify

**Files:**
- Modify: `README.md` ("Drive a box remotely" section, ~line 108)
- Modify: `PROGRESS.md` (remote control-plane track, ~lines 45–49)

**Interfaces:**
- Consumes: everything shipped in Tasks 1–6.
- Produces: user-facing docs + tracker entry; a green `make verify`.

- [ ] **Step 1: Update README**

In the "Drive a box remotely" section, add `status` to the command enumeration:

```markdown
Any control command (`create`, `deploy`, `list`, `status`, `app link`, `github setup`)
```

and extend the example block with a status line:

```bash
piper --remote ab12-alice.public.getpiper.co list
piper --remote ab12-alice.public.getpiper.co status  # box up? what's deployed?
export PIPER_REMOTE=ab12-alice.public.getpiper.co   # or set it once
piper deploy blog --path .
```

- [ ] **Step 2: Update PROGRESS.md**

In the remote control-plane list (after the `#74` line), add:

```markdown
  - ✅ health/metrics surface — relay liveness (`GET /agents/<base>`) + per-app deploy status + `piper status` — [#75](https://github.com/piperbox/piper/issues/75)
```

And in the "Epic #49 remains open" line, move `#75` from the not-built list to the done list (leaving the hosted dashboard [#76] as the only open child).

- [ ] **Step 3: Run the full gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, `go test ./...` PASS (Docker-dependent tests may skip), `make cross` builds. Fix anything it flags before committing.

- [ ] **Step 4: Commit**

```bash
git add README.md PROGRESS.md
git commit -m "docs: piper status + PROGRESS entry for the health/metrics surface

Part of #75.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Done means

- All acceptance criteria of [#75](https://github.com/piperbox/piper/issues/75) demonstrably met: liveness from the tunnel session (Task 3), authenticated per-app deploy status (Task 2), same authn/authz (inherited, pinned by tests), tests for both surfaces (Tasks 1–6).
- `make verify` green.
- PR into `main` (`gh pr create --base main`), body carries `Closes #75` and `Part of #49`; squash-merge.
