package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
	next, cmd := newManifestView().Update(githubDoneMsg{err: nil})
	_ = next
	if cmd == nil {
		t.Fatal("success should emit a pop cmd")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Fatalf("want popMsg on success, got %T", cmd())
	}
}

func TestGithubDoneErrorBanners(t *testing.T) {
	next, cmd := newManifestView().Update(githubDoneMsg{err: errors.New("exchange failed")})
	if cmd != nil {
		t.Fatalf("an error should not pop, got a cmd")
	}
	if !strings.Contains(next.(manifestView).View(), "⚠") {
		t.Fatalf("expected an error banner, got:\n%s", next.(manifestView).View())
	}
}

func TestGithubFormReadyShowsURL(t *testing.T) {
	// running view (post-start) should display the manual-open form URL.
	v, _ := newManifestView().start()
	next, _ := v.(manifestView).Update(githubFormReadyMsg{url: "http://127.0.0.1:12345", wait: nil})
	if !strings.Contains(next.(manifestView).View(), "http://127.0.0.1:12345") {
		t.Fatalf("running view should show the form URL, got:\n%s", next.(manifestView).View())
	}
}

func TestGKeyOpensManifest(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	next, cmd := m.Update(keyRunes('g'))
	m = pump(t, next.(Model), cmd)
	if _, ok := m.top().(manifestView); !ok {
		t.Fatalf("g should push the github view, got %T", m.top())
	}
}

func TestGithubEscCancelsInFlightFlow(t *testing.T) {
	canceled := false
	m := NewModel("pi4", "a", false, fakeAPI{})
	next, _ := m.Update(pushMsg{view: newManifestView()})
	m = next.(Model)
	m.githubCancel = func() { canceled = true }
	next, _ = m.Update(keyEsc())
	m = next.(Model)
	if !canceled {
		t.Fatal("esc leaving the github view should cancel the in-flight flow")
	}
	if _, ok := m.top().(manifestView); ok {
		t.Fatal("esc should pop the github view")
	}
	if m.githubCancel != nil {
		t.Fatal("cancel should be cleared after use")
	}
}

func TestGithubFormReadySchedulesWaitAfterNavAway(t *testing.T) {
	// The github view is NOT on top (user already left); wait must still run so
	// the flow's servers are torn down.
	m := NewModel("pi4", "a", false, fakeAPI{})
	ran := false
	wait := func() tea.Msg { ran = true; return githubDoneMsg{} }
	_, cmd := m.Update(githubFormReadyMsg{url: "http://x", wait: wait})
	if cmd == nil {
		t.Fatal("formReady must schedule wait even when the github view is gone")
	}
	_ = cmd()
	if !ran {
		t.Fatal("the wait cmd must run so the servers get closed")
	}
}

func TestGithubDoneAfterNavAwayIsConsumedAndCancels(t *testing.T) {
	canceled := false
	m := NewModel("pi4", "a", false, fakeAPI{}) // top is appsView, not github
	m.githubCancel = func() { canceled = true }
	next, cmd := m.Update(githubDoneMsg{err: errors.New("late")})
	m = next.(Model)
	if !canceled {
		t.Fatal("a done msg should release the flow context")
	}
	if cmd != nil {
		t.Fatal("a late done msg with no github view on top should be a no-op")
	}
	if _, ok := m.top().(appsView); !ok {
		t.Fatal("a late done msg must not disturb the current top view")
	}
}

func TestGithubDoneOnTopErrorBannersAndCancels(t *testing.T) {
	canceled := false
	m := NewModel("pi4", "a", false, fakeAPI{})
	next, _ := m.Update(pushMsg{view: newManifestView()})
	m = next.(Model)
	m.githubCancel = func() { canceled = true }
	next, _ = m.Update(githubDoneMsg{err: errors.New("boom")})
	m = next.(Model)
	if !canceled {
		t.Fatal("done should release the context")
	}
	gv, ok := m.top().(manifestView)
	if !ok {
		t.Fatal("an error done should keep the github view to banner, not pop")
	}
	if !strings.Contains(gv.View(), "⚠") {
		t.Fatalf("error should banner in the view, got:\n%s", gv.View())
	}
}
