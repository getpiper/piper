package api

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/store"
)

type fakeDeployer struct {
	gotApp  string
	gotFile string
}

func (f *fakeDeployer) Deploy(_ context.Context, app, srcDir string) (store.Deployment, error) {
	f.gotApp = app
	contents, err := os.ReadFile(filepath.Join(srcDir, "Dockerfile"))
	if err != nil {
		return store.Deployment{}, err
	}
	f.gotFile = string(contents)
	return store.Deployment{ID: "dep1", App: app, Status: "running", HostPort: 40001}, nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return New(newTestStore(t), &fakeDeployer{}, "piper.localhost", "", nil)
}

func TestCreateAndListApp(t *testing.T) {
	h := newTestHandler(t)

	create := httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"blog","port":8080}`))
	created := httptest.NewRecorder()
	h.ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}

	listed := httptest.NewRecorder()
	h.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listed.Code, listed.Body.String())
	}
	var apps []store.App
	if err := json.NewDecoder(listed.Body).Decode(&apps); err != nil {
		t.Fatalf("decode apps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" || apps[0].Port != 8080 {
		t.Errorf("apps = %+v, want blog on port 8080", apps)
	}
}

func TestCreateDuplicateReturnsConflict(t *testing.T) {
	h := newTestHandler(t)
	body := `{"name":"blog","port":8080}`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(body)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestCreateAppDefaultsPort(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"blog"}`)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var app store.App
	if err := json.NewDecoder(rec.Body).Decode(&app); err != nil {
		t.Fatalf("decode app: %v", err)
	}
	if app.Port != 8080 {
		t.Errorf("port = %d, want 8080", app.Port)
	}
}

func TestGetApp(t *testing.T) {
	h := newTestHandler(t)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"blog"}`)))

	found := httptest.NewRecorder()
	h.ServeHTTP(found, httptest.NewRequest(http.MethodGet, "/v1/apps/blog", nil))
	if found.Code != http.StatusOK {
		t.Fatalf("found status = %d, body = %s", found.Code, found.Body.String())
	}
	var app store.App
	if err := json.NewDecoder(found.Body).Decode(&app); err != nil {
		t.Fatalf("decode app: %v", err)
	}
	if app.Name != "blog" {
		t.Errorf("name = %q, want blog", app.Name)
	}

	missing := httptest.NewRecorder()
	h.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/apps/missing", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing status = %d, want %d", missing.Code, http.StatusNotFound)
	}
}

func TestListAppsEmptyReturnsArray(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("body = %s, want []", body)
	}
}

func TestDeployUploadExtractsAndCallsDeployer(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deployer := &fakeDeployer{}
	h := New(s, deployer, "piper.localhost", "", nil)

	var body bytes.Buffer
	tw := tar.NewWriter(&body)
	contents := []byte("FROM alpine\n")
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(contents))}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write(contents); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/apps/blog/deploy", &body)
	req.Header.Set("Content-Type", "application/x-tar")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if deployer.gotApp != "blog" || deployer.gotFile != "FROM alpine\n" {
		t.Errorf("deployer got app=%q file=%q", deployer.gotApp, deployer.gotFile)
	}
	var dep store.Deployment
	if err := json.NewDecoder(rec.Body).Decode(&dep); err != nil {
		t.Fatalf("decode deployment: %v", err)
	}
	if dep.ID != "dep1" || dep.Status != "running" {
		t.Errorf("deployment = %+v", dep)
	}
}

func TestReservedNameRejected(t *testing.T) {
	h := New(newTestStore(t), &fakeDeployer{}, "piper.localhost", "", nil)
	body := strings.NewReader(`{"name":"hooks","port":8080}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestLinkApp(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	h := New(s, &fakeDeployer{}, "piper.localhost", "", nil)
	body := strings.NewReader(`{"repo":"alice/blog","branch":"main"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/link", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rec.Code)
	}
	got, _ := s.AppByRepo("alice/blog")
	if got.Name != "blog" || got.Branch != "main" {
		t.Fatalf("link not persisted: %+v", got)
	}
}

func TestManifestEndpoint(t *testing.T) {
	h := New(newTestStore(t), &fakeDeployer{}, "alice.dev", "", nil)
	body := strings.NewReader(`{"redirect_url":"http://localhost:5000/cb"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/github/manifest", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hooks.alice.dev") {
		t.Fatalf("manifest missing webhook host: %s", rec.Body.String())
	}
}

func TestExchangeSavesCredsAndInvokesCallback(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app-manifests/thecode/conversions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":42,"pem":"KEY","webhook_secret":"SEKRIT"}`)
	}))
	defer gh.Close()

	s := newTestStore(t)
	called := false
	h := New(s, &fakeDeployer{}, "piper.localhost", gh.URL, func() { called = true })

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"code":"thecode"}`)
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/github/exchange", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("onGitHubApp callback was not invoked after exchange")
	}
	saved, err := s.GetGitHubApp()
	if err != nil {
		t.Fatalf("GetGitHubApp: %v", err)
	}
	if saved.AppID != 42 || saved.WebhookSecret != "SEKRIT" {
		t.Fatalf("creds not persisted: %+v", saved)
	}
}

func TestUntarRejectsPathTraversal(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "destination")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	var body bytes.Buffer
	tw := tar.NewWriter(&body)
	contents := []byte("escaped")
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: int64(len(contents))}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write(contents); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar: %v", err)
	}

	if err := untar(&body, dir); err == nil {
		t.Fatal("untar returned nil, want path traversal error")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
		t.Errorf("escape file exists or stat failed: %v", err)
	}
}

func TestListAppsIncludesDeployStatus(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{}, "piper.localhost", "", nil)
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "running", ""); err != nil {
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
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed", ""); err != nil {
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
