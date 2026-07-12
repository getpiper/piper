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
}

func (f fakeAPI) ListApps() ([]api.App, error) { return f.apps, f.err }

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
	m := NewModel("pi4", "http://192.168.1.6:8088", f)
	m = pump(t, m, m.refresh())
	out := m.View()
	for _, want := range []string{"● pi4", "http://192.168.1.6:8088", "1 apps", "blog", "piper", "apps"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestModelPollFailureShowsUnreachable(t *testing.T) {
	m := NewModel("pi4", "http://192.168.1.6:8088", fakeAPI{err: errors.New("dial tcp: refused")})
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "○ pi4") || !strings.Contains(out, "unreachable") {
		t.Fatalf("want unreachable bar, got:\n%s", out)
	}
}

func TestModelRecoversAfterFailure(t *testing.T) {
	m := NewModel("pi4", "addr", fakeAPI{err: errors.New("down")})
	m = pump(t, m, m.refresh())
	m.client = fakeAPI{apps: nil}
	m = pump(t, m, m.refresh())
	if out := m.View(); !strings.Contains(out, "● pi4") {
		t.Fatalf("bar did not recover:\n%s", out)
	}
}

func TestModelTickReschedulesAndRefreshes(t *testing.T) {
	m := NewModel("pi4", "addr", fakeAPI{})
	if _, cmd := m.Update(tickMsg{}); cmd == nil {
		t.Fatal("tick must return refresh+tick batch")
	}
}

func TestModelQuitKeys(t *testing.T) {
	m := NewModel("pi4", "addr", fakeAPI{})
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
