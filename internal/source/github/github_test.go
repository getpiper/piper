package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testKeyPEM returns a fresh PKCS#1 RSA private key in PEM form.
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

func TestInstallationToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/99/access_tokens" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"token":"ghs_installtoken"}`)
	}))
	defer srv.Close()

	p, err := New(Config{AppID: 7, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := p.installationToken(context.Background(), 99)
	if err != nil {
		t.Fatalf("installationToken: %v", err)
	}
	if tok != "ghs_installtoken" {
		t.Fatalf("token = %q", tok)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") || strings.Count(gotAuth, ".") != 2 {
		t.Fatalf("expected a Bearer JWT, got %q", gotAuth)
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New(Config{AppID: 1, PrivateKeyPEM: "not a key", WebhookSecret: "s"}); err == nil {
		t.Fatal("expected error for bad key")
	}
}
