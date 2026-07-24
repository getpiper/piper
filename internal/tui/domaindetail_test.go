package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/domain"
)

func TestDomainDetailShowsCNAMEStatusAndNote(t *testing.T) {
	v := newDomainDetailView("blog", fixtureDomains()[0])
	out := v.View()
	for _, want := range []string{
		"blog.example.com", "blog", "◌ pending", "dns  no",
		"CNAME", "relay.getpiper.dev", "issuance starts once DNS points at the relay",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestDomainDetailPollTracksStatus(t *testing.T) {
	v := newDomainDetailView("blog", fixtureDomains()[0])
	active := fixtureDomains()[0]
	active.Status = "active"
	active.DNSOK = true
	msg := v.refresh(fakeAPI{domains: []domain.AppDomainStatus{active}})()
	loaded, ok := msg.(domainDetailLoadedMsg)
	if !ok || !loaded.found {
		t.Fatalf("want found domainDetailLoadedMsg, got %#v", msg)
	}
	m, _ := v.Update(loaded)
	out := m.View()
	if !strings.Contains(out, "● active") || !strings.Contains(out, "dns  ok") {
		t.Fatalf("want live active status:\n%s", out)
	}
	if strings.Contains(out, "issuance starts") {
		t.Fatalf("active domain should drop the issuance note:\n%s", out)
	}
}

func TestDomainDetailGoneKeepsLastState(t *testing.T) {
	v := newDomainDetailView("blog", fixtureDomains()[0])
	msg := v.refresh(fakeAPI{})()
	loaded, ok := msg.(domainDetailLoadedMsg)
	if !ok || loaded.found {
		t.Fatalf("want not-found result, got %#v", msg)
	}
	m, _ := v.Update(loaded)
	if !strings.Contains(m.View(), "blog.example.com") {
		t.Fatalf("last-known state dropped:\n%s", m.View())
	}
}

func TestDomainDetailFailedShowsError(t *testing.T) {
	st := fixtureDomains()[0]
	st.Status = "failed"
	st.Error = "acme: challenge timed out"
	if out := newDomainDetailView("blog", st).View(); !strings.Contains(out, "challenge timed out") {
		t.Fatalf("failed error missing:\n%s", out)
	}
}

func TestDomainDetailErrBanner(t *testing.T) {
	v := newDomainDetailView("blog", fixtureDomains()[0])
	m, _ := v.Update(errMsg{err: errors.New("connection refused")})
	if !strings.Contains(m.View(), "connection refused") {
		t.Fatalf("want error banner:\n%s", m.View())
	}
}
