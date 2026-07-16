package relay

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// hitLogin fires one request at a limited login endpoint from ip and returns
// the status code.
func hitLogin(t *testing.T, api http.Handler, method, target, ip string) int {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = ip + ":12345"
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	return rr.Code
}

func TestLoginDeviceRateLimited(t *testing.T) {
	api, _, _ := newTestAPI(t)
	for i := 0; i < loginLimitBurst; i++ {
		if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.1"); c != http.StatusOK {
			t.Fatalf("device login #%d = %d, want 200", i+1, c)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/login/device", nil)
	req.RemoteAddr = "203.0.113.1:12345"
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("burst+1 device login = %d, want 429", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "rate limit") {
		t.Fatalf("429 body = %q, want a short plain explanation", rr.Body.String())
	}
}

func TestLoginWebRateLimited(t *testing.T) {
	api, _ := newWebTestAPI(t)
	target := "/v1/login/web?redirect_uri=" + url.QueryEscape("https://dash.getpiper.co/auth")
	for i := 0; i < loginLimitBurst; i++ {
		if c := hitLogin(t, api, http.MethodGet, target, "203.0.113.2"); c != http.StatusFound {
			t.Fatalf("web login #%d = %d, want 302", i+1, c)
		}
	}
	if c := hitLogin(t, api, http.MethodGet, target, "203.0.113.2"); c != http.StatusTooManyRequests {
		t.Fatalf("burst+1 web login = %d, want 429", c)
	}
}

// Both unauthenticated login endpoints draw from the same per-IP bucket.
func TestLoginRateLimitSharedAcrossEndpoints(t *testing.T) {
	api, _ := newWebTestAPI(t)
	target := "/v1/login/web?redirect_uri=" + url.QueryEscape("https://dash.getpiper.co/auth")
	half := loginLimitBurst / 2
	for i := 0; i < half; i++ {
		hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.3")
		hitLogin(t, api, http.MethodGet, target, "203.0.113.3")
	}
	if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.3"); c != http.StatusTooManyRequests {
		t.Fatalf("device login after mixed burst = %d, want 429", c)
	}
	if c := hitLogin(t, api, http.MethodGet, target, "203.0.113.3"); c != http.StatusTooManyRequests {
		t.Fatalf("web login after mixed burst = %d, want 429", c)
	}
}

func TestLoginRateLimitPerIPIndependent(t *testing.T) {
	api, _, _ := newTestAPI(t)
	for i := 0; i < loginLimitBurst; i++ {
		hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.4")
	}
	if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.4"); c != http.StatusTooManyRequests {
		t.Fatalf("exhausted IP = %d, want 429", c)
	}
	if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.5"); c != http.StatusOK {
		t.Fatalf("fresh IP = %d, want 200", c)
	}
}

// The bucket refills over time: exhaust it, advance the limiter's clock past
// one refill interval, and the same IP is allowed again. No sleeps — the
// limiter's now func is the test seam.
func TestLoginRateLimitRefills(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	a := &api{st: st, v: NewFakeVerifier(), webStates: map[string]webState{}}
	fakeNow := time.Now()
	a.loginLimit.now = func() time.Time { return fakeNow }

	for i := 0; i < loginLimitBurst; i++ {
		if !a.loginLimit.allow("203.0.113.6") {
			t.Fatalf("request #%d rejected before burst exhausted", i+1)
		}
	}
	if a.loginLimit.allow("203.0.113.6") {
		t.Fatal("request past burst allowed, want limited")
	}
	fakeNow = fakeNow.Add(time.Minute / loginLimitPerMin) // exactly one token
	if !a.loginLimit.allow("203.0.113.6") {
		t.Fatal("request after one refill interval rejected, want allowed")
	}
}

// Idle buckets are evicted inline, mirroring the web-state sweep: after the
// idle TTL, a stale entry is gone and the map holds only active IPs.
func TestLoginRateLimitSweepsIdleBuckets(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	a := &api{st: st, v: NewFakeVerifier(), webStates: map[string]webState{}}
	fakeNow := time.Now()
	a.loginLimit.now = func() time.Time { return fakeNow }

	a.loginLimit.allow("203.0.113.7")
	fakeNow = fakeNow.Add(loginLimitMaxIdle + time.Minute)
	a.loginLimit.allow("203.0.113.8")

	a.loginLimit.mu.Lock()
	defer a.loginLimit.mu.Unlock()
	if _, ok := a.loginLimit.buckets["203.0.113.7"]; ok {
		t.Fatal("idle bucket not swept")
	}
	if len(a.loginLimit.buckets) != 1 {
		t.Fatalf("buckets size = %d, want 1 (only the active IP)", len(a.loginLimit.buckets))
	}
}
