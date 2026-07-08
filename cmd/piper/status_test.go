package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/store"
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
