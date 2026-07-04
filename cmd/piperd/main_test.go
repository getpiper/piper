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
	"strings"
	"testing"
	"time"
)

type fakeCertificateManager struct {
	cert, key []byte
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
