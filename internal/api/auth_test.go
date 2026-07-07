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
