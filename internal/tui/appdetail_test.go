package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/domain"
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

func fixtureDomains() []domain.AppDomainStatus {
	exp := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return []domain.AppDomainStatus{
		{Domain: "blog.example.com", App: "blog", Status: "pending",
			DNSRecords: []domain.DNSRecord{{Type: "CNAME", Name: "blog.example.com", Value: "relay.getpiper.dev"}}},
		{Domain: "www.example.com", App: "blog", Status: "active", CertNotAfter: &exp, DNSOK: true},
	}
}

func TestAppDetailRendersDomainsSection(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps(), domains: fixtureDomains(),
	})
	out := m.View()
	for _, want := range []string{
		"DOMAIN", "CERT EXPIRES", "DNS",
		"blog.example.com", "◌ pending",
		"www.example.com", "● active", "2026-10-01", "ok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestAppDetailDomainsRenderWithoutDeployments(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, domains: fixtureDomains(),
	})
	out := m.View()
	if !strings.Contains(out, "no deployments yet") || !strings.Contains(out, "blog.example.com") {
		t.Fatalf("want empty deployments plus domains table:\n%s", out)
	}
}

func TestAppDetailCursorSpansDomainsAndXRemoves(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps(), domains: fixtureDomains(),
	})
	// two deployments first: j, j lands on the first domain row
	m, _ = m.Update(keyRunes('j'))
	m, _ = m.Update(keyRunes('j'))
	_, cmd := m.Update(keyRunes('x'))
	if cmd == nil {
		t.Fatal("x on a domain row should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if !strings.Contains(pm.view.View(), "Remove blog.example.com") {
		t.Fatalf("want remove-domain confirm, got:\n%s", pm.view.View())
	}
}

func TestAppDetailXOnDeploymentStillDeletesApp(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps(), domains: fixtureDomains(),
	})
	_, cmd := m.Update(keyRunes('x'))
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if !strings.Contains(pm.view.View(), "Delete blog") {
		t.Fatalf("want delete-app confirm, got:\n%s", pm.view.View())
	}
}

func TestAppDetailAKeyPushesDomainForm(t *testing.T) {
	_, cmd := newAppDetailView("blog", false).Update(keyRunes('a'))
	if cmd == nil {
		t.Fatal("a should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "add domain" {
		t.Fatalf("want the domain form, got title %q", pm.view.title())
	}
}

func TestAppDetailEnterOnDomainPushesDetail(t *testing.T) {
	m, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps(), domains: fixtureDomains(),
	})
	m, _ = m.Update(keyRunes('j'))
	m, _ = m.Update(keyRunes('j'))
	_, cmd := m.Update(keyEnter())
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "domain" {
		t.Fatalf("want domain detail pushed, got title %q", pm.view.title())
	}
}

func TestAppDetailCursorStopsAtLastDomain(t *testing.T) {
	v, _ := newAppDetailView("blog", false).Update(appDetailLoadedMsg{
		app: api.App{App: store.App{Name: "blog"}}, deps: fixtureDeps(), domains: fixtureDomains(),
	})
	for range 10 {
		v, _ = v.Update(keyRunes('j'))
	}
	if c := v.(appDetailView).cursor; c != 3 { // 2 deps + 2 domains - 1
		t.Fatalf("cursor overran: %d", c)
	}
}

func TestAppDetailRefreshIncludesDomains(t *testing.T) {
	msg := newAppDetailView("blog", false).refresh(fakeAPI{domains: fixtureDomains()})()
	loaded, ok := msg.(appDetailLoadedMsg)
	if !ok {
		t.Fatalf("want appDetailLoadedMsg, got %T", msg)
	}
	if len(loaded.domains) != 2 {
		t.Fatalf("want 2 domains, got %d", len(loaded.domains))
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
