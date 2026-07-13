package tui

import (
	"strings"
	"testing"
)

func TestStopConfirmYesEmitsStop(t *testing.T) {
	v := newStopConfirm("blog")
	if !strings.Contains(v.View(), "Stop blog") {
		t.Fatalf("prompt missing:\n%s", v.View())
	}
	_, cmd := v.Update(keyRunes('y'))
	if _, ok := cmd().(stopAppMsg); !ok {
		t.Fatalf("y should emit stopAppMsg, got %T", cmd())
	}
}

func TestStopConfirmNoPops(t *testing.T) {
	_, cmd := newStopConfirm("blog").Update(keyRunes('n'))
	if cmd == nil {
		t.Fatal("n should emit a command")
	}
	if pm, ok := cmd().(popMsg); !ok || pm.n != 1 {
		t.Fatalf("n should emit popMsg{1}, got %#v", cmd())
	}
}

func TestDeleteConfirmGatesOnTypedName(t *testing.T) {
	v := newDeleteConfirm("blog")
	// wrong name: enter does not delete, shows mismatch
	wrong := typeInto2(v, "bloop")
	m, cmd := wrong.Update(keyEnter())
	if cmd != nil {
		t.Fatal("mismatched name must not delete")
	}
	if !strings.Contains(m.(confirmView).View(), "match") {
		t.Fatalf("want mismatch banner:\n%s", m.(confirmView).View())
	}
	// exact name: enter deletes
	right := typeInto2(newDeleteConfirm("blog"), "blog")
	_, cmd = right.Update(keyEnter())
	if _, ok := cmd().(deleteAppMsg); !ok {
		t.Fatalf("exact name should emit deleteAppMsg, got %v", cmd())
	}
}

// typeInto2 feeds each rune of s into a confirmView's text input.
func typeInto2(v confirmView, s string) confirmView {
	for _, r := range s {
		m, _ := v.Update(keyRunes(r))
		v = m.(confirmView)
	}
	return v
}
