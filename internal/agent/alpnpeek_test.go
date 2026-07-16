package agent

import (
	"bytes"
	"crypto/tls"
	"net"
	"testing"
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
