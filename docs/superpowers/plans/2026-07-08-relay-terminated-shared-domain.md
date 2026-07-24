# Relay-terminated shared domain — Plan 3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve a self-enrolled free-tier box on the operator's shared apex: the relay assigns each app a single-label hostname under `*.public.getpiper.co`, terminates its TLS with a static wildcard cert, and forwards plaintext HTTP to the box over a typed tunnel stream — completing `login → connect → deploy → curl` on the shared domain.

**Architecture:** One new tunnel primitive — a 1-byte **stream kind** — lets the relay open `T` (passthrough, today's path) or `H` (relay-terminated HTTP) streams to the box, and lets the box open `C` (control) streams to the relay to register hostnames. Registration rides the box's *already-authenticated* tunnel, so it needs no new auth and stays off the deferred #73 Token-B plane. The relay owns naming (`<app-hash>-<username>.<apex>`), keeps a `hostname → session` routing map, and terminates matching SNI with an operator-supplied wildcard PEM; the box, in "terminated" mode, holds no cert and serves apps on `:80`.

**Tech Stack:** Go 1.26, `github.com/hashicorp/yamux`, `modernc.org/sqlite` (pure-Go), stdlib `crypto/tls`, `net`, `net/http`, `net/http/httptest`, `encoding/json`. No new dependencies.

This is **Plan 3 of 3** for the slice specced in [`docs/superpowers/specs/2026-07-07-public-relay-onboarding-design.md`](../specs/2026-07-07-public-relay-onboarding-design.md), realized by [`docs/superpowers/specs/2026-07-08-relay-terminated-shared-domain-design.md`](../specs/2026-07-08-relay-terminated-shared-domain-design.md). Plans 1 (relay accounts + device-flow + enrollment) and 2 (`piper login`/`connect`) have landed.

## Global Constraints

- **No cgo.** All builds pass with `CGO_ENABLED=0`; SQLite is `modernc.org/sqlite` only. `make cross` (linux/arm64) must stay green. **No new dependencies.**
- **Module path** `github.com/piperbox/piper`.
- **TDD.** Every task is failing-test-first. Run `make test` before each commit; it must pass.
- **Layering.** `internal/tunnel` imports only stdlib + yamux. `internal/relay` imports `internal/tunnel` + stdlib + the existing OAuth/OIDC libs (never `store`/`deploy`/`api`/`runtime`/`caddy`). `internal/agent` imports only `internal/tunnel` + stdlib. `internal/deploy` defines its registrar interface locally and imports only `store`/`runtime` (+ stdlib); `cmd/piperd` wires `agent` into `deploy`. **Nothing imports "up".**
- **Secrets hashed at rest** (`sha256` hex, `hashToken`) — unchanged; this plan adds no new secrets.
- **Stream-kind bytes** are exactly `'T'` (passthrough), `'H'` (terminated HTTP), `'C'` (control).
- **Deployment status strings** remain exactly `"building"`, `"running"`, `"failed"`, `"stopped"`.
- **Free-tier apex default** `public.getpiper.co`; **default per-account app cap** `10`.
- Commit messages are conventional-commit style ending with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`, and reference `Part of #49`.

## File Structure

- Modify `internal/tunnel/tunnel.go` — stream-kind constants; `OpenKind`/`AcceptKind`; `ControlRequest`/`ControlResponse` types; exported `WriteMsg`/`ReadMsg` framing helpers.
- Modify `internal/tunnel/tunnel_test.go` — kind round-trip + control-message round-trip.
- Modify `internal/relay/schema.sql` — `hostnames` table.
- Modify `internal/relay/store.go` — `maxApps` field + extend `Configure`; `AgentAccount`.
- Create `internal/relay/hostnames.go` — `RegisterHostname`/`DeregisterHostname`, app-cap, hostname derivation.
- Create `internal/relay/hostnames_test.go` — tests for the above.
- Modify `internal/relay/router.go` — `byHost` map: `RegisterHost`/`UnregisterHost`/`LookupHost`; `Unregister` drops a session's hosts.
- Modify `internal/relay/router_test.go` — byHost coverage.
- Create `internal/relay/terminate.go` — `LoadWildcardConfig`; `prefixConn`; `terminate()`.
- Create `internal/relay/terminate_test.go` — cert load + prefixConn + terminate-vs-passthrough branch.
- Modify `internal/relay/server.go` — `Serve(tlsAddr, tunnelAddr, st, tlsCfg)`; SNI branch; per-session control-stream handler.
- Modify `internal/relay/server_test.go` (create if absent) — control register/deregister over a real loopback tunnel.
- Modify `cmd/piper-relay/main.go` — load wildcard cert from env, `Configure` app cap, pass `tlsCfg`; env-gated fake auto-approve seam.
- Create `internal/agent/tunnelclient.go` additions — `TunnelClient` type (`Run`, `Register`, `Deregister`); kind-dispatch dial.
- Modify `internal/agent/tunnelclient_test.go` — kind dial selection + register round-trip.
- Modify `internal/config/config.go` — `RelayFile.Terminated`; `Config.Terminated`; `Load` precedence.
- Modify `internal/config/config_test.go` — terminated round-trip + env override.
- Modify `cmd/piper/relayonboard.go` — `connect` writes `Terminated: true`; `--install-only --terminated`.
- Modify `cmd/piper/relayonboard_test.go` — connect writes terminated.
- Modify `internal/deploy/deploy.go` — `HostnameRegistrar` interface; `SetHostnameRegistrar`; terminated deploy/teardown path.
- Modify `internal/deploy/deploy_test.go` — fake registrar coverage.
- Modify `cmd/piperd/main.go` — terminated-mode wiring (skip box TLS/`:443`, kind-dispatch dial, inject registrar).
- Create `test/e2e/relay_terminated_test.go` — `login → connect → deploy → curl` through relay termination.
- Modify `README.md`, `PROGRESS.md` — shared-domain quickstart; mark the slice complete.

---

### Task 1: `tunnel` — typed streams + control messages

**Files:**
- Modify: `internal/tunnel/tunnel.go`
- Modify: `internal/tunnel/tunnel_test.go`

**Interfaces:**
- Consumes: existing `Session{mux *yamux.Session}`, `writeFrame`/`readFrame` (unexported) in `tunnel.go`.
- Produces:
  - `const KindPassthrough byte = 'T'`, `KindHTTP byte = 'H'`, `KindControl byte = 'C'`
  - `func (s *Session) OpenKind(kind byte) (net.Conn, error)` — opens a stream and writes the kind byte.
  - `func (s *Session) AcceptKind() (byte, net.Conn, error)` — accepts a stream and reads its kind byte.
  - `type ControlRequest struct { Op, App, Hostname string }` (json `op`/`app`/`hostname`); ops `"register"`, `"deregister"`.
  - `type ControlResponse struct { Hostname, Error string }` (json `hostname`/`error`).
  - `func WriteMsg(w io.Writer, v any) error` — length-prefixed JSON (wraps `writeFrame`).
  - `func ReadMsg(r io.Reader, v any) error` — reads one length-prefixed JSON frame (wraps `readFrame`).

- [ ] **Step 1: Write the failing test**

Add to `internal/tunnel/tunnel_test.go`:

```go
func TestOpenAcceptKind(t *testing.T) {
	c1, c2 := net.Pipe()
	srvSess := make(chan *Session, 1)
	go func() {
		s, err := Serve(c2, func(_, _ string) error { return nil })
		if err != nil {
			t.Errorf("Serve: %v", err)
			return
		}
		srvSess <- s
	}()
	cli, err := Dial(c1, "tok", "base.example.com")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	srv := <-srvSess

	// Client opens a control stream; server accepts and sees the kind.
	go func() {
		stream, err := cli.OpenKind(KindControl)
		if err != nil {
			t.Errorf("OpenKind: %v", err)
			return
		}
		_ = WriteMsg(stream, ControlRequest{Op: "register", App: "blog"})
		var resp ControlResponse
		_ = ReadMsg(stream, &resp)
		if resp.Hostname != "blog-alice.public.getpiper.co" {
			t.Errorf("resp = %+v", resp)
		}
		stream.Close()
	}()

	kind, stream, err := srv.AcceptKind()
	if err != nil {
		t.Fatalf("AcceptKind: %v", err)
	}
	if kind != KindControl {
		t.Fatalf("kind = %q, want C", kind)
	}
	var req ControlRequest
	if err := ReadMsg(stream, &req); err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	if req.Op != "register" || req.App != "blog" {
		t.Fatalf("req = %+v", req)
	}
	_ = WriteMsg(stream, ControlResponse{Hostname: "blog-alice.public.getpiper.co"})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tunnel/ -run TestOpenAcceptKind -v`
Expected: FAIL — `OpenKind`/`AcceptKind`/`KindControl`/`ControlRequest`/`WriteMsg` undefined.

- [ ] **Step 3: Implement kinds, kind streams, and message framing**

In `internal/tunnel/tunnel.go`, add `"encoding/json"` is already imported. Append:

```go
// Stream kinds: every stream opens with a single kind byte so each end can
// dispatch by purpose. The agent opens only Control streams; the relay opens
// only Passthrough/HTTP streams.
const (
	KindPassthrough byte = 'T' // relay→agent: replayed ClientHello follows; agent pipes to :443
	KindHTTP        byte = 'H' // relay→agent: relay-terminated plaintext HTTP; agent pipes to :80
	KindControl     byte = 'C' // agent→relay: a length-prefixed ControlRequest/ControlResponse
)

// OpenKind opens a new stream and writes its kind byte.
func (s *Session) OpenKind(kind byte) (net.Conn, error) {
	c, err := s.mux.Open()
	if err != nil {
		return nil, err
	}
	if _, err := c.Write([]byte{kind}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// AcceptKind accepts a stream and reads its leading kind byte.
func (s *Session) AcceptKind() (byte, net.Conn, error) {
	c, err := s.mux.Accept()
	if err != nil {
		return 0, nil, err
	}
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		c.Close()
		return 0, nil, err
	}
	return b[0], c, nil
}

// ControlRequest is an agent→relay control message on a KindControl stream.
type ControlRequest struct {
	Op       string `json:"op"` // "register" | "deregister"
	App      string `json:"app,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// ControlResponse is the relay's reply. Error is non-empty on failure.
type ControlResponse struct {
	Hostname string `json:"hostname,omitempty"`
	Error    string `json:"error,omitempty"`
}

// WriteMsg writes v as a single length-prefixed JSON frame.
func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, b)
}

