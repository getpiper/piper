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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/getpiper/piper/internal/deploy"
	"github.com/getpiper/piper/internal/runtime"
	"github.com/getpiper/piper/internal/source/github"
	"github.com/getpiper/piper/internal/store"
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
	s.UpdateAppRepo("blog", "alice/blog", "main", "")

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

type recordingCaddy struct {
	mu      sync.Mutex
	routes  map[string]int
	removed map[string]bool
}

func newRecordingCaddy() *recordingCaddy {
	return &recordingCaddy{routes: map[string]int{}, removed: map[string]bool{}}
}
func (c *recordingCaddy) UpsertRoute(host string, port int) error {
	c.mu.Lock()
	c.routes[host] = port
	c.mu.Unlock()
	return nil
}
func (c *recordingCaddy) UpsertRouteTLS(host string, port int) error {
	c.mu.Lock()
	c.routes[host] = port
	c.mu.Unlock()
	return nil
}
func (c *recordingCaddy) RemoveRoute(host string) error {
	c.mu.Lock()
	delete(c.routes, host)
	c.removed[host] = true
	c.mu.Unlock()
	return nil
}
func (c *recordingCaddy) has(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.routes[host]
	return ok
}
func (c *recordingCaddy) wasRemoved(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.removed[host]
}

func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

func emptyTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func postEvent(t *testing.T, h http.Handler, event, secret, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%s: code = %d", event, rec.Code)
	}
}

func TestPRPreviewLifecycleEndToEnd(t *testing.T) {
	const secret = "s3cr3t"

	// GitHub API stub: tokens, deployment create/list, statuses, tarball.
	// Status bodies are asserted at unit level (Tasks 5/6); here we only need
	// the API to succeed so the lifecycle runs end to end.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			io.WriteString(w, `{"token":"ghs_x"}`)
		case r.URL.Path == "/repos/alice/blog/deployments" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			io.WriteString(w, `{"id":555}`)
		case r.URL.Path == "/repos/alice/blog/deployments" && r.Method == http.MethodGet:
			io.WriteString(w, `[{"id":555}]`)
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "/tarball/"):
			w.Write(emptyTarball(t))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	prov, err := github.New(github.Config{
		AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: secret, APIBase: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")

	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img"},
		RunResultVal:   runtime.RunResult{ContainerID: "prev-c", HostPort: 40010},
	}
	caddy := newRecordingCaddy()
	dep := deploy.New(s, rt, caddy, "piper.localhost")
	h := webhook.New(prov, s, dep, "piper.localhost")

	const host = "pr-9-blog.piper.localhost"

	openBody := `{"action":"opened","number":9,"pull_request":{"head":{"ref":"feat","sha":"sha9"}},"repository":{"full_name":"alice/blog"},"installation":{"id":99}}`
	postEvent(t, h, "pull_request", secret, openBody)
	h.Wait()
	if !caddy.has(host) {
		t.Fatalf("after open, route %s missing; routes=%v", host, caddy.routes)
	}
	if _, err := s.PreviewRunning("blog", 9); err != nil {
		t.Fatalf("PreviewRunning after open: %v", err)
	}

	closeBody := `{"action":"closed","number":9,"pull_request":{"head":{"ref":"feat","sha":"sha9"}},"repository":{"full_name":"alice/blog"},"installation":{"id":99}}`
	postEvent(t, h, "pull_request", secret, closeBody)
	h.Wait()
	if caddy.has(host) || !caddy.wasRemoved(host) {
		t.Fatalf("after close, route %s should be removed; routes=%v removed=%v", host, caddy.routes, caddy.removed)
	}
	if _, err := s.PreviewRunning("blog", 9); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PreviewRunning after close err = %v, want ErrNotFound", err)
	}
}
