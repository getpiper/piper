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

	"github.com/getpiper/piper/internal/source"
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

	key, err := parsePrivateKey(testKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	a := &appTokenSource{appID: 7, key: key, apiBase: srv.URL, http: srv.Client()}
	tok, err := a.Token(context.Background(), source.Event{InstallationID: 99})
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

func TestInstallationTokenResolvesInstallationFromRepo(t *testing.T) {
	var gotLookupAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/alice/blog/installation":
			gotLookupAuth = r.Header.Get("Authorization")
			io.WriteString(w, `{"id":99}`)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_installtoken"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	key, err := parsePrivateKey(testKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	a := &appTokenSource{appID: 7, key: key, apiBase: srv.URL, http: srv.Client()}
	tok, err := a.Token(context.Background(), source.Event{Repo: "alice/blog"})
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_installtoken" {
		t.Fatalf("token = %q", tok)
	}
	if !strings.HasPrefix(gotLookupAuth, "Bearer ") || strings.Count(gotLookupAuth, ".") != 2 {
		t.Fatalf("expected a Bearer JWT on the installation lookup, got %q", gotLookupAuth)
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New(Config{AppID: 1, PrivateKeyPEM: "not a key", WebhookSecret: "s"}); err == nil {
		t.Fatal("expected error for bad key")
	}
}
