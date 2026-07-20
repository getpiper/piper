package ghjwt

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
}

func TestSignProducesThreeSegments(t *testing.T) {
	key, err := ParseKey(testKeyPEM(t))
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	tok, err := Sign("12345", key, time.Now())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if n := strings.Count(tok, "."); n != 2 {
		t.Fatalf("token has %d dots, want 2: %q", n, tok)
	}
}

func TestParseKeyRejectsGarbage(t *testing.T) {
	if _, err := ParseKey("not a pem"); err == nil {
		t.Fatal("ParseKey accepted garbage")
	}
}
