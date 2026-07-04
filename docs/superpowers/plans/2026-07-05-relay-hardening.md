# Relay Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete issue #27's remaining Plan 2 correctness and lifecycle cleanups while preserving the already-merged pre-authentication deadline behavior.

**Architecture:** Keep each fix in its owning package: Caddy request semantics in `internal/caddy`, relay uniqueness in its SQLite schema, provider selection in `cmd/piperd`, and tunnel-client cancellation in `internal/agent`. Introduce only narrow interfaces around the renewal loop so it can be driven deterministically in tests.

**Tech Stack:** Go, `net/http`, Caddy JSON admin API, modernc SQLite, lego DNS providers, hashicorp yamux.

## Global Constraints

- Do not change the relay wire protocol or the existing pre-authentication deadline implementation.
- Empty `PIPER_DNS_PROVIDER` and explicit `cloudflare` both select Cloudflare; every other value fails clearly.
- Keep the module pure Go and compatible with `CGO_ENABLED=0` Linux ARM64 builds.
- Production packages read environment configuration only through `internal/config`; provider credential lookup remains inside lego's Cloudflare adapter.
- Every behavior change starts with a failing test.
- Completion requires `gofmt -l .` with no output, `go vet ./...`, `make test`, and `make cross`.

---

### Task 1: Replace Caddy certificates during renewal

**Files:**
- Modify: `internal/caddy/client.go`
- Modify: `internal/caddy/tls_test.go`
- Modify: `cmd/piperd/main.go`
- Create: `cmd/piperd/main_test.go`

**Interfaces:**
- Produces: `func (c *Client) ReplaceCert(certPEM, keyPEM string) error`.
- Produces: package-local `certificateManager` and `certificateReplacer` interfaces plus `runRenewLoop(..., ticks <-chan time.Time, now func() time.Time)`.

- [ ] **Step 1: Add a failing Caddy replacement test**

Append a test to `internal/caddy/tls_test.go` which records the request, calls `ReplaceCert("CERTPEM", "KEYPEM")`, and asserts:

```go
if gotMethod != http.MethodPatch {
	 t.Fatalf("method = %q, want PATCH", gotMethod)
}
if gotPath != "/config/apps/tls/certificates/load_pem" {
	 t.Fatalf("path = %q", gotPath)
}
var got []map[string]string
if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
	 t.Fatalf("body not a JSON array: %v (%s)", err, gotBody)
}
if len(got) != 1 || got[0]["certificate"] != "CERTPEM" || got[0]["key"] != "KEYPEM" {
	 t.Fatalf("bad replacement body: %s", gotBody)
}
```

- [ ] **Step 2: Verify the replacement test fails**

Run: `go test ./internal/caddy -run TestReplaceCert -v`

Expected: build failure because `(*Client).ReplaceCert` is undefined.

- [ ] **Step 3: Implement full-list replacement**

Add to `internal/caddy/client.go`:

```go
// ReplaceCert replaces Caddy's complete load_pem certificate list with one
// cert/key pair. Renewal uses this instead of appending duplicate entries.
func (c *Client) ReplaceCert(certPEM, keyPEM string) error {
	body, _ := json.Marshal([]map[string]string{{
		"certificate": certPEM,
		"key":         keyPEM,
	}})
	url := c.base + "/config/apps/tls/certificates/load_pem"
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("caddy replace cert: status %d", resp.StatusCode)
	}
	return nil
}
```

- [ ] **Step 4: Verify the Caddy test passes**

Run: `go test ./internal/caddy -run 'Test(Load|Replace)Cert' -v`

Expected: both tests pass.

- [ ] **Step 5: Add a failing deterministic renewal test**

Create `cmd/piperd/main_test.go` with:

```go
package main

import (
	"context"
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

type fakeCertificateManager struct {
	cert, key []byte
}

func (f fakeCertificateManager) Obtain([]string) ([]byte, []byte, error) {
	return f.cert, f.key, nil
}

type fakeCertificateReplacer struct {
	cert, key string
	called    chan struct{}
}

func (f *fakeCertificateReplacer) ReplaceCert(cert, key string) error {
	f.cert, f.key = cert, key
	close(f.called)
	return nil
}

func expiringCert(t *testing.T, now time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{CommonName: "example.com"},
		NotBefore: now.Add(-time.Hour),
		NotAfter: now.Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestRunRenewLoopReplacesCertificate(t *testing.T) {
	now := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	mgr := fakeCertificateManager{cert: []byte("NEW CERT"), key: []byte("NEW KEY")}
	replacer := &fakeCertificateReplacer{called: make(chan struct{})}
	ticks := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runRenewLoop(ctx, mgr, replacer, "example.com", expiringCert(t, now), ticks, func() time.Time { return now })
		close(done)
	}()
	ticks <- now
	select {
	case <-replacer.called:
	case <-time.After(time.Second):
		t.Fatal("certificate was not replaced")
	}
	if replacer.cert != "NEW CERT" || replacer.key != "NEW KEY" {
		t.Fatalf("replacement = %q, %q", replacer.cert, replacer.key)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renew loop did not stop")
	}
}
```

