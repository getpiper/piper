package agent

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

// The tunnel client forwards an accepted stream to the local dialer. We stand up
// a real relay-side listener + tunnel.Serve, run the client against it, open a
// stream from the server, and check bytes reach a fake "local Caddy".
func TestTunnelClientForwardsToLocal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

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

	sessCh := make(chan *tunnel.Session, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		sess, err := tunnel.Serve(conn, func(_, _ string) error { return nil })
		if err != nil {
			return
		}
		sessCh <- sess
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunTunnelClient(ctx, ln.Addr().String(), "tok", "alice.example.com", func() (net.Conn, error) {
		return net.Dial("tcp", local.Addr().String())
	})

	sess := <-sessCh
	stream, err := sess.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
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
	go RunTunnelClient(ctx, ln.Addr().String(), "tok", "alice.example.com", func() (net.Conn, error) {
		return nil, io.EOF // never actually reached; session dies before Accept
	})

	time.Sleep(500 * time.Millisecond)
	cancel()

	if n := atomic.LoadInt64(&accepted); n >= 5 {
		t.Fatalf("accepted %d connections in 500ms; reconnect loop is busy-spinning (want < 5)", n)
	}
}
