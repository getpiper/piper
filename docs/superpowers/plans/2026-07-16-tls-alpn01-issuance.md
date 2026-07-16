# TLS-ALPN-01 Issuance Path Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Issue exact-host certificates via TLS-ALPN-01 — no DNS-provider API token — with the challenge answered by an in-process solver that the tunnel passthrough path splices `acme-tls/1` connections to.

**Architecture:** `internal/certs` grows a second challenge mode (`ALPNSolver` alongside `DNSProvider`, exactly one required). A new `certs.ALPNSolver` is both a lego `challenge.Provider` and a loopback TLS listener serving per-domain challenge certs. The agent's `dialLocal` gains the stream so `cmd/piperd` can peek each passthrough ClientHello (`internal/agent.PeekALPN`, the relay's recording-conn trick) and route `acme-tls/1` to the solver, everything else to Caddy's `:443` unchanged.

**Tech Stack:** Go, `github.com/go-acme/lego/v4` (already a dependency; `challenge/tlsalpn01` provides `ChallengeCert` and the `acme-tls/1` protocol ID), `crypto/tls`.

**Spec:** `docs/superpowers/specs/2026-07-16-tls-alpn01-issuance-design.md` · **Issue:** #226 · **Epic:** #224

## Global Constraints

- `CGO_ENABLED=0` must keep building (no cgo anywhere).
- Module path `github.com/getpiper/piper`.
- Layering: `internal/agent` depends only on `internal/tunnel` + stdlib — do NOT import lego there (use a local `"acme-tls/1"` constant). `internal/relay` is not touched at all.
- DNS-01 behavior must stay byte-for-byte: `NewCloudflareIssuer`, `Manager.Obtain`, account-key handling unchanged.
- Run `make verify` (gofmt → vet → test → arm64 cross-build) before claiming done.
- Branch: `faruk/alpn01-certs` (already created; spec is committed on it). One commit per task, conventional-commit style, ending with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

### Task 1: `certs.Config` — exactly one challenge mode

`Config` gains an optional `ALPNSolver challenge.Provider`; `New` requires exactly one of `DNSProvider`/`ALPNSolver` and calls the matching lego setter. Validation happens before any network I/O (ACME registration), so the error cases unit-test offline.

**Files:**
- Modify: `internal/certs/certs.go`
- Test: `internal/certs/certs_test.go` (create)

**Interfaces:**
- Consumes: nothing new.
- Produces: `certs.Config{Email string, CADirURL string, DNSProvider challenge.Provider, ALPNSolver challenge.Provider, AccountKey *ecdsa.PrivateKey}`; `certs.New(Config) (*Manager, error)` errors unless exactly one provider field is set. Task 3 constructs `New(Config{…, ALPNSolver: solver, …})`.

- [ ] **Step 1: Write the failing test**

Create `internal/certs/certs_test.go` (`fakeDNS` already exists in `obtain_test.go`, same package):

```go
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
)

// New must refuse ambiguous or empty challenge configuration: exactly one of
// DNSProvider (wildcard, box-wide BYO) or ALPNSolver (exact-host, per-app
// BYO) — and it must refuse before any ACME network I/O, which is what lets
// this test run offline.
func TestNewRequiresExactlyOneChallengeMode(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"neither", Config{Email: "e@example.com", AccountKey: key}},
		{"both", Config{Email: "e@example.com", AccountKey: key, DNSProvider: fakeDNS{}, ALPNSolver: fakeDNS{}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.cfg); err == nil {
				t.Fatal("New() error = nil, want exactly-one-challenge-mode error")
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/certs/ -run TestNewRequiresExactlyOneChallengeMode -v`
Expected: FAIL — build error `unknown field ALPNSolver in struct literal of type Config`.

- [ ] **Step 3: Implement**

In `internal/certs/certs.go`, replace the `Config` doc comment + struct and the challenge wiring inside `New`:

