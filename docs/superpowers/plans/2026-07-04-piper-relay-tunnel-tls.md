# Plan 2 — Relay + Outbound Tunnel + On-Box TLS — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a live app reachable at a public HTTPS URL from behind NAT/CGNAT — the box dials an outbound yamux tunnel to a relay, the relay routes public `:443` by SNI down that tunnel, and TLS terminates on the box with a lego-issued wildcard cert.

**Architecture:** One new pure-Go binary (`piper-relay`) does SNI-peek routing + a token-authenticated tunnel server backed by a tiny SQLite enrollment store. Two new agent packages: `internal/tunnel` (yamux transport, client + server halves) and `internal/certs` (lego DNS-01 wildcard + renewal-due logic). `internal/caddy` gains a `:443` TLS listener and a load-PEM call. `piperd` gains a reconnecting tunnel-client loop and wires certs → Caddy. Relay mode is additive: with no `PIPER_RELAY_ADDR`, `piperd` behaves exactly as Plan 1.

**Tech Stack:** Go (`CGO_ENABLED=0`), `github.com/hashicorp/yamux`, `github.com/go-acme/lego/v4`, `modernc.org/sqlite`, stock Caddy over its admin API.

## Global Constraints

- **No cgo.** Every build must pass `CGO_ENABLED=0`; use pure-Go deps only (`modernc.org/sqlite`, never a cgo SQLite driver). Prove with `make cross` (linux/arm64).
- **Module path** is `github.com/getpiper/piper`.
- **Layering:** nothing imports "up". `tunnel` knows only yamux+net; `certs` knows only lego; `relay` knows tunnel(server)+SQLite; `caddy` knows only Caddy's admin API; `piperd` wires them. `deploy`/`api`/`store`/`client` are **not modified** by this plan.
- **Deployment status strings** stay exactly `"building"`, `"running"`, `"failed"`, `"stopped"`.
- **Defaults:** control API `127.0.0.1:8088`, Caddy admin `http://127.0.0.1:2019`, app container port `8080`. New: relay tunnel `:7000`, relay TLS `:443`.
- **Test gates:** `make test` (unit; Docker/ACME/e2e tests skip cleanly when their deps are absent) and `make cross` must both pass before any task is done.
- **Commits:** conventional-commit style, one per step where the plan says "Commit". End every commit message with:
  ```
  Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
  ```

---

### Task 1: `internal/tunnel` — yamux transport + auth handshake

A shared, network-free transport: the agent dials one connection, presents a token and its base domain, and both ends multiplex streams over it with yamux. No relay or agent logic here — just handshake + mux. Fully testable in-process with `net.Pipe()`.

**Files:**
- Create: `internal/tunnel/tunnel.go`
- Test: `internal/tunnel/tunnel_test.go`

**Interfaces:**
- Consumes: nothing (new leaf package).
- Produces:
  - `type Auth func(token, baseDomain string) error`
  - `func Serve(conn net.Conn, auth Auth) (*Session, error)` — server half; reads the handshake, calls `auth`, wraps conn in a yamux **server** session.
  - `func Dial(conn net.Conn, token, baseDomain string) (*Session, error)` — client half; writes the handshake, wraps conn in a yamux **client** session.
  - `type Session struct { BaseDomain string; /* unexported mux */ }`
  - `func (s *Session) Open() (net.Conn, error)` — open a new stream (relay→agent direction).
  - `func (s *Session) Accept() (net.Conn, error)` — accept a stream (agent side).
  - `func (s *Session) CloseChan() <-chan struct{}` — closed when the mux dies (drives reconnect).
  - `func (s *Session) Close() error`

- [ ] **Step 1: Add the yamux dependency**

Run:
```bash
go get github.com/hashicorp/yamux@latest
```
Expected: `go.mod` gains `github.com/hashicorp/yamux`.

- [ ] **Step 2: Write the failing test**

Create `internal/tunnel/tunnel_test.go`:
```go
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/tunnel/ -run TestDialServe -v`
Expected: FAIL — `undefined: Serve` / `undefined: Dial`.

- [ ] **Step 4: Write the implementation**

Create `internal/tunnel/tunnel.go`:
```go
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/tunnel/ -v`
Expected: PASS (`TestDialServeRoundTrip`, `TestServeRejectsBadAuth`).

- [ ] **Step 6: Verify no-cgo cross-compile**

Run: `make cross`
Expected: exit 0.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/tunnel
git commit -m "feat(tunnel): yamux transport with token+base-domain handshake

Part of #10.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: `internal/certs` — lego DNS-01 wildcard + renewal-due logic

`piperd` obtains a wildcard cert (`*.alice.example.com` + apex) via ACME DNS-01 using the operator's own DNS credentials, and knows when to renew. The always-run unit test covers the pure renewal-due logic; a Docker-gated test exercises real issuance against Pebble.

**Files:**
- Create: `internal/certs/certs.go`, `internal/certs/renew.go`
- Test: `internal/certs/renew_test.go`, `internal/certs/obtain_test.go`

**Interfaces:**
- Consumes: nothing (new leaf package).
- Produces:
  - `type Config struct { Email, CADirURL string; DNSProvider challenge.Provider; AccountKey *ecdsa.PrivateKey }`
  - `func New(cfg Config) (*Manager, error)`
  - `func (m *Manager) Obtain(domains []string) (certPEM, keyPEM []byte, err error)`
  - `func NeedsRenewal(certPEM []byte, within time.Duration, now time.Time) (bool, error)`

- [ ] **Step 1: Add the lego dependency**

