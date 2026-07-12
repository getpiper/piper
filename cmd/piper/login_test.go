package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/store"
)

func TestLoginSavesConfigOnSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]store.App{})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--addr", srv.URL, "--token", "good"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Token != "good" || cc.Addr != srv.URL {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestLoginRejectsBadToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--addr", srv.URL, "--token", "bad"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	cc, _ := config.LoadClient()
	if cc.Token != "" {
		t.Fatalf("token should not be saved, got %q", cc.Token)
	}
	if !strings.Contains(errb.String(), "token rejected") {
		t.Fatalf("stderr = %q, want token rejected", errb.String())
	}
}

func TestLoginConnectivityErrorIsNotTokenRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	// Grab a URL that refuses connections: the dial fails before any token
	// is ever evaluated.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--addr", addr, "--token", "tok"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if strings.Contains(errb.String(), "token rejected") {
		t.Fatalf("connectivity failure reported as auth failure: %s", errb.String())
	}
	if !strings.Contains(errb.String(), "cannot reach piperd at "+addr) {
		t.Fatalf("stderr = %q, want cannot-reach message", errb.String())
	}
}

func TestLoginTokenPathStillValidates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	// --token present ⇒ LAN path, which rejects a bad token with exit 1.
	if code := run([]string{"login", "--addr", srv.URL, "--token", "bad"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

// A LAN `piper login --token` must preserve previously stored relay creds
// (RelayAPI/AccountCredential), not clobber them with a fresh ClientConfig. #84.
func TestLANLoginPreservesRelayCreds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	// Prior relay login state on disk.
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: "https://api.public.getpiper.dev",
		AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]store.App{})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--addr", srv.URL, "--token", "good"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Token != "good" || cc.Addr != srv.URL {
		t.Fatalf("LAN creds not saved: %+v", cc)
	}
	if cc.RelayAPI != "https://api.public.getpiper.dev" || cc.AccountCredential != "cred-xyz" {
		t.Fatalf("relay creds clobbered: %+v", cc)
	}
}

// The LAN login usage hint mentions sudo (systemd installs). #146.
func TestLoginUsageMentionsSudo(t *testing.T) {
	var out, errb bytes.Buffer
	if code := login("", "", &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "sudo") {
		t.Errorf("usage = %q, want a sudo hint", errb.String())
	}
}

// `piper delete --yes blog` (flag before name) must fail with a clear hint, not
// treat "--yes" as the app name. #124.
func TestDeleteRejectsFlagBeforeName(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"delete", "--yes", "blog"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "before flags") {
		t.Errorf("stderr = %q, want a name-before-flags hint", errb.String())
	}
}
