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
