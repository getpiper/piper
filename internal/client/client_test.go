package client

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/store"
)

func writeJSONTest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func TestTarDirWritesRelativeFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "cmd"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd", "run.sh"), []byte("echo ready\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := TarDir(dir, &buf); err != nil {
		t.Fatalf("TarDir: %v", err)
	}

	got := make(map[string]string)
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		contents, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("ReadAll %s: %v", hdr.Name, err)
		}
		got[hdr.Name] = string(contents)
	}
	if got["Dockerfile"] != "FROM alpine\n" || got["cmd/run.sh"] != "echo ready\n" {
		t.Errorf("tar contents = %#v", got)
	}
}

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

func TestCreateApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Name != "blog" || body.Port != 3000 {
			t.Errorf("body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	if err := New(srv.URL, "").CreateApp("blog", 3000); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
}

func TestDeploy(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/deploy" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-tar" {
			t.Errorf("Content-Type = %q", got)
		}
		tr := tar.NewReader(r.Body)
		hdr, err := tr.Next()
		if err != nil {
			t.Errorf("Next: %v", err)
		} else if hdr.Name != "Dockerfile" {
			t.Errorf("tar entry = %q", hdr.Name)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "building"})
	}))
	defer srv.Close()

	dep, err := New(srv.URL, "").Deploy("blog", srcDir)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.ID != "dep1" || dep.Status != "building" {
		t.Errorf("deployment = %+v", dep)
	}
}

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

func TestLinkApp(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if err := c.LinkApp("blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/apps/blog/link" {
		t.Fatalf("path = %s", gotPath)
	}
	if !strings.Contains(gotBody, `"alice/blog"`) || !strings.Contains(gotBody, `"main"`) {
		t.Fatalf("body = %s", gotBody)
	}
}

func TestManifestAndExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/github/manifest":
			io.WriteString(w, `{"manifest":"{\"name\":\"x\"}"}`)
		case "/v1/github/exchange":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	m, err := c.Manifest("http://localhost:5000/cb")
	if err != nil || !strings.Contains(m, `"name"`) {
		t.Fatalf("Manifest m=%q err=%v", m, err)
	}
	if err := c.ExchangeGitHub("thecode"); err != nil {
		t.Fatalf("ExchangeGitHub: %v", err)
	}
}

func TestSetsAuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode([]store.App{})
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "s3cr3t").ListApps(); err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("Authorization = %q, want Bearer s3cr3t", gotAuth)
	}
}

func TestClientMethodsReportHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusTeapot)
	}))
	defer srv.Close()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := New(srv.URL, "")
	tests := map[string]func() error{
		"create": func() error { return c.CreateApp("blog", 8080) },
		"list": func() error {
			_, err := c.ListApps()
			return err
		},
		"deploy": func() error {
			_, err := c.Deploy("blog", srcDir)
			return err
		},
	}
	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil || !strings.Contains(err.Error(), "418") || !strings.Contains(err.Error(), "boom") {
				t.Errorf("error = %v, want status and response body", err)
			}
		})
	}
}

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

func TestApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/web" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSONTest(w, api.App{
			App:    store.App{Name: "web", Port: 8080, Hostname: "web.piper.localhost"},
			Status: "running",
		})
	}))
	defer srv.Close()

	app, err := New(srv.URL, "").App("web")
	if err != nil {
		t.Fatalf("App: %v", err)
	}
	if app.Name != "web" || app.Hostname != "web.piper.localhost" || app.Status != "running" {
		t.Errorf("app = %+v", app)
	}
}

func TestDeployments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/web/deployments" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSONTest(w, []store.Deployment{
			{ID: "dep1", App: "web", Status: "running"},
			{ID: "dep2", App: "web", Status: "failed"},
		})
	}))
	defer srv.Close()

	deps, err := New(srv.URL, "").Deployments("web")
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deps) != 2 || deps[0].ID != "dep1" || deps[0].Status != "running" || deps[1].ID != "dep2" || deps[1].Status != "failed" {
		t.Errorf("deployments = %+v", deps)
	}
}

func TestDeploymentLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/web/deployments/dep1/logs" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, "line1\nline2\n")
	}))
	defer srv.Close()

	logs, err := New(srv.URL, "").DeploymentLogs("web", "dep1")
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	if logs != "line1\nline2\n" {
		t.Errorf("logs = %q", logs)
	}
}

func TestResponseErrorsCarryHTTPStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "bad").ListApps()
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v (%T), want *StatusError", err, err)
	}
	if se.Code != http.StatusUnauthorized {
		t.Fatalf("Code = %d, want %d", se.Code, http.StatusUnauthorized)
	}
}