// ReadMsg reads one length-prefixed JSON frame into v.
func ReadMsg(r io.Reader, v any) error {
	b, err := readFrame(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tunnel/ -run TestOpenAcceptKind -v`
Expected: PASS. Then `go test ./internal/tunnel/ -v` — package green.

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/tunnel.go internal/tunnel/tunnel_test.go
git commit -m "feat(tunnel): typed streams + control message framing

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: `relay` store — `hostnames` table, registration, app cap

**Files:**
- Modify: `internal/relay/schema.sql`
- Modify: `internal/relay/store.go`
- Create: `internal/relay/hostnames.go`
- Create: `internal/relay/hostnames_test.go`

**Interfaces:**
- Consumes: existing `Store{db, apex, maxAgents}`, `apexOrDefault`, `hashToken`, `Configure` in `store.go`; `Account`/`isUniqueViolation` in `accounts.go`.
- Produces:
  - `Store.maxApps int` field; `func (s *Store) Configure(apex string, maxAgents, maxApps int)` (extended signature).
  - `func (s *Store) maxAppsOrDefault() int` — returns `maxApps`, or `10` when `<= 0`.
  - `func (s *Store) AgentAccount(baseDomain string) (accountID, username string, err error)` — resolves the account owning the agent whose `base_domain` is `baseDomain`; `ErrBadToken` if no such live agent, `ErrBadCredential` if its account is disabled.
  - `func (s *Store) RegisterHostname(baseDomain, app string) (string, error)` — idempotent per `(account, app)`; enforces the per-account app cap; returns the assigned `<app-hash>-<username>.<apex>`. Errors: `ErrBadToken` (unknown agent), `ErrBadCredential` (disabled account), `ErrQuotaExceeded` (app cap).
  - `func (s *Store) DeregisterHostname(baseDomain, hostname string) error` — removes the row for `(account of baseDomain, hostname)`; a missing row is a no-op.
  - `func appHostname(accountID, app, username, apex string) string` — `"<hex>-<username>.<apex>"` where `<hex>` is the first 8 hex chars of `sha256(accountID + "/" + app)`; truncates `<username>` so the whole first label is ≤ 63 chars.

- [ ] **Step 1: Write the failing test**

Create `internal/relay/hostnames_test.go`:

```go
package relay

import (
	"path/filepath"
	"strings"
	"testing"
)

func newAccountAgent(t *testing.T) (*Store, string) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	st.Configure("public.getpiper.co", 3, 10)
	acc, err := st.UpsertAccount("google-sub-1", "alice@example.com")
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatalf("EnrollForAccount: %v", err)
	}
	return st, en.BaseDomain
}

func TestRegisterHostnameIdempotentAndDerived(t *testing.T) {
	st, base := newAccountAgent(t)
	h1, err := st.RegisterHostname(base, "blog")
	if err != nil {
		t.Fatalf("RegisterHostname: %v", err)
	}
	if !strings.HasSuffix(h1, "-alice.public.getpiper.co") {
		t.Fatalf("hostname = %q, want -alice.public.getpiper.co suffix", h1)
	}
	if strings.Count(strings.TrimSuffix(h1, ".public.getpiper.co"), ".") != 0 {
		t.Fatalf("hostname %q is not a single label under the apex", h1)
	}
	h2, err := st.RegisterHostname(base, "blog")
	if err != nil || h2 != h1 {
		t.Fatalf("re-register = %q,%v want %q (idempotent)", h2, err, h1)
	}
	h3, err := st.RegisterHostname(base, "api")
	if err != nil || h3 == h1 {
		t.Fatalf("distinct app got %q,%v (want a different hostname)", h3, err)
	}
}

