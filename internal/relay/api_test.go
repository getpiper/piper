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
