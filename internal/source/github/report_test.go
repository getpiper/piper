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

func TestReportPendingUsesPREnvironment(t *testing.T) {
	var gotEnv string
	var gotTransient bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case "/repos/alice/blog/deployments":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			gotEnv, _ = body["environment"].(string)
			gotTransient, _ = body["transient_environment"].(bool)
			w.WriteHeader(201)
			io.WriteString(w, `{"id":1}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	ev := source.Event{Repo: "alice/blog", SHA: "sha1", InstallationID: 99, PR: 42}
	if err := p.Report(context.Background(), ev, source.StatusPending, ""); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotEnv != "pr-42" || !gotTransient {
		t.Fatalf("environment=%q transient=%v, want pr-42/true", gotEnv, gotTransient)
	}
}

func TestReportPRScopesDeploymentLookupToPREnvironment(t *testing.T) {
	var gotEnv, gotSHA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case r.URL.Path == "/repos/alice/blog/deployments" && r.Method == http.MethodGet:
			gotEnv = r.URL.Query().Get("environment")
			gotSHA = r.URL.Query().Get("sha")
			io.WriteString(w, `[{"id":555}]`)
		case r.URL.Path == "/repos/alice/blog/deployments/555/statuses":
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	ev := source.Event{Repo: "alice/blog", SHA: "sha1", InstallationID: 99, PR: 42}
	if err := p.Report(context.Background(), ev, source.StatusSuccess, ""); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotSHA != "sha1" {
		t.Errorf("sha filter = %q, want sha1", gotSHA)
	}
	if gotEnv != "pr-42" {
		t.Fatalf("environment filter = %q, want pr-42 (a status could otherwise post to a production deployment on the same SHA)", gotEnv)
	}
}

func TestReportProductionScopesDeploymentLookupToProductionEnvironment(t *testing.T) {
	var gotEnv, gotSHA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case r.URL.Path == "/repos/alice/blog/deployments" && r.Method == http.MethodGet:
			gotEnv = r.URL.Query().Get("environment")
			gotSHA = r.URL.Query().Get("sha")
			io.WriteString(w, `[{"id":555}]`)
		case r.URL.Path == "/repos/alice/blog/deployments/555/statuses":
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	ev := source.Event{Repo: "alice/blog", SHA: "sha1", InstallationID: 99}
	if err := p.Report(context.Background(), ev, source.StatusSuccess, ""); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotSHA != "sha1" {
		t.Errorf("sha filter = %q, want sha1", gotSHA)
	}
	if gotEnv != "production" {
		t.Fatalf("environment filter = %q, want production (a status could otherwise post to a PR-preview deployment on the same SHA)", gotEnv)
	}
}

func TestReportInactivePostsInactiveState(t *testing.T) {
	var gotState string
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
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	ev := source.Event{Repo: "alice/blog", SHA: "sha1", InstallationID: 99, PR: 42}
	if err := p.Report(context.Background(), ev, source.StatusInactive, ""); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotState != "inactive" {
		t.Fatalf("state=%q, want inactive", gotState)
	}
}
