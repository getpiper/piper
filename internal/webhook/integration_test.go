package webhook_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/source/github"
	"github.com/getpiper/piper/internal/webhook"
)

// gzTarball returns a gzipped tar with a single top-level dir wrapping a
// Dockerfile, mimicking GitHub's codeload tarball so the real provider's Fetch
// extracts cleanly.
func gzTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := "FROM scratch\n"
	if err := tw.WriteHeader(&tar.Header{
		Name: "alice-blog-deadbeef/Dockerfile", Mode: 0o644,
		Size: int64(len(content)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestWebhookIntegrationRealProvider(t *testing.T) {
	// Stub GitHub API: installation token + deployments + statuses. The success
	// path is proven by the /statuses branch being hit (any unexpected path fails).
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			io.WriteString(w, `{"token":"ghs_x"}`)
		case strings.HasSuffix(r.URL.Path, "/deployments") && r.Method == http.MethodPost:
			w.WriteHeader(201)
			io.WriteString(w, `{"id":1}`)
		case strings.HasSuffix(r.URL.Path, "/deployments") && r.Method == http.MethodGet:
			io.WriteString(w, `[{"id":1}]`)
		case strings.Contains(r.URL.Path, "/tarball/"):
			w.Write(gzTarball(t))
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	prov, err := github.New(github.Config{
		AppID: 1, PrivateKeyPEM: keyPEM, WebhookSecret: "whsec", APIBase: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")

	d := &fakeDeployer{}
	h := webhook.New(prov, s, d, "alice.dev")

	body := `{"ref":"refs/heads/main","after":"deadbeef","repository":{"full_name":"alice/blog"},"installation":{"id":99}}`
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()

	if d.count() != 1 {
		t.Fatalf("deploy calls = %d", d.count())
	}
}
