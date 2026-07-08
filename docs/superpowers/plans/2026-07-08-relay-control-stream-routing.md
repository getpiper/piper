# Relay Control-Stream Routing + Caller→Agent Authz Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `piper` caller can drive a box's control API through the relay — authenticated by relay account, authorized by agent ownership, forwarded over the existing outbound tunnel with the box's Token B injected — implementing [#73](https://github.com/getpiper/piper/issues/73) per [`docs/superpowers/specs/2026-07-08-relay-control-stream-routing-design.md`](../specs/2026-07-08-relay-control-stream-routing-design.md).

**Architecture:** The relay gains a TLS-terminated control plane at `api.<apex>`, SNI-dispatched on the existing `:443` listener using the existing wildcard cert. Control requests (`/agents/<base-domain>/v1/...`) are account-authenticated, ownership-authorized, and reverse-proxied over a new `KindControlAPI` tunnel stream into piperd's `127.0.0.1:8088`, swapping the caller's account credential for the box's Token B. piperd provisions that Token B itself: on first connect after enrollment it mints a control token locally and *pushes* it to the relay over the existing agent→relay control channel (agent-push, not relay-pull — see the spec's deviation note).

**Tech Stack:** Go 1.26 stdlib (`net/http/httputil.ReverseProxy` with `Rewrite`), hashicorp/yamux via `internal/tunnel`, modernc.org/sqlite.

## Global Constraints

- **No cgo** — everything must build with `CGO_ENABLED=0` (`make cross` proves arm64).
- Module path `github.com/getpiper/piper`; work lands on branch `faruk/relay-control-routing`, PR into `main`.
- Run `make verify` (gofmt → vet → test → cross) before claiming done; `make fmt` fixes formatting.
- One commit per task, conventional-commit style, ending with:
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`
- Reference the issue in commits (`Part of #73`); the final PR body carries `Closes #73`.
- Layering: `internal/agent` imports only `internal/tunnel` + stdlib; `internal/relay` never imports `internal/store`; nothing imports "up".
- Error semantics at the relay proxy (from the spec): bad/missing account credential → **401**; unknown agent OR another account's agent → **404** (no existence leak); owned but tunnel not connected → **503**; otherwise the box's response verbatim. No Token B stored → forward *without* Authorization (box 401s; not special-cased).
- App traffic (`KindPassthrough`/`KindHTTP`) must be untouched by every change.

---

### Task 1: Relay store — `control_token` column + accessors