- [ ] **Step 6: Verify the renewal test fails**

Run: `go test ./cmd/piperd -run TestRunRenewLoopReplacesCertificate -v`

Expected: build failure because `runRenewLoop` is undefined.

- [ ] **Step 7: Extract the renewal seams and use replacement**

In `cmd/piperd/main.go`, add:

```go
type certificateManager interface {
	Obtain([]string) ([]byte, []byte, error)
}

type certificateReplacer interface {
	ReplaceCert(certPEM, keyPEM string) error
}
```

Keep `renewLoop` responsible for ticker ownership, and delegate its loop:

```go
func renewLoop(ctx context.Context, mgr certificateManager, cc certificateReplacer, base string, certPEM []byte) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	runRenewLoop(ctx, mgr, cc, base, certPEM, ticker.C, time.Now)
}

func runRenewLoop(ctx context.Context, mgr certificateManager, cc certificateReplacer, base string, certPEM []byte, ticks <-chan time.Time, now func() time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			due, err := certs.NeedsRenewal(certPEM, 30*24*time.Hour, now())
			if err != nil || !due {
				continue
			}
			newCert, newKey, err := mgr.Obtain([]string{"*." + base, base})
			if err != nil {
				log.Printf("renew: %v", err)
				continue
			}
			if err := cc.ReplaceCert(string(newCert), string(newKey)); err != nil {
				log.Printf("renew load: %v", err)
				continue
			}
			certPEM = newCert
		}
	}
}
```

- [ ] **Step 8: Verify renewal and Caddy packages pass**

Run: `go test ./cmd/piperd ./internal/caddy -v`

Expected: all tests pass.

- [ ] **Step 9: Commit certificate replacement**

```bash
git add cmd/piperd/main.go cmd/piperd/main_test.go internal/caddy/client.go internal/caddy/tls_test.go
git commit -m "fix(proxy): replace renewed certificate list"
```

---

### Task 2: Enforce unique relay base domains

**Files:**
- Modify: `internal/relay/schema.sql`
- Modify: `internal/relay/store_test.go`

**Interfaces:**
- Consumes: existing `(*Store).Enroll(name, baseDomain)`.
- Produces: database invariant that one base domain belongs to one agent.

- [ ] **Step 1: Add a failing duplicate-domain test**

Append to `internal/relay/store_test.go`:

```go
func TestEnrollRejectsDuplicateBaseDomain(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Enroll("alice", "shared.example.com"); err != nil {
		t.Fatalf("first Enroll: %v", err)
	}
	if _, err := st.Enroll("bob", "shared.example.com"); err == nil {
		t.Fatal("second Enroll succeeded for duplicate base domain")
	}
}
```

- [ ] **Step 2: Verify the duplicate-domain test fails**

Run: `go test ./internal/relay -run TestEnrollRejectsDuplicateBaseDomain -v`

Expected: failure that the second enrollment succeeded.

- [ ] **Step 3: Add the migration-safe unique index**

Append to `internal/relay/schema.sql`:

```sql

CREATE UNIQUE INDEX IF NOT EXISTS agents_base_domain_unique
    ON agents(base_domain);
```

- [ ] **Step 4: Verify relay store tests pass**

Run: `go test ./internal/relay -run 'TestEnroll' -v`

Expected: all enrollment tests pass.

- [ ] **Step 5: Commit the uniqueness invariant**

```bash
git add internal/relay/schema.sql internal/relay/store_test.go
git commit -m "fix(relay): reject duplicate base domains"
```

---

### Task 3: Validate DNS provider selection and clean config tests

