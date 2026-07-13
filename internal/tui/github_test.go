package tui

import (
	"errors"
	"strings"
	"testing"
)

func TestManifestActionURL(t *testing.T) {
	if got := manifestActionURL(""); got != "https://github.com/settings/apps/new" {
		t.Fatalf("personal URL wrong: %s", got)
	}
	if got := manifestActionURL("acme"); got != "https://github.com/organizations/acme/settings/apps/new" {
		t.Fatalf("org URL wrong: %s", got)
	}
}

func TestGithubDoneSuccessPops(t *testing.T) {
	next, cmd := newGithubView().Update(githubDoneMsg{err: nil})
	_ = next
	if cmd == nil {
		t.Fatal("success should emit a pop cmd")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Fatalf("want popMsg on success, got %T", cmd())
	}
}

func TestGithubDoneErrorBanners(t *testing.T) {
	next, cmd := newGithubView().Update(githubDoneMsg{err: errors.New("exchange failed")})
	if cmd != nil {
		t.Fatalf("an error should not pop, got a cmd")
	}
	if !strings.Contains(next.(githubView).View(), "⚠") {
		t.Fatalf("expected an error banner, got:\n%s", next.(githubView).View())
	}
}

func TestGKeyOpensGithub(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	next, cmd := m.Update(keyRunes('g'))
	m = pump(t, next.(Model), cmd)
	if _, ok := m.top().(githubView); !ok {
		t.Fatalf("g should push the github view, got %T", m.top())
	}
}
