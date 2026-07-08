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

func TestRouterByHost(t *testing.T) {
	r := NewRouter()
	s1 := &tunnel.Session{BaseDomain: "aaaa-alice.public.getpiper.co"}
	s2 := &tunnel.Session{BaseDomain: "bbbb-bob.public.getpiper.co"}
	r.Register(s1)
	r.Register(s2)
	r.RegisterHost("blog-alice.public.getpiper.co", s1)
	r.RegisterHost("api-bob.public.getpiper.co", s2)

	if got, ok := r.LookupHost("blog-alice.public.getpiper.co"); !ok || got != s1 {
		t.Fatalf("LookupHost blog = %v,%v", got, ok)
	}
	// Terminated lookup is exact — no suffix matching.
	if _, ok := r.LookupHost("x.blog-alice.public.getpiper.co"); ok {
		t.Fatal("LookupHost must not suffix-match")
	}
	// Session teardown drops its terminated hostnames.
	r.Unregister(s1)
	if _, ok := r.LookupHost("blog-alice.public.getpiper.co"); ok {
		t.Fatal("host should be gone after Unregister(s1)")
	}
	if _, ok := r.LookupHost("api-bob.public.getpiper.co"); !ok {
		t.Fatal("s2 host must survive s1 teardown")
	}
}