```go
// Config configures ACME issuance. Exactly one challenge mode must be set:
// DNSProvider (DNS-01, any lego challenge provider — needed for wildcard
// certs, box-wide BYO) or ALPNSolver (TLS-ALPN-01 — tokenless exact-host
// certs, per-app BYO). AccountKey is the persisted ACME account key.
type Config struct {
	Email       string
	CADirURL    string
	DNSProvider challenge.Provider
	ALPNSolver  challenge.Provider
	AccountKey  *ecdsa.PrivateKey
}
```

At the top of `New`, before constructing the lego client (registration is a network call; invalid config must fail before it):

```go
func New(cfg Config) (*Manager, error) {
	if (cfg.DNSProvider == nil) == (cfg.ALPNSolver == nil) {
		return nil, fmt.Errorf("certs: exactly one of DNSProvider or ALPNSolver must be set")
	}
	u := &user{email: cfg.Email, key: cfg.AccountKey}
	...
```

and replace the unconditional `SetDNS01Provider` call:

```go
	if cfg.DNSProvider != nil {
		if err := client.Challenge.SetDNS01Provider(cfg.DNSProvider); err != nil {
			return nil, err
		}
	} else {
		if err := client.Challenge.SetTLSALPN01Provider(cfg.ALPNSolver); err != nil {
			return nil, err
		}
	}
```

