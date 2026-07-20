package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGitHubTokenForRejectsUnboundRepoWithNonNilApp proves the binding check
// — not the nil-App guard — is what rejects an unbound repo: the App here is
// non-nil, so ErrRepoNotBound can only come from AgentBoundToRepo.
func TestGitHubTokenForRejectsUnboundRepoWithNonNilApp(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = st.GitHubTokenFor(context.Background(), app, agent, "alice/blog")
	if !errors.Is(err, ErrRepoNotBound) {
		t.Fatalf("err = %v, want ErrRepoNotBound", err)
	}
}

// TestGitHubTokenForReturnsTokenWhenBoundAndLinked is the happy path: a
// bound repo whose account holds a linked installation gets a real token
// through to the App.
func TestGitHubTokenForReturnsTokenWhenBoundAndLinked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/55/access_tokens" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_ok","expires_at":"2026-07-20T12:00:00Z"}`))
	}))
	defer srv.Close()

	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "User", "alice"); err != nil {
		t.Fatal(err)
	}

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, _, err := st.GitHubTokenFor(context.Background(), app, agent, "alice/blog")
	if err != nil {
		t.Fatalf("GitHubTokenFor: %v", err)
	}
	if tok != "ghs_ok" {
		t.Fatalf("token = %q, want ghs_ok", tok)
	}
}