Run:
```bash
go get github.com/go-acme/lego/v4@latest
```
Expected: `go.mod` gains `github.com/go-acme/lego/v4`.

- [ ] **Step 2: Write the failing renewal-due test**

Create `internal/certs/renew_test.go`:
```go
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func selfSigned(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "alice.example.com"},
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestNeedsRenewal(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	within := 30 * 24 * time.Hour

	// Expires in 60 days: not due.
	far := selfSigned(t, now.Add(60*24*time.Hour))
	if due, err := NeedsRenewal(far, within, now); err != nil || due {
		t.Fatalf("far: due=%v err=%v; want false", due, err)
	}
	// Expires in 10 days: due.
	near := selfSigned(t, now.Add(10*24*time.Hour))
	if due, err := NeedsRenewal(near, within, now); err != nil || !due {
		t.Fatalf("near: due=%v err=%v; want true", due, err)
	}
	// Garbage PEM: error.
	if _, err := NeedsRenewal([]byte("nope"), within, now); err == nil {
		t.Fatal("garbage PEM: want error")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/certs/ -run TestNeedsRenewal -v`
Expected: FAIL — `undefined: NeedsRenewal`.

- [ ] **Step 4: Implement renewal-due logic**

Create `internal/certs/renew.go`:
```go
package certs

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"
)

// NeedsRenewal reports whether the leaf certificate in certPEM expires within
// the given window as measured from now.
func NeedsRenewal(certPEM []byte, within time.Duration, now time.Time) (bool, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false, fmt.Errorf("no PEM block in cert")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, fmt.Errorf("parse cert: %w", err)
	}
	return now.Add(within).After(crt.NotAfter), nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/certs/ -run TestNeedsRenewal -v`
Expected: PASS.

- [ ] **Step 6: Implement the ACME Manager**

Create `internal/certs/certs.go`:
```go
package certs

import (
	"crypto"
	"crypto/ecdsa"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
)

// Config configures ACME DNS-01 issuance. DNSProvider is any lego challenge
// provider (e.g. providers/dns/cloudflare); AccountKey is the persisted ACME
// account key.
type Config struct {
	Email       string
	CADirURL    string
	DNSProvider challenge.Provider
	AccountKey  *ecdsa.PrivateKey
}

// Manager obtains certificates via ACME DNS-01.
type Manager struct {
	client *lego.Client
}

// user implements lego's registration.User.
type user struct {
	email string
	key   crypto.PrivateKey
	reg   *registration.Resource
}

func (u *user) GetEmail() string                        { return u.email }
func (u *user) GetRegistration() *registration.Resource { return u.reg }
func (u *user) GetPrivateKey() crypto.PrivateKey        { return u.key }

// New builds a Manager and registers the ACME account.
func New(cfg Config) (*Manager, error) {
	u := &user{email: cfg.Email, key: cfg.AccountKey}
	lc := lego.NewConfig(u)
	if cfg.CADirURL != "" {
		lc.CADirURL = cfg.CADirURL
	}
	client, err := lego.NewClient(lc)
	if err != nil {
		return nil, err
	}
	if err := client.Challenge.SetDNS01Provider(cfg.DNSProvider); err != nil {
		return nil, err
	}
	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, err
	}
	u.reg = reg
	return &Manager{client: client}, nil
}

// Obtain returns a PEM-encoded certificate chain and private key covering the
// given domains (e.g. []string{"*.alice.example.com", "alice.example.com"}).
func (m *Manager) Obtain(domains []string) (certPEM, keyPEM []byte, err error) {
	res, err := m.client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: domains,
		Bundle:  true,
	})
	if err != nil {
		return nil, nil, err
	}
	return res.Certificate, res.PrivateKey, nil
}
```

- [ ] **Step 7: Write the Pebble-gated issuance test**

Create `internal/certs/obtain_test.go`:
```go
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"testing"

	"github.com/go-acme/lego/v4/challenge/dns01"
)

// fakeDNS is a no-op DNS-01 provider; it works against Pebble's challtestsrv
// only when that server is configured to answer. Used to exercise the lego
// wiring end to end when RUN_ACME=1 points at a Pebble directory.
type fakeDNS struct{}

func (fakeDNS) Present(domain, token, keyAuth string) error { return nil }
func (fakeDNS) CleanUp(domain, token, keyAuth string) error { return nil }

func TestObtainAgainstPebble(t *testing.T) {
	dir := os.Getenv("RUN_ACME")
	if dir == "" {
		t.Skip("set RUN_ACME=<pebble directory URL> with a reachable Pebble + DNS to run")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	m, err := New(Config{
		Email:       "e2e@example.com",
		CADirURL:    dir,
		DNSProvider: fakeDNS{},
		AccountKey:  key,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = dns01.DefaultPropagationTimeout // ensure dns01 import is used
	certPEM, keyPEM, err := m.Obtain([]string{"alice.example.com"})
	if err != nil {
		t.Fatalf("Obtain: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("empty cert/key")
	}
}
```

- [ ] **Step 8: Run tests + verify skips + cross-compile**

Run: `go test ./internal/certs/ -v`
Expected: `TestNeedsRenewal` PASS; `TestObtainAgainstPebble` SKIP (RUN_ACME unset).

Run: `make cross`
Expected: exit 0 (confirms lego cross-compiles no-cgo for arm64).

- [ ] **Step 9: Commit**

