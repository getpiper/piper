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
// It returns the agent session, the relay's TLS address, and the agent base domain.
func startTestRelay(t *testing.T, tlsCfg *tls.Config) (*tunnel.Session, string, string) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Configure("public.getpiper.co", 3, 10)
	acc, _ := st.UpsertAccount("sub-1", "alice@example.com")
	en, _ := st.EnrollForAccount(acc.ID)

	tlsLn, _ := net.Listen("tcp", "127.0.0.1:0")
	tunLn, _ := net.Listen("tcp", "127.0.0.1:0")
	router := NewRouter()
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
			go handlePublic(c, router, tlsCfg)
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
	return sess, tlsLn.Addr().String(), en.BaseDomain
}

func TestControlRegisterThenTerminate(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	sess, tlsAddr, _ := startTestRelay(t, tlsCfg)

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
