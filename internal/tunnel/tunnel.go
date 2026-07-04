// Package tunnel multiplexes streams over a single connection between the agent
// and the relay. The agent dials out (beating NAT/CGNAT), presents a token and
// its base domain, and both ends open/accept yamux streams over that link.
package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"

	"github.com/hashicorp/yamux"
)

// Auth validates a client's presented token and claimed base domain. A non-nil
// return rejects the connection.
type Auth func(token, baseDomain string) error

type handshake struct {
	Token      string `json:"token"`
	BaseDomain string `json:"base_domain"`
}

// Session is a live multiplexed link. Open (server→agent) and Accept (agent
// side) yield net.Conn streams.
type Session struct {
	BaseDomain string
	mux        *yamux.Session
}

func (s *Session) Open() (net.Conn, error)     { return s.mux.Open() }
func (s *Session) Accept() (net.Conn, error)   { return s.mux.Accept() }
func (s *Session) CloseChan() <-chan struct{}  { return s.mux.CloseChan() }
func (s *Session) Close() error                { return s.mux.Close() }

// writeFrame writes a uint16-length-prefixed payload. Length-prefixing (rather
// than a json.Decoder) guarantees we consume exactly the handshake bytes and
// leave the rest of the stream untouched for yamux.
func writeFrame(w io.Writer, b []byte) error {
	if len(b) > 0xffff {
		return fmt.Errorf("handshake too large")
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// Dial performs the client handshake over conn, then starts a yamux client.
func Dial(conn net.Conn, token, baseDomain string) (*Session, error) {
	payload, _ := json.Marshal(handshake{Token: token, BaseDomain: baseDomain})
	if err := writeFrame(conn, payload); err != nil {
		return nil, err
	}
	mux, err := yamux.Client(conn, nil)
	if err != nil {
		return nil, err
	}
	return &Session{BaseDomain: baseDomain, mux: mux}, nil
}

// Serve reads the client handshake over conn, authorizes it, then starts a
// yamux server. On auth failure it returns the auth error (caller closes conn).
func Serve(conn net.Conn, auth Auth) (*Session, error) {
	payload, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	var hs handshake
	if err := json.Unmarshal(payload, &hs); err != nil {
		return nil, err
	}
	if err := auth(hs.Token, hs.BaseDomain); err != nil {
		return nil, err
	}
	mux, err := yamux.Server(conn, nil)
	if err != nil {
		return nil, err
	}
	return &Session{BaseDomain: hs.BaseDomain, mux: mux}, nil
}
