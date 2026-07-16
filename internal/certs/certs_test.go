package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
)

// New must refuse ambiguous or empty challenge configuration: exactly one of
// DNSProvider (wildcard, box-wide BYO) or ALPNSolver (exact-host, per-app
// BYO) — and it must refuse before any ACME network I/O, which is what lets
// this test run offline.
func TestNewRequiresExactlyOneChallengeMode(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"neither", Config{Email: "e@example.com", AccountKey: key}},
		{"both", Config{Email: "e@example.com", AccountKey: key, DNSProvider: fakeDNS{}, ALPNSolver: fakeDNS{}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.cfg); err == nil {
				t.Fatal("New() error = nil, want exactly-one-challenge-mode error")
			}
		})
	}
}