func TestRegisterHostnameAppCap(t *testing.T) {
	st, base := newAccountAgent(t)
	st.Configure("public.getpiper.co", 3, 2) // cap 2 apps
	if _, err := st.RegisterHostname(base, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(base, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(base, "c"); err != ErrQuotaExceeded {
		t.Fatalf("third app err = %v, want ErrQuotaExceeded", err)
	}
	// A re-register of an existing app must not be blocked by the cap.
	if _, err := st.RegisterHostname(base, "a"); err != nil {
		t.Fatalf("re-register under cap: %v", err)
	}
}

func TestRegisterHostnameDisabledAccount(t *testing.T) {
	st, base := newAccountAgent(t)
	if err := st.DisableAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(base, "blog"); err != ErrBadCredential {
		t.Fatalf("disabled-account register err = %v, want ErrBadCredential", err)
	}
}

func TestDeregisterHostname(t *testing.T) {
	st, base := newAccountAgent(t)
	h, err := st.RegisterHostname(base, "blog")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeregisterHostname(base, h); err != nil {
		t.Fatalf("DeregisterHostname: %v", err)
	}
	// Now under cap again and re-registerable.
	if _, err := st.DeregisterHostname(base, h); err != nil {
		t.Fatalf("deregister missing row should be no-op: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestRegisterHostname|TestDeregisterHostname' -v`
Expected: FAIL — `Configure` takes 2 args, `RegisterHostname`/`DeregisterHostname`/`appHostname` undefined.

- [ ] **Step 3a: Extend the schema**

Append to `internal/relay/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS hostnames (
    hostname    TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    app         TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    UNIQUE(account_id, app)
);
```

- [ ] **Step 3b: Extend `Configure` + add `maxApps`**

In `internal/relay/store.go`, change the `Store` struct and `Configure`:

```go
type Store struct {
	db        *sql.DB
	apex      string
	maxAgents int
	maxApps   int
}

// Configure sets the free-tier apex, the per-account agent cap (EnrollForAccount)
// and the per-account app cap (RegisterHostname). Safe to call once after Open.
func (s *Store) Configure(apex string, maxAgents, maxApps int) {
	s.apex = apex
	s.maxAgents = maxAgents
	s.maxApps = maxApps
}

func (s *Store) maxAppsOrDefault() int {
	if s.maxApps <= 0 {
		return 10
	}
	return s.maxApps
}
```

- [ ] **Step 3c: Implement registration**

Create `internal/relay/hostnames.go`:

```go
package relay

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// appHostname derives the single-label public hostname for (account, app):
// "<hex>-<username>.<apex>", where <hex> is the first 8 hex chars of
// sha256(accountID + "/" + app). It truncates <username> so the whole first
// label stays within DNS's 63-char limit (the 8-char hash preserves
// uniqueness). Deterministic — the same (account, app) always maps to the same
// hostname.
func appHostname(accountID, app, username, apex string) string {
	sum := sha256.Sum256([]byte(accountID + "/" + app))
	h := hex.EncodeToString(sum[:])[:8]
	// first label is "<h>-<username>": budget 63 - len(h) - 1 for the username.
	if max := 63 - len(h) - 1; len(username) > max {
		username = username[:max]
	}
	return h + "-" + username + "." + apex
}

// AgentAccount resolves the account owning the agent whose base_domain is
// baseDomain. ErrBadToken if there is no such agent; ErrBadCredential if the
// owning account is disabled.
func (s *Store) AgentAccount(baseDomain string) (accountID, username string, err error) {
	var disabled sql.NullInt64
	err = s.db.QueryRow(
		`SELECT acc.id, acc.username, acc.disabled
		   FROM agents ag JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.base_domain = ?`, baseDomain).
		Scan(&accountID, &username, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrBadToken
	}
	if err != nil {
		return "", "", err
	}
	if disabled.Valid && disabled.Int64 != 0 {
		return "", "", ErrBadCredential
	}
	return accountID, username, nil
}

// RegisterHostname assigns (idempotently) the public hostname for app on the
// account owning baseDomain, enforcing the per-account app cap. Returns the
// assigned "<app-hash>-<username>.<apex>".
func (s *Store) RegisterHostname(baseDomain, app string) (string, error) {
	accountID, username, err := s.AgentAccount(baseDomain)
	if err != nil {
		return "", err
	}

	var existing string
	err = s.db.QueryRow(`SELECT hostname FROM hostnames WHERE account_id=? AND app=?`, accountID, app).Scan(&existing)
	if err == nil {
		return existing, nil // idempotent
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM hostnames WHERE account_id=?`, accountID).Scan(&count); err != nil {
		return "", err
	}
	if count >= s.maxAppsOrDefault() {
		return "", ErrQuotaExceeded
	}

	hostname := appHostname(accountID, app, username, s.apexOrDefault())
	_, err = s.db.Exec(
		`INSERT INTO hostnames(hostname, account_id, app, created_at) VALUES(?,?,?,?)`,
		hostname, accountID, app, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return hostname, nil
}

// DeregisterHostname removes the hostname row for the account owning baseDomain.
// A missing row is not an error.
func (s *Store) DeregisterHostname(baseDomain, hostname string) error {
	accountID, _, err := s.AgentAccount(baseDomain)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM hostnames WHERE account_id=? AND hostname=?`, accountID, hostname)
	return err
}
```

- [ ] **Step 3d: Fix the existing `Configure` caller so the package compiles**

In `cmd/piper-relay/main.go`, update the `Configure` call (full env wiring lands in Task 5; this keeps the build green now):

```go
st.Configure(
	env("PIPER_RELAY_APEX", "public.getpiper.co"),
	atoiOr(env("PIPER_RELAY_MAX_AGENTS", "3"), 3),
	atoiOr(env("PIPER_RELAY_MAX_APPS", "10"), 10),
)
```

Also update any `Configure(` calls in `internal/relay/*_test.go` (e.g. `accounts_test.go`, `api_test.go`) to pass the third arg `10`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/relay/ -run 'TestRegisterHostname|TestDeregisterHostname' -v`
Expected: PASS. Then `go test ./internal/relay/ ./cmd/piper-relay/ -v` — both packages green.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/schema.sql internal/relay/store.go internal/relay/hostnames.go internal/relay/hostnames_test.go internal/relay/accounts_test.go internal/relay/api_test.go cmd/piper-relay/main.go
git commit -m "feat(relay): hostnames registry — assign, app-cap, kill-switch

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: `relay` router — `byHost` map + session cleanup

**Files:**
- Modify: `internal/relay/router.go`
- Modify: `internal/relay/router_test.go`

**Interfaces:**
- Consumes: existing `Router{byBase map[string]*tunnel.Session}`, `Register`/`Unregister`/`Lookup` in `router.go`.
- Produces:
  - `func (r *Router) RegisterHost(hostname string, sess *tunnel.Session)` — adds `hostname → sess` to the terminated map.
  - `func (r *Router) UnregisterHost(hostname string)` — removes one terminated hostname.
  - `func (r *Router) LookupHost(hostname string) (*tunnel.Session, bool)` — exact match in the terminated map only (no suffix logic).
  - `Router.Unregister(sess)` also drops every `byHost` entry pointing at `sess` (session teardown).

- [ ] **Step 1: Write the failing test**

Add to `internal/relay/router_test.go`:

```go
func TestRouterByHost(t *testing.T) {
	r := NewRouter()
	s1 := &tunnel.Session{BaseDomain: "aaaa-alice.public.getpiper.co"}
	s2 := &tunnel.Session{BaseDomain: "bbbb-bob.public.getpiper.co"}
	r.Register(s1)
	r.Register(s2)
	r.RegisterHost("blog-alice.public.getpiper.co", s1)
	r.RegisterHost("api-bob.public.getpiper.co", s2)

	if got, ok := r.LookupHost("blog-alice.public.getpiper.co"); !ok || got != s1 {
		t.Fatalf("LookupHost blog = %v,%v", got, ok)
	}
	// Terminated lookup is exact — no suffix matching.
	if _, ok := r.LookupHost("x.blog-alice.public.getpiper.co"); ok {
		t.Fatal("LookupHost must not suffix-match")
	}
	// Session teardown drops its terminated hostnames.
	r.Unregister(s1)
	if _, ok := r.LookupHost("blog-alice.public.getpiper.co"); ok {
		t.Fatal("host should be gone after Unregister(s1)")
	}
	if _, ok := r.LookupHost("api-bob.public.getpiper.co"); !ok {
		t.Fatal("s2 host must survive s1 teardown")
	}
}
```

Ensure the file imports `"github.com/piperbox/piper/internal/tunnel"` (add it if the existing test file does not already).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestRouterByHost -v`
Expected: FAIL — `RegisterHost`/`LookupHost`/`UnregisterHost` undefined.

- [ ] **Step 3: Implement `byHost`**

In `internal/relay/router.go`, add the field and methods, and extend `Unregister`:

```go
type Router struct {
	mu     sync.RWMutex
	byBase map[string]*tunnel.Session
	byHost map[string]*tunnel.Session
}

func NewRouter() *Router {
	return &Router{
		byBase: map[string]*tunnel.Session{},
		byHost: map[string]*tunnel.Session{},
	}
}
```

Add after `Register`:

```go
// RegisterHost maps an exact relay-terminated hostname to a session.
func (r *Router) RegisterHost(hostname string, sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byHost[hostname] = sess
}

// UnregisterHost removes a single terminated hostname.
func (r *Router) UnregisterHost(hostname string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byHost, hostname)
}

// LookupHost returns the session for an exact terminated hostname.
func (r *Router) LookupHost(hostname string) (*tunnel.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byHost[hostname]
	return s, ok
}
```

Replace `Unregister` so it also drops the session's terminated hostnames:

```go
func (r *Router) Unregister(sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byBase[sess.BaseDomain] == sess {
		delete(r.byBase, sess.BaseDomain)
	}
	for host, s := range r.byHost {
		if s == sess {
			delete(r.byHost, host)
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/relay/ -run 'TestRouter' -v`
Expected: PASS (new byHost test + the existing router test).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/router.go internal/relay/router_test.go
git commit -m "feat(relay): router byHost map for terminated hostnames

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: `relay` — wildcard termination + control-stream handler in `Serve`

**Files:**
- Create: `internal/relay/terminate.go`
- Create: `internal/relay/terminate_test.go`
- Modify: `internal/relay/server.go`
- Create: `internal/relay/server_test.go`

**Interfaces:**
- Consumes: `readSNI` (`sni.go`), `Router` (Task 3), `Store.RegisterHostname`/`DeregisterHostname` (Task 2), `tunnel.Session.OpenKind`/`AcceptKind`, `tunnel.KindHTTP`/`KindPassthrough`/`KindControl`, `tunnel.ControlRequest`/`ControlResponse`/`ReadMsg`/`WriteMsg` (Task 1).
- Produces:
  - `func LoadWildcardConfig(certFile, keyFile string) (*tls.Config, error)` — loads a cert/key into a `*tls.Config`; returns `(nil, nil)` when both paths are empty (passthrough-only relay).
  - `type prefixConn struct{ ... }` — a `net.Conn` that replays a byte prefix before reading the live conn.
  - `func Serve(tlsAddr, tunnelAddr string, st *Store, tlsCfg *tls.Config) error` — extended signature; nil `tlsCfg` ⇒ passthrough-only.
  - unexported `terminate`, `passthrough`, `serveControl`, `handleControl` in `server.go`.

- [ ] **Step 1: Write the failing test**

Create `internal/relay/terminate_test.go`:

```go
package relay

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeWildcard(t *testing.T, apex string) (certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*." + apex},
		DNSNames:     []string{"*." + apex},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	cf, _ := os.Create(certFile)
	pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cf.Close()
	kb, _ := x509.MarshalECPrivateKey(key)
	kf, _ := os.Create(keyFile)
	pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	kf.Close()
	return certFile, keyFile
}

func TestLoadWildcardConfig(t *testing.T) {
	if cfg, err := LoadWildcardConfig("", ""); err != nil || cfg != nil {
		t.Fatalf("empty paths = %v,%v want nil,nil", cfg, err)
	}
	cert, key := writeWildcard(t, "public.getpiper.co")
	cfg, err := LoadWildcardConfig(cert, key)
	if err != nil || cfg == nil || len(cfg.Certificates) != 1 {
		t.Fatalf("LoadWildcardConfig = %v,%v", cfg, err)
	}
}

func TestPrefixConnReplaysThenReads(t *testing.T) {
	inner := &fakeConn{readBuf: []byte("world")}
	pc := &prefixConn{Conn: inner, prefix: []byte("hello ")}
	got := make([]byte, 11)
	n, _ := readFull(pc, got)
	if string(got[:n]) != "hello world" {
		t.Fatalf("prefixConn read %q", got[:n])
	}
}
```

Add the small helpers this test needs to `terminate_test.go`:

```go
// fakeConn is a minimal net.Conn whose Read drains readBuf then EOFs.
type fakeConn struct {
	readBuf []byte
}

func (c *fakeConn) Read(p []byte) (int, error) {
	if len(c.readBuf) == 0 {
		return 0, os.ErrDeadlineExceeded // any non-nil EOF-ish
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}
func (c *fakeConn) Write(p []byte) (int, error)        { return len(p), nil }
func (c *fakeConn) Close() error                       { return nil }
func (c *fakeConn) LocalAddr() net.Addr                { return nil }
func (c *fakeConn) RemoteAddr() net.Addr               { return nil }
func (c *fakeConn) SetDeadline(t time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(t time.Time) error { return nil }

func readFull(r interface{ Read([]byte) (int, error) }, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := r.Read(p[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
```

Add the imports `"net"` and `"time"` to `terminate_test.go` (used by `fakeConn`), and drop the `bufio`/`http` imports shown in the Step-1 header block — those belong to `server_test.go`'s branch test (Step 4), not here. Keep `terminate_test.go`'s imports to exactly what its two tests + `fakeConn` use (`crypto/*`, `encoding/pem`, `math/big`, `net`, `os`, `path/filepath`, `testing`, `time`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestLoadWildcardConfig|TestPrefixConn' -v`
Expected: FAIL — `LoadWildcardConfig` / `prefixConn` undefined.

- [ ] **Step 3: Implement termination primitives**

Create `internal/relay/terminate.go`:

```go
package relay

import (
	"crypto/tls"
	"io"
	"net"

	"github.com/piperbox/piper/internal/tunnel"
)

// LoadWildcardConfig loads certFile/keyFile into a *tls.Config the relay uses to
// terminate shared-domain app TLS. Both paths empty ⇒ (nil, nil): the relay runs
// passthrough-only and never arms the terminate branch.
func LoadWildcardConfig(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// prefixConn is a net.Conn whose Read first drains a byte prefix (the ClientHello
// bytes readSNI already consumed) before reading the underlying conn. Writes and
// everything else pass straight through — so a tls.Server built on it can replay
// the recorded ClientHello and then complete a real handshake with the client.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// terminate completes a TLS handshake with the wildcard cert (replaying the
// consumed ClientHello via prefixConn), then pipes decrypted plaintext to a
// KindHTTP stream on the app's session. The relay sees plaintext HTTP but never
// parses it — it is a byte pump into the box's :80.
func terminate(conn net.Conn, buffered []byte, sess *tunnel.Session, tlsCfg *tls.Config) {
	tlsConn := tls.Server(&prefixConn{Conn: conn, prefix: buffered}, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	stream, err := sess.OpenKind(tunnel.KindHTTP)
	if err != nil {
		return
	}
	defer stream.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(stream, tlsConn); done <- struct{}{} }()
	go func() { io.Copy(tlsConn, stream); done <- struct{}{} }()
	<-done
}
```

- [ ] **Step 4: Run the primitive tests, then add the branch test**

Run: `go test ./internal/relay/ -run 'TestLoadWildcardConfig|TestPrefixConn' -v`
Expected: PASS.

Now wire `Serve` and add an end-to-end branch test. Replace `internal/relay/server.go` `Serve`, `handlePublic`, and `acceptTunnels`:

```go
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
```

Add `"crypto/tls"` to `server.go` imports.

Create `internal/relay/server_test.go` — a real loopback tunnel proving control registration and the terminate branch:

```go
package relay

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// startTestRelay opens a store with one enrolled account-bound agent, starts
// Serve on ephemeral ports with the given tlsCfg, and dials an agent tunnel back.
// It returns the agent session, the relay's TLS address, and the agent base domain.
func startTestRelay(t *testing.T, tlsCfg *tls.Config) (*tunnel.Session, string, string) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.Configure("public.getpiper.co", 3, 10)
	acc, _ := st.UpsertAccount("sub-1", "alice@example.com")
	en, _ := st.EnrollForAccount(acc.ID)

	tlsLn, _ := net.Listen("tcp", "127.0.0.1:0")
	tunLn, _ := net.Listen("tcp", "127.0.0.1:0")
	router := NewRouter()
	go func() {
		for {
			c, err := tunLn.Accept()
			if err != nil {
				return
			}
			sess, err := tunnel.Serve(c, func(tok, base string) error {
				ag, err := st.Authenticate(tok)
				if err != nil {
					return err
				}
				if ag.BaseDomain != base {
					return ErrBadToken
				}
				return nil
			})
			if err != nil {
				c.Close()
				continue
			}
			router.Register(sess)
			go serveControl(sess, st, router)
		}
	}()
	go func() {
		for {
			c, err := tlsLn.Accept()
			if err != nil {
				return
			}
			go handlePublic(c, router, tlsCfg)
		}
	}()
	t.Cleanup(func() { tlsLn.Close(); tunLn.Close() })

	conn, err := net.Dial("tcp", tunLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := tunnel.Dial(conn, en.Token, en.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	return sess, tlsLn.Addr().String(), en.BaseDomain
}

func TestControlRegisterThenTerminate(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	sess, tlsAddr, _ := startTestRelay(t, tlsCfg)

	// Agent side: register a hostname over a control stream.
	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "register", App: "blog"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	cs.Close()
	if resp.Error != "" || resp.Hostname == "" {
		t.Fatalf("register resp = %+v", resp)
	}
	hostname := resp.Hostname

	// Agent side: accept the relay's KindHTTP stream and answer HTTP/1.1.
	go func() {
		for {
			kind, stream, err := sess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindHTTP {
				stream.Close()
				continue
			}
			go func() {
				defer stream.Close()
				br := bufio.NewReader(stream)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				io.WriteString(stream, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
			}()
		}
	}()

	// Visitor side: TLS to the relay with SNI = the assigned hostname, GET /.
	deadline := time.Now().Add(5 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		d := &tls.Dialer{Config: &tls.Config{ServerName: hostname, InsecureSkipVerify: true}}
		c, err := d.Dial("tcp", tlsAddr)
		if err == nil {
			fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostname)
			b, _ := io.ReadAll(c)
			c.Close()
			if len(b) > 0 {
				body = string(b)
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if body == "" || !contains(body, "hi") {
		t.Fatalf("terminated response = %q", body)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

Move `writeWildcard` into `server_test.go` if the linter flags duplicate declarations — keep exactly one copy in the package (it is defined in `terminate_test.go` at Step 1; delete the copy here and rely on that one, since both files are `package relay`).

- [ ] **Step 5: Run the branch test**

Run: `go test ./internal/relay/ -run 'TestControlRegisterThenTerminate' -v`
Expected: PASS — the visitor gets `hi` back through relay termination.
Then `go test ./internal/relay/ -v` — whole package green (existing SNI/router/accounts/api tests still pass; the passthrough path is unchanged behaviorally).

- [ ] **Step 6: Commit**

```bash
git add internal/relay/terminate.go internal/relay/terminate_test.go internal/relay/server.go internal/relay/server_test.go
git commit -m "feat(relay): wildcard TLS termination + control-stream hostname registration

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: `piper-relay` binary — wire the wildcard cert + fake auto-approve seam

**Files:**
- Modify: `cmd/piper-relay/main.go`

**Interfaces:**
- Consumes: `relay.LoadWildcardConfig` (Task 4), the extended `relay.Serve(…, tlsCfg)` (Task 4), `relay.NewFakeVerifier` (Plan 1).
- Produces: no new exported symbols — env wiring only.
  - `PIPER_RELAY_TLS_CERT` / `PIPER_RELAY_TLS_KEY` — wildcard PEM paths (empty ⇒ passthrough-only).
  - `PIPER_RELAY_MAX_APPS` — per-account app cap (already threaded into `Configure` in Task 2, Step 3d).
  - `PIPER_RELAY_FAKE_APPROVE=1` — when the fake verifier is in use, auto-approve device-flow polls with a canned identity, so the loopback e2e can complete `piper login` without a real Google. Ignored when a real Google client ID is set.

- [ ] **Step 1: Add the auto-approve seam to the fake verifier**

Read `internal/relay/verifier.go` for the current `Verifier` interface and `FakeVerifier`. Add an exported constructor next to `NewFakeVerifier` that returns a fake whose `Poll` (or equivalent completion method) succeeds immediately with a fixed identity:

```go
// NewAutoApproveVerifier is a FakeVerifier whose device-flow poll completes
// immediately with a canned identity. It exists so the loopback e2e can drive
// `piper login`/`connect` end-to-end without a real Google IdP. NEVER selected
// in production: main.go uses it only under PIPER_RELAY_FAKE_APPROVE=1 and only
// when no real Google client ID is configured.
func NewAutoApproveVerifier(sub, email string) *FakeVerifier { /* set fields so Poll returns Identity{sub,email} at once */ }
```

Match the exact field/method names in the existing `FakeVerifier`; if it already exposes an "approve" hook, set it in the constructor so the first poll succeeds. Do not change `NewFakeVerifier`'s existing (manual-approval) semantics — the unit tests depend on them.

- [ ] **Step 2: Wire cert + verifier selection in `main.go`**

In `cmd/piper-relay/main.go`, replace the verifier-selection block and the final `Serve` call:

```go
	var v relay.Verifier
	if id := env("PIPER_RELAY_GOOGLE_CLIENT_ID", ""); id != "" {
		gv, err := relay.NewGoogleVerifier(context.Background(), id, env("PIPER_RELAY_GOOGLE_CLIENT_SECRET", ""))
		if err != nil {
			log.Fatalf("google verifier: %v", err)
		}
		v = gv
	} else if env("PIPER_RELAY_FAKE_APPROVE", "") == "1" {
		log.Print("piper-relay: PIPER_RELAY_FAKE_APPROVE=1 — device login auto-approves (TEST ONLY)")
		v = relay.NewAutoApproveVerifier("e2e-sub", "e2e@localhost")
	} else {
		log.Print("piper-relay: no PIPER_RELAY_GOOGLE_CLIENT_ID; self-service login disabled")
		v = relay.NewFakeVerifier()
	}
```

Then, after the API goroutine, load the wildcard cert and pass it to `Serve`:

```go
	tlsCfg, err := relay.LoadWildcardConfig(env("PIPER_RELAY_TLS_CERT", ""), env("PIPER_RELAY_TLS_KEY", ""))
	if err != nil {
		log.Fatalf("wildcard cert: %v", err)
	}
	if tlsCfg == nil {
		log.Print("piper-relay: no wildcard cert (PIPER_RELAY_TLS_CERT/KEY); passthrough-only, shared-domain termination disabled")
	}

	log.Printf("piper-relay: TLS %s, tunnel %s", tlsAddr, tunnelAddr)
	log.Fatal(relay.Serve(tlsAddr, tunnelAddr, st, tlsCfg))
```

- [ ] **Step 3: Build to verify it compiles**

Run: `go build ./cmd/piper-relay/ && go vet ./cmd/piper-relay/`
Expected: no errors.
Run: `go test ./internal/relay/ ./cmd/piper-relay/`
Expected: PASS (verifier unit tests unchanged; new constructor covered indirectly by the e2e in Task 10).

- [ ] **Step 4: Commit**

```bash
git add cmd/piper-relay/main.go internal/relay/verifier.go
git commit -m "feat(relay): wire wildcard cert env + test auto-approve verifier

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: `agent` — `TunnelClient` with kind-dispatch dial + register/deregister

**Files:**
- Modify: `internal/agent/tunnelclient.go`
- Modify: `internal/agent/tunnelclient_test.go`

**Interfaces:**
- Consumes: `tunnel.Dial`/`Session`/`OpenKind`/`AcceptKind`/`KindHTTP`/`KindPassthrough`/`KindControl`/`ControlRequest`/`ControlResponse`/`WriteMsg`/`ReadMsg` (Task 1); existing `nextBackoff`/`sleep`/`healthyThreshold` in `tunnelclient.go`.
- Produces:
  - `type TunnelClient struct { … }` with a mutex-guarded current `*tunnel.Session`.
  - `func (c *TunnelClient) Run(ctx context.Context, relayAddr, token, baseDomain string, dialLocal func(kind byte) (net.Conn, error))` — the reconnect loop; on each accepted stream, calls `dialLocal(kind)` and pipes. Replaces `RunTunnelClient`.
  - `func (c *TunnelClient) Register(app string) (string, error)` — opens a `KindControl` stream on the current session, sends `register`, returns the assigned hostname. `ErrNotConnected` if no live session.
  - `func (c *TunnelClient) Deregister(hostname string) error` — sends `deregister`; `ErrNotConnected` if no live session.
  - `var ErrNotConnected = errors.New("relay tunnel not connected")`

- [ ] **Step 1: Write the failing test**

Replace the body of `internal/agent/tunnelclient_test.go` with tests for the new type. Keep any existing helpers you still need; the key new coverage:

```go
package agent

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// fakeRelay accepts one agent tunnel and exposes its session for the test to
// drive (open T/H streams, accept C streams).
func fakeRelay(t *testing.T) (addr string, sessCh chan *tunnel.Session) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	sessCh = make(chan *tunnel.Session, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		sess, err := tunnel.Serve(c, func(_, _ string) error { return nil })
		if err != nil {
			return
		}
		sessCh <- sess
	}()
	return ln.Addr().String(), sessCh
}

func TestTunnelClientDialsByKind(t *testing.T) {
	// Two local listeners stand in for the box's :443 and :80.
	ln443, _ := net.Listen("tcp", "127.0.0.1:0")
	ln80, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln443.Close()
	defer ln80.Close()
	got := make(chan byte, 1)
	accept := func(ln net.Listener, mark byte) {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			got <- mark
			c.Close()
		}
	}
	go accept(ln443, 'T')
	go accept(ln80, 'H')

	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(kind byte) (net.Conn, error) {
		if kind == tunnel.KindHTTP {
			return net.Dial("tcp", ln80.Addr().String())
		}
		return net.Dial("tcp", ln443.Addr().String())
	})
	relaySess := <-sessCh

	// Relay opens an H stream → agent must dial :80.
	hs, _ := relaySess.OpenKind(tunnel.KindHTTP)
	hs.Close()
	if mark := <-got; mark != 'H' {
		t.Fatalf("H stream dialed %q, want :80", mark)
	}
	// Relay opens a T stream → agent must dial :443.
	ts, _ := relaySess.OpenKind(tunnel.KindPassthrough)
	ts.Close()
	if mark := <-got; mark != 'T' {
		t.Fatalf("T stream dialed %q, want :443", mark)
	}
}

func TestTunnelClientRegister(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte) (net.Conn, error) {
		return net.Dial("tcp", "127.0.0.1:9") // unused in this test
	})
	relaySess := <-sessCh

	// Relay control handler: answer register with a canned hostname.
	go func() {
		kind, stream, err := relaySess.AcceptKind()
		if err != nil || kind != tunnel.KindControl {
			return
		}
		var req tunnel.ControlRequest
		_ = tunnel.ReadMsg(stream, &req)
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Hostname: req.App + "-alice.public.getpiper.co"})
		stream.Close()
	}()

	// Give Run a moment to publish its session.
	var host string
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		host, err = c.Register("blog")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || host != "blog-alice.public.getpiper.co" {
		t.Fatalf("Register = %q,%v", host, err)
	}
}

