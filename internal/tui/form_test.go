package tui

import (
	"strings"
	"testing"
)

func typeInto(v formView, s string) formView {
	for _, r := range s {
		m, _ := v.Update(keyRunes(r))
		v = m.(formView)
	}
	return v
}

func TestFormRendersFieldsWithPortDefault(t *testing.T) {
	out := newFormView().View()
	for _, want := range []string{"new app", "name", "port", "8080", "create"} {
		if !strings.Contains(out, want) {
			t.Fatalf("form view missing %q:\n%s", want, out)
		}
	}
}

func TestFormSubmitEmitsCreateIntent(t *testing.T) {
	v := typeInto(newFormView(), "blog")
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("valid submit should emit a command")
	}
	msg, ok := cmd().(createAppMsg)
	if !ok {
		t.Fatalf("want createAppMsg, got %T", cmd())
	}
	if msg.name != "blog" || msg.port != 8080 {
		t.Fatalf("want {blog,8080}, got %+v", msg)
	}
}

func TestFormValidationBanners(t *testing.T) {
	// empty name
	v := newFormView()
	m, cmd := v.Update(keyEnter())
	if cmd != nil {
		t.Fatal("empty name must not submit")
	}
	if !strings.Contains(m.(formView).View(), "name") {
		t.Fatalf("want name-required banner:\n%s", m.(formView).View())
	}
	// bad port: clear the 8080 default and type letters
	v = newFormView()
	m2, _ := v.Update(keyTab()) // focus port
	v = m2.(formView)
	// wipe default then type non-numeric
	for range "8080" {
		mm, _ := v.Update(keyBackspace())
		v = mm.(formView)
	}
	v = typeInto(v, " abc")
	m3, cmd := v.Update(keyEnter())
	if cmd != nil {
		t.Fatal("bad port must not submit")
	}
	if !strings.Contains(m3.(formView).View(), "port") {
		t.Fatalf("want port banner:\n%s", m3.(formView).View())
	}
}

func TestFormCapturesLetterShortcuts(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	m2, _ := m.Update(pushMsg{newFormView()})
	m = m2.(Model)
	// "qr" are the root's quit/refresh shortcuts; the form must receive them
	for _, r := range "qr" {
		m2, _ = m.Update(keyRunes(r))
		m = m2.(Model)
	}
	if len(m.stack) != 2 {
		t.Fatalf("q/r must not pop the form; stack depth %d", len(m.stack))
	}
	_, cmd := m.Update(keyEnter())
	if msg, ok := cmd().(createAppMsg); !ok || msg.name != "qr" {
		t.Fatalf("form should have captured \"qr\" as the name, got %#v", cmd())
	}
}
