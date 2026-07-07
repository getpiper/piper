package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
}

func TestLoginRequiresToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := run([]string{"login"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}