```bash
git add go.mod go.sum internal/certs
git commit -m "feat(certs): lego DNS-01 wildcard issuance + renewal-due check

Part of #10.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: `internal/caddy` — `:443` TLS listener + load-PEM

Extend the Caddy driver so `piperd` can run Caddy with a TLS `:443` listener (automatic HTTPS disabled — `piperd` owns certs) and push a PEM cert/key pair in at runtime. Additive: `StartManager`'s existing callers are unaffected via a variadic option.

**Files:**
- Modify: `internal/caddy/manager.go`, `internal/caddy/client.go`
- Test: `internal/caddy/tls_test.go`

**Interfaces:**
- Consumes: existing `Client`, `Manager`, `StartManager(ctx, adminBase, httpListen string, ...Option)`.
- Produces:
  - `type Option func(*managerOpts)`
  - `func WithHTTPS(listen string) Option` — adds a TLS listener, disables automatic HTTPS, and enables the `tls` app.
  - `func (c *Client) LoadCert(certPEM, keyPEM string) error` — appends the pair to `apps/tls/certificates/load_pem`.

- [ ] **Step 1: Write the failing test**

Create `internal/caddy/tls_test.go`:
```go
package caddy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithHTTPSBaseConfig(t *testing.T) {
	o := &managerOpts{httpListen: ":80"}
	WithHTTPS(":443")(o)
	base := o.baseConfig()

	srv := base["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["piper"].(map[string]any)
	listens := srv["listen"].([]string)
	found := false
	for _, l := range listens {
		if l == ":443" {
			found = true
		}
	}
	if !found {
		t.Fatalf("piper server should listen on :443, got %v", listens)
	}
	if srv["automatic_https"] == nil {
		t.Fatal("automatic_https should be set (disabled) when TLS is enabled")
	}
	if _, ok := base["apps"].(map[string]any)["tls"]; !ok {
		t.Fatal("tls app should be present when TLS is enabled")
	}
}

func TestLoadCert(t *testing.T) {
	var gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	if err := c.LoadCert("CERTPEM", "KEYPEM"); err != nil {
		t.Fatalf("LoadCert: %v", err)
	}
	if gotPath != "/config/apps/tls/certificates/load_pem" {
		t.Fatalf("path = %q", gotPath)
	}
	var got []map[string]string
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("body not a JSON array: %v (%s)", err, gotBody)
	}
	if len(got) != 1 || got[0]["certificate"] != "CERTPEM" || got[0]["key"] != "KEYPEM" {
		t.Fatalf("bad load_pem body: %s", gotBody)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/caddy/ -run 'TestWithHTTPS|TestLoadCert' -v`
Expected: FAIL — `undefined: managerOpts` / `WithHTTPS` / `LoadCert`.

- [ ] **Step 3: Refactor the manager to use options + baseConfig**

Replace `StartManager` and add the options type in `internal/caddy/manager.go`. Change the top of the file's `StartManager` block to:
```go
type managerOpts struct {
	httpListen  string
	httpsListen string // "" ⇒ no TLS listener
	adminAddr   string
}

// Option configures StartManager.
type Option func(*managerOpts)

// WithHTTPS adds a TLS listener on listen, disables Caddy's automatic HTTPS
// (piperd owns certs), and enables the tls app so load_pem certs are served.
func WithHTTPS(listen string) Option {
	return func(o *managerOpts) { o.httpsListen = listen }
}

// baseConfig builds the Caddy JSON bootstrap config for these options.
func (o *managerOpts) baseConfig() map[string]any {
	listens := []string{o.httpListen}
	piper := map[string]any{"listen": listens, "routes": []any{}}
	apps := map[string]any{"http": map[string]any{"servers": map[string]any{"piper": piper}}}
	if o.httpsListen != "" {
		piper["listen"] = []string{o.httpListen, o.httpsListen}
		piper["automatic_https"] = map[string]any{"disable": true}
		apps["tls"] = map[string]any{"certificates": map[string]any{"load_pem": []any{}}}
	}
	return map[string]any{
		"admin": map[string]any{"listen": o.adminAddr},
		"apps":  apps,
	}
}

// StartManager launches `caddy run` with an admin-enabled base config: one HTTP
// server named "piper" on httpListen with empty routes. Options can add a TLS
// listener (WithHTTPS).
func StartManager(ctx context.Context, adminBase, httpListen string, opts ...Option) (*Manager, error) {
	o := &managerOpts{httpListen: httpListen, adminAddr: strings.TrimPrefix(adminBase, "http://")}
	for _, opt := range opts {
		opt(o)
	}
	cfg, _ := json.Marshal(o.baseConfig())
	cmd := exec.CommandContext(ctx, "caddy", "run", "--config", "-", "--adapter", "")
	cmd.Stdin = bytes.NewReader(cfg)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start caddy (is it installed?): %w", err)
	}
	m := &Manager{cmd: cmd}
	if err := waitAdmin(adminBase, 10*time.Second); err != nil {
		m.Stop()
		return nil, err
	}
	return m, nil
}
```
(Leave `waitAdmin` and `Stop` unchanged. Remove the old inline `base := map[string]any{...}` construction now that `baseConfig()` owns it.)

- [ ] **Step 4: Add LoadCert to the client**

Append to `internal/caddy/client.go`:
```go
// LoadCert appends a PEM cert/key pair to Caddy's tls.certificates.load_pem so
// Caddy serves it for matching SNI. Requires the tls app to exist (WithHTTPS).
func (c *Client) LoadCert(certPEM, keyPEM string) error {
	body, _ := json.Marshal([]map[string]string{{"certificate": certPEM, "key": keyPEM}})
	url := c.base + "/config/apps/tls/certificates/load_pem"
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("caddy load cert: status %d", resp.StatusCode)
	}
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/caddy/ -v`
Expected: PASS, including the pre-existing route tests (unchanged behavior for the no-option path).

- [ ] **Step 6: Commit**

```bash
git add internal/caddy
git commit -m "feat(caddy): optional :443 TLS listener + load_pem cert push

Part of #10.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: `cmd/piper-relay` + `internal/relay` — SNI shim, routing, enrollment

The relay binary: a SQLite enrollment store (`enroll` mints per-agent tokens bound to a base domain), a tunnel server that authenticates agents and registers their base domain into an in-memory suffix router, and a public `:443` listener that peeks the ClientHello SNI and pipes raw bytes down the matching tunnel. Never decrypts.

**Files:**
- Create: `internal/relay/store.go`, `internal/relay/schema.sql`, `internal/relay/router.go`, `internal/relay/sni.go`, `internal/relay/server.go`
- Create: `cmd/piper-relay/main.go`
- Test: `internal/relay/store_test.go`, `internal/relay/router_test.go`, `internal/relay/sni_test.go`

**Interfaces:**
- Consumes: `internal/tunnel` (`Serve`, `Session`), `modernc.org/sqlite`.
- Produces:
  - `Store`: `func Open(path string) (*Store, error)`; `func (s *Store) Enroll(name, baseDomain string) (token string, err error)`; `func (s *Store) Authenticate(token string) (Agent, error)`; `type Agent struct { Name, BaseDomain string }`; `var ErrBadToken = errors.New("bad token")`.
  - `Router`: `func NewRouter() *Router`; `func (r *Router) Register(sess *tunnel.Session)`; `func (r *Router) Unregister(sess *tunnel.Session)`; `func (r *Router) Lookup(sni string) (*tunnel.Session, bool)`.
  - `func readSNI(conn net.Conn) (sni string, buffered []byte, err error)`
  - `func Serve(tlsAddr, tunnelAddr string, st *Store) error` (blocks; wires everything).

- [ ] **Step 1: Write the failing store test**

Create `internal/relay/store_test.go`:
```go
package relay

import (
	"path/filepath"
	"testing"
)

func TestEnrollAndAuthenticate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tok, err := st.Enroll("alice", "alice.example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	ag, err := st.Authenticate(tok)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ag.Name != "alice" || ag.BaseDomain != "alice.example.com" {
		t.Fatalf("agent = %+v", ag)
	}
	if _, err := st.Authenticate("bogus"); err != ErrBadToken {
		t.Fatalf("bogus token err = %v; want ErrBadToken", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestEnroll -v`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 3: Implement the enrollment store**

Create `internal/relay/schema.sql`:
```sql
CREATE TABLE IF NOT EXISTS agents (
    name        TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    base_domain TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
```

Create `internal/relay/store.go`:
```go
// Package relay is the cloud-side SNI-passthrough tunnel server. It authenticates
// agents by per-agent token and routes public :443 traffic down the matching
// tunnel by SNI. It never decrypts traffic.
package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

var ErrBadToken = errors.New("bad token")

type Agent struct {
	Name       string
	BaseDomain string
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Enroll mints a random token for a new agent bound to baseDomain and stores
// only its hash. The plaintext token is returned once, to the operator.
func (s *Store) Enroll(name, baseDomain string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	_, err := s.db.Exec(
		`INSERT INTO agents(name, token_hash, base_domain, created_at) VALUES(?,?,?,?)`,
		name, hashToken(tok), baseDomain, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return tok, nil
}

// Authenticate resolves a plaintext token to its Agent, or ErrBadToken.
func (s *Store) Authenticate(token string) (Agent, error) {
	var ag Agent
	err := s.db.QueryRow(`SELECT name, base_domain FROM agents WHERE token_hash=?`, hashToken(token)).
		Scan(&ag.Name, &ag.BaseDomain)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrBadToken
	}
	if err != nil {
		return Agent{}, err
	}
	return ag, nil
}
```

- [ ] **Step 4: Run the store test to verify it passes**

Run: `go test ./internal/relay/ -run TestEnroll -v`
Expected: PASS.

- [ ] **Step 5: Write the failing router test**

Create `internal/relay/router_test.go`:
```go
package relay

import (
	"testing"

	"github.com/getpiper/piper/internal/tunnel"
)

func TestRouterSuffixMatch(t *testing.T) {
	r := NewRouter()
	sess := &tunnel.Session{BaseDomain: "alice.example.com"}
	r.Register(sess)

	if got, ok := r.Lookup("blog.alice.example.com"); !ok || got != sess {
		t.Fatalf("subdomain lookup failed: %v %v", got, ok)
	}
	if got, ok := r.Lookup("alice.example.com"); !ok || got != sess {
		t.Fatalf("apex lookup failed: %v %v", got, ok)
	}
	if _, ok := r.Lookup("evil.example.com"); ok {
		t.Fatal("unrelated host should not match")
	}
	r.Unregister(sess)
	if _, ok := r.Lookup("blog.alice.example.com"); ok {
		t.Fatal("lookup after unregister should fail")
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestRouter -v`
Expected: FAIL — `undefined: NewRouter`.

- [ ] **Step 7: Implement the router**

Create `internal/relay/router.go`:
```go
package relay

import (
	"strings"
	"sync"

	"github.com/getpiper/piper/internal/tunnel"
)

// Router maps an incoming SNI hostname to the agent session whose base domain
// owns it (exact match or subdomain). Registrations are keyed by base domain.
type Router struct {
	mu     sync.RWMutex
	byBase map[string]*tunnel.Session
}

func NewRouter() *Router { return &Router{byBase: map[string]*tunnel.Session{}} }

func (r *Router) Register(sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byBase[sess.BaseDomain] = sess
}

func (r *Router) Unregister(sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byBase[sess.BaseDomain] == sess {
		delete(r.byBase, sess.BaseDomain)
	}
}

// Lookup returns the session for an SNI equal to, or a subdomain of, a
// registered base domain.
func (r *Router) Lookup(sni string) (*tunnel.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.byBase[sni]; ok {
		return s, true
	}
	for base, s := range r.byBase {
		if strings.HasSuffix(sni, "."+base) {
			return s, true
		}
	}
	return nil, false
}
```

- [ ] **Step 8: Run the router test to verify it passes**

Run: `go test ./internal/relay/ -run TestRouter -v`
Expected: PASS.

- [ ] **Step 9: Write the failing SNI test**

Create `internal/relay/sni_test.go`:
```go
package relay

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// A real TLS client sends a ClientHello with ServerName; readSNI must recover
// it and buffer the bytes it consumed.
func TestReadSNI(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	go func() {
		// Client handshake will not complete (server side aborts) — that's fine;
		// we only need the ClientHello to be written.
		conn := tls.Client(c, &tls.Config{ServerName: "blog.alice.example.com", InsecureSkipVerify: true})
		conn.SetDeadline(time.Now().Add(time.Second))
		conn.Handshake()
	}()

	s.SetDeadline(time.Now().Add(2 * time.Second))
	sni, buffered, err := readSNI(s)
	if err != nil {
		t.Fatalf("readSNI: %v", err)
	}
	if sni != "blog.alice.example.com" {
		t.Fatalf("sni = %q", sni)
	}
	if len(buffered) == 0 {
		t.Fatal("expected buffered ClientHello bytes")
	}
}
```

- [ ] **Step 10: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestReadSNI -v`
Expected: FAIL — `undefined: readSNI`.

- [ ] **Step 11: Implement the SNI peek**

Create `internal/relay/sni.go`:
```go
package relay

import (
	"crypto/tls"
	"errors"
	"net"
)

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
```

- [ ] **Step 12: Run the SNI test to verify it passes**

Run: `go test ./internal/relay/ -run TestReadSNI -v`
Expected: PASS.

- [ ] **Step 13: Implement the relay server**

Create `internal/relay/server.go`:
```go
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
```

- [ ] **Step 14: Implement the relay binary**

Create `cmd/piper-relay/main.go`:
```go
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/getpiper/piper/internal/relay"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	dataDir := env("PIPER_RELAY_DATA_DIR", "./relay-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	st, err := relay.Open(filepath.Join(dataDir, "relay.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		fs := flag.NewFlagSet("enroll", flag.ExitOnError)
		domain := fs.String("domain", "", "base domain the agent may serve (e.g. alice.example.com)")
		fs.Parse(os.Args[2:])
		name := fs.Arg(0)
		if name == "" || *domain == "" {
			log.Fatal("usage: piper-relay enroll <name> --domain <base-domain>")
		}
		tok, err := st.Enroll(name, *domain)
		if err != nil {
			log.Fatalf("enroll: %v", err)
		}
		fmt.Printf("enrolled %s for %s\ntoken: %s\n", name, *domain, tok)
		return
	}

	tlsAddr := env("PIPER_RELAY_TLS_ADDR", ":443")
	tunnelAddr := env("PIPER_RELAY_TUNNEL_ADDR", ":7000")
	log.Printf("piper-relay: TLS %s, tunnel %s", tlsAddr, tunnelAddr)
	log.Fatal(relay.Serve(tlsAddr, tunnelAddr, st))
}
```

- [ ] **Step 15: Update the Makefile to build the relay binary**

In `Makefile`, find the `build` target's two `go build` lines and add a third:
```make
	CGO_ENABLED=0 go build -o bin/piper-relay ./cmd/piper-relay
```

- [ ] **Step 16: Run the full package, build, and cross-compile**

Run: `go test ./internal/relay/ -v`
Expected: PASS (store, router, SNI).

Run: `make build && make cross`
Expected: `bin/piper-relay` built; cross exits 0.

- [ ] **Step 17: Commit**

```bash
git add internal/relay cmd/piper-relay Makefile
git commit -m "feat(relay): piper-relay binary — enrollment, SNI routing, tunnel server

Part of #10.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: `piperd` — tunnel-client loop + cert wiring + config

Wire the agent side. Add relay/ACME config (all additive). When `PIPER_RELAY_ADDR` is set, `piperd`: (a) obtains or loads a wildcard cert and pushes it to Caddy, (b) starts Caddy with a `:443` TLS listener, (c) runs a reconnecting tunnel client that registers its base domain and forwards each accepted stream to local Caddy `:443`. With no relay addr, behavior is exactly Plan 1.

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/agent/tunnelclient.go`
- Modify: `cmd/piperd/main.go`
- Test: `internal/config/config_test.go` (extend), `internal/agent/tunnelclient_test.go`

**Interfaces:**
- Consumes: `tunnel.Dial`, `certs.New/Obtain/NeedsRenewal`, `caddy.StartManager(...WithHTTPS)`, `caddy.Client.LoadCert`, `config.Config`.
- Produces:
  - `config.Config` gains: `RelayAddr, RelayToken, ACMEEmail, ACMECA, DNSProvider, TLSCertFile, TLSKeyFile string`.
  - `func RunTunnelClient(ctx context.Context, relayAddr, token, baseDomain string, dialLocal func() (net.Conn, error))` — blocks; reconnects with backoff; for each accepted stream calls `dialLocal` and pipes bytes.

- [ ] **Step 1: Write the failing config test**

Add to `internal/config/config_test.go` (create if absent):
```go
package config

import (
	"os"
	"testing"
)

func TestLoadRelayFields(t *testing.T) {
	os.Setenv("PIPER_RELAY_ADDR", "relay.example.com:7000")
	os.Setenv("PIPER_RELAY_TOKEN", "tok-xyz")
	os.Setenv("PIPER_ACME_EMAIL", "me@example.com")
	defer func() {
		os.Unsetenv("PIPER_RELAY_ADDR")
		os.Unsetenv("PIPER_RELAY_TOKEN")
		os.Unsetenv("PIPER_ACME_EMAIL")
	}()
	cfg := Load()
	if cfg.RelayAddr != "relay.example.com:7000" {
		t.Errorf("RelayAddr = %q", cfg.RelayAddr)
	}
	if cfg.RelayToken != "tok-xyz" {
		t.Errorf("RelayToken = %q", cfg.RelayToken)
	}
	if cfg.ACMEEmail != "me@example.com" {
		t.Errorf("ACMEEmail = %q", cfg.ACMEEmail)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoadRelay -v`
Expected: FAIL — `cfg.RelayAddr undefined`.

- [ ] **Step 3: Extend Config**

In `internal/config/config.go`, add fields to the `Config` struct:
```go
	RelayAddr   string // relay tunnel endpoint; empty ⇒ LAN-only (Plan 1)
	RelayToken  string // enrollment token presented to the relay
	ACMEEmail   string // ACME account email
	ACMECA      string // ACME directory URL; empty ⇒ Let's Encrypt production
	DNSProvider string // lego DNS provider name (e.g. "cloudflare")
	TLSCertFile string // static cert path; set ⇒ skip ACME (tests / BYO cert)
	TLSKeyFile  string // static key path
```
And in `Load()`'s returned struct literal, add:
```go
		RelayAddr:   env("PIPER_RELAY_ADDR", ""),
		RelayToken:  env("PIPER_RELAY_TOKEN", ""),
		ACMEEmail:   env("PIPER_ACME_EMAIL", ""),
		ACMECA:      env("PIPER_ACME_CA", ""),
		DNSProvider: env("PIPER_DNS_PROVIDER", ""),
		TLSCertFile: env("PIPER_TLS_CERT_FILE", ""),
		TLSKeyFile:  env("PIPER_TLS_KEY_FILE", ""),
```

- [ ] **Step 4: Run config test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Write the failing tunnel-client test**

Create `internal/agent/tunnelclient_test.go`:
```go
package agent

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/tunnel"
)

// The tunnel client forwards an accepted stream to the local dialer. We stand up
// a real relay-side listener + tunnel.Serve, run the client against it, open a
// stream from the server, and check bytes reach a fake "local Caddy".
func TestTunnelClientForwardsToLocal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Fake local Caddy: echoes.
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	go func() {
		c, err := local.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c)
		c.Close()
	}()

	sessCh := make(chan *tunnel.Session, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		sess, err := tunnel.Serve(conn, func(_, _ string) error { return nil })
		if err != nil {
			return
		}
		sessCh <- sess
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunTunnelClient(ctx, ln.Addr().String(), "tok", "alice.example.com", func() (net.Conn, error) {
		return net.Dial("tcp", local.Addr().String())
	})

	sess := <-sessCh
	stream, err := sess.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	stream.SetDeadline(time.Now().Add(2 * time.Second))
	stream.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("echo = %q", buf)
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestTunnelClient -v`
Expected: FAIL — `undefined: RunTunnelClient`.

- [ ] **Step 7: Implement the tunnel client**

Create `internal/agent/tunnelclient.go`:
```go
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
```

- [ ] **Step 8: Run the tunnel-client test to verify it passes**

Run: `go test ./internal/agent/ -v`
Expected: PASS.

- [ ] **Step 9: Wire relay mode into piperd main**

In `cmd/piperd/main.go`, add imports `"crypto/ecdsa"`, `"crypto/elliptic"`, `"crypto/rand"`, `"net"`, `"os"`, `"time"`, and the packages `"github.com/getpiper/piper/internal/agent"`, `"github.com/getpiper/piper/internal/certs"`, and `"github.com/go-acme/lego/v4/providers/dns/cloudflare"`. After the existing Caddy-manager block and before `dep := deploy.New(...)`, insert the relay-mode setup:
```go
	// Relay mode: obtain/serve TLS on :443, dial the relay, register the base domain.
	if cfg.RelayAddr != "" {
		if err := setupRelayTLS(ctx, cfg); err != nil {
			log.Fatalf("relay tls: %v", err)
		}
		go agent.RunTunnelClient(ctx, cfg.RelayAddr, cfg.RelayToken, cfg.BaseDomain,
			func() (net.Conn, error) { return net.Dial("tcp", "127.0.0.1:443") })
	}
```
Change the Caddy-manager call so relay mode also opens `:443`:
```go
	if os.Getenv("PIPER_SKIP_CADDY") == "" {
		opts := []caddy.Option{}
		if cfg.RelayAddr != "" {
			opts = append(opts, caddy.WithHTTPS(":443"))
		}
		mgr, err := caddy.StartManager(ctx, cfg.CaddyAdmin, ":80", opts...)
		if err != nil {
			log.Fatalf("caddy: %v", err)
		}
		defer mgr.Stop()
	}
```
Then add this helper function to the file:
```go
// setupRelayTLS loads a wildcard cert into Caddy: a static PEM if configured
// (tests / BYO), otherwise ACME DNS-01 via lego.
func setupRelayTLS(ctx context.Context, cfg config.Config) error {
	cc := caddy.NewClient(cfg.CaddyAdmin)
	if cfg.TLSCertFile != "" {
		certPEM, err := os.ReadFile(cfg.TLSCertFile)
		if err != nil {
			return err
		}
		keyPEM, err := os.ReadFile(cfg.TLSKeyFile)
		if err != nil {
			return err
		}
		return cc.LoadCert(string(certPEM), string(keyPEM))
	}
	provider, err := cloudflare.NewDNSProvider()
	if err != nil {
		return err
	}
	acctKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mgr, err := certs.New(certs.Config{
		Email: cfg.ACMEEmail, CADirURL: cfg.ACMECA,
		DNSProvider: provider, AccountKey: acctKey,
	})
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := mgr.Obtain([]string{"*." + cfg.BaseDomain, cfg.BaseDomain})
	if err != nil {
		return err
	}
	go renewLoop(ctx, mgr, cc, cfg.BaseDomain, certPEM)
	return cc.LoadCert(string(certPEM), string(keyPEM))
}

// renewLoop re-obtains and reloads the cert when it nears expiry.
func renewLoop(ctx context.Context, mgr *certs.Manager, cc *caddy.Client, base string, certPEM []byte) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			due, err := certs.NeedsRenewal(certPEM, 30*24*time.Hour, time.Now())
			if err != nil || !due {
				continue
			}
			newCert, newKey, err := mgr.Obtain([]string{"*." + base, base})
			if err != nil {
				log.Printf("renew: %v", err)
				continue
			}
			if err := cc.LoadCert(string(newCert), string(newKey)); err != nil {
				log.Printf("renew load: %v", err)
				continue
			}
			certPEM = newCert
		}
	}
}
```

- [ ] **Step 10: Verify the full build, tests, and cross-compile**

Run: `make build`
Expected: both `piperd` and `piper-relay` build (no unused-import errors).

Run: `make test`
Expected: all packages pass; relay/certs gated tests skip cleanly.

Run: `make cross`
Expected: exit 0.

- [ ] **Step 11: Commit**

```bash
git add internal/config internal/agent cmd/piperd
git commit -m "feat(agent): relay-mode tunnel client + cert wiring in piperd

