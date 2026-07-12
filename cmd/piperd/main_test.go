package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/store"
	"github.com/getpiper/piper/internal/version"
)

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type recServer struct{ rec *recorder }

func (s *recServer) Shutdown(context.Context) error { s.rec.add("api-shutdown"); return nil }
func (s *recServer) Close() error                   { s.rec.add("api-close"); return nil }

type recWebhook struct {
	rec     *recorder
	drained bool
}

func (w *recWebhook) stop(context.Context)      { w.rec.add("webhook-stop") }
func (w *recWebhook) close()                    { w.rec.add("webhook-close") }
func (w *recWebhook) wait(context.Context) bool { w.rec.add("webhook-wait"); return w.drained }
func (w *recWebhook) cancel()                   { w.rec.add("webhook-cancel") }

type recManager struct{ rec *recorder }

func (m *recManager) Stop() { m.rec.add("caddy") }

type recStore struct{ rec *recorder }

func (s *recStore) Close() error { s.rec.add("store"); return nil }

func TestShutdownDrainsBeforeInfrastructureTeardown(t *testing.T) {
	rec := &recorder{}
	shutdownWithTimeouts(
		&recServer{rec}, &recWebhook{rec: rec, drained: true},
		&recManager{rec}, &recStore{rec}, time.Second, 2*time.Second,
	)
	got := rec.snapshot()
	if len(got) != 6 {
		t.Fatalf("events = %v, want 6 events", got)
	}
	first := map[string]bool{got[0]: true, got[1]: true}
	if !first["api-shutdown"] || !first["webhook-stop"] {
		t.Fatalf("first events = %v, want API and webhook stop", got[:2])
	}
	wantTail := []string{"webhook-wait", "webhook-cancel", "caddy", "store"}
	if !reflect.DeepEqual(got[2:], wantTail) {
		t.Fatalf("tail = %v, want %v", got[2:], wantTail)
	}
}

type blockingWebhook struct {
	rec   *recorder
	waits int
}

func (w *blockingWebhook) stop(context.Context) { w.rec.add("webhook-stop") }
func (w *blockingWebhook) close()               { w.rec.add("webhook-close") }
func (w *blockingWebhook) cancel()              { w.rec.add("webhook-cancel") }

func (w *blockingWebhook) wait(ctx context.Context) bool {
	w.waits++
	w.rec.add("webhook-wait")
	if w.waits == 1 {
		<-ctx.Done()
		return false
	}
	return true
}

func TestShutdownCancelsAtDrainDeadlineAndStillTearsDown(t *testing.T) {
	rec := &recorder{}
	started := time.Now()
	shutdownWithTimeouts(
		&recServer{rec}, &blockingWebhook{rec: rec},
		&recManager{rec}, &recStore{rec}, 20*time.Millisecond, 100*time.Millisecond,
	)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown took %v, want below 500ms", elapsed)
	}
	got := rec.snapshot()
	for _, want := range []string{"api-close", "webhook-close", "webhook-cancel", "caddy", "store"} {
		found := false
		for _, event := range got {
			found = found || event == want
		}
		if !found {
			t.Errorf("events = %v, missing %q", got, want)
		}
	}
}

func TestShutdownSkipsAbsentDependencies(t *testing.T) {
	rec := &recorder{}
	shutdownWithTimeouts(&recServer{rec}, nil, nil, &recStore{rec}, time.Second, 2*time.Second)
	got := rec.snapshot()
	want := []string{"api-shutdown", "store"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestNewDNSProviderRejectsUnsupportedProvider(t *testing.T) {
	provider, err := newDNSProvider("route53")
	if provider != nil {
		t.Fatalf("provider = %T, want nil", provider)
	}
	if err == nil || !strings.Contains(err.Error(), `unsupported DNS provider "route53"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewDNSProviderSelectsCloudflare(t *testing.T) {
	t.Setenv("CF_DNS_API_TOKEN", "test-token")
	for _, name := range []string{"", "cloudflare"} {
		t.Run(name, func(t *testing.T) {
			provider, err := newDNSProvider(name)
			if err != nil {
				t.Fatalf("newDNSProvider(%q): %v", name, err)
			}
			if provider == nil {
				t.Fatalf("newDNSProvider(%q) returned nil", name)
			}
		})
	}
}

type fakeProvisionStore struct {
	tokens  []store.Token
	created []string
	deleted []string
}

func (f *fakeProvisionStore) ListTokens() ([]store.Token, error) { return f.tokens, nil }
func (f *fakeProvisionStore) CreateToken(label, scope string) (string, error) {
	f.created = append(f.created, label+"/"+scope)
	return "tok-" + label, nil
}
func (f *fakeProvisionStore) DeleteToken(label string) error {
	f.deleted = append(f.deleted, label)
	return nil
}

func TestProvisionRelayControlFirstConnect(t *testing.T) {
	f := &fakeProvisionStore{}
	var pushed string
	provisionRelayControl(f, func(tok string) error { pushed = tok; return nil }, "base.example.com")
	if len(f.created) != 1 || f.created[0] != "relay:base.example.com/admin" {
		t.Fatalf("created = %v", f.created)
	}
	if pushed != "tok-relay:base.example.com" {
		t.Fatalf("pushed = %q", pushed)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("unexpected delete: %v", f.deleted)
	}
}

func TestProvisionRelayControlAlreadyProvisioned(t *testing.T) {
	f := &fakeProvisionStore{tokens: []store.Token{{Label: "relay:base.example.com"}}}
	provisionRelayControl(f, func(string) error { t.Fatal("must not push"); return nil }, "base.example.com")
	if len(f.created) != 0 {
		t.Fatalf("re-minted: %v", f.created)
	}
}

func TestProvisionRelayControlRevokedMeansNo(t *testing.T) {
	// A revoked row is the owner's unilateral cutoff: never re-mint for this
	// enrollment (spec: re-provisioning requires a new claim → new base domain).
	rt := time.Now()
	f := &fakeProvisionStore{tokens: []store.Token{{Label: "relay:base.example.com", RevokedAt: &rt}}}
	provisionRelayControl(f, func(string) error { t.Fatal("must not push"); return nil }, "base.example.com")
	if len(f.created) != 0 {
		t.Fatalf("re-minted after owner revoke: %v", f.created)
	}
}

func TestProvisionRelayControlPushFailureUnwinds(t *testing.T) {
	f := &fakeProvisionStore{}
	provisionRelayControl(f, func(string) error { return errors.New("session died") }, "base.example.com")
	// The mint must be unwound so the marker doesn't block the next attempt.
	if len(f.deleted) != 1 || f.deleted[0] != "relay:base.example.com" {
		t.Fatalf("deleted = %v, want the just-minted label", f.deleted)
	}
}

// piperd/piper-relay must have a version surface so the release ldflags stamp
// lands and the binary can report its build. #61.
func TestVersionRequested(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		if !versionRequested(args) {
			t.Errorf("versionRequested(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{nil, {"token"}, {"serve"}} {
		if versionRequested(args) {
			t.Errorf("versionRequested(%v) = true, want false", args)
		}
	}
	if version.String() == "" {
		t.Error("version.String() is empty")
	}
}
