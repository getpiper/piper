package tui

import (
	"errors"
	"strings"
	"testing"
)

// typeIntoDomain feeds each rune of s into the domain form's input.
func typeIntoDomain(v domainFormView, s string) domainFormView {
	for _, r := range s {
		m, _ := v.Update(keyRunes(r))
		v = m.(domainFormView)
	}
	return v
}

func TestDomainFormSubmitEmitsAddDomain(t *testing.T) {
	v := typeIntoDomain(newDomainForm("blog"), "blog.example.com")
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should emit a command")
	}
	am, ok := cmd().(addDomainMsg)
	if !ok || am.app != "blog" || am.domain != "blog.example.com" {
		t.Fatalf("want addDomainMsg{blog, blog.example.com}, got %#v", cmd())
	}
}

func TestDomainFormRequiresDomain(t *testing.T) {
	m, cmd := newDomainForm("blog").Update(keyEnter())
	if cmd != nil {
		t.Fatal("empty domain must not submit")
	}
	if !strings.Contains(m.View(), "required") {
		t.Fatalf("want required banner:\n%s", m.View())
	}
}

func TestDomainFormErrBannerClearsOnTyping(t *testing.T) {
	v := newDomainForm("blog")
	m, _ := v.Update(errMsg{err: errors.New("domain already attached")})
	if !strings.Contains(m.(domainFormView).View(), "already attached") {
		t.Fatalf("want error banner:\n%s", m.(domainFormView).View())
	}
	if out := typeIntoDomain(m.(domainFormView), "x").View(); strings.Contains(out, "already attached") {
		t.Fatalf("banner should clear on typing:\n%s", out)
	}
}
