package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/getpiper/piper/internal/source"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func newTestProvider(t *testing.T, secret string) *Provider {
	t.Helper()
	p, err := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParsePush(t *testing.T) {
	body, _ := os.ReadFile("testdata/push.json")
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	h.Set("X-Hub-Signature-256", sign("s3cr3t", string(body)))

	ev, err := p.Parse(h, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := source.Event{
		Repo: "alice/blog", Ref: "refs/heads/main", SHA: "abc123def456",
		Kind: source.KindPush, InstallationID: 99,
	}
	if ev != want {
		t.Fatalf("got %+v want %+v", ev, want)
	}
}

func TestParseBadSignature(t *testing.T) {
	body, _ := os.ReadFile("testdata/push.json")
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	h.Set("X-Hub-Signature-256", sign("WRONG", string(body)))

	if _, err := p.Parse(h, body); !errors.Is(err, source.ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestParsePing(t *testing.T) {
	body, _ := os.ReadFile("testdata/ping.json")
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "ping")
	h.Set("X-Hub-Signature-256", sign("s3cr3t", string(body)))

	ev, err := p.Parse(h, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Kind != source.KindPing {
		t.Fatalf("kind = %v", ev.Kind)
	}
}
