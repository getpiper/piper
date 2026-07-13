package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/config"
)

// typeText feeds each rune of s to the top view through the model, like a user
// typing into a focused field.
func typeText(t *testing.T, v loginView, s string) loginView {
	t.Helper()
	for _, r := range s {
		next, _ := v.Update(keyRunes(r))
		v = next.(loginView)
	}
	return v
}

func TestLoginVerifiesAndSaves(t *testing.T) {
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "pi4", Addr: "192.168.1.6:8088", Token: "old"}},
		Current: "pi4",
	})
	v := typeText(t, newLoginView(fakeDialer(fakeAPI{}, "192.168.1.6:8088", false, nil), "pi4"), "newtok")
	next, cmd := v.Update(keyEnter())
	v = next.(loginView)
	if cmd == nil {
		t.Fatal("enter on a filled token should return a verify+save cmd")
	}
	msg := cmd()
	saved, ok := msg.(boxSavedMsg)
	if !ok {
		t.Fatalf("want boxSavedMsg, got %T (%v)", msg, msg)
	}
	if saved.box.Token != "newtok" || saved.replacing != "pi4" {
		t.Fatalf("unexpected save: %+v", saved)
	}
	cf, err := config.LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if cf.Boxes[0].Token != "newtok" {
		t.Fatalf("token not persisted: %+v", cf.Boxes[0])
	}
}

func TestLoginBadTokenBannersNoWrite(t *testing.T) {
	seedConfig(t, config.ClientFile{
		Boxes:   []config.Box{{Name: "pi4", Addr: "a", Token: "old"}},
		Current: "pi4",
	})
	v := typeText(t, newLoginView(fakeDialer(fakeAPI{err: errors.New("401")}, "a", false, nil), "pi4"), "bad")
	next, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("expected a verify cmd")
	}
	// The verify cmd fails the ListApps probe → errMsg, which the view banners.
	next, _ = next.(loginView).Update(cmd())
	v = next.(loginView)
	if !strings.Contains(v.View(), "⚠") {
		t.Fatalf("expected an error banner, got:\n%s", v.View())
	}
	cf, _ := config.LoadClientFile()
	if cf.Boxes[0].Token != "old" {
		t.Fatalf("token must not change on a failed probe: %+v", cf.Boxes[0])
	}
}

func TestLoginEmptyTokenRejected(t *testing.T) {
	v := newLoginView(fakeDialer(fakeAPI{}, "a", false, nil), "pi4")
	next, cmd := v.Update(keyEnter())
	if cmd != nil {
		t.Fatal("empty token should not run a probe")
	}
	if !strings.Contains(next.(loginView).View(), "token required") {
		t.Fatalf("expected 'token required', got:\n%s", next.(loginView).View())
	}
}

func TestLKeyOpensLogin(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	next, _ := m.Update(keyRunes('L'))
	m = next.(Model)
	// pushMsg is emitted via a cmd; run it and feed it back.
	_, cmd := m.Update(keyRunes('L'))
	_ = cmd
	m2 := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	nn, c := m2.Update(keyRunes('L'))
	m2 = pump(t, nn.(Model), c)
	if _, ok := m2.top().(loginView); !ok {
		t.Fatalf("L should push the login view, got %T", m2.top())
	}
}
