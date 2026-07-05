// Package webhook receives a git host's signed webhook, resolves the bound app,
// and drives the deployer. It is the only public surface exposed through the
// tunnel; everything else in piperd stays loopback-only.
package webhook

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/getpiper/piper/internal/source"
	"github.com/getpiper/piper/internal/store"
)

const maxBody = 5 << 20 // 5 MiB

type Deployer interface {
	Deploy(ctx context.Context, app, srcDir string) (store.Deployment, error)
}

type Handler struct {
	prov    source.Provider
	store   *store.Store
	deploy  Deployer
	baseDom string

	wg      sync.WaitGroup
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	lastSHA map[string]string
}

func New(p source.Provider, s *store.Store, d Deployer, baseDomain string) *Handler {
	return &Handler{
		prov: p, store: s, deploy: d, baseDom: baseDomain,
		locks: map[string]*sync.Mutex{}, lastSHA: map[string]string{},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	ev, err := h.prov.Parse(r.Header, body)
	if errors.Is(err, source.ErrBadSignature) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if ev.Kind == source.KindPing {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "pong")
		return
	}
	w.WriteHeader(http.StatusAccepted)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.process(context.Background(), ev)
	}()
}

// Wait blocks until in-flight deploys finish. Test-only.
func (h *Handler) Wait() { h.wg.Wait() }

func (h *Handler) appLock(name string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.locks[name]
	if !ok {
		m = &sync.Mutex{}
		h.locks[name] = m
	}
	return m
}

func (h *Handler) process(ctx context.Context, ev source.Event) {
	if ev.Kind != source.KindPush {
		return // this slice acts only on push
	}
	app, err := h.store.AppByRepo(ev.Repo)
	if errors.Is(err, store.ErrNotFound) {
		log.Printf("webhook: no app bound to %s", ev.Repo)
		return
	}
	if err != nil {
		log.Printf("webhook: lookup %s: %v", ev.Repo, err)
		return
	}
	if ev.Ref != "refs/heads/"+app.Branch {
		log.Printf("webhook: %s ref %s != tracked %s", ev.Repo, ev.Ref, app.Branch)
		return
	}

	lock := h.appLock(app.Name)
	lock.Lock()
	defer lock.Unlock()

	h.mu.Lock()
	dup := h.lastSHA[app.Name] == ev.SHA
	h.mu.Unlock()
	if dup {
		log.Printf("webhook: %s already at %s, skipping", app.Name, ev.SHA)
		return
	}

	_ = h.prov.Report(ctx, ev, source.StatusPending, "")

	dir, err := os.MkdirTemp("", "piper-src-*")
	if err != nil {
		log.Printf("webhook: tmpdir: %v", err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	defer os.RemoveAll(dir)

	if err := h.prov.Fetch(ctx, ev, dir); err != nil {
		log.Printf("webhook: fetch %s@%s: %v", ev.Repo, ev.SHA, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	if _, err := h.deploy.Deploy(ctx, app.Name, dir); err != nil {
		log.Printf("webhook: deploy %s: %v", app.Name, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}

	url := "https://" + app.Name + "." + h.baseDom
	_ = h.prov.Report(ctx, ev, source.StatusSuccess, url)

	h.mu.Lock()
	h.lastSHA[app.Name] = ev.SHA
	h.mu.Unlock()
}
