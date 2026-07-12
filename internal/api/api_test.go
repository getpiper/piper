package api

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/domain"
	"github.com/getpiper/piper/internal/store"
)

type fakeDeployer struct {
	store         *store.Store
	gotApp        string
	gotFile       string
	stopped       []string
	deleted       []string
	stopErr       error
	deleteErr     error
	panicOnFinish bool
}

func (f *fakeDeployer) Begin(app string) (store.Deployment, error) {
	f.gotApp = app
	return f.store.CreateDeployment(app, "", "", 0, "building", "")
}

func (f *fakeDeployer) Finish(_ context.Context, dep store.Deployment, srcDir string) error {
	if f.panicOnFinish {
		panic("boom: simulated Finish panic")
	}
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
	s := newTestStore(t)
	return New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
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

func TestDeployPanicInFinishIsRecoveredAndFailsRow(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deployer := &fakeDeployer{store: s, panicOnFinish: true}
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

	// If the panic isn't recovered, it propagates and crashes this test
	// process's goroutine (test binary aborts) instead of reaching here.
	waitForStatus(t, s, "web", "failed")
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

func TestAppsAPIIncludesHostname(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := s.SetAppHostname("blog", "hash-blog-alice.public.getpiper.co"); err != nil {
		t.Fatalf("SetAppHostname: %v", err)
	}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/apps/blog", nil))
	var one App
	if err := json.NewDecoder(get.Body).Decode(&one); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if one.Hostname != "hash-blog-alice.public.getpiper.co" {
		t.Errorf("GET app hostname = %q", one.Hostname)
	}

	list := httptest.NewRecorder()
	h.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	var many []App
	if err := json.NewDecoder(list.Body).Decode(&many); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(many) != 1 || many[0].Hostname != "hash-blog-alice.public.getpiper.co" {
		t.Errorf("list hostname = %+v", many)
	}
}

func TestReservedNameRejected(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
	body := strings.NewReader(`{"name":"hooks","port":8080}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
}

// App names flow unescaped into URL paths and hostnames (<app>.<baseDom>,
// pr-N-<app>.…), so create rejects anything that isn't a DNS label. #120.
func TestInvalidAppNameRejected(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
	for _, name := range []string{"Blog", "my_app", "a/b", "-lead", "trail-", "app.dot", "app name", strings.Repeat("x", 64)} {
		rec := httptest.NewRecorder()
		body := strings.NewReader(`{"name":"` + name + `","port":8080}`)
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name %q: code = %d, want 400", name, rec.Code)
		}
	}
	// A valid DNS-label name still succeeds.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps",
		strings.NewReader(`{"name":"my-app-1","port":8080}`)))
	if rec.Code != http.StatusCreated {
		t.Errorf("valid name: code = %d, want 201", rec.Code)
	}
}

func TestLinkApp(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
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
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "alice.dev", "", nil, nil)
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
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", gh.URL, func() { called = true }, nil)

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
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
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
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
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
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
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
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
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

type fakeDomainManager struct {
	status  domain.Status
	setErr  error
	gotSet  []string
	removed bool
}

func (f *fakeDomainManager) Set(d, p, tok string) (domain.Status, error) {
	f.gotSet = []string{d, p, tok}
	if f.setErr != nil {
		return domain.Status{}, f.setErr
	}
	return f.status, nil
}
func (f *fakeDomainManager) Status() (domain.Status, error) { return f.status, nil }
func (f *fakeDomainManager) Remove() error                  { f.removed = true; return nil }

func TestDomainEndpoints(t *testing.T) {
	fdm := &fakeDomainManager{status: domain.Status{
		Domain: "shop.dev", DNSProvider: "cloudflare", DNSTokenSet: true,
		Source: "api", Status: "issuing",
		DNSRecords: []domain.DNSRecord{{Type: "CNAME", Name: "*.shop.dev", Value: "relay.example.net"}},
	}}
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, fdm)

	// PUT kicks Set with the body fields.
	put := httptest.NewRequest(http.MethodPut, "/v1/domain",
		strings.NewReader(`{"domain":"shop.dev","dns_provider":"cloudflare","dns_token":"cf-tok"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(fdm.gotSet) != 3 || fdm.gotSet[0] != "shop.dev" || fdm.gotSet[2] != "cf-tok" {
		t.Fatalf("Set called with %v", fdm.gotSet)
	}

	// GET returns the status; the token value must never appear.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/domain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"dns_token_set":true`) {
		t.Fatalf("GET body missing dns_token_set: %s", body)
	}
	if strings.Contains(body, `"dns_token":`) || strings.Contains(body, "cf-tok") {
		t.Fatalf("GET leaks the dns token: %s", body)
	}

	// DELETE removes.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/domain", nil))
	if rec.Code != http.StatusNoContent || !fdm.removed {
		t.Fatalf("DELETE = %d, removed = %v", rec.Code, fdm.removed)
	}
}

func TestDomainEndpointErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"env-managed", domain.ErrEnvManaged, http.StatusConflict},
		{"invalid domain", domain.ErrInvalidDomain, http.StatusBadRequest},
		{"bad provider", domain.ErrUnsupportedProvider, http.StatusBadRequest},
		{"empty token", domain.ErrTokenRequired, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil,
				&fakeDomainManager{setErr: tc.err})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/domain",
				strings.NewReader(`{"domain":"x.dev","dns_provider":"cloudflare","dns_token":"t"}`)))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestDomainEndpointsWithoutRelay(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/v1/domain", strings.NewReader(`{}`)))
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s without relay = %d, want 409", m, rec.Code)
		}
	}
}

func TestStopEndpoint(t *testing.T) {
	s := newTestStore(t)
	deployer := &fakeDeployer{store: s}
	h := New(s, deployer, "piper.localhost", "", nil, nil)
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
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s, stopErr: store.ErrNotFound}, "piper.localhost", "", nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/ghost/stop", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteAppEndpoint(t *testing.T) {
	s := newTestStore(t)
	deployer := &fakeDeployer{store: s}
	h := New(s, deployer, "piper.localhost", "", nil, nil)
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
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s, deleteErr: store.ErrNotFound}, "piper.localhost", "", nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/ghost", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A 500 must not echo the raw internal error (container IDs, Caddy admin URLs,
// file paths) to the caller — the control API is reachable remotely through the
// relay proxy. #122.
func TestServerErrorDoesNotLeakInternalDetail(t *testing.T) {
	s := newTestStore(t)
	leak := errors.New("unroute: caddy admin http://127.0.0.1:2019 failed stopping container abc123def /var/lib/piper/state")
	h := New(s, &fakeDeployer{store: s, stopErr: leak}, "piper.localhost", "", nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/stop", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"caddy", "2019", "abc123def", "/var/lib/piper", "unroute"} {
		if strings.Contains(body, secret) {
			t.Errorf("500 body leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "internal server error") {
		t.Errorf("body = %q, want a generic message", strings.TrimSpace(body))
	}
}
