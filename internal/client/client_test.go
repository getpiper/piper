package client

import (
	"archive/tar"
	"bytes"
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
		_ = json.NewEncoder(w).Encode([]store.App{{Name: "blog", Port: 8080}})
	}))
	defer srv.Close()

	apps, err := New(srv.URL).ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" || apps[0].Port != 8080 {
		t.Errorf("apps = %+v", apps)
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

	if err := New(srv.URL).CreateApp("blog", 3000); err != nil {
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
		_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "running"})
	}))
	defer srv.Close()

	dep, err := New(srv.URL).Deploy("blog", srcDir)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.ID != "dep1" || dep.Status != "running" {
		t.Errorf("deployment = %+v", dep)
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
	c := New(srv.URL)
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
