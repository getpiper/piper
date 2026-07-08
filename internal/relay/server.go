package relay

import (
	"crypto/tls"
	"io"
	"log"
	"net"

	"github.com/getpiper/piper/internal/tunnel"
)

// Serve runs the relay: it accepts agent tunnels on tunnelAddr and public TLS
// traffic on tlsAddr, routing each connection by SNI. tlsCfg is the wildcard
// config for relay-terminated shared-domain apps; nil ⇒ passthrough-only. Blocks
// until a listener fails.
func Serve(tlsAddr, tunnelAddr string, st *Store, tlsCfg *tls.Config) error {
	router := NewRouter()

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
		go handlePublic(conn, router, tlsCfg)
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
	default:
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "unknown op"})
	}
}

func handlePublic(conn net.Conn, router *Router, tlsCfg *tls.Config) {
	defer conn.Close()
	sni, buffered, err := readSNI(conn)
	if err != nil {
		return
	}
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
