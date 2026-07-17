package relay

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

// --- item 6: bearerToken scheme is case-insensitive, rest stays strict ---

func TestBearerTokenSchemeCaseInsensitive(t *testing.T) {
	accept := []struct{ header, want string }{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER abc", "abc"},
		{"BeArEr abc", "abc"},
		{"bEaReR xyz.123", "xyz.123"},
	}
	for _, c := range accept {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", c.header)
		tok, ok := bearerToken(r)
		if !ok || tok != c.want {
			t.Errorf("bearerToken(%q) = (%q,%v), want (%q,true)", c.header, tok, ok, c.want)
		}
	}

	reject := []string{
		"",              // no header
		"Bearer",        // scheme only, no space
		"Bearer ",       // empty token
		"Basic abc",     // wrong scheme
		"Token abc",     // wrong scheme
		"Bearerabc",     // no separating space
		"NotBearer abc", // scheme is a superset, must not match
	}
	for _, h := range reject {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		if tok, ok := bearerToken(r); ok {
			t.Errorf("bearerToken(%q) = (%q,true), want reject", h, tok)
		}
	}
}

// --- item 7: connQueue.Close is safe under concurrent callers ---

// Run under -race: with the old check-then-close body, racing callers either
// double-close the done channel (panic) or trip the race detector. sync.Once
// makes this clean.
func TestConnQueueCloseConcurrent(t *testing.T) {
	q := newConnQueue()
	const n = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = q.Close()
		}()
	}
	close(start)
	wg.Wait()
	// Closed exactly once ⇒ Accept reports the queue shut, not a panic.
	if _, err := q.Accept(); err != net.ErrClosed {
		t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
	}
}

// --- item 2: dialControlStream honors the caller's context ---

type dialerFunc func(byte) (net.Conn, error)

func (f dialerFunc) OpenKind(k byte) (net.Conn, error) { return f(k) }

// trackConn records whether it was closed.
type trackConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *trackConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestDialControlStreamCancelAbortsPendingOpen(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	p1, p2 := net.Pipe()
	t.Cleanup(func() { p2.Close() })
	tc := &trackConn{Conn: p1, closed: make(chan struct{})}
	d := dialerFunc(func(byte) (net.Conn, error) {
		close(entered) // the open has actually begun
		<-release      // block the open until the test releases it
		return tc, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	type res struct {
		c net.Conn
		e error
	}
	done := make(chan res, 1)
	go func() {
		c, e := dialControlStream(ctx, d)
		done <- res{c, e}
	}()

	// Wait until the open is genuinely parked in OpenKind (past the pre-cancel
	// guard, inside the select), so we exercise the mid-dial cancel path.
	<-entered
	// Cancelling the caller context must abort the dial promptly rather than
	// blocking on the tunnel.
	cancel()
	select {
	case r := <-done:
		if r.e == nil {
			t.Fatalf("dial returned a conn after cancel, want context error")
		}
		if !errors.Is(r.e, context.Canceled) {
			t.Fatalf("dial error = %v, want context.Canceled", r.e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dial did not abort after context cancel")
	}

	// The abandoned open eventually completes; its stream must be closed, never
	// leaked.
	close(release)
	select {
	case <-tc.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("abandoned tunnel stream was not closed (leak)")
	}
}

func TestDialControlStreamPreCancelledSkipsOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := dialerFunc(func(byte) (net.Conn, error) {
		t.Fatal("OpenKind called despite an already-cancelled context")
		return nil, nil
	})
	if _, err := dialControlStream(ctx, d); !errors.Is(err, context.Canceled) {
		t.Fatalf("dial error = %v, want context.Canceled", err)
	}
}

// --- item 4: ErrorHandler returns a generic 502, no err detail leaked ---

// brokenBox accepts the KindControlAPI stream and closes it immediately without
// a response, so the reverse-proxy transport errors.
func brokenBox(sess *tunnel.Session) {
	for {
		kind, stream, err := sess.AcceptKind()
		if err != nil {
			return
		}
		stream.Close()
		_ = kind
	}
}

func TestControlProxyGeneric502(t *testing.T) {
	api, _, router, aliceCred, _, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go brokenBox(agentSess)

	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("broken box: %d, want 502", rr.Code)
	}
	body := strings.TrimSpace(rr.Body.String())
	if body != "box unreachable" {
		t.Fatalf("502 body = %q, want the generic \"box unreachable\" with no err detail", body)
	}
	// No transport internals (EOF, connection reset, the old "box unreachable: <err>")
	// may reach the caller.
	for _, leak := range []string{"EOF", "reset", "unreachable:", "broken pipe", "closed"} {
		if strings.Contains(rr.Body.String(), leak) {
			t.Fatalf("502 body leaked transport detail %q: %q", leak, rr.Body.String())
		}
	}
}

// --- item 1: ResponseHeaderTimeout bounds a wedged box ---

// silentBox accepts the stream, reads the request, and then never responds,
// holding the stream open past the (test-shortened) header timeout.
func silentBox(t *testing.T, sess *tunnel.Session) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	go func() {
		for {
			kind, stream, err := sess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindControlAPI {
				stream.Close()
				continue
			}
			go func() {
				<-hold // never respond within the test
				stream.Close()
			}()
		}
	}()
}

func TestControlProxyResponseHeaderTimeout(t *testing.T) {
	orig := responseHeaderTimeout
	responseHeaderTimeout = 150 * time.Millisecond
	t.Cleanup(func() { responseHeaderTimeout = orig })

	api, _, router, aliceCred, _, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	silentBox(t, agentSess)

	start := time.Now()
	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred)
	elapsed := time.Since(start)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("wedged box: %d, want 502 from the header timeout", rr.Code)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("returned in %v — too fast to be the header timeout firing", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("returned in %v — header timeout did not bound the wait", elapsed)
	}
}
