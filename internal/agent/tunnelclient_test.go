package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

// fakeRelay accepts one agent tunnel and exposes its session for the test to
// drive (open T/H streams, accept C streams).
func fakeRelay(t *testing.T) (addr string, sessCh chan *tunnel.Session) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	sessCh = make(chan *tunnel.Session, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		sess, err := tunnel.Serve(c, func(_, _ string) error { return nil })
		if err != nil {
			return
		}
		sessCh <- sess
	}()
	return ln.Addr().String(), sessCh
}

// The tunnel client forwards an accepted stream to the local dialer. We stand up
// a real relay-side listener + tunnel.Serve, run the client against it, open a
// passthrough stream from the server, and check bytes reach a fake "local Caddy".
func TestTunnelClientForwardsToLocal(t *testing.T) {
	// Fake local Caddy: echoes.
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	go func() {
		c, err := local.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c)
		c.Close()
	}()

	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "alice.example.com", func(byte, net.Conn) (net.Conn, error) {
		return net.Dial("tcp", local.Addr().String())
	})

	sess := <-sessCh
	stream, err := sess.OpenKind(tunnel.KindPassthrough)
	if err != nil {
		t.Fatalf("OpenKind: %v", err)
	}
	stream.SetDeadline(time.Now().Add(2 * time.Second))
	stream.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("echo = %q", buf)
	}
}

func TestTunnelClientDialsByKind(t *testing.T) {
	// Two local listeners stand in for the box's :443 and :80.
	ln443, _ := net.Listen("tcp", "127.0.0.1:0")
	ln80, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln443.Close()
	defer ln80.Close()
	got := make(chan byte, 1)
	accept := func(ln net.Listener, mark byte) {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			got <- mark
			c.Close()
		}
	}
	go accept(ln443, 'T')
	go accept(ln80, 'H')

	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(kind byte, _ net.Conn) (net.Conn, error) {
		if kind == tunnel.KindHTTP {
			return net.Dial("tcp", ln80.Addr().String())
		}
		return net.Dial("tcp", ln443.Addr().String())
	})
	relaySess := <-sessCh

	// Relay opens an H stream → agent must dial :80.
	hs, _ := relaySess.OpenKind(tunnel.KindHTTP)
	hs.Close()
	if mark := <-got; mark != 'H' {
		t.Fatalf("H stream dialed %q, want :80", mark)
	}
	// Relay opens a T stream → agent must dial :443.
	ts, _ := relaySess.OpenKind(tunnel.KindPassthrough)
	ts.Close()
	if mark := <-got; mark != 'T' {
		t.Fatalf("T stream dialed %q, want :443", mark)
	}
}

func TestTunnelClientRegister(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return net.Dial("tcp", "127.0.0.1:9") // unused in this test
	})
	relaySess := <-sessCh

	// Relay control handler: answer register with a canned hostname.
	go func() {
		kind, stream, err := relaySess.AcceptKind()
		if err != nil || kind != tunnel.KindControl {
			return
		}
		var req tunnel.ControlRequest
		_ = tunnel.ReadMsg(stream, &req)
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Hostname: req.App + "-alice.public.getpiper.co"})
		stream.Close()
	}()

	// Give Run a moment to publish its session.
	var host string
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		host, err = c.Register("blog")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || host != "blog-alice.public.getpiper.co" {
		t.Fatalf("Register = %q,%v", host, err)
	}
}

func TestServeStreamsStopsOnContextCancellation(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

	serverResult := make(chan *tunnel.Session, 1)
	go func() {
		sess, _ := tunnel.Serve(serverConn, func(_, _ string) error { return nil })
		serverResult <- sess
	}()
	clientSession, err := tunnel.Dial(clientConn, "token", "example.com")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	serverSession := <-serverResult
	t.Cleanup(func() { serverSession.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		serveStreams(ctx, clientSession, func(byte, net.Conn) (net.Conn, error) {
			return nil, errors.New("unexpected local dial")
		})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveStreams did not stop after context cancellation")
	}
}

// If the relay session dies immediately after tunnel.Dial succeeds (e.g. the
// relay rejects the token and drops the connection before any yamux traffic),
// the reconnect loop must still back off instead of hammering net.Dial in a
// tight spin. We simulate that by accepting and instantly closing every
// connection, then counting how many connection attempts land within a short
// window.
func TestTunnelClientBacksOffOnImmediateSessionDeath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var accepted int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt64(&accepted, 1)
			conn.Close()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, ln.Addr().String(), "tok", "alice.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, io.EOF // never actually reached; session dies before Accept
	})

	time.Sleep(500 * time.Millisecond)
	cancel()

	if n := atomic.LoadInt64(&accepted); n >= 5 {
		t.Fatalf("accepted %d connections in 500ms; reconnect loop is busy-spinning (want < 5)", n)
	}
}

func TestTunnelClientProvision(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	relaySess := <-sessCh

	got := make(chan tunnel.ControlRequest, 1)
	go func() {
		kind, stream, err := relaySess.AcceptKind()
		if err != nil || kind != tunnel.KindControl {
			return
		}
		var req tunnel.ControlRequest
		_ = tunnel.ReadMsg(stream, &req)
		got <- req
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
		stream.Close()
	}()

	// Retry until Run publishes its session.
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err = c.Provision("box-token"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	req := <-got
	if req.Op != "provision" || req.Token != "box-token" {
		t.Fatalf("relay saw %+v, want op=provision token=box-token", req)
	}
}

func TestTunnelClientOnConnectFires(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := make(chan struct{}, 1)
	var c TunnelClient
	c.OnConnect = func() { fired <- struct{}{} }
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	<-sessCh
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("OnConnect did not fire after session establishment")
	}
}

func TestTunnelClientSetCustomDomain(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	relaySess := <-sessCh

	got := make(chan tunnel.ControlRequest, 1)
	go func() {
		kind, stream, err := relaySess.AcceptKind()
		if err != nil || kind != tunnel.KindControl {
			return
		}
		var req tunnel.ControlRequest
		_ = tunnel.ReadMsg(stream, &req)
		got <- req
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
		stream.Close()
	}()

	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err = c.SetCustomDomain("shop.example.com"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("SetCustomDomain: %v", err)
	}
	req := <-got
	if req.Op != "set-domain" || req.Domain != "shop.example.com" {
		t.Fatalf("relay saw %+v, want op=set-domain domain=shop.example.com", req)
	}
}

// The per-app domain ops (#227) are thin control-stream wrappers; assert each
// sends the right op + domain and surfaces relay errors.
func TestTunnelClientDomainOps(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	relaySess := <-sessCh

	got := make(chan tunnel.ControlRequest, 3)
	go func() {
		for {
			kind, stream, err := relaySess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindControl {
				stream.Close()
				continue
			}
			var req tunnel.ControlRequest
			_ = tunnel.ReadMsg(stream, &req)
			got <- req
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
			stream.Close()
		}
	}()

	calls := []struct {
		name   string
		call   func(string) error
		wantOp string
	}{
		{"AddCustomDomain", c.AddCustomDomain, "add-domain"},
		{"ConfirmCustomDomain", c.ConfirmCustomDomain, "domain-active"},
		{"RemoveCustomDomain", c.RemoveCustomDomain, "remove-domain"},
	}
	for _, tc := range calls {
		var err error
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err = tc.call("shop.example.com"); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		req := <-got
		if req.Op != tc.wantOp || req.Domain != "shop.example.com" {
			t.Fatalf("%s sent %+v, want op=%s domain=shop.example.com", tc.name, req, tc.wantOp)
		}
	}
}
