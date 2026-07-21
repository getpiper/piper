package relayclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoginDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/login/device" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_code": "ABCD-EFGH", "verification_uri": "https://relay.test/device",
			"device_code": "dev-1", "interval": 5, "expires_in": 300,
		})
	}))
	defer srv.Close()

	da, err := New(srv.URL).LoginDevice(context.Background())
	if err != nil {
		t.Fatalf("LoginDevice: %v", err)
	}
	if da.UserCode != "ABCD-EFGH" || da.VerificationURI != "https://relay.test/device" ||
		da.DeviceCode != "dev-1" || da.Interval != 5 || da.ExpiresIn != 300 {
		t.Fatalf("device auth = %+v", da)
	}
}

func TestLoginPollPendingThenSuccess(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DeviceCode string `json:"device_code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.DeviceCode != "dev-1" {
			t.Errorf("device_code = %q", body.DeviceCode)
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"account_credential": "cred-xyz", "username": "alice",
		})
	}))
	defer srv.Close()
	c := New(srv.URL)

	if _, err := c.LoginPoll(context.Background(), "dev-1"); err != ErrAuthPending {
		t.Fatalf("first poll err = %v, want ErrAuthPending", err)
	}
	acc, err := c.LoginPoll(context.Background(), "dev-1")
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if acc.AccountCredential != "cred-xyz" || acc.Username != "alice" {
		t.Fatalf("account = %+v", acc)
	}
}

func TestCLILoginStartAndPoll(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/cli/start":
			_ = json.NewEncoder(w).Encode(map[string]string{"handle": "h-1", "user_code": "ABCD-1234"})
		case "/v1/login/cli/poll":
			var body struct {
				Handle string `json:"handle"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Handle != "h-1" {
				t.Errorf("handle = %q", body.Handle)
			}
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"account_credential": "cred-xyz", "username": "alice",
				"install_url": "https://github.com/apps/piper/installations/new",
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)

	handle, code, err := c.CLILoginStart(context.Background())
	if err != nil || handle != "h-1" || code != "ABCD-1234" {
		t.Fatalf("start = (%q,%q,%v)", handle, code, err)
	}
	if _, err := c.CLILoginPoll(context.Background(), "h-1"); err != ErrAuthPending {
		t.Fatalf("first poll err = %v, want ErrAuthPending", err)
	}
	acc, err := c.CLILoginPoll(context.Background(), "h-1")
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if acc.AccountCredential != "cred-xyz" || acc.Username != "alice" ||
		acc.InstallURL != "https://github.com/apps/piper/installations/new" {
		t.Fatalf("account = %+v", acc)
	}
}

func TestEnroll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cred-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"enrollment_token": "enr-1", "base_domain": "ab12-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
		})
	}))
	defer srv.Close()

	en, err := New(srv.URL).Enroll(context.Background(), "cred-xyz")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if en.EnrollmentToken != "enr-1" || en.BaseDomain != "ab12-alice.public.getpiper.co" ||
		en.TunnelEndpoint != "relay.getpiper.co:7000" {
		t.Fatalf("enrollment = %+v", en)
	}
}

func TestEnrollErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{{http.StatusUnauthorized, ErrBadCredential}, {http.StatusTooManyRequests, ErrQuotaExceeded}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.code)
		}))
		_, err := New(srv.URL).Enroll(context.Background(), "whatever")
		srv.Close()
		if err != tc.want {
			t.Fatalf("code %d err = %v, want %v", tc.code, err, tc.want)
		}
	}
}

func TestGitHubRepos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cred-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"repos": []map[string]any{
			{"full_name": "alice/one", "visibility": "public", "pushed_at": "2026-07-20T12:34:56Z"},
			{"full_name": "alice/two", "visibility": "private", "pushed_at": ""},
		}})
	}))
	defer srv.Close()

	repos, err := New(srv.URL).GitHubRepos(context.Background(), "cred-xyz")
	if err != nil {
		t.Fatalf("GitHubRepos: %v", err)
	}
	want := []Repo{
		{FullName: "alice/one", Visibility: "public", PushedAt: "2026-07-20T12:34:56Z"},
		{FullName: "alice/two", Visibility: "private", PushedAt: ""},
	}
	if len(repos) != len(want) || repos[0] != want[0] || repos[1] != want[1] {
		t.Fatalf("repos = %+v, want %+v", repos, want)
	}
}

func TestGitHubReposErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{{http.StatusNotFound, ErrNoInstallation}, {http.StatusInternalServerError, nil}, {http.StatusBadGateway, nil}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.code)
		}))
		_, err := New(srv.URL).GitHubRepos(context.Background(), "whatever")
		srv.Close()
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Fatalf("code %d err = %v, want %v", tc.code, err, tc.want)
			}
			continue
		}
		if err == nil || errors.Is(err, ErrNoInstallation) {
			t.Fatalf("code %d err = %v, want a non-nil, non-ErrNoInstallation error", tc.code, err)
		}
	}
}

// A cancelled context aborts an in-flight request promptly instead of waiting
// out the 30s client timeout — the cancellation seam #85/#95 asked for.
func TestRequestRespectsContextCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block // never respond until the test unblocks teardown
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(srv.URL).LoginDevice(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoginDevice err = %v, want context.Canceled", err)
	}
}
