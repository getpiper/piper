package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getpiper/piper/internal/source"
)

func TestReportPendingCreatesDeployment(t *testing.T) {
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case "/repos/alice/blog/deployments":
			if r.Method != http.MethodPost {
				t.Errorf("method = %s", r.Method)
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["ref"] != "sha1" {
				t.Errorf("ref = %v", body["ref"])
			}
			created = true
			w.WriteHeader(201)
			io.WriteString(w, `{"id":555}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	ev := source.Event{Repo: "alice/blog", SHA: "sha1", InstallationID: 99}
	if err := p.Report(context.Background(), ev, source.StatusPending, ""); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !created {
		t.Fatal("deployment not created")
	}
}

func TestReportSuccessPostsStatus(t *testing.T) {
	var gotState, gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case r.URL.Path == "/repos/alice/blog/deployments" && r.Method == http.MethodGet:
			io.WriteString(w, `[{"id":555}]`)
		case r.URL.Path == "/repos/alice/blog/deployments/555/statuses":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			gotState, _ = body["state"].(string)
			gotURL, _ = body["environment_url"].(string)
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	ev := source.Event{Repo: "alice/blog", SHA: "sha1", InstallationID: 99}
	err := p.Report(context.Background(), ev, source.StatusSuccess, "https://blog.example.com")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotState != "success" || gotURL != "https://blog.example.com" {
		t.Fatalf("state=%q url=%q", gotState, gotURL)
	}
}
