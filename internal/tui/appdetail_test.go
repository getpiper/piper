package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/store"
)

func fixtureDeps() []store.Deployment {
	return []store.Deployment{
		{ID: "dep-aaaaaaaaaaaa", Status: "running", CreatedAt: time.Now().Add(-2 * time.Minute)},
		{ID: "dep-bbbbbbbbbbbb", Status: "failed", PR: 7, CreatedAt: time.Now().Add(-90 * time.Minute)},
	}
}

func TestAppDetailRendersHeaderAndDeployments(t *testing.T) {
	v := newAppDetailView("blog", false)
	m, _ := v.Update(appDetailLoadedMsg{
		app:  api.App{App: store.App{Name: "blog", Hostname: "blog.piper.localhost", Port: 8080, Repo: "me/blog", Branch: "main"}},
		deps: fixtureDeps(),
	})
	out := m.View()
	for _, want := range []string{
		"blog", "http://blog.piper.localhost", "8080", "me/blog", "main",
		"DEPLOYMENT", "STATUS", "CREATED", "PR",
		"dep-aaaaaaaa", "● running", "2m ago",
		"✗ failed", "#7",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestAppDetailLoadingAndEmpty(t *testing.T) {
	if out := newAppDetailView("blog", false).View(); !strings.Contains(out, "loading") {
		t.Fatalf("want loading, got:\n%s", out)
	}
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{app: api.App{App: store.App{Name: "blog"}}, deps: nil})
	if out := m.View(); !strings.Contains(out, "no deployments yet") {
		t.Fatalf("want empty state, got:\n%s", out)
	}
}

func TestAppDetailCursorAndEnterPushesLogs(t *testing.T) {
	v := newAppDetailView("blog", false)
	m, _ := v.Update(appDetailLoadedMsg{app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps()})
	// move to the second deployment
	m, _ = m.Update(keyRunes('j'))
	// enter → a command that yields a pushMsg for the selected deployment's logs
	_, cmd := m.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "logs" {
		t.Fatalf("want logs view pushed, got title %q", pm.view.title())
	}
}

func TestAppDetailStopAndDeleteKeysPushConfirm(t *testing.T) {
	base := newAppDetailView("blog", false)
	for _, tc := range []struct {
		key  rune
		want string // substring the confirm prompt must contain
	}{
		{'s', "Stop blog"},
		{'x', "Delete blog"},
	} {
		_, cmd := base.Update(keyRunes(tc.key))
		if cmd == nil {
			t.Fatalf("%c should emit a push command", tc.key)
		}
		pm, ok := cmd().(pushMsg)
		if !ok {
			t.Fatalf("%c: want pushMsg, got %T", tc.key, cmd())
		}
		if pm.view.title() != "confirm" || !strings.Contains(pm.view.View(), tc.want) {
			t.Fatalf("%c: wrong confirm view: title=%q view=%s", tc.key, pm.view.title(), pm.view.View())
		}
	}
}

func TestAppDetailDKeyPushesDeploy(t *testing.T) {
	_, cmd := newAppDetailView("blog", false).Update(keyRunes('d'))
	if cmd == nil {
		t.Fatal("d should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "deploy" {
		t.Fatalf("want the deploy view, got title %q", pm.view.title())
	}
}

func TestAppDetailErrorBannerKeepsLastRows(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app:  api.App{App: store.App{Name: "blog", Hostname: "blog.piper.localhost", Port: 8080, Repo: "me/blog", Branch: "main"}},
		deps: fixtureDeps(),
	})
	m, _ = m.Update(errMsg{err: errors.New("connection refused")})
	out := m.View()
	if !strings.Contains(out, "⚠") || !strings.Contains(out, "connection refused") {
		t.Fatalf("want error banner, got:\n%s", out)
	}
	if !strings.Contains(out, "dep-aaaaaaaa") {
		t.Fatalf("stale rows dropped on error:\n%s", out)
	}
	m, _ = m.Update(appDetailLoadedMsg{
		app:  api.App{App: store.App{Name: "blog", Hostname: "blog.piper.localhost", Port: 8080, Repo: "me/blog", Branch: "main"}},
		deps: fixtureDeps(),
	})
	if out := m.View(); strings.Contains(out, "⚠") {
		t.Fatalf("banner must clear on next successful poll:\n%s", out)
	}
}
