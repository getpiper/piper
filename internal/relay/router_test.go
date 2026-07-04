package relay

import (
	"testing"

	"github.com/getpiper/piper/internal/tunnel"
)

func TestRouterSuffixMatch(t *testing.T) {
	r := NewRouter()
	sess := &tunnel.Session{BaseDomain: "alice.example.com"}
	r.Register(sess)

	if got, ok := r.Lookup("blog.alice.example.com"); !ok || got != sess {
		t.Fatalf("subdomain lookup failed: %v %v", got, ok)
	}
	if got, ok := r.Lookup("alice.example.com"); !ok || got != sess {
		t.Fatalf("apex lookup failed: %v %v", got, ok)
	}
	if _, ok := r.Lookup("evil.example.com"); ok {
		t.Fatal("unrelated host should not match")
	}
	r.Unregister(sess)
	if _, ok := r.Lookup("blog.alice.example.com"); ok {
		t.Fatal("lookup after unregister should fail")
	}
}
