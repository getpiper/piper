package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/config"
)

func TestRelayLoginStoresCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	// No real sleeps or browser during the poll loop.
	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	openBrowserFn = func(string) error { return nil }
	defer func() { openBrowserFn = openBrowser }()

	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_code": "ABCD-EFGH", "verification_uri": "https://relay.test/device",
				"device_code": "dev-1", "interval": 1, "expires_in": 300,
			})
		case "/v1/login/poll":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"account_credential": "cred-xyz", "username": "alice",
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--relay", srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.RelayAPI != srv.URL || cc.AccountCredential != "cred-xyz" {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestConnectRequiresLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", t.TempDir()}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !bytes.Contains(errb.Bytes(), []byte("piper login")) {
		t.Fatalf("stderr = %q, want a `piper login` hint", errb.String())
	}
}

func TestConnectEnrollsAndWritesRelayFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" || r.Header.Get("Authorization") != "Bearer cred-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"enrollment_token": "enr-1", "base_domain": "ab12-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
		})
	}))
	defer srv.Close()

	// Prior `piper login` state.
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	old := config.SystemEnvDir
	config.SystemEnvDir = filepath.Join(t.TempDir(), "absent") // force the non-systemd path
	defer func() { config.SystemEnvDir = old }()
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	rf, found, err := config.LoadRelayFile(dataDir)
	if err != nil || !found {
		t.Fatalf("relay file: found=%v err=%v", found, err)
	}
	want := config.RelayFile{RelayAddr: "relay.getpiper.co:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co", Terminated: true}
	if rf != want {
		t.Fatalf("relay file = %+v, want %+v", rf, want)
	}
}

func TestConnectWritesTerminated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"enrollment_token": "enr-1", "base_domain": "aaaa-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
		})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	old := config.SystemEnvDir
	config.SystemEnvDir = filepath.Join(t.TempDir(), "absent") // force the non-systemd path
	defer func() { config.SystemEnvDir = old }()
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	rf, _, err := config.LoadRelayFile(dataDir)
	if err != nil || !rf.Terminated {
		t.Fatalf("relay file terminated = %v (err %v)", rf.Terminated, err)
	}
}

func TestConnectQuotaExceeded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", t.TempDir()}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !bytes.Contains(errb.Bytes(), []byte("quota")) {
		t.Fatalf("stderr = %q, want a quota message", errb.String())
	}
}

func TestConnectSystemManagedGuidesEnvInstall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" || r.Header.Get("Authorization") != "Bearer cred-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"enrollment_token": "enr-1", "base_domain": "ab12-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
		})
	}))
	defer srv.Close()

	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}

	// A present /etc/piper marks a systemd-managed box.
	dataDir := t.TempDir()
	old := config.SystemEnvDir
	config.SystemEnvDir = t.TempDir()
	defer func() { config.SystemEnvDir = old }()

	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	// It must guide the env-file install, not write relay.json.
	if _, found, _ := config.LoadRelayFile(dataDir); found {
		t.Fatalf("relay.json written on a systemd-managed box; expected a guided env-file install")
	}
	for _, want := range []string{
		"piperd.env",
		"PIPER_RELAY_ADDR=relay.getpiper.co:7000",
		"PIPER_RELAY_TOKEN=enr-1",
		"PIPER_BASE_DOMAIN=ab12-alice.public.getpiper.co",
		"PIPER_RELAY_TERMINATED=1",
	} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("stdout missing %q; got:\n%s", want, out.String())
		}
	}
}
