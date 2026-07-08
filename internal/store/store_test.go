package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndGetApp(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := s.GetApp("blog")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Name != "blog" || got.Port != 8080 {
		t.Errorf("got %+v", got)
	}
}

func TestUpdateAppRepoAndAppByRepo(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAppRepo("blog", "alice/blog", "main"); err != nil {
		t.Fatalf("UpdateAppRepo: %v", err)
	}

	got, err := s.AppByRepo("alice/blog")
	if err != nil {
		t.Fatalf("AppByRepo: %v", err)
	}
	if got.Name != "blog" || got.Repo != "alice/blog" || got.Branch != "main" {
		t.Fatalf("got %+v", got)
	}

	if _, err := s.AppByRepo("nobody/none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGitHubAppRoundTrip(t *testing.T) {
	s := openTemp(t)

	if _, err := s.GetGitHubApp(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	want := GitHubApp{AppID: 42, PrivateKey: "-----KEY-----", WebhookSecret: "shh"}
	if err := s.SaveGitHubApp(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGitHubApp()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	// Upsert replaces, not duplicates.
	want.WebhookSecret = "newsecret"
	if err := s.SaveGitHubApp(want); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetGitHubApp()
	if got.WebhookSecret != "newsecret" {
		t.Fatalf("upsert failed: %+v", got)
	}
}

func TestGetAppNotFound(t *testing.T) {
	s := openTemp(t)
	if _, err := s.GetApp("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateAppDuplicate(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	if _, err := s.CreateApp("blog", 8080); err == nil {
		t.Error("expected error on duplicate app")
	}
}

func TestListApps(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	s.CreateApp("api", 3000)
	apps, err := s.ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 2 || apps[0].Name != "api" || apps[1].Name != "blog" {
		t.Errorf("apps = %+v, want [api blog] ordered", apps)
	}
}

func TestLatestRunning(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	if _, err := s.LatestRunning("blog"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty LatestRunning err = %v, want ErrNotFound", err)
	}
	d1, _ := s.CreateDeployment("blog", "img1", "c1", 40001, "running")
	s.CreateDeployment("blog", "img2", "c2", 40002, "failed")
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ID != d1.ID {
		t.Errorf("LatestRunning ID = %s, want %s", got.ID, d1.ID)
	}
}

func TestUpdateDeploymentStatus(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	d, _ := s.CreateDeployment("blog", "img1", "c1", 40001, "running")
	if err := s.UpdateDeploymentStatus(d.ID, "stopped"); err != nil {
		t.Fatalf("UpdateDeploymentStatus: %v", err)
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after status change to stopped, LatestRunning err = %v, want ErrNotFound", err)
	}
}

func TestPreviewDeploymentRoundTrip(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreatePreviewDeployment("blog", 7, "img", "cid", 41000, "running"); err != nil {
		t.Fatalf("CreatePreviewDeployment: %v", err)
	}
	got, err := s.PreviewRunning("blog", 7)
	if err != nil {
		t.Fatalf("PreviewRunning: %v", err)
	}
	if got.PR != 7 || got.ContainerID != "cid" || got.HostPort != 41000 {
		t.Errorf("got %+v", got)
	}
	if _, err := s.PreviewRunning("blog", 8); !errors.Is(err, ErrNotFound) {
		t.Errorf("PreviewRunning(missing) err = %v, want ErrNotFound", err)
	}
}

func TestLatestRunningIgnoresPreviews(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateDeployment("blog", "img", "main-c", 40000, "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePreviewDeployment("blog", 3, "img", "preview-c", 41000, "running"); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ContainerID != "main-c" {
		t.Errorf("LatestRunning returned %q, want main-c", got.ContainerID)
	}
}

func TestTokenCreateAuthenticateRevoke(t *testing.T) {
	s := openTemp(t)
	tok, err := s.CreateToken("laptop", "admin")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	got, err := s.AuthenticateToken(tok)
	if err != nil {
		t.Fatalf("AuthenticateToken: %v", err)
	}
	if got.Label != "laptop" || got.Scope != "admin" {
		t.Errorf("got %+v", got)
	}
	if _, err := s.AuthenticateToken("nope"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("unknown token: want ErrBadToken, got %v", err)
	}
	if err := s.RevokeToken("laptop"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := s.AuthenticateToken(tok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("after revoke: want ErrBadToken, got %v", err)
	}
	if err := s.RevokeToken("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke unknown: want ErrNotFound, got %v", err)
	}
}

func TestTokenDuplicateLabelRejected(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateToken("laptop", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken("laptop", "admin"); err == nil {
		t.Fatal("want error on duplicate label")
	}
}

func TestOpenSetsBusyTimeout(t *testing.T) {
	s := openTemp(t)
	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

func TestListTokens(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateToken("a", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken("b", "readonly"); err != nil {
		t.Fatal(err)
	}
	toks, err := s.ListTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 {
		t.Fatalf("len = %d, want 2", len(toks))
	}
}

func TestDeleteTokenHardDeletes(t *testing.T) {
	st := openTemp(t)

	if _, err := st.CreateToken("relay:base.example.com", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteToken("relay:base.example.com"); err != nil {
		t.Fatal(err)
	}
	toks, err := st.ListTokens()
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range toks {
		if tk.Label == "relay:base.example.com" {
			t.Fatal("token row still present after DeleteToken")
		}
	}
	// Deleting a non-existent label is not an error (idempotent unwind).
	if err := st.DeleteToken("relay:base.example.com"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}
