package relay

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

// httpGet80 sends one raw HTTP/1.1 request to the relay's plain-HTTP address
// and returns whatever comes back (empty ⇒ the relay dropped the connection
// without answering).
func httpGet80(t *testing.T, httpAddr, host string) string {
	t.Helper()
	conn, err := net.Dial("tcp", httpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
	b, _ := io.ReadAll(conn)
	return string(b)
}

// Port-80 traffic whose Host names a claimed custom domain must be pumped down
// the owning agent's tunnel as a KindHTTP stream, with the consumed request
// head replayed so the box can parse it (#228). The claim is deliberately left
// PENDING for most of the test: a pending domain has no cert yet, and plain
// HTTP reaching the box pre-issuance is what makes HTTP-01 a usable fallback
// challenge — the same stance the :443 SNI passthrough takes
// (TestAddDomainRoutesWhilePending). Subdomains route like the :443 path
// (suffix match), and a Host carrying a port still matches.
func TestCustomDomainHTTPPumpsDownTunnel(t *testing.T) {
	sess, _, httpAddr, _, _ := startTestRelay(t, nil, nil)

	// Fake box: answer every KindHTTP stream with a 200 echoing the Host it saw.
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
			go func(stream net.Conn) {
				defer stream.Close()
				req, err := http.ReadRequest(bufio.NewReader(stream))
				if err != nil {
					return
				}
				body := "box-saw " + req.Host
				fmt.Fprintf(stream, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			}(stream)
		}
	}()

	controlOp(t, sess, tunnel.ControlRequest{Op: "add-domain", Domain: "shop.dev"}, false)

	for _, host := range []string{"shop.dev", "www.shop.dev", "shop.dev:8080"} {
		if got := httpGet80(t, httpAddr, host); !contains(got, "box-saw "+host) {
			t.Fatalf("pending domain, Host %q: response = %q, want the box's echo", host, got)
		}
	}

	// Still routes once the claim is confirmed active.
	controlOp(t, sess, tunnel.ControlRequest{Op: "domain-active", Domain: "shop.dev"}, false)
	if got := httpGet80(t, httpAddr, "shop.dev"); !contains(got, "box-saw shop.dev") {
		t.Fatalf("active domain: response = %q, want the box's echo", got)
	}
}

// The :80 listener matches custom domains ONLY. Terminated shared hostnames,
// agent base domains and their subdomains, unknown hosts, and non-HTTP bytes
// all keep the pre-#228 behavior: the connection is dropped without a byte in
// reply and no tunnel stream is ever opened.
func TestHTTPSharedAndUnknownHostsStayDead(t *testing.T) {
	sess, _, httpAddr, base, _ := startTestRelay(t, nil, nil)

	streams := make(chan byte, 8)
	go func() {
		for {
			kind, stream, err := sess.AcceptKind()
			if err != nil {
				return
			}
			stream.Close()
			streams <- kind
		}
	}()

	resp := controlOp(t, sess, tunnel.ControlRequest{Op: "register", App: "blog"}, false)
	for _, host := range []string{resp.Hostname, base, "blog." + base, "nosuch.example"} {
		if got := httpGet80(t, httpAddr, host); got != "" {
			t.Fatalf("Host %q got %q over :80, want the connection dropped", host, got)
		}
	}

	// Non-HTTP bytes on :80 are dropped the same way.
	conn, err := net.Dial("tcp", httpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "\x16\x03\x01 not http\r\n\r\n")
	if b, _ := io.ReadAll(conn); len(b) != 0 {
		t.Fatalf("garbage on :80 got %q, want the connection dropped", b)
	}

	time.Sleep(100 * time.Millisecond) // let any wrongly-opened stream surface
	select {
	case kind := <-streams:
		t.Fatalf("unrouted :80 traffic opened a tunnel stream (kind %q)", kind)
	default:
	}
}
