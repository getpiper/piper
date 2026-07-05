package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/getpiper/piper/internal/source"
)

// makeTarball builds a gzipped tar with a single top-level dir "alice-blog-abc/"
// containing Dockerfile and app.py, mimicking GitHub's codeload format.
func makeTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"alice-blog-abc/Dockerfile": "FROM scratch\n",
		"alice-blog-abc/app.py":     "print('hi')\n",
	}
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestFetchStripsPrefix(t *testing.T) {
	tarball := makeTarball(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case r.URL.Path == "/repos/alice/blog/tarball/abc123":
			if got := r.Header.Get("Authorization"); got != "token ghs_x" {
				t.Errorf("tarball auth = %q", got)
			}
			w.Write(tarball)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p, err := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	ev := source.Event{Repo: "alice/blog", SHA: "abc123", InstallationID: 99}
	if err := p.Fetch(context.Background(), ev, dst); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Dockerfile must be at dst root, not under a nested dir.
	if _, err := os.Stat(filepath.Join(dst, "Dockerfile")); err != nil {
		t.Fatalf("Dockerfile not at root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "app.py")); err != nil {
		t.Fatalf("app.py not at root: %v", err)
	}
}