Part of #10.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Loopback e2e — mechanism proof through the relay

An `RUN_E2E`-gated test that runs the relay + `piperd` + Caddy + Docker entirely on localhost with a self-signed wildcard cert (via `PIPER_TLS_CERT_FILE`, no ACME), deploys the sample app, and fetches it **through the relay's TLS port** by SNI — proving tunnel + SNI-demux + passthrough + on-box TLS end to end without real DNS or a public host.

**Files:**
- Create: `test/e2e/relay_test.go`
- Reuse: `test/e2e/sampleapp/` (from Plan 1)

**Interfaces:**
- Consumes: built `piperd` + `piper-relay` binaries; `internal/client`.

- [ ] **Step 1: Write the e2e test**

Create `test/e2e/relay_test.go`:
```go
package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/client"
)

// TestRelayLoopback proves the full relay path locally: browser→relay:8443
// (SNI)→tunnel→piperd→Caddy:443(TLS)→container. Self-signed wildcard cert, no
// ACME, no real DNS. Uses :8443 and :7000 to avoid privileged :443.
func TestRelayLoopback(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker + caddy on PATH)")
	}
	repoRoot, _ := filepath.Abs("../..")
	base := "alice.localhost"

	// Self-signed wildcard cert for *.alice.localhost.
	certFile, keyF := writeSelfSigned(t, base)

	// Build both binaries.
	binDir := t.TempDir()
	for _, c := range []string{"piperd", "piper-relay"} {
		b := exec.Command("go", "build", "-o", filepath.Join(binDir, c), "./cmd/"+c)
		b.Dir = repoRoot
		if out, err := b.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", c, err, out)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Enroll an agent against the relay's store, capture the token.
	relayData := t.TempDir()
	enroll := exec.Command(filepath.Join(binDir, "piper-relay"), "enroll", "alice", "--domain", base)
	enroll.Env = append(os.Environ(), "PIPER_RELAY_DATA_DIR="+relayData)
	out, err := enroll.CombinedOutput()
	if err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}
	token := parseToken(t, string(out))

	// Start the relay (TLS :8443, tunnel :7000).
	relay := exec.CommandContext(ctx, filepath.Join(binDir, "piper-relay"))
	relay.Env = append(os.Environ(),
		"PIPER_RELAY_DATA_DIR="+relayData,
		"PIPER_RELAY_TLS_ADDR=127.0.0.1:8443",
		"PIPER_RELAY_TUNNEL_ADDR=127.0.0.1:7000",
	)
	relay.Stdout, relay.Stderr = os.Stdout, os.Stderr
	if err := relay.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer relay.Process.Kill()
	waitPort(t, "127.0.0.1:7000", 10*time.Second)

	// Start piperd in relay mode with the static cert.
	pd := exec.CommandContext(ctx, filepath.Join(binDir, "piperd"))
	pd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+t.TempDir(),
		"PIPER_API_ADDR=127.0.0.1:8088",
		"PIPER_BASE_DOMAIN="+base,
		"PIPER_RELAY_ADDR=127.0.0.1:7000",
		"PIPER_RELAY_TOKEN="+token,
		"PIPER_TLS_CERT_FILE="+certFile,
		"PIPER_TLS_KEY_FILE="+keyF,
	)
	pd.Stdout, pd.Stderr = os.Stdout, os.Stderr
	if err := pd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	defer pd.Process.Kill()
	waitPort(t, "127.0.0.1:8088", 15*time.Second)

	// Deploy the sample app.
	c := client.New("http://127.0.0.1:8088")
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if _, err := c.Deploy("blog", filepath.Join(repoRoot, "test/e2e/sampleapp")); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Fetch through the relay's TLS port by SNI blog.alice.localhost.
	dialer := &tls.Dialer{Config: &tls.Config{ServerName: "blog." + base, InsecureSkipVerify: true}}
	var body string
	for i := 0; i < 30; i++ {
		conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:8443")
		if err == nil {
			fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: blog.%s\r\nConnection: close\r\n\r\n", base)
			b, _ := io.ReadAll(conn)
			conn.Close()
			body = string(b)
			if body != "" {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("no response through the relay")
	}
	fmt.Printf("relay e2e response:\n%s\n", body)
}

func writeSelfSigned(t *testing.T, base string) (certFile, keyF string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*." + base},
		DNSNames:     []string{"*." + base, base},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyF = filepath.Join(dir, "key.pem")
	certOut, _ := os.Create(certFile)
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	certOut.Close()
	keyBytes, _ := x509.MarshalECPrivateKey(key)
	keyOut, _ := os.Create(keyF)
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	keyOut.Close()
	return certFile, keyF
}

func parseToken(t *testing.T, out string) string {
	t.Helper()
	const marker = "token: "
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("no token in enroll output: %q", out)
	}
	return strings.TrimSpace(out[i+len(marker):])
}
```
Add `"strings"` to the import block.

