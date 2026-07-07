package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
