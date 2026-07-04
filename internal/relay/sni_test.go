package relay

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// A real TLS client sends a ClientHello with ServerName; readSNI must recover
// it and buffer the bytes it consumed.
func TestReadSNI(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	go func() {
		// Client handshake will not complete (server side aborts) — that's fine;
		// we only need the ClientHello to be written.
		conn := tls.Client(c, &tls.Config{ServerName: "blog.alice.example.com", InsecureSkipVerify: true})
		conn.SetDeadline(time.Now().Add(time.Second))
		conn.Handshake()
	}()

	s.SetDeadline(time.Now().Add(2 * time.Second))
	sni, buffered, err := readSNI(s)
	if err != nil {
		t.Fatalf("readSNI: %v", err)
	}
	if sni != "blog.alice.example.com" {
		t.Fatalf("sni = %q", sni)
	}
	if len(buffered) == 0 {
		t.Fatal("expected buffered ClientHello bytes")
	}
}
