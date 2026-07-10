package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireTokenRejectsAndAccepts(t *testing.T) {
	s := newTestStore(t)
	tok, err := s.CreateToken("cli", "admin")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
	h := RequireToken(s, next)

	cases := []struct {
		name      string
		header    string
		wantCode  int
		wantReach bool
	}{
		{"no header", "", http.StatusUnauthorized, false},
		{"bad token", "Bearer nope", http.StatusUnauthorized, false},
		{"not bearer", "Basic xyz", http.StatusUnauthorized, false},
		{"valid", "Bearer " + tok, http.StatusOK, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantCode || reached != c.wantReach {
				t.Fatalf("code=%d reached=%v, want code=%d reached=%v",
					rec.Code, reached, c.wantCode, c.wantReach)
			}
		})
	}
}

func TestDeploymentEndpointsRequireToken(t *testing.T) {
	s := newTestStore(t)
	h := RequireToken(s, New(s, &fakeDeployer{}, "piper.localhost", "", nil, nil))
	for _, path := range []string{
		"/v1/apps/blog/deployments",
		"/v1/apps/blog/deployments/dep1/logs",
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", path, rr.Code)
		}
	}
}