- [ ] **Step 2: Run the e2e test (requires Docker + caddy)**

Run:
```bash
RUN_E2E=1 go test ./test/e2e/ -run TestRelayLoopback -v
```
Expected: PASS, printing an HTTP 200 whose body contains `hello piper`. If Docker or caddy is absent, the test is a no-op only when `RUN_E2E` is unset — with `RUN_E2E=1` it requires both.

- [ ] **Step 3: Verify the default suite still skips cleanly**

Run: `make test`
Expected: `test/e2e` package compiles; both e2e tests SKIP (RUN_E2E unset). All other packages PASS.

Run: `make cross`
Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/relay_test.go
git commit -m "test(e2e): loopback relay path — tunnel + SNI + on-box TLS

Part of #10. Closes #10.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Wrap-up — PROGRESS.md + child issues

- [ ] **Step 1: Update PROGRESS.md**

In `PROGRESS.md`, under "## Plan 2 — Relay + tunnel + TLS", replace the three `⬜` bullets with `✅` entries referencing the merged PRs, and update the `_Last updated_` line to note Plan 2 landed. Keep entries to one line each.

- [ ] **Step 2: Commit**

```bash
git add PROGRESS.md
git commit -m "docs: mark Plan 2 (relay + tunnel + TLS) complete in PROGRESS

Part of #10.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 3: Finish the branch**

Use superpowers:finishing-a-development-branch to open the PR into `main` (`Closes #10`), verify CI, and squash-merge.

