package agent

import (
	"bytes"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// writeRecorder records everything written through it so the test can assert
// the peek's consumed bytes are exactly what the client sent. net.Pipe is
// synchronous, so once PeekALPN returns, every recorded byte was consumed.
type writeRecorder struct {
	net.Conn
	buf []byte
}

func (w *writeRecorder) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return w.Conn.Write(p)
}

func TestPeekALPN(t *testing.T) {
	cases := []struct {
		name   string
		protos []string
		want   bool
	}{
		{"acme-tls/1", []string{"acme-tls/1"}, true},
		{"acme among others", []string{"h2", "acme-tls/1"}, true},
		{"http protos", []string{"h2", "http/1.1"}, false},
		{"no alpn", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			wr := &writeRecorder{Conn: client}
			go func() {
				// The handshake can't complete (the peek never answers);
				// it exists only to emit a real ClientHello.
				_ = tls.Client(wr, &tls.Config{
					ServerName:         "myshop.example.com",
					NextProtos:         c.protos,
					InsecureSkipVerify: true,
				}).Handshake()
			}()
			acme, consumed := PeekALPN(server)
			if acme != c.want {
				t.Fatalf("PeekALPN acme = %v, want %v", acme, c.want)
			}
			if !bytes.Equal(consumed, wr.buf) {
				t.Fatalf("consumed %d bytes, client sent %d — replay would corrupt the stream", len(consumed), len(wr.buf))
			}
		})
	}
}

// TestPeekALPNTimesOutOnStalledPeer exercises the stall path: a client that
// writes a partial TLS record header (promising more bytes than it sends)
// and then never sends the rest. PeekALPN must not hang past
// alpnPeekTimeout, must report acme == false, and must return exactly the
// bytes the client actually sent (so a subsequent replay isn't corrupted).
func TestPeekALPNTimesOutOnStalledPeer(t *testing.T) {
	old := alpnPeekTimeout
	alpnPeekTimeout = 100 * time.Millisecond
	defer func() { alpnPeekTimeout = old }()

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// A handshake record header (0x16 = handshake, 0x0301 = legacy version)
	// claiming a 0xffff-byte body — far more than actually sent — then stall
	// without closing.
	partial := []byte{0x16, 0x03, 0x01, 0xff, 0xff}
	go func() {
		_, _ = client.Write(partial)
		// Deliberately no further writes and no Close: the peer stalls.
	}()

	done := make(chan struct{})
	var acme bool
	var consumed []byte
	go func() {
		acme, consumed = PeekALPN(server)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PeekALPN did not return within the timeout")
	}

	if acme {
		t.Fatal("PeekALPN reported acme = true for a stalled, non-completing handshake")
	}
	if !bytes.Equal(consumed, partial) {
		t.Fatalf("consumed = %v, want %v", consumed, partial)
	}

	// The read deadline should be cleared afterwards; a subsequent read on
	// the pipe should now block indefinitely rather than fail immediately
	// with a deadline-exceeded error, so poke it with a short local timeout.
	_ = server.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	buf := make([]byte, 1)
	_, err := server.Read(buf)
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("read after PeekALPN returned = %v, want a fresh timeout (deadline was cleared)", err)
	}
}

func TestPeekALPNNotTLS(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	go func() {
		client.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		client.Close()
	}()
	acme, _ := PeekALPN(server)
	if acme {
		t.Fatal("PeekALPN reported acme for a non-TLS stream")
	}
}
