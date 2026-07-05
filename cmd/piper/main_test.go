package main

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

func TestRunCreateSupportsNameFirstFlags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.Name != "blog" || body.Port != 3000 {
			t.Errorf("body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"create", "blog", "--port", "3000"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, `created app "blog" (port 3000)`) {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunDeploySupportsNameFirstFlags(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/deploy" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		tr := tar.NewReader(r.Body)
		hdr, err := tr.Next()
		if err != nil && err != io.EOF {
			t.Errorf("Next: %v", err)
		} else if err == nil && hdr.Name != "Dockerfile" {
			t.Errorf("tar entry = %q", hdr.Name)
		}
		_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "running"})
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"deploy", "blog", "--path", srcDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "deployed blog") || !strings.Contains(got, "running") {
		t.Errorf("stdout = %q", got)
	}
}

func TestManifestFormHandlerServesHTML(t *testing.T) {
	// The manifest page starts with <form>; without an explicit Content-Type Go
	// sniffs it as text/plain and the browser shows source instead of submitting
	// the form to GitHub. Guard against that regression.
	rec := httptest.NewRecorder()
	manifestFormHandler(`<form id="f"></form>`).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
}

func TestRunList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]store.App{{Name: "api", Port: 3000}, {Name: "blog", Port: 8080}})
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "api\tport=3000\nblog\tport=8080\n" {
		t.Errorf("stdout = %q", got)
	}
}
