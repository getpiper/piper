package relay

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

// connQueue adapts SNI-dispatched control-plane connections into a
// net.Listener so one http.Server can serve them all. handlePublic pushes each
// terminated TLS conn; the server owns its lifetime from there.
type connQueue struct {
	ch   chan net.Conn
	done chan struct{}
}

func newConnQueue() *connQueue {
	return &connQueue{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (q *connQueue) Accept() (net.Conn, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.done:
		return nil, net.ErrClosed
	}
}

func (q *connQueue) Close() error {
	select {
	case <-q.done:
	default:
		close(q.done)
	}
	return nil
}

func (q *connQueue) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }

func (q *connQueue) push(c net.Conn) {
	select {
	case q.ch <- c:
	case <-q.done:
		c.Close()
	}
}

// Serve runs the relay: it accepts agent tunnels on tunnelAddr and public TLS
// traffic on tlsAddr, routing each connection by SNI. tlsCfg is the wildcard
// config for relay-terminated shared-domain apps; nil ⇒ passthrough-only.
// ctrl, when non-nil and a wildcard cert is armed, is the relay's own HTTP API,
// served TLS-terminated at SNI "api.<apex>" (#73). Blocks until a listener fails.
func Serve(tlsAddr, tunnelAddr string, st *Store, tlsCfg *tls.Config, router *Router, ctrl http.Handler) error {
	var ctrlQ *connQueue
	if ctrl != nil && tlsCfg != nil {
		ctrlQ = newConnQueue()
		srv := &http.Server{Handler: ctrl, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		go func() { _ = srv.Serve(ctrlQ) }()
	}
	ctrlHost := "api." + st.apexOrDefault()

	tunLn, err := net.Listen("tcp", tunnelAddr)
	if err != nil {
		return err
	}
	go acceptTunnels(tunLn, st, router)

	tlsLn, err := net.Listen("tcp", tlsAddr)
	if err != nil {
		return err
	}
	for {
		conn, err := tlsLn.Accept()
		if err != nil {
			return err
		}
		go handlePublic(conn, router, tlsCfg, ctrlHost, ctrlQ)
	}
}

func acceptTunnels(ln net.Listener, st *Store, router *Router) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			sess, err := tunnel.Serve(conn, func(token, base string) error {
				ag, err := st.Authenticate(token)
				if err != nil {
					return err
				}
				if ag.BaseDomain != base {
					return ErrBadToken // token may only claim its enrolled base domain
				}
				return nil
			})
			if err != nil {
				conn.Close()
				return
			}
			router.Register(sess)
			log.Printf("agent registered: %s", sess.BaseDomain)
			go serveControl(sess, st, router)
			<-sess.CloseChan()
			router.Unregister(sess)
			log.Printf("agent gone: %s", sess.BaseDomain)
		}()
	}
}

// serveControl accepts the agent's control streams (KindControl) for the life of
// the session and dispatches each. Non-control streams are ignored (the agent
// never opens them). Returns when the session dies.
func serveControl(sess *tunnel.Session, st *Store, router *Router) {
	for {
		kind, stream, err := sess.AcceptKind()
		if err != nil {
			return
		}
		if kind != tunnel.KindControl {
			stream.Close()
			continue
		}
		go handleControl(stream, sess, st, router)
	}
}

// handleControl serves one control request: register or deregister a hostname
// for this session's account.
func handleControl(stream net.Conn, sess *tunnel.Session, st *Store, router *Router) {
	defer stream.Close()
	var req tunnel.ControlRequest
	if err := tunnel.ReadMsg(stream, &req); err != nil {
		return
	}
	switch req.Op {
	case "register":
		host, err := st.RegisterHostname(sess.BaseDomain, req.App)
		if err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		router.RegisterHost(host, sess)
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Hostname: host})
	case "deregister":
		_ = st.DeregisterHostname(sess.BaseDomain, req.Hostname)
		router.UnregisterHost(req.Hostname)
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Hostname: req.Hostname})
	case "provision":
		// The box hands the relay its control-API bearer (agent-push Token B).
		// The op rides the authenticated session, so it can only ever set the
		// token for the session's own agent.
		if req.Token == "" {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "provision: empty token"})
			return
		}
		if err := st.SetControlToken(sess.BaseDomain, req.Token); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	default:
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "unknown op"})
	}
}

func handlePublic(conn net.Conn, router *Router, tlsCfg *tls.Config, ctrlHost string, ctrlQ *connQueue) {
	sni, buffered, err := readSNI(conn)
	if err != nil {
		conn.Close()
		return
	}
	// Control plane: api.<apex> is the relay's own TLS-terminated HTTP API,
	// checked before app routing so no app registration can ever shadow it.
	if ctrlQ != nil && sni == ctrlHost {
		ctrlQ.push(tls.Server(&prefixConn{Conn: conn, prefix: buffered}, tlsCfg))
		return
	}
	defer conn.Close()
	if sess, ok := router.LookupHost(sni); ok {
		if tlsCfg == nil {
			return // terminated hostname but no wildcard cert configured
		}
		terminate(conn, buffered, sess, tlsCfg)
		return
	}
	if sess, ok := router.Lookup(sni); ok {
		passthrough(conn, buffered, sess)
	}
}

// passthrough is the Plan-2 SNI-splice: replay the ClientHello down a KindPassthrough
// stream and pipe raw bytes; the box terminates TLS.
func passthrough(conn net.Conn, buffered []byte, sess *tunnel.Session) {
	stream, err := sess.OpenKind(tunnel.KindPassthrough)
	if err != nil {
		return
	}
	defer stream.Close()
	if _, err := stream.Write(buffered); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(stream, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, stream); done <- struct{}{} }()
	<-done
}
