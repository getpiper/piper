package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAPI(t *testing.T) (http.Handler, *Store, *FakeVerifier) {
	t.Helper()
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3)
	fv := NewFakeVerifier()
	return NewAPI(st, fv), st, fv
}

func TestLoginDeviceThenPoll(t *testing.T) {
	api, _, fv := newTestAPI(t)

	// Start device flow.
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/device", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("device status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var dev struct {
		UserCode   string `json:"user_code"`
		DeviceCode string `json:"device_code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &dev); err != nil {
		t.Fatal(err)
	}
	if dev.UserCode == "" || dev.DeviceCode == "" {
		t.Fatalf("empty device response: %+v", dev)
	}

	// Poll before approval → 202 pending.
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"`+dev.DeviceCode+`"}`)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("pending poll status = %d, want 202", rr.Code)
	}

	// Approve, then poll → 200 with a credential.
	fv.Approve(dev.DeviceCode, Identity{Subject: "sub-1", Email: "ivan@x.com"})
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"`+dev.DeviceCode+`"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("success poll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var ok struct {
		AccountCredential string `json:"account_credential"`
		Username          string `json:"username"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ok); err != nil {
		t.Fatal(err)
	}
	if ok.AccountCredential == "" || ok.Username != "ivan" {
		t.Fatalf("poll success body = %+v", ok)
	}
}

func TestLoginPollUnknownHandle(t *testing.T) {
	api, _, _ := newTestAPI(t)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"nope"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown-handle poll status = %d, want 400", rr.Code)
	}
}

func TestEnrollWithAccountCredential(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay.getpiper.co:7000")

	acc, _ := st.UpsertAccount("sub-1", "judy@x.com")
	cred, _ := st.MintAccountCredential(acc.ID)

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		EnrollmentToken string `json:"enrollment_token"`
		BaseDomain      string `json:"base_domain"`
		TunnelEndpoint  string `json:"tunnel_endpoint"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.EnrollmentToken == "" {
		t.Fatal("empty enrollment token")
	}
	if !strings.HasSuffix(out.BaseDomain, "-judy.public.getpiper.co") {
		t.Fatalf("base domain = %q", out.BaseDomain)
	}
	if out.TunnelEndpoint != "relay.getpiper.co:7000" {
		t.Fatalf("tunnel endpoint = %q", out.TunnelEndpoint)
	}
}

func TestEnrollRejectsBadCredential(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay:7000")

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad-cred enroll status = %d, want 401", rr.Code)
	}
}

func TestEnrollOverCapReturns429(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 1)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay:7000")
	acc, _ := st.UpsertAccount("sub-1", "ken@x.com")
	cred, _ := st.MintAccountCredential(acc.ID)

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
		req.Header.Set("Authorization", "Bearer "+cred)
		rr := httptest.NewRecorder()
		api.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := do(); c != http.StatusOK {
		t.Fatalf("first enroll = %d, want 200", c)
	}
	if c := do(); c != http.StatusTooManyRequests {
		t.Fatalf("over-cap enroll = %d, want 429", c)
	}
}