Add `"fmt"` to the imports. Also update the package-level `Manager` doc comment (`// Manager obtains certificates via ACME DNS-01.` → `// Manager obtains certificates via ACME (DNS-01 or TLS-ALPN-01).`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/certs/ ./internal/domain/ -v`
Expected: PASS (new test green; all existing DNS-01 tests untouched and green; pebble test skips).

- [ ] **Step 5: Commit**

```bash
git add internal/certs/certs.go internal/certs/certs_test.go
git commit -m "feat(certs): require exactly one challenge mode; add TLS-ALPN-01 wiring

Part of #226 (epic #224).

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: `certs.ALPNSolver` — lego provider + loopback answerer

One type that is both sides of the challenge: `Present`/`CleanUp` maintain a domain→challenge-cert map; a loopback TLS listener completes the `acme-tls/1` handshake with the matching cert. The keyed map serves N concurrent domains (needed by #229); lego's own `ProviderServer` binds a whole fixed port per challenge and can't share.

**Files:**
- Create: `internal/certs/alpn.go`
- Test: `internal/certs/alpn_test.go` (create)

**Interfaces:**
- Consumes: `tlsalpn01.ChallengeCert(domain, keyAuth string) (*tls.Certificate, error)` and `tlsalpn01.ACMETLS1Protocol` (`"acme-tls/1"`) from `github.com/go-acme/lego/v4/challenge/tlsalpn01`.
- Produces: `certs.NewALPNSolver(listenAddr string) (*ALPNSolver, error)`; methods `Present(domain, token, keyAuth string) error`, `CleanUp(domain, token, keyAuth string) error` (satisfies `challenge.Provider`), `Addr() string`, `Close() error`. Task 3 passes it as `Config.ALPNSolver`; Task 5 dials `Addr()`.

- [ ] **Step 1: Write the failing tests**

Create `internal/certs/alpn_test.go`:

```go
package certs

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"testing"

	"github.com/go-acme/lego/v4/challenge/tlsalpn01"
)

// dialSolver completes an acme-tls/1 handshake against the solver with the
// given SNI — what an ACME validator does — and returns the connection state.
func dialSolver(t *testing.T, addr, sni string) (tls.ConnectionState, error) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		ServerName:         sni,
		NextProtos:         []string{tlsalpn01.ACMETLS1Protocol},
		InsecureSkipVerify: true,
	})
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer conn.Close()
	return conn.ConnectionState(), nil
}

// assertACMEDigest checks the RFC 8737 acmeIdentifier extension (OID
// 1.3.6.1.5.5.7.1.31) carries the SHA-256 digest of keyAuth — the thing the
// ACME validator actually verifies.
func assertACMEDigest(t *testing.T, leaf *x509.Certificate, keyAuth string) {
	t.Helper()
	want := sha256.Sum256([]byte(keyAuth))
	oid := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}
	for _, ext := range leaf.Extensions {
		if !ext.Id.Equal(oid) {
			continue
		}
		var digest []byte
		if _, err := asn1.Unmarshal(ext.Value, &digest); err != nil {
			t.Fatalf("unmarshal acmeIdentifier extension: %v", err)
		}
		if !bytes.Equal(digest, want[:]) {
			t.Fatal("acmeIdentifier digest does not match keyAuth")
		}
		return
	}
	t.Fatal("presented cert has no acmeIdentifier extension")
}

func TestALPNSolverAnswersChallenge(t *testing.T) {
	s, err := NewALPNSolver("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewALPNSolver: %v", err)
	}
	defer s.Close()
	const domain, keyAuth = "myshop.example.com", "token.account-thumbprint"
	if err := s.Present(domain, "token", keyAuth); err != nil {
		t.Fatalf("Present: %v", err)
	}

	cs, err := dialSolver(t, s.Addr(), domain)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if cs.NegotiatedProtocol != tlsalpn01.ACMETLS1Protocol {
		t.Fatalf("negotiated %q, want %q", cs.NegotiatedProtocol, tlsalpn01.ACMETLS1Protocol)
	}
	leaf := cs.PeerCertificates[0]
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != domain {
		t.Fatalf("cert DNSNames = %v, want [%s]", leaf.DNSNames, domain)
	}
	assertACMEDigest(t, leaf, keyAuth)
}

func TestALPNSolverConcurrentDomains(t *testing.T) {
	s, err := NewALPNSolver("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewALPNSolver: %v", err)
	}
	defer s.Close()
	for _, d := range []string{"a.example.com", "b.example.com"} {
		if err := s.Present(d, "token", "auth-"+d); err != nil {
			t.Fatalf("Present(%s): %v", d, err)
		}
	}
	for _, d := range []string{"a.example.com", "b.example.com"} {
		cs, err := dialSolver(t, s.Addr(), d)
		if err != nil {
			t.Fatalf("handshake for %s: %v", d, err)
		}
		if got := cs.PeerCertificates[0].DNSNames; len(got) != 1 || got[0] != d {
			t.Fatalf("SNI %s got cert for %v", d, got)
		}
	}
}

func TestALPNSolverCleanUp(t *testing.T) {
	s, err := NewALPNSolver("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewALPNSolver: %v", err)
	}
	defer s.Close()
	const domain, keyAuth = "gone.example.com", "token.thumb"
	if err := s.Present(domain, "token", keyAuth); err != nil {
		t.Fatalf("Present: %v", err)
	}
	if err := s.CleanUp(domain, "token", keyAuth); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if _, err := dialSolver(t, s.Addr(), domain); err == nil {
		t.Fatal("handshake succeeded after CleanUp, want failure")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/certs/ -run TestALPNSolver -v`
Expected: FAIL — build error `undefined: NewALPNSolver`.

- [ ] **Step 3: Implement**

Create `internal/certs/alpn.go`:

```go
package certs

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/challenge/tlsalpn01"
)

// alpnHandshakeTimeout bounds one validator connection's handshake so a
// stalled peer can't pin a goroutine.
const alpnHandshakeTimeout = 10 * time.Second

// ALPNSolver answers TLS-ALPN-01 challenges. It is both a lego
// challenge.Provider (Present stores the RFC 8737 challenge cert per domain,
// CleanUp drops it) and a loopback TLS listener that completes the acme-tls/1
// handshake with the cert matching the ClientHello's SNI. The passthrough
// path splices acme-tls/1 connections here (see cmd/piperd newDialLocal);
// no HTTP is ever spoken and Caddy is not involved.
type ALPNSolver struct {
	ln net.Listener

	mu    sync.Mutex
	certs map[string]*tls.Certificate
}

// NewALPNSolver starts the solver's TLS listener on listenAddr
// ("127.0.0.1:0" for an ephemeral port; the pebble test pins the port its
// validator dials).
func NewALPNSolver(listenAddr string) (*ALPNSolver, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("alpn solver: %w", err)
	}
	s := &ALPNSolver{certs: map[string]*tls.Certificate{}}
	s.ln = tls.NewListener(ln, &tls.Config{
		NextProtos:     []string{tlsalpn01.ACMETLS1Protocol},
		GetCertificate: s.getCertificate,
	})
	go s.serve()
	return s, nil
}

// serve completes handshakes; the validator inspects the presented cert and
// closes. Exits when Close shuts the listener.
func (s *ALPNSolver) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(alpnHandshakeTimeout))
			_ = c.(*tls.Conn).Handshake()
		}(conn)
	}
}

func (s *ALPNSolver) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if crt, ok := s.certs[hello.ServerName]; ok {
		return crt, nil
	}
	return nil, fmt.Errorf("alpn solver: no pending challenge for %q", hello.ServerName)
}

// Present implements challenge.Provider: build and arm the challenge cert.
func (s *ALPNSolver) Present(domain, token, keyAuth string) error {
	crt, err := tlsalpn01.ChallengeCert(domain, keyAuth)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.certs[domain] = crt
	s.mu.Unlock()
	return nil
}

// CleanUp implements challenge.Provider: disarm the domain's challenge cert.
func (s *ALPNSolver) CleanUp(domain, token, keyAuth string) error {
	s.mu.Lock()
	delete(s.certs, domain)
	s.mu.Unlock()
	return nil
}

// Addr is where the listener landed — the passthrough peek's splice target.
func (s *ALPNSolver) Addr() string { return s.ln.Addr().String() }

// Close stops the listener.
func (s *ALPNSolver) Close() error { return s.ln.Close() }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/certs/ -v`
Expected: PASS (all three ALPNSolver tests green, existing tests green, pebble tests skip).

- [ ] **Step 5: Commit**

```bash
git add internal/certs/alpn.go internal/certs/alpn_test.go
git commit -m "feat(certs): ALPNSolver — TLS-ALPN-01 provider + loopback answerer

Part of #226 (epic #224).

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Pebble integration test — tokenless exact-host `Obtain`

The issue's acceptance criterion: `Obtain` an exact-host cert with only the ALPN solver configured, no DNS provider. Env-gated exactly like the existing `TestObtainAgainstPebble` (skips in CI; runs when `RUN_ACME` points at a local Pebble).

**Files:**
- Modify: `internal/certs/obtain_test.go` (append)

**Interfaces:**
- Consumes: `NewALPNSolver` (Task 2), `Config.ALPNSolver` (Task 1).
- Produces: nothing (test only).

- [ ] **Step 1: Write the test**

Append to `internal/certs/obtain_test.go`:

```go
// TestObtainTLSALPNAgainstPebble issues an exact-host cert with only the
// ALPN solver configured — no DNS provider (the #226 acceptance criterion).
// Pebble's validator dials the domain at its configured tlsPort (default
// 5001), so the solver listens there; point RUN_ACME at a Pebble whose DNS
// resolves test domains to 127.0.0.1 (e.g. pebble-challtestsrv) and trust
// Pebble's CA via LEGO_CA_CERTIFICATES.
func TestObtainTLSALPNAgainstPebble(t *testing.T) {
	dir := os.Getenv("RUN_ACME")
	if dir == "" {
		t.Skip("set RUN_ACME=<pebble directory URL> with a reachable Pebble to run")
	}
	solver, err := NewALPNSolver("127.0.0.1:5001")
	if err != nil {
		t.Fatalf("NewALPNSolver: %v", err)
	}
	defer solver.Close()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	m, err := New(Config{
		Email:      "e2e@example.com",
		CADirURL:   dir,
		ALPNSolver: solver,
		AccountKey: key,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	certPEM, keyPEM, err := m.Obtain([]string{"alice.example.com"})
	if err != nil {
		t.Fatalf("Obtain: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("empty cert/key")
	}
}
```

- [ ] **Step 2: Verify it compiles and skips cleanly without Pebble**

Run: `go test ./internal/certs/ -run TestObtainTLSALPNAgainstPebble -v`
Expected: SKIP with "set RUN_ACME=…".

- [ ] **Step 3 (optional, if Docker is available): run it for real against Pebble**

```bash
docker run --rm -d --name pebble -p 14000:14000 -p 15000:15000 \
  -e PEBBLE_VA_NOSLEEP=1 ghcr.io/letsencrypt/pebble:latest \
  -config test/config/pebble-config.json -dnsserver 127.0.0.1:8053 -strict
# Pebble's default config validates TLS-ALPN against port 5001. Its
# in-container DNS must resolve alice.example.com to the host; if this local
# setup isn't practical, skip this step — the gated test exists for CI/e2e
# environments that already run Pebble.
LEGO_CA_CERTIFICATES=<pebble minica cert> RUN_ACME=https://localhost:14000/dir \
  go test ./internal/certs/ -run TestObtainTLSALPNAgainstPebble -v
docker stop pebble
```

Expected when the environment is right: PASS. If the container/DNS networking can't be arranged locally, note that in the task report and move on — Step 2 is the gate.

- [ ] **Step 4: Commit**

```bash
git add internal/certs/obtain_test.go
git commit -m "test(certs): pebble-gated TLS-ALPN-01 exact-host Obtain

Part of #226 (epic #224).

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: `agent.PeekALPN` — ClientHello peek for the passthrough path

The relay's recording-conn trick (`internal/relay/sni.go`), implemented fresh in `internal/agent` (which must not import relay or lego): read the ClientHello off a passthrough stream, report whether it offers `acme-tls/1`, and hand back the consumed bytes for replay. Never writes to the stream; a non-TLS stream reports false and its unread remainder stays put for the normal splice.

**Files:**
- Create: `internal/agent/alpnpeek.go`
- Test: `internal/agent/alpnpeek_test.go` (create)

**Interfaces:**
- Consumes: nothing beyond stdlib.
- Produces: `agent.PeekALPN(stream net.Conn) (acme bool, consumed []byte)`. Task 5's `newDialLocal` calls it and replays `consumed` into whichever backend it dials.

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/alpnpeek_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestPeekALPN -v`
Expected: FAIL — build error `undefined: PeekALPN`.

- [ ] **Step 3: Implement**

Create `internal/agent/alpnpeek.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -v`
Expected: PASS (peek tests green, existing tunnel-client tests untouched and green).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/alpnpeek.go internal/agent/alpnpeek_test.go
git commit -m "feat(agent): PeekALPN — detect acme-tls/1 on passthrough streams

Part of #226 (epic #224).

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Thread the stream through `dialLocal`; splice ACME passthrough to the solver

`dialLocal` grows the stream (`func(kind byte, stream net.Conn) (net.Conn, error)`) so the passthrough branch can peek. `cmd/piperd` starts the solver in relay mode and routes `acme-tls/1` hellos to it, replaying the consumed bytes; everything else keeps going to `127.0.0.1:443`.

**Files:**
- Modify: `internal/agent/tunnelclient.go` (Run + serveStreams signatures)
- Modify: `internal/agent/tunnelclient_test.go` (all dialLocal closures)
- Modify: `cmd/piperd/main.go` (`newDialLocal`, relay-mode wiring)
- Test: `cmd/piperd/main_test.go` (update existing + new routing test)

**Interfaces:**
- Consumes: `agent.PeekALPN` (Task 4), `certs.NewALPNSolver(...)` / `.Addr()` (Task 2).
- Produces: `TunnelClient.Run(ctx, relayAddr, token, baseDomain string, dialLocal func(kind byte, stream net.Conn) (net.Conn, error))`; `newDialLocal(terminated bool, authAddr, alpnAddr string) func(kind byte, stream net.Conn) (net.Conn, error)`. #229 will reuse the solver instance created here.

- [ ] **Step 1: Write the failing test**

Append to `cmd/piperd/main_test.go`:

```go
// A passthrough stream whose ClientHello offers acme-tls/1 is a TLS-ALPN-01
// validation: it must be spliced to the in-process solver — with the peeked
// hello bytes replayed so the handshake isn't corrupted — instead of Caddy's
// :443 (#226).
func TestDialLocalPassthroughACMEGoesToSolver(t *testing.T) {
	solver, err := net.Listen("tcp", "127.0.0.1:0") // stands in for the ALPN solver
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer solver.Close()
	gotBytes := make(chan []byte, 1)
	go func() {
		c, err := solver.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		gotBytes <- buf[:n]
		c.Close()
	}()

	client, server := net.Pipe()
	defer client.Close()
	go func() {
		_ = tls.Client(client, &tls.Config{
			ServerName:         "myshop.example.com",
			NextProtos:         []string{"acme-tls/1"},
			InsecureSkipVerify: true,
		}).Handshake()
	}()

	dial := newDialLocal(false, "127.0.0.1:1", solver.Addr().String())
	conn, err := dial(tunnel.KindPassthrough, server)
	if err != nil {
		t.Fatalf("dial passthrough: %v", err)
	}
	defer conn.Close()

	select {
	case replayed := <-gotBytes:
		// 0x16 = TLS handshake record: the consumed ClientHello was replayed.
		if len(replayed) == 0 || replayed[0] != 0x16 {
			t.Fatalf("solver received %d bytes (want a replayed TLS ClientHello)", len(replayed))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acme-tls/1 passthrough never reached the solver")
	}
}
```

Add `"crypto/tls"` to `main_test.go` imports if absent.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/piperd/ -run TestDialLocalPassthroughACMEGoesToSolver -v`
Expected: FAIL — build error (`newDialLocal` takes 2 args, dial takes 1).

- [ ] **Step 3: Update `internal/agent/tunnelclient.go`**

Change both signatures and the call (three mechanical edits):

```go
// Run maintains the tunnel to relayAddr, registering baseDomain, and forwards
// each relay-opened stream to dialLocal(kind, stream). dialLocal may peek
// (read) bytes from stream before choosing a backend; it must replay whatever
// it consumed into the returned conn. It reconnects with backoff until ctx is
// cancelled. Blocks.
func (c *TunnelClient) Run(ctx context.Context, relayAddr, token, baseDomain string, dialLocal func(kind byte, stream net.Conn) (net.Conn, error)) {
```

```go
func serveStreams(ctx context.Context, sess *tunnel.Session, dialLocal func(kind byte, stream net.Conn) (net.Conn, error)) {
```

and inside the accept goroutine:

```go
			local, err := dialLocal(kind, stream)
```

- [ ] **Step 4: Update `internal/agent/tunnelclient_test.go`**

Every dialLocal closure gains the stream parameter (8 sites; they ignore it):
- `func(kind byte) (net.Conn, error) {` → `func(kind byte, _ net.Conn) (net.Conn, error) {`
- `func(byte) (net.Conn, error) {` → `func(byte, net.Conn) (net.Conn, error) {`

```bash
sed -i '' \
  -e 's/func(kind byte) (net.Conn, error)/func(kind byte, _ net.Conn) (net.Conn, error)/' \
  -e 's/func(byte) (net.Conn, error)/func(byte, net.Conn) (net.Conn, error)/' \
  internal/agent/tunnelclient_test.go
```

Then confirm with `go vet ./internal/agent/` that nothing was missed.

- [ ] **Step 5: Update `cmd/piperd/main.go`**

Replace `newDialLocal` (keep it next to its current location):

```go
// newDialLocal maps relay tunnel stream kinds to local addresses. Control
// streams go to the authenticated listener (authAddr) — never the tokenless
// local one, or the relay path would silently lose its bearer gate (#221).
// Terminated mode serves apps plaintext on :80; otherwise TLS on :443.
// Passthrough streams whose ClientHello offers acme-tls/1 are TLS-ALPN-01
// validations and are spliced to the in-process solver (alpnAddr) instead of
// Caddy, with the peeked hello replayed into whichever backend is dialed (#226).
func newDialLocal(terminated bool, authAddr, alpnAddr string) func(kind byte, stream net.Conn) (net.Conn, error) {
	return func(kind byte, stream net.Conn) (net.Conn, error) {
		switch {
		case kind == tunnel.KindControlAPI:
			return net.Dial("tcp", authAddr)
		case terminated && kind == tunnel.KindHTTP:
			return net.Dial("tcp", "127.0.0.1:80")
		default:
			acme, consumed := agent.PeekALPN(stream)
			addr := "127.0.0.1:443"
			if acme && alpnAddr != "" {
				addr = alpnAddr
			}
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				return nil, err
			}
			if _, err := conn.Write(consumed); err != nil {
				conn.Close()
				return nil, err
			}
			return conn, nil
		}
	}
}
```

In the relay-mode block (`if cfg.RelayAddr != ""`), start the solver before building dialLocal — replace:

```go
		dialLocal := newDialLocal(cfg.Terminated, authAddr)
```

with:

```go
		// The TLS-ALPN-01 solver runs whenever relay mode is up: idle it is one
		// dormant loopback listener. Issuance wiring lands with the per-domain
		// lifecycle manager (#229); this guarantees the challenge is answerable.
		alpnSolver, err := certs.NewALPNSolver("127.0.0.1:0")
		if err != nil {
			log.Fatalf("alpn solver: %v", err)
		}
		dialLocal := newDialLocal(cfg.Terminated, authAddr, alpnSolver.Addr())
```

(`internal/certs` and `internal/agent` are already imported in `main.go`.)

- [ ] **Step 6: Update the existing test in `cmd/piperd/main_test.go`**

In `TestDialLocalControlGoesToAuthListener`, update the two changed calls:

```go
		dial := newDialLocal(terminated, ln.Addr().String(), "")
		conn, err := dial(tunnel.KindControlAPI, nil)
```

(Control streams never touch the stream argument, so nil is safe and pins that property.)

- [ ] **Step 7: Run the full test suite**

Run: `go test ./internal/agent/ ./cmd/piperd/ ./internal/certs/ -v`
Expected: PASS — new routing test green, updated control-listener test green, tunnel-client tests green.

- [ ] **Step 8: Commit**

```bash
git add internal/agent/tunnelclient.go internal/agent/tunnelclient_test.go cmd/piperd/main.go cmd/piperd/main_test.go
git commit -m "feat(agent): splice acme-tls/1 passthrough to the TLS-ALPN-01 solver

dialLocal gains the stream so piperd can peek each passthrough ClientHello
and route TLS-ALPN-01 validations to the in-process solver; everything else
pipes to Caddy's :443 unchanged.

Part of #226 (epic #224).

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Verify, PROGRESS.md, PR

**Files:**
- Modify: `PROGRESS.md`

**Interfaces:** none.

- [ ] **Step 1: Run the full gate**

Run: `make verify`
Expected: gofmt clean, vet clean, all tests pass, arm64 cross-build succeeds. Fix anything it flags before proceeding.

- [ ] **Step 2: Update PROGRESS.md**

In the Plan-2 section (near the `#102` domain-config line, after the `#225` entry if one exists), add:

```markdown
  - ✅ TLS-ALPN-01 issuance path — tokenless exact-host certs; `acme-tls/1` passthrough spliced to an in-process solver — [#226](https://github.com/getpiper/piper/issues/226)
```

Match the indentation/style of the surrounding lines exactly.

- [ ] **Step 3: Commit and open the PR**

```bash
git add PROGRESS.md
git commit -m "docs: mark TLS-ALPN-01 issuance path built in PROGRESS

Part of #226 (epic #224).

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
git push -u origin faruk/alpn01-certs
gh pr create --base main --title "feat(certs): TLS-ALPN-01 issuance path — tokenless exact-host certs" --body "$(cat <<'EOF'
Adds the TLS-ALPN-01 challenge mode to `internal/certs` (exactly one of DNS-01/ALPN per `certs.New`), a combined lego-provider + loopback-answerer `certs.ALPNSolver`, and an ALPN peek on the tunnel passthrough path so `acme-tls/1` validations spliced down from the relay reach the solver while all other traffic keeps flowing to Caddy's `:443` untouched.

Design: `docs/superpowers/specs/2026-07-16-tls-alpn01-issuance-design.md`

Part of #224. Closes #226.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Expected: PR opens against `main`; CI runs the same verify gate.