---

## Self-Review

**Spec coverage:**
- `piper-relay` SNI passthrough + tunnel server → Task 4. ✅
- Outbound tunnel client in `piperd` → Tasks 1 (transport) + 5 (client loop + wiring). ✅
- DNS-01 wildcard TLS on-box → Task 2 (`certs`) + Task 3 (Caddy `:443`/load-PEM) + Task 5 (`setupRelayTLS`/renew). ✅
- Per-agent tokens / enrollment → Task 4 (`Store.Enroll`/`Authenticate`, `enroll` cmd). ✅
- Agent-owned DNS (Dokploy-like), relay never touches DNS → certs run in `piperd`; relay code has no DNS. ✅
- Additive relay mode (LAN-only unchanged) → `RelayAddr==""` guards in Task 5; Caddy option variadic in Task 3. ✅
- Testing tiers: unit (Tasks 1–5), loopback e2e (Task 6), real-infra gated (`RUN_ACME` in Task 2, documented). ✅

**Placeholder scan:** No TBD/TODO; every code step shows complete code; commands have expected output. ✅

**Type consistency:** `tunnel.Session{BaseDomain, mux}` consumed by relay router + client; `Serve`/`Dial`/`Open`/`Accept`/`CloseChan`/`Close` used consistently across Tasks 1/4/5. `certs.Config`/`New`/`Obtain`/`NeedsRenewal` consistent across Tasks 2/5. `caddy.WithHTTPS`/`LoadCert` consistent Tasks 3/5. `relay.Open`/`Enroll`/`Authenticate`/`Serve` consistent Tasks 4/6. `RunTunnelClient` signature identical in Tasks 5 def + test. ✅

**Note for implementers:** the loopback e2e (Task 6) uses `net.Dial("tcp","127.0.0.1:443")` from `piperd` to reach local Caddy in production but the *test* runs Caddy's `:443` inside the same host — Caddy binds `:443` (privileged). If `:443` needs root in your environment, run the e2e with elevated privileges or adjust Caddy's HTTPS listen + the piperd local-dial target together to a high port. This mirrors Plan 1's `:80` caveat.
