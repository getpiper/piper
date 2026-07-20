package relay

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

func relayTestKeyPEM(t *testing.T) string {
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

func TestVerifySignature(t *testing.T) {
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"hello":"world"}`)
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write(body)
	good := "sha256=" + hex.EncodeToString(m.Sum(nil))

	if !app.VerifySignature(good, body) {
		t.Fatal("valid signature rejected")
	}
	if app.VerifySignature("sha256=deadbeef", body) {
		t.Fatal("bad signature accepted")
	}
	if app.VerifySignature("", body) {
		t.Fatal("empty signature accepted")
	}
}

func TestNewGitHubAppRequiresWebhookSecret(t *testing.T) {
	if _, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t),
	}); err == nil {
		t.Fatal("expected error when WebhookSecret is empty")
	}

	if _, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s3cret",
	}); err != nil {
		t.Fatalf("unexpected error with a webhook secret set: %v", err)
	}
}

func TestRepoTokenIsScopedToOneRepo(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_scoped","expires_at":"2026-07-20T12:00:00Z"}`))
	}))
	defer srv.Close()

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, exp, err := app.RepoToken(context.Background(), "55", "Alice/Blog")
	if err != nil {
		t.Fatalf("RepoToken: %v", err)
	}
	if tok != "ghs_scoped" {
		t.Fatalf("token = %q", tok)
	}
	if exp.IsZero() {
		t.Fatal("expiry not parsed")
	}
	if gotPath != "/app/installations/55/access_tokens" {
		t.Fatalf("path = %q", gotPath)
	}
	repos, _ := gotBody["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "Blog" {
		t.Fatalf("repositories = %v, want [Blog]", gotBody["repositories"])
	}
	perms, _ := gotBody["permissions"].(map[string]any)
	if perms["contents"] != "read" || perms["deployments"] != "write" {
		t.Fatalf("permissions = %v", perms)
	}
}

func TestReposListsInstallationRepositories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/55/access_tokens" {
			_, _ = w.Write([]byte(`{"token":"t","expires_at":"2026-07-20T12:00:00Z"}`))
			return
		}
		if r.URL.Path != "/installation/repositories" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"repositories":[{"full_name":"alice/blog"},{"full_name":"alice/api"}]}`))
	}))
	defer srv.Close()

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := app.Repos(context.Background(), "55")
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if len(repos) != 2 || repos[0] != "alice/blog" || repos[1] != "alice/api" {
		t.Fatalf("repos = %v", repos)
	}
}
