package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildManifest(t *testing.T) {
	raw, err := BuildManifest("piper-alice", "https://hooks.alice.dev", "http://localhost:5000/cb")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != "piper-alice" {
		t.Errorf("name = %v", m["name"])
	}
	hook, _ := m["hook_attributes"].(map[string]any)
	if hook == nil || hook["url"] != "https://hooks.alice.dev" {
		t.Errorf("hook_attributes = %v", m["hook_attributes"])
	}
	if m["redirect_url"] != "http://localhost:5000/cb" {
		t.Errorf("redirect_url = %v", m["redirect_url"])
	}
	events, _ := m["default_events"].([]any)
	if len(events) == 0 {
		t.Error("expected default_events")
	}
}

func TestBuildManifestSlugsName(t *testing.T) {
	raw, err := BuildManifest("piper-apps.example.com", "https://hooks.apps.example.com", "http://localhost/cb")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	name, _ := m["name"].(string)
	if strings.ContainsAny(name, ".") {
		t.Errorf("name %q contains a dot; GitHub App names must be slugged", name)
	}
	if name != "piper-apps-example-com" {
		t.Errorf("name = %q, want piper-apps-example-com", name)
	}
	if len(name) > 34 {
		t.Errorf("name %q exceeds GitHub's 34-char limit", name)
	}
}

func TestExchangeCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/app-manifests/") || !strings.HasSuffix(r.URL.Path, "/conversions") {
			t.Errorf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"id":123,"slug":"piper-abc","pem":"-----PEM-----","webhook_secret":"whsec"}`)
	}))
	defer srv.Close()

	got, err := ExchangeCode(context.Background(), srv.URL, "thecode")
	if err != nil {
		t.Fatal(err)
	}
	want := AppCredentials{AppID: 123, Slug: "piper-abc", PrivateKeyPEM: "-----PEM-----", WebhookSecret: "whsec"}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
