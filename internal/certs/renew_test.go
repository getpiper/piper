package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func selfSigned(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "alice.example.com"},
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestNeedsRenewal(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	within := 30 * 24 * time.Hour

	// Expires in 60 days: not due.
	far := selfSigned(t, now.Add(60*24*time.Hour))
	if due, err := NeedsRenewal(far, within, now); err != nil || due {
		t.Fatalf("far: due=%v err=%v; want false", due, err)
	}
	// Expires in 10 days: due.
	near := selfSigned(t, now.Add(10*24*time.Hour))
	if due, err := NeedsRenewal(near, within, now); err != nil || !due {
		t.Fatalf("near: due=%v err=%v; want true", due, err)
	}
	// Garbage PEM: error.
	if _, err := NeedsRenewal([]byte("nope"), within, now); err == nil {
		t.Fatal("garbage PEM: want error")
	}
}

func TestNotAfter(t *testing.T) {
	want := time.Now().Add(48 * time.Hour).Truncate(time.Second).UTC()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     want,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	got, err := NotAfter(certPEM)
	if err != nil {
		t.Fatalf("NotAfter: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("NotAfter = %v, want %v", got, want)
	}
	if _, err := NotAfter([]byte("not pem")); err == nil {
		t.Fatal("garbage input: want error")
	}
}
