package agent

import (
	"crypto/tls"
	"errors"
	"net"
	"time"
)

// acmeTLSProtocol is the TLS-ALPN-01 protocol ID (RFC 8737). Kept as a local
// literal so this package stays lego-free (it depends only on internal/tunnel
// and the standard library).
const acmeTLSProtocol = "acme-tls/1"

// alpnPeekTimeout bounds the ClientHello read off a passthrough stream. The
// relay replays the hello immediately after opening the stream, so this only
// trips on a stalled or broken peer.
var alpnPeekTimeout = 10 * time.Second

// recordingConn records every byte read from the underlying conn (so the
// consumed ClientHello can be replayed into the local backend) and blackholes
// writes (the peek handshake must never answer the client). Mirrors
// internal/relay/sni.go.
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

var errHelloCaptured = errors.New("client hello captured")

// PeekALPN reads the TLS ClientHello from stream and reports whether it
// offers the acme-tls/1 ALPN protocol (a TLS-ALPN-01 validation), returning
// the bytes consumed so the caller can replay them into whichever local
// backend it dials. A stream that isn't TLS (or times out) reports false;
// whatever was consumed is still returned and the unread remainder stays in
// the stream for the normal splice.
func PeekALPN(stream net.Conn) (acme bool, consumed []byte) {
	_ = stream.SetReadDeadline(time.Now().Add(alpnPeekTimeout))
	defer stream.SetReadDeadline(time.Time{})

	rec := &recordingConn{Conn: stream}
	cfg := &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			for _, p := range hello.SupportedProtos {
				if p == acmeTLSProtocol {
					acme = true
				}
			}
			return nil, errHelloCaptured // abort the handshake immediately
		},
	}
	_ = tls.Server(rec, cfg).Handshake()
	return acme, rec.buf
}
