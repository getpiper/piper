package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/store"
)

func TestRunRemoteFlagRejectedForLocalOnlyCommands(t *testing.T) {
	for _, cmd := range []string{"version", "login", "connect"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--remote", "box.example.com", cmd}, &stdout, &stderr); code != 2 {
			t.Errorf("%s: code = %d, want 2", cmd, code)
		}
		if got := stderr.String(); !strings.Contains(got, "--remote does not apply") {
			t.Errorf("%s: stderr = %q", cmd, got)
		}
	}
}

// Pins the env-vs-flag guard-rail asymmetry: PIPER_REMOTE must NOT affect
// local-only commands (it passes trivially today; it guards against Task 2
// and later work wiring the env into these commands by accident).
func TestRunVersionIgnoresPiperRemoteEnv(t *testing.T) {
	t.Setenv("PIPER_REMOTE", "box.example.com")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}

func TestRunRemoteListRoutesThroughRelay(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/ab12-alice.public.getpiper.co/v1/apps" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cred-xyz" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]store.App{{Name: "api", Port: 3000}})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "ab12-alice.public.getpiper.co", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "api\tport=3000\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunRemoteEnvSelectsTarget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	t.Setenv("PIPER_REMOTE", "env-box.public.getpiper.co")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/env-box.public.getpiper.co/v1/apps" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]store.App{})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}

func TestRunRemoteFlagOverridesEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	t.Setenv("PIPER_REMOTE", "wrong-box.public.getpiper.co")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/right-box.public.getpiper.co/v1/apps" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]store.App{})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "right-box.public.getpiper.co", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}

func TestRunRemoteRequiresRelayLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // empty config: no RelayAPI/AccountCredential
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "box.example.com", "list"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %s", code, stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "piper login") {
		t.Errorf("stderr = %q, want a pointer to `piper login`", got)
	}
}

func TestRunRemoteDeployPrintsNoLocalURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/ab12-alice.public.getpiper.co/v1/apps/blog/deploy" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "running"})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "ab12-alice.public.getpiper.co", "deploy", "blog", "--path", srcDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "deployed blog (running)\n" {
		t.Errorf("stdout = %q, want %q", got, "deployed blog (running)\n")
	}
}
