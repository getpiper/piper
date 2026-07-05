package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCertificateManager struct {
	cert, key []byte
}

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

func (f fakeCertificateManager) Obtain([]string) ([]byte, []byte, error) {
	return f.cert, f.key, nil
}

type fakeCertificateReplacer struct {
	cert, key string
	called    chan struct{}
}

func (f *fakeCertificateReplacer) ReplaceCert(cert, key string) error {
	f.cert, f.key = cert, key
	close(f.called)
	return nil
}

func expiringCert(t *testing.T, now time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "example.com"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestRunRenewLoopReplacesCertificate(t *testing.T) {
	now := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	mgr := fakeCertificateManager{cert: []byte("NEW CERT"), key: []byte("NEW KEY")}
	replacer := &fakeCertificateReplacer{called: make(chan struct{})}
	ticks := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	currentCert := expiringCert(t, now)
	go func() {
		runRenewLoop(ctx, mgr, replacer, "example.com", currentCert, ticks, func() time.Time { return now })
		close(done)
	}()
	ticks <- now
	select {
	case <-replacer.called:
	case <-time.After(time.Second):
		t.Fatal("certificate was not replaced")
	}
	if replacer.cert != "NEW CERT" || replacer.key != "NEW KEY" {
		t.Fatalf("replacement = %q, %q", replacer.cert, replacer.key)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renew loop did not stop")
	}
}
