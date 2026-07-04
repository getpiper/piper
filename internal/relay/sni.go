package relay

import (
	"crypto/tls"
	"errors"
	"net"
	"time"
)

// preAuthReadTimeout bounds the unauthenticated read of the TLS ClientHello on
// the public TLS listener. Without it a client that connects and sends nothing
// pins a goroutine + fd forever (slowloris / fd-exhaustion). Tests override it
// to a tiny value. Cleared once the ClientHello is in hand.
var preAuthReadTimeout = 10 * time.Second

// recordingConn records every byte read from the underlying conn (so the
// consumed ClientHello can be replayed) and blocks writes (the handshake we run
// must never send a ServerHello back to the client).
type recordingConn struct {
	net.Conn
	buf []byte
}

func (r *recordingConn) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	r.buf = append(r.buf, p[:n]...)
	return n, err
}

func (r *recordingConn) Write(p []byte) (int, error) { return len(p), nil }

var errSNICaptured = errors.New("sni captured")

// readSNI peeks the TLS ClientHello on conn, returns its SNI and the raw bytes
// consumed (to be replayed down the tunnel). It never completes a handshake and
// never decrypts application data.
func readSNI(conn net.Conn) (string, []byte, error) {
	// Deadline the unauthenticated ClientHello read; clear it once captured so
	// the established pipe isn't killed mid-traffic.
	_ = conn.SetReadDeadline(time.Now().Add(preAuthReadTimeout))
	defer conn.SetReadDeadline(time.Time{})

	rec := &recordingConn{Conn: conn}
	var sni string
	cfg := &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			sni = hello.ServerName
			return nil, errSNICaptured // abort the handshake immediately
		},
	}
	err := tls.Server(rec, cfg).Handshake()
	if sni == "" {
		if err == nil {
			err = errors.New("no SNI in ClientHello")
		}
		return "", rec.buf, err
	}
	return sni, rec.buf, nil
}
