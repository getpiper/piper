package relay

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

func TestAcceptTunnelsRebindsCustomDomainOnReconnect(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Configure("public.getpiper.co", 3, 10, 5)

	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	router := NewRouter()
	go acceptTunnels(ln, st, router, nil, nil)

	customDomain := "app.example.com"

	connect := func() *tunnel.Session {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		sess, err := tunnel.Dial(conn, en.Token, en.BaseDomain)
		if err != nil {
			t.Fatal(err)
		}
		return sess
	}

	controlDomain := func(sess *tunnel.Session, op, domain string) {
		t.Helper()
		cs, err := sess.OpenKind(tunnel.KindControl)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: op, Domain: domain}); err != nil {
			t.Fatal(err)
		}
		var resp tunnel.ControlResponse
		if err := tunnel.ReadMsg(cs, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Error != "" {
			t.Fatalf("%s %q error: %s", op, domain, resp.Error)
		}
	}

	waitFor := func(desc string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s", desc)
	}

	sess1 := connect()
	controlDomain(sess1, "add-domain", customDomain)
	controlDomain(sess1, "domain-active", customDomain)
	waitFor("custom domain on first session", func() bool {
		s, ok := router.Lookup(customDomain)
		return ok && s.BaseDomain == en.BaseDomain
	})

	sess1.Close()
	waitFor("custom domain unregistration", func() bool {
		_, ok := router.Lookup(customDomain)
		return !ok
	})

	sess2 := connect()
	waitFor("custom domain rebind after reconnect", func() bool {
		s, ok := router.Lookup(customDomain)
		return ok && s.BaseDomain == en.BaseDomain
	})

	sess2.Close()
}
