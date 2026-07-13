package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestQuestionMarkPushesHelpOverlay(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	_, cmd := m.Update(keyRunes('?'))
	if cmd == nil {
		t.Fatal("? should push a help view")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if _, ok := push.view.(helpView); !ok {
		t.Fatalf("want helpView pushed, got %T", push.view)
	}
	// the rendered overlay carries the full keymap
	out := helpView{}.View()
	for _, want := range []string{"new app", "deploy", "toggle follow", "refresh"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help overlay missing %q:\n%s", want, out)
		}
	}
}

func TestQuestionMarkIsLiteralInTextFields(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	// push the new-app form: capturesText() is true
	m2, _ := m.Update(pushMsg{newFormView()})
	m = m2.(Model)
	_, cmd := m.Update(keyRunes('?'))
	if cmd != nil {
		if _, ok := cmd().(pushMsg); ok {
			t.Fatal("? must not push help while a text field is focused")
		}
	}
}

func TestQuestionMarkDoesNotStackHelp(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	m2, _ := m.Update(pushMsg{helpView{}})
	m = m2.(Model)
	depth := len(m.stack)
	_, cmd := m.Update(keyRunes('?'))
	if cmd != nil {
		if _, ok := cmd().(pushMsg); ok {
			t.Fatal("? on the help view must not push a second help")
		}
	}
	if len(m.stack) != depth {
		t.Fatalf("stack depth changed: %d -> %d", depth, len(m.stack))
	}
}

func TestEscPopsHelpOverlay(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	m2, _ := m.Update(pushMsg{helpView{}})
	m = m2.(Model)
	if len(m.stack) != 2 {
		t.Fatalf("setup: want depth 2, got %d", len(m.stack))
	}
	m2, _ = m.Update(keyEsc())
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("esc should pop help back to root, got depth %d", len(m.stack))
	}
}

func keyEsc() tea.KeyMsg { return tea.KeyMsg(tea.Key{Type: tea.KeyEsc}) }