The relay must hold, per agent, the plaintext Token B the box pushes (plaintext by necessity: the relay presents it verbatim on forwarded requests — the spec's stated trust cost).

**Files:**
- Modify: `internal/relay/store.go` (generalize the column migration; add two methods)
- Test: `internal/relay/store_test.go`

**Interfaces:**
- Consumes: existing `Store`, `openTestStore(t)` test helper, `UpsertAccount`, `EnrollForAccount`.
- Produces: `(*Store).SetControlToken(baseDomain, token string) error` (unknown agent → `ErrBadToken`); `(*Store).ControlToken(baseDomain string) (string, error)` (`""` when never provisioned; unknown agent → `ErrBadToken`); `ensureAgentColumn(db *sql.DB, column string) error` replacing `ensureAgentAccountColumn`.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/store_test.go`:

```go
func TestControlTokenRoundTrip(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	acc, err := st.UpsertAccount("sub-ct", "ct@example.com")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Never provisioned: empty token, no error.
	if tok, err := st.ControlToken(en.BaseDomain); err != nil || tok != "" {
		t.Fatalf("fresh ControlToken = %q, %v (want \"\", nil)", tok, err)
	}
	if err := st.SetControlToken(en.BaseDomain, "tok-1"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := st.ControlToken(en.BaseDomain); tok != "tok-1" {
		t.Fatalf("ControlToken = %q, want tok-1", tok)
	}
	// A re-push overwrites (re-claim provisions a fresh token).
	if err := st.SetControlToken(en.BaseDomain, "tok-2"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := st.ControlToken(en.BaseDomain); tok != "tok-2" {
		t.Fatalf("ControlToken = %q, want tok-2", tok)
	}
	// Unknown agents fail closed in both directions.
	if err := st.SetControlToken("nope.example.com", "t"); err == nil {
		t.Fatal("SetControlToken(unknown agent) = nil, want error")
	}
	if _, err := st.ControlToken("nope.example.com"); err == nil {
		t.Fatal("ControlToken(unknown agent) = nil error, want error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestControlTokenRoundTrip -v`
Expected: FAIL — `st.ControlToken undefined` (compile error).

- [ ] **Step 3: Implement**

In `internal/relay/store.go`, replace `ensureAgentAccountColumn` with a generic helper (keep the doc-comment style) and migrate both columns in `Open`:

```go
// ensureAgentColumn adds a column to agents if an older DB predates it.
// CREATE TABLE IF NOT EXISTS can't alter an existing table, so we add the
// column idempotently.
func ensureAgentColumn(db *sql.DB, column string) error {
	rows, err := db.Query(`PRAGMA table_info(agents)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already migrated
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE agents ADD COLUMN ` + column + ` TEXT`)
	return err
}
```

In `Open`, replace the single `ensureAgentAccountColumn(db)` call with:

```go
	for _, col := range []string{"account_id", "control_token"} {
		if err := ensureAgentColumn(db, col); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate agents: %w", err)
		}
	}
```

Add the accessors (below `Authenticate`):

```go
// SetControlToken stores the plaintext control-API bearer the box pushed for
// this enrollment. Plaintext by necessity: the relay must present it verbatim
// on forwarded control requests (see the control-stream routing design).
func (s *Store) SetControlToken(baseDomain, token string) error {
	res, err := s.db.Exec(`UPDATE agents SET control_token=? WHERE base_domain=?`, token, baseDomain)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBadToken
	}
	return nil
}

// ControlToken returns the stored control bearer for baseDomain, "" if the box
// never provisioned one. Unknown agents are ErrBadToken.
func (s *Store) ControlToken(baseDomain string) (string, error) {
	var tok sql.NullString
	err := s.db.QueryRow(`SELECT control_token FROM agents WHERE base_domain=?`, baseDomain).Scan(&tok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadToken
	}
	if err != nil {
		return "", err
	}
	return tok.String, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/relay/ -v`
Expected: PASS (all, including existing).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/store.go internal/relay/store_test.go
git commit -m "feat(relay): per-agent control_token storage

Part of #73.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Tunnel `provision` op — box pushes its Token B to the relay

**Files:**
- Modify: `internal/tunnel/tunnel.go` (add `Token` field to `ControlRequest`)
- Modify: `internal/relay/server.go:88-110` (`handleControl` gains a `provision` case)
- Modify+Test: `internal/relay/server_test.go` (`startTestRelay` also returns the `*Store`; new test)

**Interfaces:**
- Consumes: Task 1's `SetControlToken`.
- Produces: `tunnel.ControlRequest.Token string` (json `token,omitempty`); relay handles `{"op":"provision","token":...}` by storing the token for `sess.BaseDomain` and replying `ControlResponse{}`; `startTestRelay(t, tlsCfg)` now returns `(*tunnel.Session, string, string, *Store)`.

- [ ] **Step 1: Write the failing test**

In `internal/relay/server_test.go`, change `startTestRelay`'s signature and returns:

```go
func startTestRelay(t *testing.T, tlsCfg *tls.Config) (*tunnel.Session, string, string, *Store) {
```

…and its final line to `return sess, tlsLn.Addr().String(), en.BaseDomain, st`. Update the existing caller in `TestControlRegisterThenTerminate`:

```go
	sess, tlsAddr, _, _ := startTestRelay(t, tlsCfg)
```

Append the new test:

```go
func TestControlProvisionStoresToken(t *testing.T) {
	sess, _, base, st := startTestRelay(t, nil)

	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "provision", Token: "box-tok"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" {
		t.Fatalf("provision error: %s", resp.Error)
	}
	if got, err := st.ControlToken(base); err != nil || got != "box-tok" {
		t.Fatalf("ControlToken = %q, %v (want box-tok)", got, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestControlProvisionStoresToken -v`
Expected: FAIL — `unknown field Token in struct literal` (compile error).

- [ ] **Step 3: Implement**

In `internal/tunnel/tunnel.go`, extend `ControlRequest`:

```go
// ControlRequest is an agent→relay control message on a KindControl stream.
type ControlRequest struct {
	Op       string `json:"op"` // "register" | "deregister" | "provision"
	App      string `json:"app,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Token    string `json:"token,omitempty"` // "provision": the box's control-API bearer for the relay to inject
}
```

In `internal/relay/server.go` `handleControl`, add before `default:`:

```go
	case "provision":
		// The box hands the relay its control-API bearer (agent-push Token B).
		// The op rides the authenticated session, so it can only ever set the
		// token for the session's own agent.
		if err := st.SetControlToken(sess.BaseDomain, req.Token); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/relay/ ./internal/tunnel/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/tunnel.go internal/relay/server.go internal/relay/server_test.go
git commit -m "feat(relay): provision control op — box pushes its Token B over the tunnel

Part of #73.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Agent — `TunnelClient.Provision` + `OnConnect` hook

**Files:**
- Modify: `internal/agent/tunnelclient.go`
- Test: `internal/agent/tunnelclient_test.go`

**Interfaces:**
- Consumes: Task 2's `ControlRequest.Token`; existing unexported `(*TunnelClient).control`.
- Produces: `(*TunnelClient).Provision(token string) error`; exported field `TunnelClient.OnConnect func()` invoked (own goroutine) each time a session is established, set before `Run`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/tunnelclient_test.go` (the file already imports `errors`):

```go
func TestTunnelClientProvision(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	relaySess := <-sessCh

	got := make(chan tunnel.ControlRequest, 1)
	go func() {
		kind, stream, err := relaySess.AcceptKind()
		if err != nil || kind != tunnel.KindControl {
			return
		}
		var req tunnel.ControlRequest
		_ = tunnel.ReadMsg(stream, &req)
		got <- req
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
		stream.Close()
	}()

	// Retry until Run publishes its session.
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err = c.Provision("box-token"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	req := <-got
	if req.Op != "provision" || req.Token != "box-token" {
		t.Fatalf("relay saw %+v, want op=provision token=box-token", req)
	}
}

func TestTunnelClientOnConnectFires(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := make(chan struct{}, 1)
	var c TunnelClient
	c.OnConnect = func() { fired <- struct{}{} }
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	<-sessCh
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("OnConnect did not fire after session establishment")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run 'TestTunnelClientProvision|TestTunnelClientOnConnectFires' -v`
Expected: FAIL — `c.Provision undefined` / `c.OnConnect undefined` (compile errors).

- [ ] **Step 3: Implement**

In `internal/agent/tunnelclient.go`, extend the struct:

```go
type TunnelClient struct {
	mu   sync.Mutex
	sess *tunnel.Session

	// OnConnect, if set before Run, is invoked in its own goroutine each time a
	// relay session is established — piperd uses it to provision the relay's
	// control bearer (see the control-stream routing design).
	OnConnect func()
}
```

In `Run`, after `c.setSession(sess)` add:

```go
		if c.OnConnect != nil {
			go c.OnConnect()
		}
```

Add next to `Register`/`Deregister`:

```go
// Provision hands the relay this box's control-API bearer for the enrollment.
// It rides the authenticated session, so it can only set this agent's token.
func (c *TunnelClient) Provision(token string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "provision", Token: token})
	return err
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/agent/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/tunnelclient.go internal/agent/tunnelclient_test.go
git commit -m "feat(agent): TunnelClient.Provision + OnConnect hook

Part of #73.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Relay control proxy — authz + reverse proxy over `KindControlAPI`

The core of #73: `/agents/<base-domain>/v1/...` → authenticate account → authorize ownership → reverse-proxy over a new tunnel stream kind, injecting Token B.

**Files:**
- Modify: `internal/tunnel/tunnel.go:89-93` (add `KindControlAPI`)
- Create: `internal/relay/proxy.go`
- Modify: `internal/relay/api.go` (constructors gain a `*Router`; mount the proxy)
- Modify: `cmd/piper-relay/main.go:150` (temporary `nil` router arg — replaced in Task 5)
- Test: `internal/relay/proxy_test.go`

**Interfaces:**
- Consumes: Task 1's `AgentAccount`/`ControlToken`, existing `AuthenticateAccount`, `Router.Lookup`, `bearerToken` (api.go), `openTestStore`, `NewFakeVerifier`.
- Produces: `tunnel.KindControlAPI byte = 'A'`; `relay.NewControlProxy(st *Store, router *Router) http.Handler`; `NewAPIWithTunnel(st *Store, v Verifier, tunnelEndpoint string, router *Router) http.Handler` (router nil ⇒ no proxy routes; `NewAPI(st, v)` unchanged, passes nil).

- [ ] **Step 1: Write the failing tests**

Create `internal/relay/proxy_test.go`:

```go
package relay

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/tunnel"
)

// pipeSession builds an in-memory relay↔agent tunnel pair whose relay-side
// session carries base as its BaseDomain.
func pipeSession(t *testing.T, base string) (relaySide, agentSide *tunnel.Session) {
	t.Helper()
	cc, sc := net.Pipe()
	t.Cleanup(func() { cc.Close(); sc.Close() })
	srvCh := make(chan *tunnel.Session, 1)
	go func() {
		s, err := tunnel.Serve(sc, func(_, _ string) error { return nil })
		if err == nil {
			srvCh <- s
		}
	}()
	agentSess, err := tunnel.Dial(cc, "tok", base)
	if err != nil {
		t.Fatal(err)
	}
	return <-srvCh, agentSess
}

// fakeBox answers KindControlAPI streams: one HTTP request per stream, echoing
// method, path and Authorization so tests see exactly what the proxy forwarded.
func fakeBox(sess *tunnel.Session) {
	for {
		kind, stream, err := sess.AcceptKind()
		if err != nil {
			return
		}
		if kind != tunnel.KindControlAPI {
			stream.Close()
			continue
		}
		go func() {
			defer stream.Close()
			req, err := http.ReadRequest(bufio.NewReader(stream))
			if err != nil {
				return
			}
			body := req.Method + " " + req.URL.RequestURI() + " auth=" + req.Header.Get("Authorization")
			fmt.Fprintf(stream, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		}()
	}
}

// proxyFixture: alice owns an enrolled agent; mallory is another tenant.
func proxyFixture(t *testing.T) (api http.Handler, st *Store, router *Router, aliceCred, malloryCred, base string) {
	t.Helper()
	st = openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	alice, err := st.UpsertAccount("sub-alice", "alice@x.com")
	if err != nil {
		t.Fatal(err)
	}
	aliceCred, _ = st.MintAccountCredential(alice.ID)
	mallory, err := st.UpsertAccount("sub-mallory", "mallory@x.com")
	if err != nil {
		t.Fatal(err)
	}
	malloryCred, _ = st.MintAccountCredential(mallory.ID)
	en, err := st.EnrollForAccount(alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	base = en.BaseDomain
	router = NewRouter()
	api = NewAPIWithTunnel(st, NewFakeVerifier(), "", router)
	return
}

func proxyGet(t *testing.T, api http.Handler, path, cred string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	return rr
}

func TestControlProxyAuthz(t *testing.T) {
	api, _, router, aliceCred, malloryCred, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)

	// No credential → 401.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no cred: %d, want 401", rr.Code)
	}
	// Unknown credential → 401.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", "bogus"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad cred: %d, want 401", rr.Code)
	}
	// Another tenant's credential → 404, indistinguishable from unknown agent.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant: %d, want 404", rr.Code)
	}
	// Unknown agent → 404.
	if rr := proxyGet(t, api, "/agents/nope.public.getpiper.co/v1/apps", aliceCred); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d, want 404", rr.Code)
	}
	// Path without /v1/ → 404.
	if rr := proxyGet(t, api, "/agents/"+base+"/secrets", aliceCred); rr.Code != http.StatusNotFound {
		t.Fatalf("non-v1 path: %d, want 404", rr.Code)
	}
}

func TestControlProxyOfflineAgent(t *testing.T) {
	api, _, _, aliceCred, _, base := proxyFixture(t)
	// Agent enrolled but no live session registered.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline agent: %d, want 503", rr.Code)
	}
}

func TestControlProxyForwardsWithTokenB(t *testing.T) {
	api, st, router, aliceCred, _, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)
	if err := st.SetControlToken(base, "boxtok"); err != nil {
		t.Fatal(err)
	}

	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps?limit=2", aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("proxied: %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "GET /v1/apps?limit=2 ") {
		t.Fatalf("prefix not stripped / query lost: %q", body)
	}
	if !strings.Contains(body, "auth=Bearer boxtok") {
		t.Fatalf("Token B not injected: %q", body)
	}
	if strings.Contains(body, aliceCred) {
		t.Fatalf("account credential leaked to the box: %q", body)
	}
}

func TestControlProxyNoTokenBForwardsBare(t *testing.T) {
	api, _, router, aliceCred, _, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)

	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("proxied: %d", rr.Code)
	}
	// Never provisioned: forwarded with NO Authorization (a real box would 401).
	if !strings.Contains(rr.Body.String(), "auth= ") && !strings.HasSuffix(strings.TrimSpace(rr.Body.String()), "auth=") {
		t.Fatalf("expected empty forwarded auth, got %q", rr.Body.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run TestControlProxy -v`
Expected: FAIL — `undefined: NewControlProxy` / wrong arg count for `NewAPIWithTunnel` (compile errors).

- [ ] **Step 3: Implement**

In `internal/tunnel/tunnel.go`, extend the kind block:

```go
const (
	KindPassthrough byte = 'T' // relay→agent: replayed ClientHello follows; agent pipes to :443
	KindHTTP        byte = 'H' // relay→agent: relay-terminated plaintext HTTP; agent pipes to :80
	KindControl     byte = 'C' // agent→relay: a length-prefixed ControlRequest/ControlResponse
	KindControlAPI  byte = 'A' // relay→agent: a forwarded control-plane HTTP request; agent pipes to the control API
)
```

Create `internal/relay/proxy.go`:

```go
package relay

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/getpiper/piper/internal/tunnel"
)

// NewControlProxy serves /agents/<base-domain>/v1/*: it authenticates the
// caller's relay account credential, authorizes that the account owns the
// agent, and reverse-proxies the request over the agent's tunnel as a
// KindControlAPI stream — swapping the caller's credential for the box's
// stored control bearer. The box still validates that bearer on every request
// (#77); the relay hop grants nothing at the box. Unknown and unowned agents
// are both 404 so existence is never leaked across tenants.
func NewControlProxy(st *Store, router *Router) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing bearer credential", http.StatusUnauthorized)
			return
		}
		acc, err := st.AuthenticateAccount(cred)
		if err != nil {
			http.Error(w, "bad credential", http.StatusUnauthorized)
			return
		}

		// Path shape: /agents/<base-domain>/v1/...
		rest := strings.TrimPrefix(r.URL.Path, "/agents/")
		base, tail, found := strings.Cut(rest, "/")
		if !found || base == "" || !strings.HasPrefix(tail, "v1/") {
			http.NotFound(w, r)
			return
		}

		ownerID, _, err := st.AgentAccount(base)
		if err != nil || ownerID != acc.ID {
			http.NotFound(w, r)
			return
		}

		sess, ok := router.Lookup(base)
		if !ok {
			http.Error(w, "agent not connected", http.StatusServiceUnavailable)
			return
		}
		boxToken, err := st.ControlToken(base)
		if err != nil {
			http.Error(w, "agent lookup failed", http.StatusInternalServerError)
			return
		}

		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "http"
				pr.Out.URL.Host = base
				pr.Out.URL.Path = "/" + tail
				// Never forward the caller's account credential to the box.
				// Inject the box's own bearer; if the box never provisioned one,
				// forward bare and let its auth gate answer 401.
				pr.Out.Header.Del("Authorization")
				if boxToken != "" {
					pr.Out.Header.Set("Authorization", "Bearer "+boxToken)
				}
			},
			Transport: &http.Transport{
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					return sess.OpenKind(tunnel.KindControlAPI)
				},
				// One tunnel stream per request: a pooled stream must never
				// outlive its session.
				DisableKeepAlives: true,
			},
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				http.Error(w, "box unreachable: "+err.Error(), http.StatusBadGateway)
			},
		}
		rp.ServeHTTP(w, r)
	})
}
```

In `internal/relay/api.go`, change the constructors:

```go
// NewAPI returns the account API without a tunnel endpoint or control proxy
// (tests / LAN). Use NewAPIWithTunnel in production.
func NewAPI(st *Store, v Verifier) http.Handler { return NewAPIWithTunnel(st, v, "", nil) }

// NewAPIWithTunnel is the full account-facing API: device login, enroll, and —
// when router is non-nil — the /agents/ control proxy (#73).
func NewAPIWithTunnel(st *Store, v Verifier, tunnelEndpoint string, router *Router) http.Handler {
	a := &api{st: st, v: v, tunnelEndpoint: tunnelEndpoint}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/login/device", a.loginDevice)
	mux.HandleFunc("POST /v1/login/poll", a.loginPoll)
	mux.HandleFunc("POST /v1/enroll", a.enroll)
	if router != nil {
		mux.Handle("/agents/", NewControlProxy(st, router))
	}
	return mux
}
```

In `cmd/piper-relay/main.go:150`, make the build pass with a `nil` router for now (Task 5 wires the real one):

```go
		if err := http.ListenAndServe(apiAddr, relay.NewAPIWithTunnel(st, v, tunnelPublic, nil)); err != nil {
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/relay/ ./internal/tunnel/ -v && go build ./...`
Expected: PASS; build OK.

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/tunnel.go internal/relay/proxy.go internal/relay/api.go internal/relay/proxy_test.go cmd/piper-relay/main.go
git commit -m "feat(relay): account-authz'd control proxy over KindControlAPI tunnel streams

Part of #73.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: SNI-dispatch the control plane at `api.<apex>` on `:443`

**Files:**
- Modify: `internal/relay/server.go` (`Serve`/`handlePublic` signatures; `connQueue`)
- Modify: `cmd/piper-relay/main.go` (create the router in main; pass handler+router to `Serve`)
- Test: `internal/relay/server_test.go` (`startTestRelay` gains a `ctrl` param; new dispatch test)

**Interfaces:**
- Consumes: Task 4's `NewAPIWithTunnel(..., router)`; existing `prefixConn`, `LoadWildcardConfig`, `writeWildcard` (terminate_test.go).
- Produces: `Serve(tlsAddr, tunnelAddr string, st *Store, tlsCfg *tls.Config, router *Router, ctrl http.Handler) error` — router now injected, `ctrl` (with a wildcard cert) served on SNI `api.<apex>`; `startTestRelay(t, tlsCfg, ctrl)` returning `(*tunnel.Session, string, string, *Store)`.

- [ ] **Step 1: Write the failing test**

In `internal/relay/server_test.go`, add a `ctrl http.Handler` parameter to `startTestRelay` and mirror what `Serve` will do (the helper replicates `Serve`'s loops because `Serve` doesn't expose ephemeral ports):

```go
func startTestRelay(t *testing.T, tlsCfg *tls.Config, ctrl http.Handler) (*tunnel.Session, string, string, *Store) {
```

Inside, before the `tlsLn` accept loop, arm the control queue:

```go
	var ctrlQ *connQueue
	if ctrl != nil && tlsCfg != nil {
		ctrlQ = newConnQueue()
		go func() { _ = http.Serve(ctrlQ, ctrl) }()
		t.Cleanup(func() { ctrlQ.Close() })
	}
	ctrlHost := "api." + st.apexOrDefault()
```

…and change the accept-loop call to `go handlePublic(c, router, tlsCfg, ctrlHost, ctrlQ)`. Update existing callers: `startTestRelay(t, tlsCfg, nil)` in `TestControlRegisterThenTerminate` and `startTestRelay(t, nil, nil)` in `TestControlProvisionStoresToken`.

Append the dispatch test (file already imports `crypto/tls`, `fmt`, `io`, `net/http`, `time`):

```go
func TestControlPlaneSNIDispatch(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ctrl := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ctrl-ok "+r.URL.Path)
	})
	_, tlsAddr, _, _ := startTestRelay(t, tlsCfg, ctrl)

	d := &tls.Dialer{Config: &tls.Config{ServerName: "api.public.getpiper.co", InsecureSkipVerify: true}}
	c, err := d.Dial("tcp", tlsAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /ping HTTP/1.1\r\nHost: api.public.getpiper.co\r\nConnection: close\r\n\r\n")
	b, _ := io.ReadAll(c)
	if !contains(string(b), "ctrl-ok /ping") {
		t.Fatalf("control dispatch response = %q", b)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestControlPlaneSNIDispatch -v`
Expected: FAIL — `undefined: newConnQueue` / `handlePublic` arg mismatch (compile errors).

- [ ] **Step 3: Implement**

In `internal/relay/server.go` add (plus `"net/http"` to imports):

```go
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
```

Change `Serve` to accept the router and control handler:

```go
// Serve runs the relay: it accepts agent tunnels on tunnelAddr and public TLS
// traffic on tlsAddr, routing each connection by SNI. tlsCfg is the wildcard
// config for relay-terminated shared-domain apps; nil ⇒ passthrough-only.
// ctrl, when non-nil and a wildcard cert is armed, is the relay's own HTTP API,
// served TLS-terminated at SNI "api.<apex>" (#73). Blocks until a listener fails.
func Serve(tlsAddr, tunnelAddr string, st *Store, tlsCfg *tls.Config, router *Router, ctrl http.Handler) error {
	var ctrlQ *connQueue
	if ctrl != nil && tlsCfg != nil {
		ctrlQ = newConnQueue()
		go func() { _ = http.Serve(ctrlQ, ctrl) }()
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
```

(The `router := NewRouter()` line is removed — the router is injected.)

Rework `handlePublic` — the control branch must NOT defer-close the conn (the http.Server owns it):

```go
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
```

In `cmd/piper-relay/main.go`, create the router before the API goroutine and share the handler:

```go
	router := relay.NewRouter()
	apiHandler := relay.NewAPIWithTunnel(st, v, tunnelPublic, router)

	go func() {
		log.Printf("piper-relay: control API %s", apiAddr)
		if err := http.ListenAndServe(apiAddr, apiHandler); err != nil {
			log.Fatalf("control API: %v", err)
		}
	}()
```

…and the last line becomes:

```go
	log.Fatal(relay.Serve(tlsAddr, tunnelAddr, st, tlsCfg, router, apiHandler))
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/relay/ -v && go build ./...`
Expected: PASS (including the existing terminate/passthrough tests, proving app traffic untouched); build OK.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/server.go internal/relay/server_test.go cmd/piper-relay/main.go
git commit -m "feat(relay): TLS-terminated control plane at api.<apex>, SNI-dispatched on :443

Part of #73.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: piperd — serve `KindControlAPI` + provision Token B on connect

**Files:**
- Modify: `internal/store/store.go` (add `DeleteToken`)
- Modify: `cmd/piperd/main.go` (dialLocal branches; `provisionRelayControl`; `OnConnect` wiring)
- Test: `internal/store/store_test.go`, `cmd/piperd/main_test.go`

**Interfaces:**
- Consumes: Task 3's `Provision`/`OnConnect`, Task 4's `tunnel.KindControlAPI`, existing `store.CreateToken/ListTokens/RevokeToken`, `store.Token{Label, RevokedAt}`.
- Produces: `(*store.Store).DeleteToken(label string) error` (hard delete); `provisionRelayControl(st relayTokenStore, push func(string) error, baseDomain string)` in `cmd/piperd` with `type relayTokenStore interface { ListTokens() ([]store.Token, error); CreateToken(label, scope string) (string, error); DeleteToken(label string) error }`. Provisioned tokens carry label `relay:<base-domain>`, scope `admin`.

- [ ] **Step 1: Write the failing store test**

Append to `internal/store/store_test.go` (the file's existing temp-store helper is `openTemp(t)`):

```go
func TestDeleteTokenHardDeletes(t *testing.T) {
	st := openTemp(t)

	if _, err := st.CreateToken("relay:base.example.com", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteToken("relay:base.example.com"); err != nil {
		t.Fatal(err)
	}
	toks, err := st.ListTokens()
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range toks {
		if tk.Label == "relay:base.example.com" {
			t.Fatal("token row still present after DeleteToken")
		}
	}
	// Deleting a non-existent label is not an error (idempotent unwind).
	if err := st.DeleteToken("relay:base.example.com"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}
```

- [ ] **Step 2: Run it, verify failure, implement `DeleteToken`**

Run: `go test ./internal/store/ -run TestDeleteTokenHardDeletes -v` → FAIL (`st.DeleteToken undefined`).

Add to `internal/store/store.go`, next to `RevokeToken`:

```go
// DeleteToken hard-deletes the token with the given label. It exists only to
// unwind a relay-provisioning push that failed after mint (cmd/piperd), so the
// next connect can retry. Owner-facing revocation is RevokeToken — soft, so the
// revoked row remains as the "never re-provision" marker.
func (s *Store) DeleteToken(label string) error {
	_, err := s.db.Exec(`DELETE FROM tokens WHERE label=?`, label)
	return err
}
```

Run again → PASS.

- [ ] **Step 3: Write the failing provisioning tests**

Append to `cmd/piperd/main_test.go` (add imports `"errors"`, `"time"`, and `"github.com/getpiper/piper/internal/store"` as needed; follow the file's existing fake style):

```go
type fakeProvisionStore struct {
	tokens  []store.Token
	created []string
	deleted []string
}

func (f *fakeProvisionStore) ListTokens() ([]store.Token, error) { return f.tokens, nil }
func (f *fakeProvisionStore) CreateToken(label, scope string) (string, error) {
	f.created = append(f.created, label+"/"+scope)
	return "tok-" + label, nil
}
func (f *fakeProvisionStore) DeleteToken(label string) error {
	f.deleted = append(f.deleted, label)
	return nil
}

func TestProvisionRelayControlFirstConnect(t *testing.T) {
	f := &fakeProvisionStore{}
	var pushed string
	provisionRelayControl(f, func(tok string) error { pushed = tok; return nil }, "base.example.com")
	if len(f.created) != 1 || f.created[0] != "relay:base.example.com/admin" {
		t.Fatalf("created = %v", f.created)
	}
	if pushed != "tok-relay:base.example.com" {
		t.Fatalf("pushed = %q", pushed)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("unexpected delete: %v", f.deleted)
	}
}

func TestProvisionRelayControlAlreadyProvisioned(t *testing.T) {
	f := &fakeProvisionStore{tokens: []store.Token{{Label: "relay:base.example.com"}}}
	provisionRelayControl(f, func(string) error { t.Fatal("must not push"); return nil }, "base.example.com")
	if len(f.created) != 0 {
		t.Fatalf("re-minted: %v", f.created)
	}
}

func TestProvisionRelayControlRevokedMeansNo(t *testing.T) {
	// A revoked row is the owner's unilateral cutoff: never re-mint for this
	// enrollment (spec: re-provisioning requires a new claim → new base domain).
	rt := time.Now()
	f := &fakeProvisionStore{tokens: []store.Token{{Label: "relay:base.example.com", RevokedAt: &rt}}}
	provisionRelayControl(f, func(string) error { t.Fatal("must not push"); return nil }, "base.example.com")
	if len(f.created) != 0 {
		t.Fatalf("re-minted after owner revoke: %v", f.created)
	}
}

func TestProvisionRelayControlPushFailureUnwinds(t *testing.T) {
	f := &fakeProvisionStore{}
	provisionRelayControl(f, func(string) error { return errors.New("session died") }, "base.example.com")
	// The mint must be unwound so the marker doesn't block the next attempt.
	if len(f.deleted) != 1 || f.deleted[0] != "relay:base.example.com" {
		t.Fatalf("deleted = %v, want the just-minted label", f.deleted)
	}
}
```

Run: `go test ./cmd/piperd/ -run TestProvisionRelayControl -v` → FAIL (`undefined: provisionRelayControl`).

- [ ] **Step 4: Implement provisioning + dialLocal + wiring**

In `cmd/piperd/main.go`, add near `tokenStore`:

```go
// relayTokenStore is the store slice relay-control provisioning needs.
type relayTokenStore interface {
	ListTokens() ([]store.Token, error)
	CreateToken(label, scope string) (string, error)
	DeleteToken(label string) error
}

// provisionRelayControl mints a control-API token for the relay and pushes it
// over the tunnel, once per enrollment (agent-push Token B — see the
// control-stream routing design). The token row itself is the marker: any row
// labeled relay:<base>, live OR revoked, means "already provisioned" or "the
// owner cut the relay off" — never re-mint. A new `piper connect` creates a new
// enrollment (new base domain) and so a fresh mint. If the push fails, the
// just-minted row is deleted so the next connect retries.
func provisionRelayControl(st relayTokenStore, push func(string) error, baseDomain string) {
	label := "relay:" + baseDomain
	toks, err := st.ListTokens()
	if err != nil {
		log.Printf("relay control provision: list tokens: %v", err)
		return
	}
	for _, tk := range toks {
		if tk.Label == label {
			return
		}
	}
	tok, err := st.CreateToken(label, "admin")
	if err != nil {
		log.Printf("relay control provision: mint: %v", err)
		return
	}
	if err := push(tok); err != nil {
		log.Printf("relay control provision: push: %v (will retry next connect)", err)
		_ = st.DeleteToken(label)
		return
	}
	log.Printf("relay control provision: pushed control bearer for %s", baseDomain)
}
```

In `main()`, replace the two `dialLocal` closures so both modes serve the control kind (`cfg.APIAddr` is the loopback control API, default `127.0.0.1:8088`):

```go
		var dialLocal func(kind byte) (net.Conn, error)
		if cfg.Terminated {
			dialLocal = func(kind byte) (net.Conn, error) {
				switch kind {
				case tunnel.KindControlAPI:
					return net.Dial("tcp", cfg.APIAddr)
				case tunnel.KindHTTP:
					return net.Dial("tcp", "127.0.0.1:80")
				default:
					return net.Dial("tcp", "127.0.0.1:443")
				}
			}
		} else {
			if err := setupRelayTLS(ctx, cfg); err != nil {
				log.Fatalf("relay tls: %v", err)
			}
			dialLocal = func(kind byte) (net.Conn, error) {
				if kind == tunnel.KindControlAPI {
					return net.Dial("tcp", cfg.APIAddr)
				}
				return net.Dial("tcp", "127.0.0.1:443")
			}
		}
```

…and wire the hook where the client is built (before `go tc.Run(...)`):

```go
		tc := &agent.TunnelClient{}
		tc.OnConnect = func() { provisionRelayControl(st, tc.Provision, cfg.BaseDomain) }
		go tc.Run(ctx, cfg.RelayAddr, cfg.RelayToken, cfg.BaseDomain, dialLocal)
```

- [ ] **Step 5: Run tests**

Run: `go test ./cmd/piperd/ ./internal/store/ -v && go build ./...`
Expected: PASS; build OK.

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go cmd/piperd/main.go cmd/piperd/main_test.go
git commit -m "feat(agent): serve relay control streams; agent-push Token B provisioning

Part of #73.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: E2E — remote control through the relay + authz denial; verify; PROGRESS

**Files:**
- Modify: `test/e2e/relay_terminated_test.go`
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: the whole stack; CLI config at `$HOME/.piper/piper/config.json` (`account_credential` field); relay DB tables `agents`, `accounts`, `account_creds` (`token_hash` = hex sha256 of the plaintext credential).
- Produces: nothing downstream — this is the acceptance gate for #73's criteria.

- [ ] **Step 1: Extend the e2e test**

In `test/e2e/relay_terminated_test.go`, add imports `"bufio"`, `"crypto/sha256"`, `"encoding/hex"`, `"encoding/json"`, `"net/http"`. Append to the end of `TestRelayTerminatedSelfService` (after the existing terminated-response check):

```go
	// ---- Remote control plane through the relay (#73) ----
	base := agentBaseDomain(t, relayData)
	cred := accountCredential(t, home)

	// Owner's credential → the box's real, Token-B-gated /v1/apps.
	apps := controlRequest(t, "api."+apex, "127.0.0.1:8443", "/agents/"+base+"/v1/apps", cred, http.StatusOK, 30*time.Second)
	if !strings.Contains(apps, "blog") {
		t.Fatalf("control response missing deployed app: %q", apps)
	}

	// Unknown credential → 401 at the relay.
	controlRequest(t, "api."+apex, "127.0.0.1:8443", "/agents/"+base+"/v1/apps", "bogus-cred", http.StatusUnauthorized, 10*time.Second)

	// Another tenant → 404 at the relay: never reaches the box, existence not leaked.
	mcred := insertSecondAccount(t, relayData)
	controlRequest(t, "api."+apex, "127.0.0.1:8443", "/agents/"+base+"/v1/apps", mcred, http.StatusNotFound, 10*time.Second)
```

Append the helpers:

```go
// agentBaseDomain reads the enrolled agent's base domain from the relay store.
func agentBaseDomain(t *testing.T, relayData string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(relayData, "relay.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var base string
	if err := db.QueryRow(`SELECT base_domain FROM agents LIMIT 1`).Scan(&base); err != nil {
		t.Fatalf("read agent base domain: %v", err)
	}
	return base
}

// accountCredential reads the relay account credential `piper login` saved.
func accountCredential(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".piper", "piper", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cc struct {
		AccountCredential string `json:"account_credential"`
	}
	if err := json.Unmarshal(b, &cc); err != nil {
		t.Fatal(err)
	}
	if cc.AccountCredential == "" {
		t.Fatal("no account_credential in CLI config")
	}
	return cc.AccountCredential
}

// insertSecondAccount plants a second tenant directly in the relay store (the
// auto-approve verifier always yields the same account, so cross-tenant denial
// needs a hand-made one) and returns its plaintext credential.
func insertSecondAccount(t *testing.T, relayData string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(relayData, "relay.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(
		`INSERT INTO accounts(id, google_sub, username, disabled, created_at) VALUES('mallory-id','mallory-sub','mallory',0,?)`, now); err != nil {
		t.Fatal(err)
	}
	cred := "mallory-cred-e2e"
	sum := sha256.Sum256([]byte(cred))
	if _, err := db.Exec(
		`INSERT INTO account_creds(token_hash, account_id, created_at) VALUES(?,'mallory-id',?)`,
		hex.EncodeToString(sum[:]), now); err != nil {
		t.Fatal(err)
	}
	return cred
}

// controlRequest performs one control-plane HTTPS request against the relay
// (SNI-dispatched api.<apex>), retrying until it sees wantStatus; returns the body.
func controlRequest(t *testing.T, sni, addr, path, cred string, wantStatus int, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		d := &tls.Dialer{Config: &tls.Config{ServerName: sni, InsecureSkipVerify: true}}
		conn, err := d.Dial("tcp", addr)
		if err != nil {
			last = err.Error()
			time.Sleep(500 * time.Millisecond)
			continue
		}
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n", path, sni, cred)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			last = err.Error()
			conn.Close()
			time.Sleep(500 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		conn.Close()
		if resp.StatusCode == wantStatus {
			return string(b)
		}
		last = fmt.Sprintf("status %d body %q", resp.StatusCode, b)
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("control %s: want %d, last: %s", path, wantStatus, last)
	return ""
}
```

- [ ] **Step 2: Run the e2e (needs Docker)**

Run: `RUN_E2E=1 go test ./test/e2e/ -run TestRelayTerminatedSelfService -v -timeout 10m`
Expected: PASS — control response listing `blog`, 401 for bogus cred, 404 for the second tenant. If Docker is unavailable, say so explicitly and do not claim the e2e passed.

- [ ] **Step 3: Update PROGRESS.md**

Under the `#90` block's child list (after the `#89` line, before the `⬜ surface the relay-assigned public host` line), add:

```markdown
  - ✅ Relay control-stream routing — account-authz'd control plane at `api.<apex>` (SNI-dispatched, wildcard cert), forwarded over `KindControlAPI` tunnel streams with agent-push Token B provisioning — [#73](https://github.com/getpiper/piper/issues/73)
```

- [ ] **Step 4: Full verify**

Run: `make verify`
Expected: gofmt clean, vet clean, all tests pass, arm64 cross-build OK.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/relay_terminated_test.go PROGRESS.md
git commit -m "test(e2e): remote control plane through the relay + authz denial

Part of #73.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Completion

After Task 7: push `faruk/relay-control-routing`, open a PR into `main` titled `feat(relay): control-stream routing + caller→agent authz` with `Closes #73` and a pointer to the design doc in the body (squash-merge per repo convention). Use the superpowers:finishing-a-development-branch skill.
