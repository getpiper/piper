package relay

import (
	"io"
	"log"
	"net"

	"github.com/getpiper/piper/internal/tunnel"
)

// Serve runs the relay: it accepts agent tunnels on tunnelAddr and public TLS
// traffic on tlsAddr, routing each connection by SNI. Blocks until a listener
// fails.
func Serve(tlsAddr, tunnelAddr string, st *Store) error {
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
		go handlePublic(conn, router)
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
			<-sess.CloseChan()
			router.Unregister(sess)
			log.Printf("agent gone: %s", sess.BaseDomain)
		}()
	}
}

func handlePublic(conn net.Conn, router *Router) {
	defer conn.Close()
	sni, buffered, err := readSNI(conn)
	if err != nil {
		return
	}
	sess, ok := router.Lookup(sni)
	if !ok {
		return
	}
	stream, err := sess.Open()
	if err != nil {
		return
	}
	defer stream.Close()
	// Replay the ClientHello bytes we consumed, then pipe both directions.
	if _, err := stream.Write(buffered); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(stream, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, stream); done <- struct{}{} }()
	<-done
}
