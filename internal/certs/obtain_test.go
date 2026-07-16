package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"

	"github.com/go-acme/lego/v4/challenge/dns01"
)

// fakeDNS is a no-op DNS-01 provider; it works against Pebble's challtestsrv
// only when that server is configured to answer. Used to exercise the lego
// wiring end to end when RUN_ACME=1 points at a Pebble directory.
type fakeDNS struct{}

func (fakeDNS) Present(domain, token, keyAuth string) error { return nil }
func (fakeDNS) CleanUp(domain, token, keyAuth string) error { return nil }

func TestObtainAgainstPebble(t *testing.T) {
	dir := os.Getenv("RUN_ACME")
	if dir == "" {
		t.Skip("set RUN_ACME=<pebble directory URL> with a reachable Pebble + DNS to run")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	m, err := New(Config{
		Email:       "e2e@example.com",
		CADirURL:    dir,
		DNSProvider: fakeDNS{},
		AccountKey:  key,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = dns01.DefaultPropagationTimeout // ensure dns01 import is used
	certPEM, keyPEM, err := m.Obtain([]string{"alice.example.com"})
	if err != nil {
		t.Fatalf("Obtain: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("empty cert/key")
	}
}

// TestObtainTLSALPNAgainstPebble issues an exact-host cert with only the
// ALPN solver configured — no DNS provider (the #226 acceptance criterion).
// Pebble's validator dials the domain at its configured tlsPort (default
// 5001), so the solver listens there; point RUN_ACME at a Pebble whose DNS
// resolves test domains to 127.0.0.1 (e.g. pebble-challtestsrv) and trust
// Pebble's CA via LEGO_CA_CERTIFICATES.
func TestObtainTLSALPNAgainstPebble(t *testing.T) {
	dir := os.Getenv("RUN_ACME")
	if dir == "" {
		t.Skip("set RUN_ACME=<pebble directory URL> with a reachable Pebble to run")
	}
	solver, err := NewALPNSolver("127.0.0.1:5001")
	if err != nil {
		t.Fatalf("NewALPNSolver: %v", err)
	}
	defer solver.Close()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	m, err := New(Config{
		Email:      "e2e@example.com",
		CADirURL:   dir,
		ALPNSolver: solver,
		AccountKey: key,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	certPEM, keyPEM, err := m.Obtain([]string{"alice.example.com"})
	if err != nil {
		t.Fatalf("Obtain: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("empty cert/key")
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("pem.Decode: no block found in certPEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if err := leaf.VerifyHostname("alice.example.com"); err != nil {
		t.Fatalf("VerifyHostname(%q): %v (DNSNames = %v)", "alice.example.com", err, leaf.DNSNames)
	}
}
