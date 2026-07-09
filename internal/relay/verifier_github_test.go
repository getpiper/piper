package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeGitHub fakes github.com (device code + token) and api.github.com (/user)
// on one httptest server. Poll responses are scripted via tokenResponses.
type fakeGitHub struct {
	t *testing.T

	mu             sync.Mutex
	tokenResponses []map[string]any // popped one per access_token poll
	tokenForms     []map[string]string
	userCalls      int
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("client_id") != "test-client" {
			f.t.Errorf("device/code client_id = %q", r.FormValue("client_id"))
		}
		if r.FormValue("scope") != "" {
			f.t.Errorf("device/code sent scope %q, want none", r.FormValue("scope"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-1", "user_code": "ABCD-1234",
			"verification_uri": "https://github.test/login/device",
			"expires_in":       900, "interval": 5,
		})
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for k := range r.Form {
			form[k] = r.FormValue(k)
		}
		f.mu.Lock()
		f.tokenForms = append(f.tokenForms, form)
		var resp map[string]any
		if len(f.tokenResponses) > 0 {
			resp = f.tokenResponses[0]
			f.tokenResponses = f.tokenResponses[1:]
		} else {
			resp = map[string]any{"error": "authorization_pending"}
		}
		f.mu.Unlock()
		// GitHub returns poll errors in 200-OK bodies.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gho_tok" {
			f.t.Errorf("/user Authorization = %q", got)
		}
		f.mu.Lock()
		f.userCalls++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 583231, "login": "Octo-Cat"})
	})
	return mux
}

// newTestGitHubVerifier points a GitHubVerifier at the fake and makes sleeps
// instant, recording requested durations.
func newTestGitHubVerifier(t *testing.T, fake *fakeGitHub) (*GitHubVerifier, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	v := NewGitHubVerifier("test-client", "test-secret")
	v.oauthBase = srv.URL
	v.apiBase = srv.URL
	var slept []time.Duration
	var mu sync.Mutex
	v.sleep = func(d time.Duration) { mu.Lock(); slept = append(slept, d); mu.Unlock() }
	return v, &slept
}

// waitDone polls the verifier until the flow completes or times out.
func waitDone(t *testing.T, v *GitHubVerifier, handle string) (Identity, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		id, err := v.Poll(context.Background(), handle)
		if err != ErrAuthPending {
			return id, err
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("flow never completed")
	return Identity{}, nil
}

func TestGitHubDeviceFlowApproved(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "authorization_pending"},
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v, _ := newTestGitHubVerifier(t, fake)

	handle, da, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if da.UserCode != "ABCD-1234" || da.VerificationURI != "https://github.test/login/device" ||
		da.Interval != 5 || da.ExpiresIn != 900 {
		t.Fatalf("DeviceAuth = %+v", da)
	}

	id, err := waitDone(t, v, handle)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if id.Subject != "583231" || id.Login != "Octo-Cat" {
		t.Fatalf("identity = %+v", id)
	}
	// The poll used the device grant.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.tokenForms) == 0 ||
		fake.tokenForms[0]["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" ||
		fake.tokenForms[0]["device_code"] != "dc-1" {
		t.Fatalf("token forms = %+v", fake.tokenForms)
	}
}

func TestGitHubDeviceFlowSlowDown(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "slow_down"},
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v, slept := newTestGitHubVerifier(t, fake)

	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := waitDone(t, v, handle); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// First sleep at the server interval (5s), then slow_down adds 5s (GitHub semantics).
	if len(*slept) < 2 || (*slept)[0] != 5*time.Second || (*slept)[1] != 10*time.Second {
		t.Fatalf("sleeps = %v, want [5s 10s ...]", *slept)
	}
}

func TestGitHubDeviceFlowDenied(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "access_denied"},
	}}
	v, _ := newTestGitHubVerifier(t, fake)

	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := waitDone(t, v, handle); err == nil || err == ErrAuthPending {
		t.Fatalf("denied flow err = %v, want terminal error", err)
	}
}

func TestGitHubVerifierPollUnknownHandle(t *testing.T) {
	v := NewGitHubVerifier("test-client", "test-secret")
	if _, err := v.Poll(context.Background(), "never-started"); err == nil {
		t.Fatal("Poll(unknown) succeeded, want error")
	}
}
