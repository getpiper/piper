package tunnel

import (
	"io"
	"net"
	"testing"
	"time"
)

// handshake + a round-trip stream over an in-process pipe.
func TestDialServeRoundTrip(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	type res struct {
		sess *Session
		err  error
	}
	srvCh := make(chan res, 1)
	go func() {
		sess, err := Serve(s, func(token, base string) error {
			if token != "tok-123" || base != "alice.example.com" {
				t.Errorf("bad handshake: %q %q", token, base)
			}
			return nil
		})
		srvCh <- res{sess, err}
	}()

	cli, err := Dial(c, "tok-123", "alice.example.com")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sr := <-srvCh
	if sr.err != nil {
		t.Fatalf("Serve: %v", sr.err)
	}
	if sr.sess.BaseDomain != "alice.example.com" {
		t.Fatalf("server BaseDomain = %q", sr.sess.BaseDomain)
	}

	// Server opens a stream; agent accepts and echoes.
	go func() {
		st, err := cli.Accept()
		if err != nil {
			return
		}
		io.Copy(st, st)
		st.Close()
	}()

	stream, err := sr.sess.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	stream.SetDeadline(time.Now().Add(2 * time.Second))
	stream.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
}

func TestServeRejectsBadAuth(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	go Dial(c, "wrong", "alice.example.com")
	_, err := Serve(s, func(token, base string) error {
		return io.EOF // any non-nil rejects
	})
	if err == nil {
		t.Fatal("expected Serve to reject bad auth")
	}
}
