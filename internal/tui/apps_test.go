package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/store"
)

func fixtureApps() []api.App {
	return []api.App{
		{App: store.App{Name: "blog", Hostname: "blog.piper.localhost"}, Status: "running"},
		{App: store.App{Name: "shop", Hostname: "shop.piper.localhost"}, Status: "building"},
		{App: store.App{Name: "new"}, Status: ""},
	}
}

func TestAppsViewRendersRows(t *testing.T) {
	m, _ := newAppsView(false).Update(appsLoadedMsg{apps: fixtureApps()})
	out := m.View()
	for _, want := range []string{
		"NAME", "STATUS", "URL",
		"blog", "● running", "http://blog.piper.localhost",
		"shop", "◐ building",
		"new", "—",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestAppsViewLoadingAndEmptyStates(t *testing.T) {
	if out := newAppsView(false).View(); !strings.Contains(out, "loading") {
		t.Fatalf("want loading state, got:\n%s", out)
	}
	m, _ := newAppsView(false).Update(appsLoadedMsg{apps: nil})
	if out := m.View(); !strings.Contains(out, "no apps yet") {
		t.Fatalf("want empty state, got:\n%s", out)
	}
}

func TestAppsViewErrorBannerKeepsLastRows(t *testing.T) {
	m, _ := newAppsView(false).Update(appsLoadedMsg{apps: fixtureApps()})
	m, _ = m.Update(errMsg{err: errors.New("connection refused")})
	out := m.View()
	if !strings.Contains(out, "⚠") || !strings.Contains(out, "connection refused") {
		t.Fatalf("want error banner, got:\n%s", out)
	}
	if !strings.Contains(out, "blog") {
		t.Fatalf("stale rows dropped on error:\n%s", out)
	}
	m, _ = m.Update(appsLoadedMsg{apps: fixtureApps()})
	if out := m.View(); strings.Contains(out, "⚠") {
		t.Fatalf("banner must clear on next successful poll:\n%s", out)
	}
}
