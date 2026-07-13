package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// pushView emits a pushMsg for v and applies it, returning the model with v on
// top. It mirrors what the root does when a view requests navigation.
func pushView(t *testing.T, m Model, v view) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(pushMsg{view: v})
	return next.(Model), cmd
}

func typeLinkRepo(t *testing.T, v linkFormView, s string) linkFormView {
	t.Helper()
	for _, r := range s {
		next, _ := v.Update(keyRunes(r))
		v = next.(linkFormView)
	}
	return v
}

func TestLinkFormSubmitEmitsLinkAppMsg(t *testing.T) {
	v := typeLinkRepo(t, newLinkForm("blog"), "octo/blog")
	next, cmd := v.Update(keyEnter())
	_ = next
	if cmd == nil {
		t.Fatal("a filled repo should emit a link cmd")
	}
	msg, ok := cmd().(linkAppMsg)
	if !ok {
		t.Fatalf("want linkAppMsg, got %T", cmd())
	}
	if msg.name != "blog" || msg.repo != "octo/blog" || msg.branch != "main" {
		t.Fatalf("unexpected linkAppMsg: %+v", msg)
	}
}

func TestLinkFormEmptyRepoRejected(t *testing.T) {
	next, cmd := newLinkForm("blog").Update(keyEnter())
	if cmd != nil {
		t.Fatal("empty repo should not emit a cmd")
	}
	if !strings.Contains(next.(linkFormView).View(), "repo is required") {
		t.Fatalf("expected 'repo is required', got:\n%s", next.(linkFormView).View())
	}
}

func TestLinkAppRootRunsClientAndPops(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("pi4", "a", false, fakeAPI{rec: rec})
	// push app detail then the link form so a successful link pops back to detail
	m, _ = pushView(t, m, newAppDetailView("blog", false))
	m, _ = pushView(t, m, newLinkForm("blog"))
	depth := len(m.stack)
	next, cmd := m.Update(linkAppMsg{name: "blog", repo: "octo/blog", branch: "main"})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("expected LinkApp to run as a cmd")
	}
	m = pump(t, m, cmd) // actionResultMsg{nil,1}
	if rec.linkRepo != "octo/blog" || rec.linkName != "blog" {
		t.Fatalf("LinkApp not called with the right args: %+v", rec)
	}
	if len(m.stack) != depth-1 {
		t.Fatalf("success should pop one level: was %d now %d", depth, len(m.stack))
	}
}

func TestLKeyLowercaseOpensLinkFromDetail(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{})
	m, _ = pushView(t, m, newAppDetailView("blog", false))
	next, cmd := m.top().Update(keyRunes('l'))
	m.stack[len(m.stack)-1] = next.(view)
	m = pump(t, m, cmd)
	if _, ok := m.top().(linkFormView); !ok {
		t.Fatalf("l from app detail should push the link form, got %T", m.top())
	}
}
