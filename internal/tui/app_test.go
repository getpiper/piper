package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/store"
)

type fakeAPI struct {
	apps []api.App
	err  error
	app  api.App
	deps []store.Deployment
	logs string
}

func (f fakeAPI) ListApps() ([]api.App, error)                   { return f.apps, f.err }
func (f fakeAPI) App(string) (api.App, error)                    { return f.app, f.err }
func (f fakeAPI) Deployments(string) ([]store.Deployment, error) { return f.deps, f.err }
func (f fakeAPI) DeploymentLogs(string, string) (string, error)  { return f.logs, f.err }

func keyRunes(r rune) tea.KeyMsg { return tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune{r}}) }
func keyEnter() tea.KeyMsg       { return tea.KeyMsg(tea.Key{Type: tea.KeyEnter}) }

// pump runs the poll cmd and feeds its message back, like the tea runtime.
func pump(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	next, _ := m.Update(cmd())
	return next.(Model)
}

func TestModelPollSuccessUpdatesStatusBar(t *testing.T) {
	f := fakeAPI{apps: []api.App{{App: store.App{Name: "blog", Hostname: "blog.piper.localhost"}, Status: "running"}}}
	m := NewModel("pi4", "http://192.168.1.6:8088", false, f)
	m = pump(t, m, m.refresh())
	out := m.View()
	for _, want := range []string{"● pi4", "http://192.168.1.6:8088", "1 app", "blog", "piper", "apps"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestModelPollFailureShowsUnreachable(t *testing.T) {
	m := NewModel("pi4", "http://192.168.1.6:8088", false, fakeAPI{err: errors.New("dial tcp: refused")})
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "○ pi4") || !strings.Contains(out, "unreachable") {
		t.Fatalf("want unreachable bar, got:\n%s", out)
	}
}

func TestModelRecoversAfterFailure(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{err: errors.New("down")})
	m = pump(t, m, m.refresh())
	m.client = fakeAPI{apps: nil}
	m = pump(t, m, m.refresh())
	if out := m.View(); !strings.Contains(out, "● pi4") {
		t.Fatalf("bar did not recover:\n%s", out)
	}
}

func TestModelTickReschedulesAndRefreshes(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	if _, cmd := m.Update(tickMsg{}); cmd == nil {
		t.Fatal("tick must return refresh+tick batch")
	}
}

func TestModelQuitKeys(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	keys := []tea.KeyMsg{
		tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune{'q'}}), // q at root quits
		tea.KeyMsg(tea.Key{Type: tea.KeyCtrlC}),                     // ctrl+c quits anywhere
	}
	for _, key := range keys {
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Fatalf("%s: expected quit cmd", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s: expected tea.QuitMsg, got %T", key, cmd())
		}
	}
}

func TestModelBreadcrumbAndRelayBar(t *testing.T) {
	f := fakeAPI{apps: []api.App{{App: store.App{Name: "blog", Hostname: "blog.example.dev"}, Status: "running"}}}
	m := NewModel("pi4", "pi4.example.dev", true, f)
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "piper") || !strings.Contains(out, "apps") {
		t.Fatalf("breadcrumb missing:\n%s", out)
	}
	if !strings.Contains(out, "pi4.example.dev") {
		t.Fatalf("relay base domain missing from bar:\n%s", out)
	}
	if !strings.Contains(out, "https://blog.example.dev") {
		t.Fatalf("relay app should render https:\n%s", out)
	}
}
