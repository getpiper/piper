package caddy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpsertRoutePutsRouteByID(t *testing.T) {
	type req struct{ method, path, body string }
	var reqs []req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, req{r.Method, r.URL.Path, string(b)})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if err := c.UpsertRoute("blog.piper.localhost", 40001); err != nil {
		t.Fatalf("UpsertRoute: %v", err)
	}

	// The route must be addressed by its stable id somewhere (the delete-by-id).
	var sawID bool
	var post *req
	for i := range reqs {
		if strings.Contains(reqs[i].path, "piper-blog.piper.localhost") {
			sawID = true
		}
		if reqs[i].method == http.MethodPost {
			post = &reqs[i]
		}
	}
	if !sawID {
		t.Errorf("no request referenced the route id; got %+v", reqs)
	}
	if post == nil {
		t.Fatalf("no POST to append the route; got %+v", reqs)
	}
	// POST body must be valid JSON carrying the @id and the upstream.
	var route map[string]any
	if err := json.Unmarshal([]byte(post.body), &route); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, post.body)
	}
	if !strings.Contains(post.body, "127.0.0.1:40001") {
		t.Errorf("body missing upstream: %s", post.body)
	}
	if !strings.Contains(post.body, "piper-blog.piper.localhost") {
		t.Errorf("body missing route @id: %s", post.body)
	}
}

func TestEnsureHTTPSCreatesTLSAppAndServer(t *testing.T) {
	var puts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/config/apps/":
			// no tls app, no piper-tls server yet
			io.WriteString(w, `{"http":{"servers":{"piper":{"listen":[":80"]}}}}`)
		case r.Method == http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			puts = append(puts, r.URL.Path+" "+string(b))
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	if err := NewClient(ts.URL).EnsureHTTPS(":8443"); err != nil {
		t.Fatalf("EnsureHTTPS: %v", err)
	}
	if len(puts) != 2 {
		t.Fatalf("puts = %v, want tls app + piper-tls server", puts)
	}
	if !strings.Contains(puts[0], "/config/apps/tls ") || !strings.Contains(puts[0], "load_pem") {
		t.Fatalf("first put = %q, want tls app", puts[0])
	}
	if !strings.Contains(puts[1], "/config/apps/http/servers/piper-tls ") ||
		!strings.Contains(puts[1], `":8443"`) ||
		!strings.Contains(puts[1], "tls_connection_policies") {
		t.Fatalf("second put = %q, want piper-tls server on :8443", puts[1])
	}
}

func TestEnsureHTTPSIsIdempotent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/config/apps/" {
			io.WriteString(w, `{"http":{"servers":{"piper":{},"piper-tls":{}}},"tls":{}}`)
			return
		}
		t.Errorf("unexpected write %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	if err := NewClient(ts.URL).EnsureHTTPS(":8443"); err != nil {
		t.Fatalf("EnsureHTTPS: %v", err)
	}
}

func TestUpsertRouteTLSTargetsTLSServer(t *testing.T) {
	var postPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound) // no prior route
			return
		}
		if r.Method == http.MethodPost {
			postPath = r.URL.Path
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if err := NewClient(ts.URL).UpsertRouteTLS("blog.example.com", 40001); err != nil {
		t.Fatalf("UpsertRouteTLS: %v", err)
	}
	if postPath != "/config/apps/http/servers/piper-tls/routes" {
		t.Fatalf("posted to %q, want piper-tls routes", postPath)
	}
}
