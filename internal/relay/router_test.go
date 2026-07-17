package relay

import (
	"net"
	"testing"
	"time"

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

// newSessionPair returns a live server/client tunnel session pair over an
// in-memory pipe, so a test can close the server side and exercise the router's
// refuse-closed-session guard against a genuinely torn-down session.
func newSessionPair(t *testing.T) (server, client *tunnel.Session) {
	t.Helper()
	c1, c2 := net.Pipe()
	type result struct {
		sess *tunnel.Session
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := tunnel.Serve(c1, func(string, string) error { return nil })
		ch <- result{s, err}
	}()
	client, err := tunnel.Dial(c2, "tok", "closed.example.com")
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.sess, client
}

// TestRegisterRefusesClosedSession pins the stale-entry fix: a register that
// arrives for an already-closed session must no-op under the router lock, so no
// permanent byBase/byHost/custom entry can outlive the session's Unregister.
func TestRegisterRefusesClosedSession(t *testing.T) {
	r := NewRouter()
	server, client := newSessionPair(t)
	defer client.Close()

	server.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !server.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("server session did not close")
		}
		time.Sleep(5 * time.Millisecond)
	}

	r.Register(server)
	if _, ok := r.Lookup(server.BaseDomain); ok {
		t.Fatal("Register must refuse a closed session")
	}
	r.RegisterHost("host.example.com", server)
	if _, ok := r.LookupHost("host.example.com"); ok {
		t.Fatal("RegisterHost must refuse a closed session")
	}
	r.RegisterCustom("custom.example.com", server)
	if _, ok := r.Lookup("custom.example.com"); ok {
		t.Fatal("RegisterCustom must refuse a closed session")
	}
}

func TestRouterCustomDomain(t *testing.T) {
	r := NewRouter()
	sess := &tunnel.Session{BaseDomain: "alice.example.com"}
	r.Register(sess)
	r.RegisterCustom("shop.dev", sess)

	if s, ok := r.Lookup("blog.shop.dev"); !ok || s != sess {
		t.Fatal("subdomain of custom domain should route to the session")
	}
	if s, ok := r.Lookup("shop.dev"); !ok || s != sess {
		t.Fatal("custom apex should route to the session")
	}

	r.UnregisterCustom("shop.dev")
	if _, ok := r.Lookup("blog.shop.dev"); ok {
		t.Fatal("custom domain should be gone after UnregisterCustom")
	}

	// Unregister(sess) sweeps custom entries too.
	r.RegisterCustom("shop.dev", sess)
	r.Unregister(sess)
	if _, ok := r.Lookup("blog.shop.dev"); ok {
		t.Fatal("custom domain should be swept by Unregister")
	}
	if _, ok := r.Lookup("x.alice.example.com"); ok {
		t.Fatal("base domain should be swept by Unregister")
	}
}
