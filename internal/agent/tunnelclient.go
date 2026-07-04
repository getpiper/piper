// Package agent holds piperd's relay-mode runtime helpers (the outbound tunnel
// client). It depends only on internal/tunnel and the standard library.
package agent

import (
	"context"
	"io"
	"log"
	"net"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

// RunTunnelClient maintains an outbound tunnel to relayAddr, registering
// baseDomain, and forwards each accepted stream to a fresh dialLocal() conn. It
// reconnects with backoff until ctx is cancelled. Blocks.
func RunTunnelClient(ctx context.Context, relayAddr, token, baseDomain string, dialLocal func() (net.Conn, error)) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := net.Dial("tcp", relayAddr)
		if err != nil {
			log.Printf("tunnel: dial relay: %v (retry in %s)", err, backoff)
			sleep(ctx, backoff)
			backoff = nextBackoff(backoff)
			continue
		}
		sess, err := tunnel.Dial(conn, token, baseDomain)
		if err != nil {
			log.Printf("tunnel: handshake: %v (retry in %s)", err, backoff)
			conn.Close()
			sleep(ctx, backoff)
			backoff = nextBackoff(backoff)
			continue
		}
		log.Printf("tunnel: connected to relay %s as %s", relayAddr, baseDomain)
		backoff = time.Second
		serveStreams(ctx, sess, dialLocal)
	}
}

func serveStreams(ctx context.Context, sess *tunnel.Session, dialLocal func() (net.Conn, error)) {
	defer sess.Close()
	for {
		stream, err := sess.Accept()
		if err != nil {
			return // session died; caller reconnects
		}
		go func() {
			defer stream.Close()
			local, err := dialLocal()
			if err != nil {
				log.Printf("tunnel: dial local: %v", err)
				return
			}
			defer local.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(local, stream); done <- struct{}{} }()
			go func() { io.Copy(stream, local); done <- struct{}{} }()
			<-done
		}()
	}
}

func nextBackoff(d time.Duration) time.Duration {
	if d >= 30*time.Second {
		return 30 * time.Second
	}
	return d * 2
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