**Files:**
- Modify: `cmd/piperd/main.go`
- Modify: `cmd/piperd/main_test.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `config.Config.DNSProvider`.
- Produces: `func newDNSProvider(name string) (challenge.Provider, error)` with empty/Cloudflare selection and explicit unsupported-provider errors.

- [ ] **Step 1: Add failing provider-selection tests**

Add the `challenge` import and table-driven tests to `cmd/piperd/main_test.go`:

```go
func TestNewDNSProviderRejectsUnsupportedProvider(t *testing.T) {
	provider, err := newDNSProvider("route53")
	if provider != nil {
		t.Fatalf("provider = %T, want nil", provider)
	}
	if err == nil || !strings.Contains(err.Error(), `unsupported DNS provider "route53"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewDNSProviderSelectsCloudflare(t *testing.T) {
	t.Setenv("CF_DNS_API_TOKEN", "test-token")
	for _, name := range []string{"", "cloudflare"} {
		t.Run(name, func(t *testing.T) {
			provider, err := newDNSProvider(name)
			if err != nil {
				t.Fatalf("newDNSProvider(%q): %v", name, err)
			}
			if _, ok := provider.(challenge.Provider); !ok {
				t.Fatalf("provider = %T", provider)
			}
		})
	}
}
```

Also import `strings` and `github.com/go-acme/lego/v4/challenge`.

- [ ] **Step 2: Verify provider tests fail**

Run: `go test ./cmd/piperd -run TestNewDNSProvider -v`

Expected: build failure because `newDNSProvider` is undefined.

- [ ] **Step 3: Implement provider selection and wire it into TLS setup**

Import `fmt` and `github.com/go-acme/lego/v4/challenge`, then add:

```go
func newDNSProvider(name string) (challenge.Provider, error) {
	switch name {
	case "", "cloudflare":
		return cloudflare.NewDNSProvider()
	default:
		return nil, fmt.Errorf("unsupported DNS provider %q", name)
	}
}
```

In `setupRelayTLS`, replace `cloudflare.NewDNSProvider()` with:

```go
provider, err := newDNSProvider(cfg.DNSProvider)
```

- [ ] **Step 4: Replace manual relay test environment cleanup**

In `TestLoadRelayFields`, remove the `os` import, three `os.Setenv` calls, and deferred `os.Unsetenv` block. Replace them with:

```go
t.Setenv("PIPER_RELAY_ADDR", "relay.example.com:7000")
t.Setenv("PIPER_RELAY_TOKEN", "tok-xyz")
t.Setenv("PIPER_ACME_EMAIL", "me@example.com")
```

- [ ] **Step 5: Verify provider and config tests pass**

Run: `go test ./cmd/piperd ./internal/config -v`

Expected: all tests pass.

- [ ] **Step 6: Commit provider validation and test hygiene**

```bash
git add cmd/piperd/main.go cmd/piperd/main_test.go internal/config/config_test.go
git commit -m "fix(agent): validate DNS provider selection"
```

---

### Task 4: Stop tunnel stream acceptance on cancellation

**Files:**
- Modify: `internal/agent/tunnelclient.go`
- Modify: `internal/agent/tunnelclient_test.go`

**Interfaces:**
- Consumes: existing `serveStreams(ctx, sess, dialLocal)`.
- Produces: prompt `serveStreams` return after `ctx.Done()` while preserving normal session-death behavior.

- [ ] **Step 1: Add a failing cancellation test**

Append to `internal/agent/tunnelclient_test.go`:

```go
func TestServeStreamsStopsOnContextCancellation(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

	serverResult := make(chan *tunnel.Session, 1)
	go func() {
		sess, _ := tunnel.Serve(serverConn, func(_, _ string) error { return nil })
		serverResult <- sess
	}()
	clientSession, err := tunnel.Dial(clientConn, "token", "example.com")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	serverSession := <-serverResult
	t.Cleanup(func() { serverSession.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		serveStreams(ctx, clientSession, func() (net.Conn, error) {
			return nil, errors.New("unexpected local dial")
		})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveStreams did not stop after context cancellation")
	}
}
```

Add the standard-library `errors` import.

- [ ] **Step 2: Verify the cancellation test fails**

Run: `go test ./internal/agent -run TestServeStreamsStopsOnContextCancellation -v`

Expected: failure after one second because `Accept` remains blocked.

- [ ] **Step 3: Close the session when context is cancelled**

At the start of `serveStreams`, after `defer sess.Close()`, add:

```go
stopCancel := context.AfterFunc(ctx, func() {
	_ = sess.Close()
})
defer stopCancel()
```

This makes cancellation close yamux and unblock `Accept`; stopping the callback on normal return avoids retaining the session.

- [ ] **Step 4: Verify agent tests pass**

Run: `go test ./internal/agent -v`

Expected: all tests pass.

- [ ] **Step 5: Commit cancellation behavior**

```bash
git add internal/agent/tunnelclient.go internal/agent/tunnelclient_test.go
git commit -m "fix(agent): stop tunnel streams on cancellation"
```

---

### Task 5: Full verification

**Files:**
- Verify all changed Go and SQL files.

**Interfaces:**
- Consumes: Tasks 1-4.
- Produces: a branch ready for review under issue #27.

- [ ] **Step 1: Format changed Go files**

Run: `gofmt -w cmd/piperd/main.go cmd/piperd/main_test.go internal/agent/tunnelclient.go internal/agent/tunnelclient_test.go internal/caddy/client.go internal/caddy/tls_test.go internal/config/config_test.go internal/relay/store_test.go`

Expected: command exits zero.

- [ ] **Step 2: Verify formatting is clean**

Run: `gofmt -l .`

Expected: no output.

- [ ] **Step 3: Run static analysis**

Run: `go vet ./...`

Expected: exit zero with no diagnostics.

- [ ] **Step 4: Run the full test suite**

Run: `make test`

Expected: all packages pass; Docker-dependent tests skip cleanly when Docker is unavailable.

- [ ] **Step 5: Run the ARM64 no-cgo build**

Run: `make cross`

Expected: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...` succeeds.

- [ ] **Step 6: Inspect the branch diff**

Run: `git status --short && git diff --check && git log --oneline main..HEAD`

Expected: no uncommitted files, no whitespace errors, and the design plus four focused implementation commits are listed.
