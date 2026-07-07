package relayclient

import (
	"encoding/json"
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

	da, err := New(srv.URL).LoginDevice()
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
		var body struct{ DeviceCode string `json:"device_code"` }
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

	if _, err := c.LoginPoll("dev-1"); err != ErrAuthPending {
		t.Fatalf("first poll err = %v, want ErrAuthPending", err)
	}
	acc, err := c.LoginPoll("dev-1")
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if acc.AccountCredential != "cred-xyz" || acc.Username != "alice" {
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

	en, err := New(srv.URL).Enroll("cred-xyz")
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
		_, err := New(srv.URL).Enroll("whatever")
		srv.Close()
		if err != tc.want {
			t.Fatalf("code %d err = %v, want %v", tc.code, err, tc.want)
		}
	}
}
