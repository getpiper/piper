package relay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturingDeliverer struct {
	mu    sync.Mutex
	calls []Binding
	done  chan struct{}
}

func (c *capturingDeliverer) Deliver(_ context.Context, b Binding, _ string, _ []byte) error {
	c.mu.Lock()
	c.calls = append(c.calls, b)
	c.mu.Unlock()
	select {
	case c.done <- struct{}{}:
	default:
	}
	return nil
}

func (c *capturingDeliverer) seen() []Binding {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Binding(nil), c.calls...)
}

func signed(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func postEvent(t *testing.T, h http.Handler, event, sig, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/gh", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newTestIngress(t *testing.T, st *Store, d Deliverer) http.Handler {
	t.Helper()
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewGitHubIngress(st, app, d)
}

func TestIngressRejectsBadSignature(t *testing.T) {
	st := openTestStore(t)
	d := &capturingDeliverer{done: make(chan struct{}, 8)}
	h := newTestIngress(t, st, d)

	rec := postEvent(t, h, "push", "sha256=deadbeef", `{}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(d.seen()) != 0 {
		t.Fatal("delivered despite bad signature")
	}
}

func TestIngressRoutesPushToBinding(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}

	d := &capturingDeliverer{done: make(chan struct{}, 8)}
	h := newTestIngress(t, st, d)

	body := `{"ref":"refs/heads/main","after":"abc",` +
		`"repository":{"full_name":"alice/blog"},"installation":{"id":55}}`
	rec := postEvent(t, h, "push", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery within 2s")
	}
	seen := d.seen()
	if len(seen) != 1 || seen[0].AgentName != agent || seen[0].App != "blog" {
		t.Fatalf("delivered = %+v", seen)
	}
}

func TestIngressDropsUnlinkedInstallation(t *testing.T) {
	st := openTestStore(t)
	d := &capturingDeliverer{done: make(chan struct{}, 8)}
	h := newTestIngress(t, st, d)

	body := `{"ref":"refs/heads/main","repository":{"full_name":"alice/blog"},"installation":{"id":999}}`
	rec := postEvent(t, h, "push", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	time.Sleep(100 * time.Millisecond)
	if len(d.seen()) != 0 {
		t.Fatal("routed an event for an unlinked installation")
	}
}

func TestIngressLinksAndUnlinksInstallation(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.UpsertAccount("1001", "alice"); err != nil {
		t.Fatal(err)
	}
	h := newTestIngress(t, st, &capturingDeliverer{done: make(chan struct{}, 8)})

	created := `{"action":"created","installation":{"id":55,` +
		`"account":{"type":"User","login":"alice"}},"sender":{"id":1001,"login":"alice"}}`
	if rec := postEvent(t, h, "installation", signed("s3cret", []byte(created)), created); rec.Code != http.StatusAccepted {
		t.Fatalf("created status = %d", rec.Code)
	}
	if _, err := st.AccountForInstallation("55"); err != nil {
		t.Fatalf("installation not linked: %v", err)
	}

	deleted := fmt.Sprintf(`{"action":"deleted","installation":{"id":55,`+
		`"account":{"type":"User","login":"alice"}},"sender":{"id":%d,"login":"alice"}}`, 1001)
	if rec := postEvent(t, h, "installation", signed("s3cret", []byte(deleted)), deleted); rec.Code != http.StatusAccepted {
		t.Fatalf("deleted status = %d", rec.Code)
	}
	if _, err := st.AccountForInstallation("55"); err == nil {
		t.Fatal("installation survived deletion")
	}
}

func TestIngressPongsPing(t *testing.T) {
	st := openTestStore(t)
	h := newTestIngress(t, st, &capturingDeliverer{done: make(chan struct{}, 8)})
	body := `{"zen":"hi"}`
	rec := postEvent(t, h, "ping", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
