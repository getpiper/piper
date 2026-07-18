// Package agent holds piperd's relay-mode runtime helpers (the outbound tunnel
// client). It depends only on internal/tunnel and the standard library.
package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

// ErrNotConnected is returned by Register/Deregister when no relay session is live.
var ErrNotConnected = errors.New("relay tunnel not connected")

// TunnelClient maintains an outbound tunnel to the relay and exposes hostname
// registration over it. The current session is published under a mutex so the
// deploy path can open control streams on whatever session is live.
type TunnelClient struct {
	mu   sync.Mutex
	sess *tunnel.Session

	// OnConnect, if set before Run, is invoked in its own goroutine each time a
	// relay session is established — piperd uses it to provision the relay's
	// control bearer (see the control-stream routing design).
	OnConnect func()
}

func (c *TunnelClient) setSession(s *tunnel.Session) {
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
}

func (c *TunnelClient) current() *tunnel.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess
}

// Run maintains the tunnel to relayAddr, registering baseDomain, and forwards
// each relay-opened stream to dialLocal(kind, stream). dialLocal may peek
// (read) bytes from stream before choosing a backend; it must replay whatever
// it consumed into the returned conn. It reconnects with backoff until ctx is
// cancelled. Blocks.
func (c *TunnelClient) Run(ctx context.Context, relayAddr, token, baseDomain string, dialLocal func(kind byte, stream net.Conn) (net.Conn, error)) {
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
		c.setSession(sess)
		if c.OnConnect != nil {
			go c.OnConnect()
		}
		start := time.Now()
		serveStreams(ctx, sess, dialLocal)
		c.setSession(nil)
		if time.Since(start) > healthyThreshold {
			backoff = time.Second
		}
		sleep(ctx, backoff)
		backoff = nextBackoff(backoff)
	}
}

// Register opens a control stream on the current session and asks the relay to
// assign/return the public hostname for app.
func (c *TunnelClient) Register(app string) (string, error) {
	return c.control(tunnel.ControlRequest{Op: "register", App: app})
}

// Deregister asks the relay to drop hostname.
func (c *TunnelClient) Deregister(hostname string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "deregister", Hostname: hostname})
	return err
}

// Provision hands the relay this box's control-API bearer for the enrollment.
// It rides the authenticated session, so it can only set this agent's token.
func (c *TunnelClient) Provision(token string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "provision", Token: token})
	return err
}

// AddCustomDomain claims domain on the relay as a pending custom domain
// (#227): routable immediately so the TLS-ALPN-01 challenge can reach this
// box, evictable if not confirmed within the relay's pending TTL. It rides
// the authenticated session, so it can only ever claim for this agent.
func (c *TunnelClient) AddCustomDomain(domain string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "add-domain", Domain: domain})
	return err
}

// RemoveCustomDomain drops this agent's claim on domain and its routing.
func (c *TunnelClient) RemoveCustomDomain(domain string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "remove-domain", Domain: domain})
	return err
}

// ConfirmCustomDomain reports that this box holds an issued cert for domain,
// flipping the relay's pending claim to active (permanent, reconnect-safe).
func (c *TunnelClient) ConfirmCustomDomain(domain string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "domain-active", Domain: domain})
	return err
}

func (c *TunnelClient) control(req tunnel.ControlRequest) (string, error) {
	sess := c.current()
	if sess == nil {
		return "", ErrNotConnected
	}
	stream, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	if err := tunnel.WriteMsg(stream, req); err != nil {
		return "", err
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(stream, &resp); err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	return resp.Hostname, nil
}

// healthyThreshold is how long a session must stay up before a subsequent
// disconnect is treated as "was fine" and resets backoff to the floor. A
// session that dies before this (e.g. relay rejects auth immediately) keeps
// the backoff growing so a misconfigured token doesn't busy-spin reconnects.
const healthyThreshold = 10 * time.Second

func serveStreams(ctx context.Context, sess *tunnel.Session, dialLocal func(kind byte, stream net.Conn) (net.Conn, error)) {
	defer sess.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stopCancel()
	for {
		kind, stream, err := sess.AcceptKind()
		if err != nil {
			return // session died; caller reconnects
		}
		go func() {
			defer stream.Close()
			local, err := dialLocal(kind, stream)
			if err != nil {
				log.Printf("tunnel: dial local (kind %q): %v", kind, err)
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
