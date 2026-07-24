package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/source"
)

// stubTokens records the event it was asked about and returns a canned token.
type stubTokens struct {
	tok     string
	gotRepo string
}

func (s *stubTokens) Token(_ context.Context, ev source.Event) (string, error) {
	s.gotRepo = ev.Repo
	return s.tok, nil
}

func TestProviderWithTokenSourceFetches(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write(makeTarball(t))
	}))
	defer srv.Close()

	ts := &stubTokens{tok: "brokered-tok"}
	p := NewWithTokens(Config{WebhookSecret: "s", APIBase: srv.URL}, ts)

	dir := t.TempDir()
	ev := source.Event{Repo: "alice/blog", SHA: "abc"}
	if err := p.Fetch(context.Background(), ev, dir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAuth != "token brokered-tok" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "token brokered-tok")
	}
	if ts.gotRepo != "alice/blog" {
		t.Fatalf("TokenSource saw repo %q, want alice/blog", ts.gotRepo)
	}
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		t.Fatalf("Dockerfile not extracted: %v", err)
	}
}
