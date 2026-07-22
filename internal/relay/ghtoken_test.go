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

// TestGitHubTokenForPicksInstallationByRepoOwner: an account holding a personal
// install (id 55) and a getpiper-org install (id 66) mints a token for
// getpiper/app from the org installation, not the most-recent one.
func TestGitHubTokenForPicksInstallationByRepoOwner(t *testing.T) {
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_ok","expires_at":"2026-07-20T12:00:00Z"}`))
	}))
	defer srv.Close()

	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	// The owner-matching org install (66, "GetPiper") is linked FIRST, so the
	// personal install (55) is the most-recent — a most-recent-wins bug would
	// mint from 55, not 66. Mixed-case target_login also forces EqualFold.
	if err := st.LinkInstallation("66", "1001", "org", "GetPiper"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "app", "getpiper/app", "main"); err != nil {
		t.Fatal(err)
	}

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.GitHubTokenFor(context.Background(), app, agent, "getpiper/app"); err != nil {
		t.Fatalf("GitHubTokenFor: %v", err)
	}
	if hit != "/app/installations/66/access_tokens" {
		t.Fatalf("minted from %q, want installation 66 (getpiper org)", hit)
	}
}

// TestGitHubTokenForNoInstallationForRepoOwner: bound repo whose owner has no
// linked installation → ErrNoInstallation.
func TestGitHubTokenForNoInstallationForRepoOwner(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "app", "getpiper/app", "main"); err != nil {
		t.Fatal(err)
	}
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.GitHubTokenFor(context.Background(), app, agent, "getpiper/app")
	if !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("err = %v, want ErrNoInstallation", err)
	}
}