// keep http/bufio/io imports honest for future use
var _ = bufio.NewReader
var _ = http.ReadRequest
var _ = io.Copy
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run 'TestTunnelClient' -v`
Expected: FAIL — `TunnelClient`/`Run`/`Register` undefined.

- [ ] **Step 3: Implement `TunnelClient`**

Rewrite `internal/agent/tunnelclient.go`, keeping `nextBackoff`/`sleep`/`healthyThreshold` and replacing `RunTunnelClient`/`serveStreams`:

```go
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

	"github.com/piperbox/piper/internal/tunnel"
)

// ErrNotConnected is returned by Register/Deregister when no relay session is live.
var ErrNotConnected = errors.New("relay tunnel not connected")

// TunnelClient maintains an outbound tunnel to the relay and exposes hostname
// registration over it. The current session is published under a mutex so the
// deploy path can open control streams on whatever session is live.
type TunnelClient struct {
	mu   sync.Mutex
	sess *tunnel.Session
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
// each relay-opened stream to dialLocal(kind). It reconnects with backoff until
// ctx is cancelled. Blocks.
func (c *TunnelClient) Run(ctx context.Context, relayAddr, token, baseDomain string, dialLocal func(kind byte) (net.Conn, error)) {
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

func serveStreams(ctx context.Context, sess *tunnel.Session, dialLocal func(kind byte) (net.Conn, error)) {
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
			local, err := dialLocal(kind)
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -v`
Expected: PASS.
Note: `cmd/piperd` still references the removed `RunTunnelClient` and will not build yet — that is rewired in Task 9. Do not build `./...` at this step.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/tunnelclient.go internal/agent/tunnelclient_test.go
git commit -m "feat(agent): TunnelClient with typed-stream dial + hostname register

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: config + CLI — the `terminated` mode marker in `relay.json`

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/piper/relayonboard.go`
- Modify: `cmd/piper/relayonboard_test.go`

**Interfaces:**
- Consumes: existing `RelayFile{RelayAddr,RelayToken,BaseDomain}`, `Config`, `Load`, `firstNonEmpty`, `env` in `config.go`; the `connect`/`connectOpts` flow in `relayonboard.go`.
- Produces:
  - `RelayFile.Terminated bool` (json `terminated`).
  - `Config.Terminated bool`.
  - `Load()` sets `Terminated` = `PIPER_RELAY_TERMINATED == "1"` OR (env unset AND `rf.Terminated`).
  - `piper connect` (device-flow path) writes `Terminated: true`; `--install-only` gains a `--terminated` bool that flows into the written `RelayFile` and the systemd-guided command it prints.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestRelayFileTerminatedRoundTripAndLoad(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1",
		BaseDomain: "aaaa-alice.public.getpiper.co", Terminated: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadRelayFile(dir)
	if err != nil || !got.Terminated {
		t.Fatalf("relay file terminated = %v (err %v)", got.Terminated, err)
	}

	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	t.Setenv("PIPER_RELAY_TERMINATED", "")
	if cfg := Load(); !cfg.Terminated {
		t.Fatal("Load did not read terminated from relay.json")
	}
	t.Setenv("PIPER_RELAY_TERMINATED", "0")
	// An explicit env value of 0 should win over the file's true only if we treat
	// env as authoritative; keep the rule simple: env "1" forces on, otherwise the
	// file decides. Document via this assertion:
	if cfg := Load(); !cfg.Terminated {
		t.Fatal("non-1 env must not override a terminated relay.json")
	}
}
```

Add to `cmd/piper/relayonboard_test.go` an assertion that the device-flow connect writes `Terminated: true`. Extend the existing `TestConnectEnrollsAndWritesRelayFile` expectation:

```go
func TestConnectWritesTerminated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"enrollment_token": "enr-1", "base_domain": "aaaa-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
		})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	rf, _, err := config.LoadRelayFile(dataDir)
	if err != nil || !rf.Terminated {
		t.Fatalf("relay file terminated = %v (err %v)", rf.Terminated, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestRelayFileTerminated -v` then `go test ./cmd/piper/ -run TestConnectWritesTerminated -v`
Expected: FAIL — `RelayFile.Terminated` / `Config.Terminated` undefined; connect doesn't set it.

- [ ] **Step 3a: Add the config fields + Load rule**

In `internal/config/config.go`:

```go
type RelayFile struct {
	RelayAddr  string `json:"relay_addr"`
	RelayToken string `json:"relay_token"`
	BaseDomain string `json:"base_domain"`
	Terminated bool   `json:"terminated,omitempty"`
}
```

Add `Terminated bool` to the `Config` struct (next to `RelayAddr`/`RelayToken`), and in `Load()` set it:

```go
		Terminated: os.Getenv("PIPER_RELAY_TERMINATED") == "1" || (os.Getenv("PIPER_RELAY_TERMINATED") == "" && rf.Terminated),
```

- [ ] **Step 3b: Set `Terminated` in `connect`**

In `cmd/piper/relayonboard.go`, in the device-flow `SaveRelayFile` call (the non-systemd normal path), add `Terminated: true`:

```go
	if err := config.SaveRelayFile(o.dataDir, config.RelayFile{
		RelayAddr:  en.TunnelEndpoint,
		RelayToken: en.EnrollmentToken,
		BaseDomain: en.BaseDomain,
		Terminated: true,
	}); err != nil {
```

For the `--install-only` path, add a `terminated` field to `connectOpts` and set it from a new `--terminated` flag in `main.go`'s `connect` dispatch, and write it:

```go
		if err := config.SaveRelayFile(o.dataDir, config.RelayFile{
			RelayAddr: o.relayAddr, RelayToken: o.relayToken, BaseDomain: o.baseDomain,
			Terminated: o.terminated,
		}); err != nil {
```

In the systemd-guided branch, append `--terminated` to the printed `systemd-run … connect --install-only …` command so the privileged install preserves the mode. (Add `--terminated \` to the `Fprintf` template and confirm the normal self-service path implies terminated.)

Locate the `connect` flag parsing in `cmd/piper/main.go` and register `terminated := fs.Bool("terminated", false, "mark this box relay-terminated (free-tier shared domain)")`, threading it into `connectOpts.terminated`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ ./cmd/piper/ -v`
Expected: PASS (new terminated tests + existing config/CLI tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go cmd/piper/relayonboard.go cmd/piper/relayonboard_test.go cmd/piper/main.go
git commit -m "feat(cli): terminated mode marker in relay.json

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: `deploy` — `HostnameRegistrar` seam + terminated deploy path

**Files:**
- Modify: `internal/deploy/deploy.go`
- Modify: `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: existing `Deployer{store,runtime,routes,baseDom}`, `New`, `hostFor`, `Deploy`, `TeardownPreview` in `deploy.go`; `RouteSetter`.
- Produces:
  - `type HostnameRegistrar interface { Register(app string) (string, error); Deregister(hostname string) error }`.
  - `func (d *Deployer) SetHostnameRegistrar(r HostnameRegistrar)` — nil-safe; when set, `Deploy` routes the relay-assigned hostname instead of `hostFor(app)`.
  - `Deploy` in terminated mode: `host, err := d.registrar.Register(app)`; on success `UpsertRoute(host, hostPort)`; the assigned host is stored on the deployment path via the existing route only (no schema change).

- [ ] **Step 1: Write the failing test**

Add to `internal/deploy/deploy_test.go` a fake registrar and a terminated-deploy test:

```go
type fakeRegistrar struct {
	host    string
	deregs  []string
	failing bool
}

func (f *fakeRegistrar) Register(app string) (string, error) {
	if f.failing {
		return "", errors.New("quota")
	}
	f.host = "hash-" + app + "-alice.public.getpiper.co"
	return f.host, nil
}
func (f *fakeRegistrar) Deregister(hostname string) error {
	f.deregs = append(f.deregs, hostname)
	return nil
}

func TestDeployTerminatedRoutesAssignedHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// Route must be the relay-assigned single-label host, NOT blog.public.getpiper.co.
	if _, ok := routes.upserts["hash-blog-alice.public.getpiper.co"]; !ok {
		t.Fatalf("routes = %v, want the assigned hostname", routes.upserts)
	}
	if _, ok := routes.upserts["blog.public.getpiper.co"]; ok {
		t.Fatal("terminated deploy must not route <app>.<baseDom>")
	}
}

func TestDeployTerminatedRegistrarFails(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "public.getpiper.co")
	d.SetHostnameRegistrar(&fakeRegistrar{failing: true})
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected deploy to fail when registration fails")
	}
}
```

Add `"errors"` to the test file imports if not already present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/deploy/ -run TestDeployTerminated -v`
Expected: FAIL — `SetHostnameRegistrar` undefined.

- [ ] **Step 3: Implement the seam**

In `internal/deploy/deploy.go`, add the interface + field + setter, and branch `Deploy`'s routing:

```go
// HostnameRegistrar assigns a relay-terminated public hostname for an app over
// the tunnel. In terminated (free-tier) mode the Deployer routes that hostname
// instead of "<app>.<baseDom>". Implemented by *agent.TunnelClient; injected by
// piperd. Nil in LAN / BYO-domain mode.
type HostnameRegistrar interface {
	Register(app string) (string, error)
	Deregister(hostname string) error
}

type Deployer struct {
	store      *store.Store
	runtime    runtime.Runtime
	routes     RouteSetter
	baseDom    string
	registrar  HostnameRegistrar
}
```

Add the setter after `New`:

```go
// SetHostnameRegistrar puts the Deployer into relay-terminated mode: Deploy asks
// the registrar for each app's public hostname and routes that. Nil restores
// LAN/BYO behavior.
func (d *Deployer) SetHostnameRegistrar(r HostnameRegistrar) { d.registrar = r }
```

In `Deploy`, replace the routing block (after the "running" deployment is created):

```go
	host := d.hostFor(appName)
	if d.registrar != nil {
		host, err = d.registrar.Register(appName)
		if err != nil {
			return store.Deployment{}, fmt.Errorf("register hostname: %w", err)
		}
	}
	if err := d.routes.UpsertRoute(host, run.HostPort); err != nil {
		return store.Deployment{}, fmt.Errorf("route: %w", err)
	}
```

(Leave `DeployPreview`/`TeardownPreview` on the `hostFor`/`hostForPreview` path — free-tier previews are out of scope for this plan; see the spec's follow-ups.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/deploy/ -v`
Expected: PASS (new terminated tests + existing deploy tests, which use a nil registrar and keep `hostFor`).

- [ ] **Step 5: Commit**

```bash
git add internal/deploy/deploy.go internal/deploy/deploy_test.go
git commit -m "feat(deploy): HostnameRegistrar seam for relay-terminated hostnames

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 9: `piperd` — terminated-mode wiring

**Files:**
- Modify: `cmd/piperd/main.go`

**Interfaces:**
- Consumes: `config.Config.Terminated` (Task 7), `agent.TunnelClient`/`Run`/`Register`/`Deregister` (Task 6), `deploy.Deployer.SetHostnameRegistrar` (Task 8), `tunnel.KindHTTP` (Task 1).
- Produces: no new exported symbols. Behavior: in terminated mode piperd serves apps on `:80` only (no box `:443`/wildcard), dials `:80` for `KindHTTP` streams and `:443` for others, and injects the `TunnelClient` as the deploy registrar.

- [ ] **Step 1: Gate the box TLS listener on non-terminated relay mode**

In `cmd/piperd/main.go`, change the caddy-opts block so `:443` is only added for BYO/passthrough relay mode:

```go
	if os.Getenv("PIPER_SKIP_CADDY") == "" {
		opts := []caddy.Option{}
		if cfg.RelayAddr != "" && !cfg.Terminated {
			opts = append(opts, caddy.WithHTTPS(":443"))
		}
		mgr, err = caddy.StartManager(cfg.CaddyAdmin, ":80", opts...)
		if err != nil {
			log.Fatalf("caddy: %v", err)
		}
	}
```

- [ ] **Step 2: Wire the tunnel client + registrar by mode**

Replace the relay-mode block (`if cfg.RelayAddr != "" { … }`) so terminated mode skips `setupRelayTLS`, dials `:80` for HTTP streams, and injects the registrar. Build the `Deployer` first so the registrar can be attached:

```go
	dep := deploy.New(st, rt, caddy.NewClient(cfg.CaddyAdmin), cfg.BaseDomain)

	var wh *webhookStarter
	if cfg.RelayAddr != "" {
		var dialLocal func(kind byte) (net.Conn, error)
		if cfg.Terminated {
			// Relay terminates TLS; the box serves plaintext HTTP on :80. No box cert.
			dialLocal = func(kind byte) (net.Conn, error) {
				if kind == tunnel.KindHTTP {
					return net.Dial("tcp", "127.0.0.1:80")
				}
				return net.Dial("tcp", "127.0.0.1:443")
			}
		} else {
			if err := setupRelayTLS(ctx, cfg); err != nil {
				log.Fatalf("relay tls: %v", err)
			}
			dialLocal = func(byte) (net.Conn, error) { return net.Dial("tcp", "127.0.0.1:443") }
		}
		tc := &agent.TunnelClient{}
		go tc.Run(ctx, cfg.RelayAddr, cfg.RelayToken, cfg.BaseDomain, dialLocal)
		if cfg.Terminated {
			dep.SetHostnameRegistrar(tc)
		}

		wh = newWebhookStarter(cfg, st, rt)
		if _, err := st.GetGitHubApp(); err == nil {
			wh.start()
		} else {
			log.Printf("no GitHub App configured; run `piper github setup` to enable git deploys")
		}
	}

	handler := api.RequireToken(st, api.New(st, dep, cfg.BaseDomain, "", func() {
		if wh != nil {
			wh.start()
		}
	}))
```

Add `"github.com/piperbox/piper/internal/tunnel"` to the imports. Remove the now-unused prior `dep := deploy.New(...)` line further down (the block above defines `dep` earlier); ensure there is exactly one `dep` definition.

- [ ] **Step 3: Build + vet the whole module**

Run: `go build ./... && go vet ./...`
Expected: no errors (this is the first step since Task 6 where `cmd/piperd` compiles again).
Run: `make test`
Expected: all packages pass (Docker/e2e skip cleanly when Docker is absent).

- [ ] **Step 4: Commit**

```bash
git add cmd/piperd/main.go
git commit -m "feat(agent): piperd terminated mode — no box TLS, :80 forward, register on deploy

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 10: e2e capstone + docs

**Files:**
- Create: `test/e2e/relay_terminated_test.go`
- Modify: `README.md`
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: the whole stack. Drives the real `piperd`, `piper-relay`, and `piper` binaries; uses `PIPER_RELAY_FAKE_APPROVE=1` (Task 5) so `piper login` completes without Google.

- [ ] **Step 1: Write the e2e (fails until the stack is wired — it is, by Task 9)**

Create `test/e2e/relay_terminated_test.go`. It reuses `writeSelfSigned`, `waitPort`, and `parseToken` from the existing `test/e2e` files (same package):

```go
package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRelayTerminatedSelfService proves the full free-tier loop:
// piper login (device flow, auto-approved) → piper connect (account-bound enroll,
// terminated) → piper deploy → curl the relay-assigned hostname, which the relay
// terminates with its wildcard cert and forwards as HTTP to the box's :80.
func TestRelayTerminatedSelfService(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker; Caddy is embedded)")
	}
	repoRoot, _ := filepath.Abs("../..")
	apex := "public.localhost"
	certFile, keyFile := writeSelfSigned(t, apex) // *.public.localhost

	binDir := t.TempDir()
	for _, c := range []string{"piperd", "piper-relay", "piper"} {
		b := exec.Command("go", "build", "-o", filepath.Join(binDir, c), "./cmd/"+c)
		b.Dir = repoRoot
		if out, err := b.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", c, err, out)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	relayData := t.TempDir()
	relay := exec.CommandContext(ctx, filepath.Join(binDir, "piper-relay"))
	relay.Env = append(os.Environ(),
		"PIPER_RELAY_DATA_DIR="+relayData,
		"PIPER_RELAY_TLS_ADDR=127.0.0.1:8443",
		"PIPER_RELAY_TUNNEL_ADDR=127.0.0.1:7000",
		"PIPER_RELAY_API_ADDR=127.0.0.1:8080",
		"PIPER_RELAY_TUNNEL_PUBLIC=127.0.0.1:7000",
		"PIPER_RELAY_APEX="+apex,
		"PIPER_RELAY_TLS_CERT="+certFile,
		"PIPER_RELAY_TLS_KEY="+keyFile,
		"PIPER_RELAY_FAKE_APPROVE=1",
	)
	relay.Stdout, relay.Stderr = os.Stdout, os.Stderr
	if err := relay.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer relay.Process.Kill()
	waitPort(t, "127.0.0.1:7000", 10*time.Second)
	waitPort(t, "127.0.0.1:8080", 10*time.Second)

	// piper login (device flow auto-approves) → writes ~/.piper/piper.
	home := t.TempDir()
	piperEnv := append(os.Environ(), "HOME="+home, "PIPER_ADDR=", "PIPER_TOKEN=")
	login := exec.Command(filepath.Join(binDir, "piper"), "login", "--relay", "http://127.0.0.1:8080")
	login.Env = piperEnv
	if out, err := login.CombinedOutput(); err != nil {
		t.Fatalf("piper login: %v\n%s", err, out)
	}

	// piper connect --data-dir <piperd data> → account-bound enroll + relay.json (terminated).
	piperdData := t.TempDir()
	connect := exec.Command(filepath.Join(binDir, "piper"), "connect", "--data-dir", piperdData)
	connect.Env = piperEnv
	if out, err := connect.CombinedOutput(); err != nil {
		t.Fatalf("piper connect: %v\n%s", err, out)
	}

	// Mint a control-API token, then start piperd in terminated mode (reads relay.json).
	tokenCmd := exec.Command(filepath.Join(binDir, "piperd"), "token", "create", "--name", "e2e")
	tokenCmd.Env = append(os.Environ(), "PIPER_DATA_DIR="+piperdData)
	tokenOut, err := tokenCmd.Output()
	if err != nil {
		t.Fatalf("token create: %v", err)
	}
	apiToken := strings.TrimSpace(string(tokenOut))

	pd := exec.CommandContext(ctx, filepath.Join(binDir, "piperd"))
	pd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+piperdData,
		"PIPER_API_ADDR=127.0.0.1:8088",
	)
	pd.Stdout, pd.Stderr = os.Stdout, os.Stderr
	if err := pd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	defer pd.Process.Kill()
	waitPort(t, "127.0.0.1:8088", 15*time.Second)

	// Deploy via the CLI so we exercise the terminated deploy → register path.
	deploy := exec.Command(filepath.Join(binDir, "piper"),
		"create", "blog", "--port", "8080")
	deploy.Env = append(os.Environ(), "PIPER_ADDR=http://127.0.0.1:8088", "PIPER_TOKEN="+apiToken)
	if out, err := deploy.CombinedOutput(); err != nil {
		t.Fatalf("piper create: %v\n%s", err, out)
	}
	dep := exec.Command(filepath.Join(binDir, "piper"),
		"deploy", "blog", "--path", filepath.Join(repoRoot, "test/e2e/sampleapp"))
	dep.Env = append(os.Environ(), "PIPER_ADDR=http://127.0.0.1:8088", "PIPER_TOKEN="+apiToken)
	if out, err := dep.CombinedOutput(); err != nil {
		t.Fatalf("piper deploy: %v\n%s", err, out)
	}

	// The relay assigned a hostname <hash>-e2e.public.localhost. We do not know the
	// hash here; discover it from the relay DB is overkill — instead assert the
	// terminated path by connecting with the apex wildcard SNI the relay routes.
	// Read the assigned hostname from piperd's Caddy config via the deploy output
	// is not exposed; so query the relay's hostnames via the control API is also
	// not exposed. Instead, derive it the same way the relay does is not possible
	// without the account id. Therefore: fetch the app list which returns the
	// public URL.
	hostname := publicHostname(t, binDir, apiToken)
	if !strings.HasSuffix(hostname, "-"+strings.SplitN("e2e@localhost", "@", 2)[0]+"."+apex) &&
		!strings.HasSuffix(hostname, "."+apex) {
		t.Fatalf("assigned hostname %q not under %q", hostname, apex)
	}

	// Visitor: TLS to the relay :8443 with SNI = assigned hostname, GET /.
	var body string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		d := &tls.Dialer{Config: &tls.Config{ServerName: hostname, InsecureSkipVerify: true}}
		conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
		if err == nil {
			fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostname)
			b, _ := io.ReadAll(conn)
			conn.Close()
			if len(b) > 0 {
				body = string(b)
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("no response through relay termination")
	}
	fmt.Printf("terminated e2e response:\n%s\n", body)
}
```

The test needs the assigned hostname. Add a small helper that reads it from `piper list` output, and make `piper list` (or the app-list API) surface the public URL. Read `internal/api` + `internal/client` + the `piper list` command first; if the public hostname is not already surfaced, add it to the app-list response (the deploy stored the route host; expose it) and print it in `piper list`. Implement `publicHostname` to parse that output:

```go
func publicHostname(t *testing.T, binDir, apiToken string) string {
	t.Helper()
	// Assumes `piper list` prints the app's public URL/host. Adjust the parse to
	// the actual column once implemented.
	cmd := exec.Command(filepath.Join(binDir, "piper"), "list")
	cmd.Env = append(os.Environ(), "PIPER_ADDR=http://127.0.0.1:8088", "PIPER_TOKEN="+apiToken)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("piper list: %v", err)
	}
	for _, f := range strings.Fields(string(out)) {
		if strings.Contains(f, ".public.localhost") {
			return strings.TrimPrefix(strings.TrimSpace(f), "https://")
		}
	}
	t.Fatalf("no public hostname in `piper list` output:\n%s", out)
	return ""
}
```

> **Implementer note:** surfacing the assigned public hostname to `piper list` is the one piece of user-facing polish this e2e forces. If `piper list` already prints a host column, parse that and skip the API change. If not, thread the relay-assigned host back from `Deploy` into the deployment record (add a `public_host` column to `store` deployments, set it in the terminated `Deploy` path in Task 8, and include it in the app-list response). Keep that addition in this task's commit.

- [ ] **Step 2: Run the e2e locally (Docker required)**

Run: `RUN_E2E=1 go test ./test/e2e/ -run TestRelayTerminatedSelfService -v`
Expected: PASS — the sample app's body is fetched through relay termination on `*.public.localhost`. Without `RUN_E2E=1` it skips.

- [ ] **Step 3: Update README**

In `README.md`, extend the relay/onboarding section:

```markdown
### Free-tier shared domain (relay-terminated)

`piper connect` claims a box on the public relay and marks it **terminated**:
piperd holds no cert and serves apps on `:80`; the relay assigns each app a
single-label hostname `<app-hash>-<username>.public.getpiper.co`, terminates its
HTTPS with its wildcard cert, and forwards plaintext HTTP over the tunnel.

```bash
piper login          # Google device-flow; stores your account credential
piper connect        # claims this box (terminated) and writes relay.json
sudo systemctl restart piperd
piper deploy blog --path .   # → https://<hash>-<you>.public.getpiper.co
```

Bring-your-own-domain apps stay **end-to-end** (the box terminates TLS; the relay
only splices SNI) — set `PIPER_BASE_DOMAIN` + cert/DNS config instead of using
`piper connect`. Self-hosters run the relay passthrough-only by leaving
`PIPER_RELAY_TLS_CERT`/`KEY` unset.
```

- [ ] **Step 4: Update PROGRESS.md**

Mark the Plan-2/onboarding lines complete and add the shared-domain line under the Plan 2 relay epic:

```markdown
- ✅ Relay-terminated shared domain — relay assigns `<app-hash>-<username>.<apex>`, terminates wildcard TLS, forwards HTTP over a typed tunnel stream; `login → connect → deploy → curl` e2e green — [#49](https://github.com/piperbox/piper/issues/49)
```

Adjust the epic-level caveat noting the free-tier box is now served end-to-end. Keep entries to one line each.

- [ ] **Step 5: Full verification**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, `make test` green (e2e skips without `RUN_E2E=1`/Docker), `make cross` (linux/arm64) succeeds.

- [ ] **Step 6: Commit**

```bash
git add test/e2e/relay_terminated_test.go README.md PROGRESS.md internal/store internal/api internal/client cmd/piper
git commit -m "test(e2e): self-service relay-terminated shared domain loop; docs

Part of #49
Closes #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage** (against `2026-07-08-relay-terminated-shared-domain-design.md`):

- Typed tunnel streams (`T`/`H`/`C`, kind byte, control messages) → Task 1. ✅
- `hostname → session` registry + per-app single-label naming + app cap + kill-switch → Tasks 2 (store) + 3 (router) + 4 (control handler). ✅
- Relay `:443` SNI branch (terminate vs passthrough) + static wildcard cert → Task 4 (`terminate`, `handlePublic`, `LoadWildcardConfig`) + Task 5 (env wiring). ✅
- Registration rides the agent's authenticated tunnel (no Token-B plane) → Task 4 `serveControl`/`handleControl` uses `sess.BaseDomain`; no new auth. ✅
- piperd terminated mode: skip box TLS, serve `:80`, register on deploy → Tasks 7 (marker) + 8 (deploy seam) + 9 (wiring). ✅
- Relay owns naming; box never composes hostname → Task 2 `appHostname` on the relay; box calls `Register(app)` only (Task 6). ✅
- Idempotence, 63-char truncation, app-cap error, deregister no-op, session-loss cleanup → Task 2 tests + Task 3 `Unregister`. ✅
- Full `login → connect → deploy → curl` loopback e2e with a fake IdP → Task 10 (`PIPER_RELAY_FAKE_APPROVE`). ✅
- **Out of scope (per spec):** free-tier PR previews (Task 8 leaves preview on `hostForPreview`), relay DNS-01 (Task 5 loads static PEM), Token-B caller plane, tiers/custom domains — none implemented. ✅

**Placeholder scan:** every code step shows complete code. The one soft spot is Task 10's `publicHostname`/`piper list` surfacing — called out explicitly with an implementer note and a concrete fallback (add a `public_host` column), not left as "TBD". ✅

**Type consistency:** `ControlRequest{Op,App,Hostname}` / `ControlResponse{Hostname,Error}`, kind bytes `KindPassthrough/KindHTTP/KindControl`, `Store.RegisterHostname(baseDomain, app)` / `DeregisterHostname(baseDomain, hostname)` / `AgentAccount`, `Router.RegisterHost/UnregisterHost/LookupHost`, `Serve(…, tlsCfg)`, `TunnelClient.Run(ctx, addr, token, base, dialLocal func(byte)(net.Conn,error))` / `Register(app)` / `Deregister(hostname)`, `HostnameRegistrar{Register,Deregister}` / `SetHostnameRegistrar`, `RelayFile.Terminated` / `Config.Terminated`, `Configure(apex, maxAgents, maxApps)` — each name/signature is defined once and consumed identically downstream. ✅

**Layering:** `tunnel` (stdlib+yamux) ← `relay`/`agent` (+ their existing deps) ; `deploy` defines `HostnameRegistrar` locally, `agent.TunnelClient` implements it, `cmd/piperd` injects — no upward imports, no new dependencies (`make cross` unaffected). ✅

## Next steps (follow-up issues to file)

- **Free-tier PR previews** — extend registration to `pr-<N>-<app>` hostnames (inherits Task 2/6/8 machinery).
- **Relay-side DNS-01 wildcard issuance** — replace the static PEM with lego DNS-01 on the relay's zone.
- **Reconnect re-registration** — on tunnel reconnect, re-register a box's live apps so public routes survive a relay blip (piperd replays from `store`).
