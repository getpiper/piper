package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
