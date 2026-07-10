package relay

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

// startTestRelay opens a store with one enrolled account-bound agent, starts
// Serve on ephemeral ports with the given tlsCfg, and dials an agent tunnel back.
// It returns the agent session, the relay's TLS address, the agent base domain, and the store.
func startTestRelay(t *testing.T, tlsCfg *tls.Config, ctrl http.Handler) (*tunnel.Session, string, string, *Store) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Configure("public.getpiper.co", 3, 10)
	acc, _ := st.UpsertAccount("sub-1", "alice")
	en, _ := st.EnrollForAccount(acc.ID)

	tlsLn, _ := net.Listen("tcp", "127.0.0.1:0")
	tunLn, _ := net.Listen("tcp", "127.0.0.1:0")
	router := NewRouter()

	var ctrlQ *connQueue
	if ctrl != nil && tlsCfg != nil {
		ctrlQ = newConnQueue()
		srv := &http.Server{Handler: ctrl, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		go func() { _ = srv.Serve(ctrlQ) }()
		t.Cleanup(func() { ctrlQ.Close() })
	}
	ctrlHost := "api." + st.apexOrDefault()

	go func() {
		for {
			c, err := tunLn.Accept()
			if err != nil {
				return
			}
			sess, err := tunnel.Serve(c, func(tok, base string) error {
				ag, err := st.Authenticate(tok)
				if err != nil {
					return err
				}
				if ag.BaseDomain != base {
					return ErrBadToken
				}
				return nil
			})
			if err != nil {
				c.Close()
				continue
			}
			router.Register(sess)
			go serveControl(sess, st, router)
		}
	}()
	go func() {
		for {
			c, err := tlsLn.Accept()
			if err != nil {
				return
			}
			go handlePublic(c, router, tlsCfg, ctrlHost, ctrlQ)
		}
	}()
	t.Cleanup(func() { tlsLn.Close(); tunLn.Close() })

	conn, err := net.Dial("tcp", tunLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := tunnel.Dial(conn, en.Token, en.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	return sess, tlsLn.Addr().String(), en.BaseDomain, st
}

func TestControlRegisterThenTerminate(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	sess, tlsAddr, _, _ := startTestRelay(t, tlsCfg, nil)

	// Agent side: register a hostname over a control stream.
	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "register", App: "blog"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	cs.Close()
	if resp.Error != "" || resp.Hostname == "" {
		t.Fatalf("register resp = %+v", resp)
	}
	hostname := resp.Hostname

	// Agent side: accept the relay's KindHTTP stream and answer HTTP/1.1.
	go func() {
		for {
			kind, stream, err := sess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindHTTP {
				stream.Close()
				continue
			}
			go func() {
				defer stream.Close()
				br := bufio.NewReader(stream)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				io.WriteString(stream, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
			}()
		}
	}()

	// Visitor side: TLS to the relay with SNI = the assigned hostname, GET /.
	deadline := time.Now().Add(5 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		d := &tls.Dialer{Config: &tls.Config{ServerName: hostname, InsecureSkipVerify: true}}
		c, err := d.Dial("tcp", tlsAddr)
		if err == nil {
			fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostname)
			b, _ := io.ReadAll(c)
			c.Close()
			if len(b) > 0 {
				body = string(b)
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if body == "" || !contains(body, "hi") {
		t.Fatalf("terminated response = %q", body)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestControlProvisionStoresToken(t *testing.T) {
	sess, _, base, st := startTestRelay(t, nil, nil)

	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "provision", Token: "box-tok"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" {
		t.Fatalf("provision error: %s", resp.Error)
	}
	if got, err := st.ControlToken(base); err != nil || got != "box-tok" {
		t.Fatalf("ControlToken = %q, %v (want box-tok)", got, err)
	}
}

func TestControlProvisionRejectsEmptyToken(t *testing.T) {
	sess, _, base, st := startTestRelay(t, nil, nil)

	// Seed a working token first, so we can confirm an empty provision
	// doesn't clear it.
	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "provision", Token: "box-tok"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	cs.Close()
	if resp.Error != "" {
		t.Fatalf("seed provision error: %s", resp.Error)
	}

	cs2, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	if err := tunnel.WriteMsg(cs2, tunnel.ControlRequest{Op: "provision"}); err != nil {
		t.Fatal(err)
	}
	var resp2 tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs2, &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.Error == "" {
		t.Fatalf("provision with empty token: want error, got %+v", resp2)
	}
	if got, err := st.ControlToken(base); err != nil || got != "box-tok" {
		t.Fatalf("ControlToken = %q, %v (want unchanged box-tok)", got, err)
	}
}

func TestSetDomainControlOp(t *testing.T) {
	sess, _, base, st := startTestRelay(t, nil, nil)

	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "set-domain", Domain: "shop.dev"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	cs.Close()
	if resp.Error != "" {
		t.Fatalf("set-domain error: %s", resp.Error)
	}
	if got, _ := st.CustomDomain(base); got != "shop.dev" {
		t.Fatalf("stored custom domain = %q", got)
	}
}

// A hostile set-domain over the tunnel must be rejected with the error
// surfaced in ControlResponse.Error — claiming another agent's base domain
// (or the apex) would splice that victim's traffic to the attacker's box.
func TestSetDomainControlOpRejectsHijack(t *testing.T) {
	sess, _, base, st := startTestRelay(t, nil, nil)
	if _, err := st.Enroll("victim", "victim.example.com"); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{
		"victim.example.com",      // another agent's base domain
		"blog.victim.example.com", // subdomain of it
		"public.getpiper.co",      // the relay apex
		"api.public.getpiper.co",  // the relay's own control host
		base,                      // the requester's own base domain
		"Bad_Domain",              // malformed
	} {
		cs, err := sess.OpenKind(tunnel.KindControl)
		if err != nil {
			t.Fatal(err)
		}
		if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "set-domain", Domain: d}); err != nil {
			t.Fatal(err)
		}
		var resp tunnel.ControlResponse
		if err := tunnel.ReadMsg(cs, &resp); err != nil {
			t.Fatal(err)
		}
		cs.Close()
		if resp.Error == "" {
			t.Errorf("set-domain %q accepted, want rejection", d)
		}
	}
	if got, _ := st.CustomDomain(base); got != "" {
		t.Fatalf("custom domain = %q after rejected claims, want none", got)
	}
}

func TestControlPlaneSNIDispatch(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ctrl := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ctrl-ok "+r.URL.Path)
	})
	_, tlsAddr, _, _ := startTestRelay(t, tlsCfg, ctrl)

	d := &tls.Dialer{Config: &tls.Config{ServerName: "api.public.getpiper.co", InsecureSkipVerify: true}}
	c, err := d.Dial("tcp", tlsAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /ping HTTP/1.1\r\nHost: api.public.getpiper.co\r\nConnection: close\r\n\r\n")
	b, _ := io.ReadAll(c)
	if !contains(string(b), "ctrl-ok /ping") {
		t.Fatalf("control dispatch response = %q", b)
	}
}
