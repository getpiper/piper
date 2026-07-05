package webhook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/getpiper/piper/internal/source"
	"github.com/getpiper/piper/internal/store"
	"github.com/getpiper/piper/internal/webhook"
)

type fakeProvider struct {
	mu       sync.Mutex
	parseErr error
	ev       source.Event
	reports  []source.Status
	fetchErr error
}

func (f *fakeProvider) Parse(http.Header, []byte) (source.Event, error) {
	return f.ev, f.parseErr
}
func (f *fakeProvider) Fetch(context.Context, source.Event, string) error { return f.fetchErr }
func (f *fakeProvider) Report(_ context.Context, _ source.Event, s source.Status, _ string) error {
	f.mu.Lock()
	f.reports = append(f.reports, s)
	f.mu.Unlock()
	return nil
}
func (f *fakeProvider) statuses() []source.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]source.Status(nil), f.reports...)
}

type fakeDeployer struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (d *fakeDeployer) Deploy(context.Context, string, string) (store.Deployment, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return store.Deployment{}, d.err
}
func (d *fakeDeployer) count() int { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func post(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	h.ServeHTTP(rec, req)
	return rec
}

func TestBadSignatureReturns401(t *testing.T) {
	p := &fakeProvider{parseErr: source.ErrBadSignature}
	d := &fakeDeployer{}
	h := webhook.New(p, newStore(t), d, "piper.localhost")
	if rec := post(h); rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.count() != 0 {
		t.Fatal("deploy must not run on bad signature")
	}
}

func TestUnknownRepoNoOp(t *testing.T) {
	p := &fakeProvider{ev: source.Event{Kind: source.KindPush, Repo: "nobody/x", Ref: "refs/heads/main"}}
	d := &fakeDeployer{}
	h := webhook.New(p, newStore(t), d, "piper.localhost")
	rec := post(h)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.count() != 0 {
		t.Fatal("deploy must not run for unknown repo")
	}
}

func TestPushDeploysAndReports(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main", SHA: "s1",
	}}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")

	if rec := post(h); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.count() != 1 {
		t.Fatalf("deploy calls = %d", d.count())
	}
	got := p.statuses()
	if len(got) != 2 || got[0] != source.StatusPending || got[1] != source.StatusSuccess {
		t.Fatalf("statuses = %v", got)
	}
}

func TestWrongBranchNoOp(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/dev", SHA: "s1",
	}}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	if d.count() != 0 {
		t.Fatal("deploy must not run for non-tracked branch")
	}
}

func TestDeployFailureReportsFailure(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main", SHA: "s1",
	}}
	d := &fakeDeployer{err: context.DeadlineExceeded}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	got := p.statuses()
	if len(got) != 2 || got[1] != source.StatusFailure {
		t.Fatalf("statuses = %v", got)
	}
}
