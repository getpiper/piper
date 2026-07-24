# Domain-Config API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remote callers can set a box's BYO base domain + DNS creds on the authenticated control API and watch cert issuance live — no piperd restart (issue #102, spec: `docs/superpowers/specs/2026-07-10-domain-config-api-design.md`).

**Architecture:** New `internal/domain.Manager` owns the custom-domain lifecycle (issuing → active/failed, persisted in SQLite) and orchestrates `certs`/`caddy`/tunnel through interfaces, mirroring `deploy`. The relay learns the custom domain via a new `set-domain` control op and splices its SNI as passthrough. Shared-domain (terminated) URLs coexist: a second Caddy server `piper-tls` serves :443 for the custom domain while :80 keeps serving relay-terminated HTTP.

**Tech Stack:** Go, modernc.org/sqlite (pure Go), lego (ACME DNS-01, cloudflare), embedded Caddy admin API, yamux tunnel.

## Global Constraints

- **No cgo** — everything passes `CGO_ENABLED=0`; `make cross` (linux/arm64) must stay green.
- Module path `github.com/piperbox/piper`.
- Domain-config status strings are exactly `""`, `"issuing"`, `"active"`, `"failed"` (deployment statuses `"building"`/`"running"`/`"failed"`/`"stopped"` are untouched).
- Defaults: control API `127.0.0.1:8088`, Caddy admin `http://127.0.0.1:2019`, base domain `piper.localhost`, HTTPS listen `:443`.
- Layering: `store` persistence only; `caddy` Caddy admin only; `domain` orchestrates via interfaces; `api` transport. Nothing imports "up".
- Secrets: `dns_token` is write-only on the API (never in any response); cert private key + ACME account key never leave the box (0600 files under DataDir).
- Branch: `ozykhan/domain-config-api`. One commit per task, conventional-commit style, body line `Part of #102`, and end every commit message with:
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`
- Each task: run the named package tests per step; run `make verify` before the task's commit.

---

### Task 1: `store` — `domain_config` table + CRUD

**Files:**
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: existing `store.Open`, `ErrNotFound`, `openTemp(t)` test helper.
- Produces (later tasks call these on `*store.Store`):
  - `type DomainConfig struct { Domain, DNSProvider, DNSToken, Status, Error string; CertNotAfter, UpdatedAt time.Time }`
  - `SetDomainConfig(domain, provider, token string) error` — upserts the single row, resets `Status` to `"issuing"`, clears `Error`/`CertNotAfter`.
  - `GetDomainConfig() (DomainConfig, error)` — `ErrNotFound` when unset.
  - `UpdateDomainStatus(status, errMsg string, notAfter time.Time) error` — `ErrNotFound` if no row.
  - `DeleteDomainConfig() error`

- [ ] **Step 1: Write the failing test**

Append to `internal/store/store_test.go`:

```go
func TestDomainConfigRoundTrip(t *testing.T) {
	s := openTemp(t)

	if _, err := s.GetDomainConfig(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetDomainConfig on empty store: err = %v, want ErrNotFound", err)
	}

	if err := s.SetDomainConfig("example.com", "cloudflare", "cf-token"); err != nil {
		t.Fatalf("SetDomainConfig: %v", err)
	}
	dc, err := s.GetDomainConfig()
	if err != nil {
		t.Fatalf("GetDomainConfig: %v", err)
	}
	if dc.Domain != "example.com" || dc.DNSProvider != "cloudflare" || dc.DNSToken != "cf-token" {
		t.Fatalf("round-trip = %+v", dc)
	}
	if dc.Status != "issuing" || dc.Error != "" || !dc.CertNotAfter.IsZero() {
		t.Fatalf("fresh config = %+v, want status=issuing, no error, zero not-after", dc)
	}

	notAfter := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	if err := s.UpdateDomainStatus("active", "", notAfter); err != nil {
		t.Fatalf("UpdateDomainStatus: %v", err)
	}
	dc, _ = s.GetDomainConfig()
	if dc.Status != "active" || !dc.CertNotAfter.Equal(notAfter) {
		t.Fatalf("after update = %+v", dc)
	}

	if err := s.UpdateDomainStatus("failed", "acme: boom", time.Time{}); err != nil {
		t.Fatalf("UpdateDomainStatus failed: %v", err)
	}
	dc, _ = s.GetDomainConfig()
	if dc.Status != "failed" || dc.Error != "acme: boom" {
		t.Fatalf("failed update = %+v", dc)
	}

	// Re-Set replaces the row and resets status/error.
	if err := s.SetDomainConfig("other.dev", "cloudflare", "tok2"); err != nil {
		t.Fatalf("re-SetDomainConfig: %v", err)
	}
	dc, _ = s.GetDomainConfig()
	if dc.Domain != "other.dev" || dc.Status != "issuing" || dc.Error != "" {
		t.Fatalf("after re-set = %+v", dc)
	}

	if err := s.DeleteDomainConfig(); err != nil {
		t.Fatalf("DeleteDomainConfig: %v", err)
	}
	if _, err := s.GetDomainConfig(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
}

func TestUpdateDomainStatusWithoutRow(t *testing.T) {
	s := openTemp(t)
	if err := s.UpdateDomainStatus("active", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
```

(`errors` and `time` are already imported by this test file; add them if the compiler complains.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestDomainConfig -v`
Expected: FAIL — `s.GetDomainConfig undefined`.

- [ ] **Step 3: Write the implementation**

Append to `internal/store/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS domain_config (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    domain         TEXT NOT NULL,
    dns_provider   TEXT NOT NULL,
    dns_token      TEXT NOT NULL,
    status         TEXT NOT NULL,
    error          TEXT NOT NULL DEFAULT '',
    cert_not_after TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL
);
```

Append to `internal/store/store.go`:

```go
// DomainConfig is the box's BYO custom-domain config (single row). DNSToken is
// a secret: it is stored for issuance and must never leave the box via the API.
type DomainConfig struct {
	Domain       string
	DNSProvider  string
	DNSToken     string
	Status       string // "issuing" | "active" | "failed"
	Error        string
	CertNotAfter time.Time
	UpdatedAt    time.Time
}

// SetDomainConfig upserts the custom-domain config, resetting it to a fresh
// "issuing" state.
func (s *Store) SetDomainConfig(domain, provider, token string) error {
	_, err := s.db.Exec(
		`INSERT INTO domain_config(id, domain, dns_provider, dns_token, status, error, cert_not_after, updated_at)
		 VALUES(1,?,?,?,'issuing','','',?)
		 ON CONFLICT(id) DO UPDATE SET domain=excluded.domain,
		   dns_provider=excluded.dns_provider, dns_token=excluded.dns_token,
		   status='issuing', error='', cert_not_after='', updated_at=excluded.updated_at`,
		domain, provider, token, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// GetDomainConfig returns the config, or ErrNotFound when no domain is set.
func (s *Store) GetDomainConfig() (DomainConfig, error) {
	var dc DomainConfig
	var notAfter, updated string
	err := s.db.QueryRow(
		`SELECT domain, dns_provider, dns_token, status, error, cert_not_after, updated_at
		 FROM domain_config WHERE id=1`).
		Scan(&dc.Domain, &dc.DNSProvider, &dc.DNSToken, &dc.Status, &dc.Error, &notAfter, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DomainConfig{}, ErrNotFound
	}
	if err != nil {
		return DomainConfig{}, err
	}
	if notAfter != "" {
		dc.CertNotAfter, _ = time.Parse(time.RFC3339Nano, notAfter)
	}
	dc.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return dc, nil
}

// UpdateDomainStatus records the outcome of an issuance/renewal step. A zero
// notAfter stores the empty string.
func (s *Store) UpdateDomainStatus(status, errMsg string, notAfter time.Time) error {
	na := ""
	if !notAfter.IsZero() {
		na = notAfter.UTC().Format(time.RFC3339Nano)
	}
	res, err := s.db.Exec(
		`UPDATE domain_config SET status=?, error=?, cert_not_after=?, updated_at=? WHERE id=1`,
		status, errMsg, na, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDomainConfig removes the custom-domain config. Deleting an absent
// config is not an error.
func (s *Store) DeleteDomainConfig() error {
	_, err := s.db.Exec(`DELETE FROM domain_config WHERE id=1`)
	return err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS (all, including the new two).

- [ ] **Step 5: `make verify`, then commit**

```bash
git add internal/store/
git commit -m "feat(store): domain_config table + CRUD

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: `certs` — leaf expiry helper, persisted ACME account key, token-configured cloudflare issuer

**Files:**
- Modify: `internal/certs/renew.go` (add `NotAfter`)
- Create: `internal/certs/account.go`, `internal/certs/cloudflare.go`
- Test: `internal/certs/account_test.go`, extend `internal/certs/renew_test.go`

**Interfaces:**
- Consumes: existing `certs.New(Config)`, `certs.Manager.Obtain`.
- Produces:
  - `func NotAfter(certPEM []byte) (time.Time, error)`
  - `func LoadOrCreateAccountKey(path string) (*ecdsa.PrivateKey, error)` — 0600 PEM file, stable across calls.
  - `func NewCloudflareIssuer(email, caDirURL, token string, accountKey *ecdsa.PrivateKey) (*Manager, error)` — DNS creds passed explicitly, not read from env.

- [ ] **Step 1: Write the failing tests**

Create `internal/certs/account_test.go`:

```go
package certs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateAccountKeyIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acme_account.key")
	k1, err := LoadOrCreateAccountKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perms = %v, want 0600", fi.Mode().Perm())
	}
	k2, err := LoadOrCreateAccountKey(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if k1.D.Cmp(k2.D) != 0 {
		t.Fatal("second load returned a different key")
	}
}

func TestNewCloudflareIssuerRequiresToken(t *testing.T) {
	key, err := LoadOrCreateAccountKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCloudflareIssuer("a@b.c", "", "", key); err == nil {
		t.Fatal("empty token: want error")
	}
}
```

Append to `internal/certs/renew_test.go` (it already builds a self-signed PEM in its existing test — reuse its helper if present; otherwise generate inline as below):

```go
func TestNotAfter(t *testing.T) {
	want := time.Now().Add(48 * time.Hour).Truncate(time.Second).UTC()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     want,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	got, err := NotAfter(certPEM)
	if err != nil {
		t.Fatalf("NotAfter: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("NotAfter = %v, want %v", got, want)
	}
	if _, err := NotAfter([]byte("not pem")); err == nil {
		t.Fatal("garbage input: want error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/certs/ -v`
Expected: FAIL — `LoadOrCreateAccountKey`/`NewCloudflareIssuer`/`NotAfter` undefined.

- [ ] **Step 3: Write the implementation**

Append to `internal/certs/renew.go`:

```go
// NotAfter returns the leaf certificate's expiry.
func NotAfter(certPEM []byte) (time.Time, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return time.Time{}, fmt.Errorf("no PEM block in cert")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse cert: %w", err)
	}
	return crt.NotAfter, nil
}
```

Create `internal/certs/account.go`:

```go
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// LoadOrCreateAccountKey returns the ACME account key persisted at path,
// generating and saving (0600) a new P-256 key when the file is absent.
// Persisting it keeps retries and renewals on one Let's Encrypt account
// instead of registering a fresh one per run.
func LoadOrCreateAccountKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("no PEM block in %s", path)
		}
		return x509.ParseECPrivateKey(block.Bytes)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	data = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
```

Create `internal/certs/cloudflare.go`:

```go
package certs

import (
	"crypto/ecdsa"
	"fmt"

	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
)

// NewCloudflareIssuer builds a Manager whose DNS-01 challenges use the given
// Cloudflare API token explicitly (the API-managed domain path), instead of
// lego's env-var lookup.
func NewCloudflareIssuer(email, caDirURL, token string, accountKey *ecdsa.PrivateKey) (*Manager, error) {
	if token == "" {
		return nil, fmt.Errorf("cloudflare: empty API token")
	}
	cfg := cloudflare.NewDefaultConfig()
	cfg.AuthToken = token
	provider, err := cloudflare.NewDNSProviderConfig(cfg)
	if err != nil {
		return nil, err
	}
	return New(Config{Email: email, CADirURL: caDirURL, DNSProvider: provider, AccountKey: accountKey})
}
```

Note: the empty-token guard returns before any network I/O, so the test passes offline. (`certs.New` performs ACME registration, so `NewCloudflareIssuer`'s happy path is only exercised by e2e/manual runs.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/certs/ -v`
Expected: PASS.

- [ ] **Step 5: `make verify`, then commit**

```bash
git add internal/certs/
git commit -m "feat(certs): persisted ACME account key + token-configured cloudflare issuer

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: `caddy` — runtime HTTPS enablement (`piper-tls` server) + TLS routes

**Why a second server:** `tls_connection_policies` applies to a whole Caddy server, all listeners. The existing `piper` server must keep serving plaintext HTTP on :80 for relay-terminated traffic, so the custom domain gets its own `piper-tls` server on the HTTPS listen. (The env-BYO startup path via `WithHTTPS` is untouched.)

**Files:**
- Modify: `internal/caddy/client.go`
- Test: `internal/caddy/client_test.go` (fake-admin tests), `internal/caddy/manager_test.go` (one real end-to-end TLS test)

**Interfaces:**
- Consumes: existing `NewClient`, `StartManager`, `LoadCert`, `ReplaceCert`, `routeID`.
- Produces:
  - `func (c *Client) EnsureHTTPS(listen string) error` — idempotent; creates the `tls` app (empty `load_pem`) if absent and a `piper-tls` server (`listen: [listen]`, empty routes, automatic_https disabled, `tls_connection_policies: [{}]`) if absent.
  - `func (c *Client) UpsertRouteTLS(host string, upstreamHostPort int) error` — like `UpsertRoute` but appends to `piper-tls`'s routes. `RemoveRoute` already works for both (id-addressed).

- [ ] **Step 1: Write the failing fake-admin test**

Append to `internal/caddy/client_test.go`:

```go
func TestEnsureHTTPSCreatesTLSAppAndServer(t *testing.T) {
	var puts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/config/apps/":
			// no tls app, no piper-tls server yet
			io.WriteString(w, `{"http":{"servers":{"piper":{"listen":[":80"]}}}}`)
		case r.Method == http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			puts = append(puts, r.URL.Path+" "+string(b))
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	if err := NewClient(ts.URL).EnsureHTTPS(":8443"); err != nil {
		t.Fatalf("EnsureHTTPS: %v", err)
	}
	if len(puts) != 2 {
		t.Fatalf("puts = %v, want tls app + piper-tls server", puts)
	}
	if !strings.Contains(puts[0], "/config/apps/tls ") || !strings.Contains(puts[0], "load_pem") {
		t.Fatalf("first put = %q, want tls app", puts[0])
	}
	if !strings.Contains(puts[1], "/config/apps/http/servers/piper-tls ") ||
		!strings.Contains(puts[1], `":8443"`) ||
		!strings.Contains(puts[1], "tls_connection_policies") {
		t.Fatalf("second put = %q, want piper-tls server on :8443", puts[1])
	}
}

func TestEnsureHTTPSIsIdempotent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/config/apps/" {
			io.WriteString(w, `{"http":{"servers":{"piper":{},"piper-tls":{}}},"tls":{}}`)
			return
		}
		t.Errorf("unexpected write %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	if err := NewClient(ts.URL).EnsureHTTPS(":8443"); err != nil {
		t.Fatalf("EnsureHTTPS: %v", err)
	}
}

func TestUpsertRouteTLSTargetsTLSServer(t *testing.T) {
	var postPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound) // no prior route
			return
		}
		if r.Method == http.MethodPost {
			postPath = r.URL.Path
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if err := NewClient(ts.URL).UpsertRouteTLS("blog.example.com", 40001); err != nil {
		t.Fatalf("UpsertRouteTLS: %v", err)
	}
	if postPath != "/config/apps/http/servers/piper-tls/routes" {
		t.Fatalf("posted to %q, want piper-tls routes", postPath)
	}
}
```

Add `"strings"` to the test file imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/caddy/ -run 'TestEnsureHTTPS|TestUpsertRouteTLS' -v`
Expected: FAIL — `EnsureHTTPS`/`UpsertRouteTLS` undefined.

- [ ] **Step 3: Write the implementation**

In `internal/caddy/client.go`, refactor `UpsertRoute` to a shared helper and add the new methods:

```go
// UpsertRoute makes Caddy reverse-proxy host to 127.0.0.1:<port> over HTTP.
// The route carries a stable @id so re-deploys replace it: existing route is
// removed by id (404 ignored), then a fresh one is appended.
func (c *Client) UpsertRoute(host string, upstreamHostPort int) error {
	return c.upsertRoute("piper", host, upstreamHostPort)
}

// UpsertRouteTLS is UpsertRoute for the piper-tls server — the runtime-enabled
// HTTPS listener that serves the BYO custom domain (see EnsureHTTPS).
func (c *Client) UpsertRouteTLS(host string, upstreamHostPort int) error {
	return c.upsertRoute("piper-tls", host, upstreamHostPort)
}

func (c *Client) upsertRoute(server, host string, upstreamHostPort int) error {
	route := map[string]any{
		"@id":   routeID(host),
		"match": []map[string]any{{"host": []string{host}}},
		"handle": []map[string]any{{
			"handler":   "reverse_proxy",
			"upstreams": []map[string]any{{"dial": fmt.Sprintf("127.0.0.1:%d", upstreamHostPort)}},
		}},
	}
	if err := c.RemoveRoute(host); err != nil {
		return err
	}
	return c.write(http.MethodPost, "/config/apps/http/servers/"+server+"/routes", route)
}

// EnsureHTTPS arms TLS serving at runtime for managers started HTTP-only:
// it creates the tls app (empty load_pem) and a dedicated "piper-tls" server
// on listen. A separate server, because tls_connection_policies applies to a
// whole server — the "piper" server must keep speaking plaintext on :80 for
// relay-terminated traffic. Idempotent.
func (c *Client) EnsureHTTPS(listen string) error {
	resp, err := c.http.Get(c.base + "/config/apps/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var apps struct {
		TLS  json.RawMessage `json:"tls"`
		HTTP struct {
			Servers map[string]json.RawMessage `json:"servers"`
		} `json:"http"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return fmt.Errorf("caddy read config: %w", err)
	}
	if len(apps.TLS) == 0 || string(apps.TLS) == "null" {
		tlsApp := map[string]any{"certificates": map[string]any{"load_pem": []any{}}}
		if err := c.write(http.MethodPut, "/config/apps/tls", tlsApp); err != nil {
			return err
		}
	}
	if _, ok := apps.HTTP.Servers["piper-tls"]; !ok {
		srv := map[string]any{
			"listen":                  []string{listen},
			"routes":                  []any{},
			"automatic_https":         map[string]any{"disable": true},
			"tls_connection_policies": []any{map[string]any{}},
		}
		if err := c.write(http.MethodPut, "/config/apps/http/servers/piper-tls", srv); err != nil {
			return err
		}
	}
	return nil
}

// write sends a JSON body to the admin API and errors on non-2xx.
func (c *Client) write(method, path string, body any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, c.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("caddy %s %s: status %d", method, path, resp.StatusCode)
	}
	return nil
}
```

- [ ] **Step 4: Run the fake-admin tests**

Run: `go test ./internal/caddy/ -v`
Expected: PASS.

- [ ] **Step 5: Add a real embedded-Caddy coexistence test**

Append to `internal/caddy/manager_test.go` (match its existing admin/listen port choices; the ports below assume the file uses `http://127.0.0.1:2999` style fixed test ports — pick unused ones consistent with the file):

```go
// EnsureHTTPS at runtime must leave :80-style plaintext serving intact while
// the new piper-tls server terminates TLS with a load_pem cert (coexistence).
func TestEnsureHTTPSServesTLSAlongsidePlaintext(t *testing.T) {
	admin := "http://127.0.0.1:2996"
	m, err := StartManager(admin, "127.0.0.1:8097")
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	defer m.Stop()

	c := NewClient(admin)
	if err := c.EnsureHTTPS("127.0.0.1:8497"); err != nil {
		t.Fatalf("EnsureHTTPS: %v", err)
	}
	// Idempotent second call.
	if err := c.EnsureHTTPS("127.0.0.1:8497"); err != nil {
		t.Fatalf("EnsureHTTPS twice: %v", err)
	}

	certPEM, keyPEM := selfSignedPEM(t, "shop.example.com")
	if err := c.ReplaceCert(string(certPEM), string(keyPEM)); err != nil {
		t.Fatalf("ReplaceCert: %v", err)
	}

	// Plaintext HTTP on the original listener still answers.
	resp, err := http.Get("http://127.0.0.1:8097/")
	if err != nil {
		t.Fatalf("plaintext GET after EnsureHTTPS: %v", err)
	}
	resp.Body.Close()

	// TLS handshake on the new listener serves the loaded cert.
	conn, err := tls.Dial("tcp", "127.0.0.1:8497", &tls.Config{
		ServerName: "blog.shop.example.com", InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("TLS dial piper-tls: %v", err)
	}
	conn.Close()
}

func selfSignedPEM(t *testing.T, base string) (certPEM, keyPEM []byte) {
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
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM
}
```

Add the needed imports (`crypto/ecdsa`, `crypto/elliptic`, `crypto/rand`, `crypto/tls`, `crypto/x509`, `crypto/x509/pkix`, `encoding/pem`, `math/big`). If `StartManager` in this test file conflicts with other tests over the process-global Caddy, follow the file's existing sequencing pattern (each test stops its manager).

- [ ] **Step 6: Run the full caddy package**

Run: `go test ./internal/caddy/ -v`
Expected: PASS.

- [ ] **Step 7: `make verify`, then commit**

```bash
git add internal/caddy/
git commit -m "feat(proxy): runtime HTTPS enablement (piper-tls server) + TLS routes

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: `tunnel` + `agent` — `set-domain` control op

**Files:**
- Modify: `internal/tunnel/tunnel.go` (ControlRequest field + op comment)
- Modify: `internal/agent/tunnelclient.go`
- Test: `internal/agent/tunnelclient_test.go`

**Interfaces:**
- Consumes: `tunnel.ControlRequest`, `TunnelClient.control` (existing unexported helper).
- Produces:
  - `tunnel.ControlRequest.Domain string` (json `domain,omitempty`), op string `"set-domain"`.
  - `func (c *TunnelClient) SetCustomDomain(domain string) error` — empty string clears.

- [ ] **Step 1: Write the failing test**

Append to `internal/agent/tunnelclient_test.go` (mirrors `TestTunnelClientProvision`):

```go
func TestTunnelClientSetCustomDomain(t *testing.T) {
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

	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err = c.SetCustomDomain("shop.example.com"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("SetCustomDomain: %v", err)
	}
	req := <-got
	if req.Op != "set-domain" || req.Domain != "shop.example.com" {
		t.Fatalf("relay saw %+v, want op=set-domain domain=shop.example.com", req)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestTunnelClientSetCustomDomain -v`
Expected: FAIL — `SetCustomDomain` undefined.

- [ ] **Step 3: Implement**

In `internal/tunnel/tunnel.go`, extend `ControlRequest`:

```go
// ControlRequest is an agent→relay control message on a KindControl stream.
type ControlRequest struct {
	Op       string `json:"op"` // "register" | "deregister" | "provision" | "set-domain"
	App      string `json:"app,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Token    string `json:"token,omitempty"`  // "provision": the box's control-API bearer for the relay to inject
	Domain   string `json:"domain,omitempty"` // "set-domain": the BYO custom domain; empty clears
}
```

In `internal/agent/tunnelclient.go`, after `Provision`:

```go
// SetCustomDomain tells the relay to splice SNI for domain (and subdomains)
// down this tunnel as passthrough. Empty domain clears the mapping. It rides
// the authenticated session, so it can only ever set this agent's domain.
func (c *TunnelClient) SetCustomDomain(domain string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "set-domain", Domain: domain})
	return err
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/agent/ ./internal/tunnel/ -v`
Expected: PASS.

- [ ] **Step 5: `make verify`, then commit**

```bash
git add internal/tunnel/ internal/agent/
git commit -m "feat(agent): set-domain tunnel control op

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: `relay` — custom-domain record, routing, control dispatch

**Files:**
- Modify: `internal/relay/store.go`, `internal/relay/router.go`, `internal/relay/server.go`
- Test: `internal/relay/store_test.go`, `internal/relay/router_test.go`, `internal/relay/server_test.go`

**Interfaces:**
- Consumes: `ensureAgentColumn`, `startTestRelay(t, tlsCfg, ctrl)` test helper, `tunnel.ControlRequest.Domain` (Task 4).
- Produces:
  - `var ErrDomainTaken = errors.New("domain already in use")`
  - `func (s *Store) SetCustomDomain(baseDomain, domain string) (previous string, err error)` — uniqueness across agents; `ErrBadToken` for unknown agent; empty domain clears.
  - `func (s *Store) CustomDomain(baseDomain string) (string, error)`
  - `func (r *Router) RegisterCustom(domain string, sess *tunnel.Session)` / `UnregisterCustom(domain string)`
  - `serveControl` handles op `"set-domain"`; `acceptTunnels` re-binds the stored custom domain on session registration; `Router.Unregister` sweeps custom entries.

- [ ] **Step 1: Write the failing store test**

Append to `internal/relay/store_test.go`:

```go
func TestSetCustomDomain(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Enroll("alice", "alice.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enroll("bob", "bob.example.com"); err != nil {
		t.Fatal(err)
	}

	prev, err := st.SetCustomDomain("alice.example.com", "shop.dev")
	if err != nil || prev != "" {
		t.Fatalf("first set = %q, %v", prev, err)
	}
	got, err := st.CustomDomain("alice.example.com")
	if err != nil || got != "shop.dev" {
		t.Fatalf("CustomDomain = %q, %v", got, err)
	}

	// Uniqueness: bob may not claim alice's domain.
	if _, err := st.SetCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("bob claiming shop.dev: err = %v, want ErrDomainTaken", err)
	}
	// Re-setting your own domain is fine.
	if prev, err := st.SetCustomDomain("alice.example.com", "shop.dev"); err != nil || prev != "shop.dev" {
		t.Fatalf("re-set = %q, %v", prev, err)
	}
	// Clearing frees it for others.
	if _, err := st.SetCustomDomain("alice.example.com", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := st.SetCustomDomain("bob.example.com", "shop.dev"); err != nil {
		t.Fatalf("bob after clear: %v", err)
	}
	// Unknown agent.
	if _, err := st.SetCustomDomain("nobody.example.com", "x.dev"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("unknown agent: err = %v, want ErrBadToken", err)
	}
}
```

(Add `"errors"` to imports if missing.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/relay/ -run TestSetCustomDomain -v`
Expected: FAIL — `SetCustomDomain` undefined.

- [ ] **Step 3: Implement the store methods**

In `internal/relay/store.go`: add `"custom_domain"` to the `ensureAgentColumn` loop in `Open`:

```go
	for _, col := range []string{"account_id", "control_token", "custom_domain"} {
```

Append:

```go
// ErrDomainTaken is returned when another agent already holds a custom domain.
var ErrDomainTaken = errors.New("domain already in use")

// SetCustomDomain records domain as the BYO custom domain for the agent
// enrolled at baseDomain and returns the previous value. Empty domain clears.
// First-come-first-served across agents (ErrDomainTaken); the real ownership
// proof is the DNS-01 cert the box obtained before asking.
func (s *Store) SetCustomDomain(baseDomain, domain string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if domain != "" {
		var other string
		err := tx.QueryRow(
			`SELECT base_domain FROM agents WHERE custom_domain=? AND base_domain!=?`,
			domain, baseDomain).Scan(&other)
		if err == nil {
			return "", ErrDomainTaken
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	var prev sql.NullString
	err = tx.QueryRow(`SELECT custom_domain FROM agents WHERE base_domain=?`, baseDomain).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadToken
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE agents SET custom_domain=? WHERE base_domain=?`, domain, baseDomain); err != nil {
		return "", err
	}
	return prev.String, tx.Commit()
}

// CustomDomain returns the agent's BYO custom domain, "" if none is set.
func (s *Store) CustomDomain(baseDomain string) (string, error) {
	var d sql.NullString
	err := s.db.QueryRow(`SELECT custom_domain FROM agents WHERE base_domain=?`, baseDomain).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadToken
	}
	if err != nil {
		return "", err
	}
	return d.String, nil
}
```

Run: `go test ./internal/relay/ -run 'TestSetCustomDomain' -v` — expected PASS.

- [ ] **Step 4: Write the failing router test**

Append to `internal/relay/router_test.go`:

```go
func TestRouterCustomDomain(t *testing.T) {
	r := NewRouter()
	sess := &tunnel.Session{BaseDomain: "alice.example.com"}
	r.Register(sess)
	r.RegisterCustom("shop.dev", sess)

	if s, ok := r.Lookup("blog.shop.dev"); !ok || s != sess {
		t.Fatal("subdomain of custom domain should route to the session")
	}
	if s, ok := r.Lookup("shop.dev"); !ok || s != sess {
		t.Fatal("custom apex should route to the session")
	}

	r.UnregisterCustom("shop.dev")
	if _, ok := r.Lookup("blog.shop.dev"); ok {
		t.Fatal("custom domain should be gone after UnregisterCustom")
	}

	// Unregister(sess) sweeps custom entries too.
	r.RegisterCustom("shop.dev", sess)
	r.Unregister(sess)
	if _, ok := r.Lookup("blog.shop.dev"); ok {
		t.Fatal("custom domain should be swept by Unregister")
	}
	if _, ok := r.Lookup("x.alice.example.com"); ok {
		t.Fatal("base domain should be swept by Unregister")
	}
}
```

Run: `go test ./internal/relay/ -run TestRouterCustomDomain -v` — expected FAIL (`RegisterCustom` undefined).

- [ ] **Step 5: Implement the router changes**

In `internal/relay/router.go`, add methods and make `Unregister` sweep `byBase` by session:

```go
// RegisterCustom maps a BYO custom domain to sess. It shares byBase so
// Lookup's exact + subdomain matching applies unchanged.
func (r *Router) RegisterCustom(domain string, sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byBase[domain] = sess
}

// UnregisterCustom removes a custom-domain mapping.
func (r *Router) UnregisterCustom(domain string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byBase, domain)
}
```

Replace the body of `Unregister` so the byBase sweep is session-wide (custom domains included):

```go
func (r *Router) Unregister(sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for base, s := range r.byBase {
		if s == sess {
			delete(r.byBase, base)
		}
	}
	for host, s := range r.byHost {
		if s == sess {
			delete(r.byHost, host)
		}
	}
}
```

Run: `go test ./internal/relay/ -run TestRouterCustomDomain -v` — expected PASS.

- [ ] **Step 6: Write the failing server test (control dispatch + reconnect rebind)**

Append to `internal/relay/server_test.go` (uses the file's `startTestRelay` helper; open a control stream the same way its provision test does):

```go
func TestSetDomainControlOp(t *testing.T) {
	sess, _, base, st := startTestRelay(t, nil, nil)

	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "set-domain", Domain: "shop.dev"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	cs.Close()
	if resp.Error != "" {
		t.Fatalf("set-domain error: %s", resp.Error)
	}
	if got, _ := st.CustomDomain(base); got != "shop.dev" {
		t.Fatalf("stored custom domain = %q", got)
	}
}
```

Note: `startTestRelay` returns the *agent-side* session; adapt to the helper's actual return values (agent session, TLS addr, base domain, store) and how its sibling tests drive control ops — copy the provision test's structure exactly.

- [ ] **Step 7: Run to verify it fails**

Run: `go test ./internal/relay/ -run TestSetDomainControlOp -v`
Expected: FAIL — relay answers `unknown op`.

- [ ] **Step 8: Implement server dispatch + reconnect rebind**

In `internal/relay/server.go` `handleControl`, add a case before `default`:

```go
	case "set-domain":
		// BYO custom domain (#102). Rides the authenticated session, so it can
		// only ever set the session agent's own domain. The box already proved
		// domain control by obtaining its DNS-01 cert before asking.
		prev, err := st.SetCustomDomain(sess.BaseDomain, req.Domain)
		if err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		if prev != "" && prev != req.Domain {
			router.UnregisterCustom(prev)
		}
		if req.Domain != "" {
			router.RegisterCustom(req.Domain, sess)
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
```

In `acceptTunnels`, after `router.Register(sess)`, re-derive the mapping so a reconnecting agent keeps its custom domain without a new set-domain call:

```go
			router.Register(sess)
			if cd, err := st.CustomDomain(sess.BaseDomain); err == nil && cd != "" {
				router.RegisterCustom(cd, sess)
			}
```

- [ ] **Step 9: Run the relay package**

Run: `go test ./internal/relay/ -v`
Expected: PASS.

- [ ] **Step 10: `make verify`, then commit**

```bash
git add internal/relay/
git commit -m "feat(relay): custom-domain record, SNI routing, set-domain control op

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: `internal/domain` — Manager: Set → issue → activate

**Files:**
- Create: `internal/domain/domain.go`
- Test: `internal/domain/domain_test.go`

**Interfaces:**
- Consumes: `store.Store` concrete (`GetDomainConfig`, `SetDomainConfig`, `UpdateDomainStatus`, `DeleteDomainConfig`, `ListApps`, `LatestRunning`), `certs.NotAfter`, `certs.NeedsRenewal`.
- Produces (used by Tasks 7–11):
  - `const StatusIssuing = "issuing"; StatusActive = "active"; StatusFailed = "failed"`
  - `var ErrEnvManaged, ErrInvalidDomain, ErrUnsupportedProvider, ErrTokenRequired error`
  - `type Issuer interface { Obtain(domains []string) (certPEM, keyPEM []byte, err error) }`
  - `type IssuerFactory func(provider, token string) (Issuer, error)`
  - `type Proxy interface { EnsureHTTPS(listen string) error; ReplaceCert(certPEM, keyPEM string) error; UpsertRouteTLS(host string, upstreamHostPort int) error; RemoveRoute(host string) error }` (satisfied by `*caddy.Client`)
  - `type RelayNotifier interface { SetCustomDomain(domain string) error }` (satisfied by `*agent.TunnelClient`)
  - `type Options struct { Store *store.Store; Issuer IssuerFactory; Proxy Proxy; DataDir, RelayHost, HTTPSListen, EnvDomain string }`
  - `func New(o Options) *Manager`, `func (m *Manager) SetRelay(r RelayNotifier)`
  - `func (m *Manager) Set(domainName, provider, token string) (Status, error)`
  - `type Status struct` / `type DNSRecord struct` (wire shapes, JSON tags below)

- [ ] **Step 1: Write the failing test**

Create `internal/domain/domain_test.go`:

```go
package domain

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/store"
)

// selfSignedPEM issues a throwaway wildcard cert so the fake issuer's output
// parses (activation reads NotAfter and hostname coverage from it).
func selfSignedPEM(t *testing.T, notAfter time.Time, domains ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM
}

type fakeIssuer struct {
	mu       sync.Mutex
	calls    int
	failures int // fail the first N Obtain calls
	notAfter time.Time
}

func (f *fakeIssuer) Obtain(domains []string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failures {
		return nil, nil, errors.New("acme: boom")
	}
	na := f.notAfter
	if na.IsZero() {
		na = time.Now().Add(90 * 24 * time.Hour)
	}
	c, k := selfSignedPEMForObtain(na, domains)
	return c, k, nil
}

// selfSignedPEMForObtain is selfSignedPEM without *testing.T (Obtain has none).
func selfSignedPEMForObtain(notAfter time.Time, domains []string) ([]byte, []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM
}

func (f *fakeIssuer) obtainCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeProxy struct {
	mu       sync.Mutex
	ensured  []string
	replaced int
	routes   map[string]int
	removed  []string
}

func (f *fakeProxy) EnsureHTTPS(listen string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, listen)
	return nil
}
func (f *fakeProxy) ReplaceCert(cert, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaced++
	return nil
}
func (f *fakeProxy) UpsertRouteTLS(host string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.routes == nil {
		f.routes = map[string]int{}
	}
	f.routes[host] = port
	return nil
}
func (f *fakeProxy) RemoveRoute(host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, host)
	return nil
}
func (f *fakeProxy) route(host string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.routes[host]
	return p, ok
}

type fakeNotifier struct {
	mu   sync.Mutex
	got  []string
	fail error
}

func (f *fakeNotifier) SetCustomDomain(d string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, d)
	return nil
}
func (f *fakeNotifier) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		return ""
	}
	return f.got[len(f.got)-1]
}

func newTestManager(t *testing.T, iss *fakeIssuer) (*Manager, *store.Store, *fakeProxy, *fakeNotifier, string) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	proxy := &fakeProxy{}
	relay := &fakeNotifier{}
	m := New(Options{
		Store:  st,
		Issuer: func(provider, token string) (Issuer, error) { return iss, nil },
		Proxy:  proxy, DataDir: dataDir,
		RelayHost: "relay.example.net", HTTPSListen: ":8443",
	})
	m.SetRelay(relay)
	m.retryDelay = func(int) time.Duration { return time.Millisecond }
	return m, st, proxy, relay, dataDir
}

// waitStatus polls the store until the domain config reaches status (2s cap).
func waitStatus(t *testing.T, st *store.Store, status string) store.DomainConfig {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var dc store.DomainConfig
	var err error
	for time.Now().Before(deadline) {
		dc, err = st.GetDomainConfig()
		if err == nil && dc.Status == status {
			return dc
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("status never reached %q (last: %+v, err %v)", status, dc, err)
	return dc
}

func TestSetIssuesAndActivates(t *testing.T) {
	m, st, proxy, relay, dataDir := newTestManager(t, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment("blog", "img", "ctr", 40001, "running", ""); err != nil {
		t.Fatal(err)
	}

	status, err := m.Set("Example.COM", "cloudflare", "cf-token")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if status.Domain != "example.com" || status.Status != StatusIssuing {
		t.Fatalf("Set returned %+v", status)
	}

	dc := waitStatus(t, st, StatusActive)
	if dc.CertNotAfter.IsZero() {
		t.Fatal("active config has zero cert_not_after")
	}
	if got := relay.last(); got != "example.com" {
		t.Fatalf("relay notified with %q", got)
	}
	if p, ok := proxy.route("blog.example.com"); !ok || p != 40001 {
		t.Fatalf("blog route = %d,%v; want 40001 on blog.example.com", p, ok)
	}
	proxy.mu.Lock()
	ensured, replaced := proxy.ensured, proxy.replaced
	proxy.mu.Unlock()
	if len(ensured) == 0 || ensured[0] != ":8443" || replaced == 0 {
		t.Fatalf("proxy calls: ensured=%v replaced=%d", ensured, replaced)
	}
	for _, f := range []string{"cert.pem", "key.pem"} {
		fi, err := os.Stat(filepath.Join(dataDir, "domain", f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s perms = %v, want 0600", f, fi.Mode().Perm())
		}
	}
}

func TestSetValidation(t *testing.T) {
	m, _, _, _, _ := newTestManager(t, &fakeIssuer{})
	if _, err := m.Set("not a domain", "cloudflare", "tok"); !errors.Is(err, ErrInvalidDomain) {
		t.Fatalf("bad domain: %v", err)
	}
	if _, err := m.Set("ok.example.com", "route53", "tok"); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("bad provider: %v", err)
	}
	if _, err := m.Set("ok.example.com", "cloudflare", ""); !errors.Is(err, ErrTokenRequired) {
		t.Fatalf("empty token: %v", err)
	}
}

func TestIssueFailureRecordsFailedThenRetriesToActive(t *testing.T) {
	iss := &fakeIssuer{failures: 2}
	m, st, _, _, _ := newTestManager(t, iss)
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitStatus(t, st, StatusActive)
	if dc.Error != "" {
		t.Fatalf("active config still carries error %q", dc.Error)
	}
	if iss.obtainCalls() < 3 {
		t.Fatalf("obtain calls = %d, want ≥3 (2 failures + success)", iss.obtainCalls())
	}
}

func TestRelayRejectionSurfacesAsFailed(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, _, relay, _ := newTestManager(t, iss)
	relay.fail = errors.New("domain already in use")
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitStatus(t, st, StatusFailed)
	if dc.Error != "domain already in use" {
		t.Fatalf("error = %q", dc.Error)
	}
	// Retries must reuse the disk cert instead of re-obtaining (rate limits).
	before := iss.obtainCalls()
	time.Sleep(50 * time.Millisecond)
	if after := iss.obtainCalls(); after != before {
		t.Fatalf("obtain calls grew %d→%d during relay-only retries", before, after)
	}
	// Unblock the relay; the loop must converge to active.
	relay.mu.Lock()
	relay.fail = nil
	relay.mu.Unlock()
	waitStatus(t, st, StatusActive)
}

func TestSetEnvManaged(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(Options{Store: st, Proxy: &fakeProxy{}, EnvDomain: "env.example.com",
		Issuer: func(string, string) (Issuer, error) { return nil, errors.New("unused") }})
	if _, err := m.Set("x.dev", "cloudflare", "tok"); !errors.Is(err, ErrEnvManaged) {
		t.Fatalf("Set on env-managed: %v", err)
	}
	if err := m.Remove(); !errors.Is(err, ErrEnvManaged) {
		t.Fatalf("Remove on env-managed: %v", err)
	}
}
```

(`TestSetEnvManaged` references `Remove` — its minimal implementation is included in this task's Step 3; Task 7 adds the teardown-behavior tests.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/ -v`
Expected: FAIL to build — package doesn't exist yet.

- [ ] **Step 3: Write the implementation**

Create `internal/domain/domain.go`:

```go
// Package domain manages the box's BYO custom domain: cert issuance via ACME
// DNS-01, live activation in Caddy, relay routing, renewal, and teardown. It
// orchestrates certs/caddy/tunnel through interfaces (the deploy pattern) so
// it unit-tests with fakes. See docs/superpowers/specs/2026-07-10-domain-config-api-design.md.
package domain

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/piperbox/piper/internal/certs"
	"github.com/piperbox/piper/internal/store"
)

const (
	StatusIssuing = "issuing"
	StatusActive  = "active"
	StatusFailed  = "failed"
)

var (
	ErrEnvManaged          = errors.New("domain config is env-managed (PIPER_BASE_DOMAIN); unset it to manage via the API")
	ErrInvalidDomain       = errors.New("invalid domain")
	ErrUnsupportedProvider = errors.New("unsupported dns provider")
	ErrTokenRequired       = errors.New("dns_token required")
)

// Issuer obtains one PEM cert/key covering domains. *certs.Manager satisfies it.
type Issuer interface {
	Obtain(domains []string) (certPEM, keyPEM []byte, err error)
}

// IssuerFactory builds an Issuer for one issuance run from the stored DNS
// creds. Construction registers the ACME account, so it is deferred to
// issuance time rather than Manager construction.
type IssuerFactory func(provider, token string) (Issuer, error)

// Proxy is the caddy slice the Manager drives. *caddy.Client satisfies it.
type Proxy interface {
	EnsureHTTPS(listen string) error
	ReplaceCert(certPEM, keyPEM string) error
	UpsertRouteTLS(host string, upstreamHostPort int) error
	RemoveRoute(host string) error
}

// RelayNotifier pushes the custom domain to the relay over the tunnel.
// *agent.TunnelClient satisfies it.
type RelayNotifier interface {
	SetCustomDomain(domain string) error
}

// Options wires a Manager. EnvDomain non-empty means domain config is
// env-managed (the pre-#102 PIPER_BASE_DOMAIN BYO path): API writes are
// rejected and Status reports source "env".
type Options struct {
	Store       *store.Store
	Issuer      IssuerFactory
	Proxy       Proxy
	DataDir     string
	RelayHost   string // host part of the relay address; the DNS-record target
	HTTPSListen string // e.g. ":443"
	EnvDomain   string
}

// Manager owns the custom-domain lifecycle: issuing → active/failed,
// persisted in the store so the dashboard can poll it and restarts resume it.
type Manager struct {
	st          *store.Store
	newIssuer   IssuerFactory
	proxy       Proxy
	dataDir     string
	relayHost   string
	httpsListen string
	envDomain   string

	relayMu sync.Mutex
	relay   RelayNotifier

	issueMu sync.Mutex // serializes issuance/renewal runs

	// test seams
	retryDelay func(attempt int) time.Duration
	resolve    func(ctx context.Context, host string) ([]net.IP, error)
}

func New(o Options) *Manager {
	return &Manager{
		st: o.Store, newIssuer: o.Issuer, proxy: o.Proxy,
		dataDir: o.DataDir, relayHost: o.RelayHost,
		httpsListen: o.HTTPSListen, envDomain: o.EnvDomain,
		retryDelay: defaultRetryDelay,
		resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
	}
}

// SetRelay injects the tunnel client once relay mode is up (piperd creates the
// tunnel client after the Manager). Nil is tolerated: activation then skips
// the relay push (LAN/tests).
func (m *Manager) SetRelay(r RelayNotifier) {
	m.relayMu.Lock()
	m.relay = r
	m.relayMu.Unlock()
}

func (m *Manager) notifier() RelayNotifier {
	m.relayMu.Lock()
	defer m.relayMu.Unlock()
	return m.relay
}

// domainRE accepts lowercase dotted DNS names ("shop.example.com").
var domainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*[a-z0-9]$`)

// Set validates and persists a new custom-domain config, then starts issuance
// asynchronously. The returned Status is the freshly-kicked "issuing" state.
func (m *Manager) Set(domainName, provider, token string) (Status, error) {
	if m.envDomain != "" {
		return Status{}, ErrEnvManaged
	}
	d := strings.ToLower(strings.TrimSpace(domainName))
	if !domainRE.MatchString(d) {
		return Status{}, ErrInvalidDomain
	}
	if provider != "cloudflare" {
		return Status{}, ErrUnsupportedProvider
	}
	if token == "" {
		return Status{}, ErrTokenRequired
	}
	// Replacing a different domain tears the old one down first.
	if prev, err := m.st.GetDomainConfig(); err == nil && prev.Domain != d {
		m.teardown(prev)
	}
	if err := m.st.SetDomainConfig(d, provider, token); err != nil {
		return Status{}, err
	}
	go m.issueLoop(d)
	return m.Status()
}

// issueLoop drives one config to activation with capped-backoff retries. It
// exits when the stored config no longer matches domain (replaced or deleted)
// or activation succeeds.
func (m *Manager) issueLoop(domain string) {
	for attempt := 0; ; attempt++ {
		dc, err := m.st.GetDomainConfig()
		if err != nil || dc.Domain != domain || dc.Status == StatusActive {
			return
		}
		if err := m.issueOnce(dc); err == nil {
			return
		} else {
			_ = m.st.UpdateDomainStatus(StatusFailed, err.Error(), time.Time{})
		}
		time.Sleep(m.retryDelay(attempt))
	}
}

// defaultRetryDelay backs off 1m, 2m, 4m, … capped at 1h.
func defaultRetryDelay(attempt int) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	return time.Minute << uint(attempt)
}

// issueOnce obtains (or reuses) the cert, arms Caddy, then tells the relay.
// The disk-cert reuse keeps retries and restarts inside LE rate limits: a
// relay hiccup must not burn a fresh certificate.
func (m *Manager) issueOnce(dc store.DomainConfig) error {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	certPEM, keyPEM, err := m.readCert()
	if err != nil || !certCovers(certPEM, dc.Domain, time.Now()) {
		iss, err := m.newIssuer(dc.DNSProvider, dc.DNSToken)
		if err != nil {
			return err
		}
		certPEM, keyPEM, err = iss.Obtain([]string{"*." + dc.Domain, dc.Domain})
		if err != nil {
			return err
		}
		if err := m.writeCert(certPEM, keyPEM); err != nil {
			return err
		}
	}
	if err := m.arm(dc, certPEM, keyPEM); err != nil {
		return err
	}
	if r := m.notifier(); r != nil {
		if err := r.SetCustomDomain(dc.Domain); err != nil {
			return err
		}
	}
	notAfter, err := certs.NotAfter(certPEM)
	if err != nil {
		return err
	}
	return m.st.UpdateDomainStatus(StatusActive, "", notAfter)
}

// arm loads the cert and app routes into Caddy — the box must answer before
// the relay routes to it. Shared by first activation and restart resume.
func (m *Manager) arm(dc store.DomainConfig, certPEM, keyPEM []byte) error {
	if err := m.proxy.EnsureHTTPS(m.httpsListen); err != nil {
		return err
	}
	if err := m.proxy.ReplaceCert(string(certPEM), string(keyPEM)); err != nil {
		return err
	}
	apps, err := m.st.ListApps()
	if err != nil {
		return err
	}
	for _, a := range apps {
		dep, err := m.st.LatestRunning(a.Name)
		if err != nil {
			continue // never deployed or not running: nothing to route
		}
		if err := m.proxy.UpsertRouteTLS(a.Name+"."+dc.Domain, dep.HostPort); err != nil {
			return err
		}
	}
	return nil
}

// teardown reverses activation: relay mapping first, then routes, then files.
func (m *Manager) teardown(dc store.DomainConfig) {
	if r := m.notifier(); r != nil {
		_ = r.SetCustomDomain("")
	}
	if apps, err := m.st.ListApps(); err == nil {
		for _, a := range apps {
			_ = m.proxy.RemoveRoute(a.Name + "." + dc.Domain)
		}
	}
	_ = os.RemoveAll(filepath.Join(m.dataDir, "domain"))
}

func (m *Manager) certDir() string  { return filepath.Join(m.dataDir, "domain") }
func (m *Manager) certPath() string { return filepath.Join(m.certDir(), "cert.pem") }
func (m *Manager) keyPath() string  { return filepath.Join(m.certDir(), "key.pem") }

func (m *Manager) writeCert(certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(m.certDir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(m.certPath(), certPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(m.keyPath(), keyPEM, 0o600)
}

func (m *Manager) readCert() (certPEM, keyPEM []byte, err error) {
	certPEM, err = os.ReadFile(m.certPath())
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = os.ReadFile(m.keyPath())
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// certCovers reports whether the leaf in certPEM is valid for hosts under
// domain and not expiring within 24h — the disk-cert reuse test.
func certCovers(certPEM []byte, domain string, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if crt.VerifyHostname("piper-probe."+domain) != nil {
		return false
	}
	return now.Add(24 * time.Hour).Before(crt.NotAfter)
}
```

Also add `Status()` and its wire types. This version is complete except `DNSOK`, which Task 7 adds (one line plus the `dnsOK` helper):

```go
// Status assembles the wire state (GET /v1/domain). DNSOK is computed in
// dnsOK (added in Task 7).
func (m *Manager) Status() (Status, error) {
	if m.envDomain != "" {
		return Status{Domain: m.envDomain, Source: "env", Status: StatusActive, DNSRecords: m.dnsRecords(m.envDomain)}, nil
	}
	dc, err := m.st.GetDomainConfig()
	if errors.Is(err, store.ErrNotFound) {
		return Status{Source: "api", DNSRecords: []DNSRecord{}}, nil
	}
	if err != nil {
		return Status{}, err
	}
	st := Status{
		Domain: dc.Domain, DNSProvider: dc.DNSProvider,
		DNSTokenSet: dc.DNSToken != "", Source: "api",
		Status: dc.Status, Error: dc.Error,
		DNSRecords: m.dnsRecords(dc.Domain),
	}
	if !dc.CertNotAfter.IsZero() {
		t := dc.CertNotAfter
		st.CertNotAfter = &t
	}
	return st, nil
}

// DNSRecord is one record the user must create at their DNS host.
type DNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Status is the wire state of the box's custom-domain config. The DNS token
// never appears; DNSTokenSet signals presence.
type Status struct {
	Domain       string      `json:"domain"`
	DNSProvider  string      `json:"dns_provider"`
	DNSTokenSet  bool        `json:"dns_token_set"`
	Source       string      `json:"source"` // "api" | "env"
	Status       string      `json:"status"` // "" | "issuing" | "active" | "failed"
	Error        string      `json:"error"`
	CertNotAfter *time.Time  `json:"cert_not_after,omitempty"`
	DNSRecords   []DNSRecord `json:"dns_records"`
	DNSOK        bool        `json:"dns_ok"`
}

func (m *Manager) dnsRecords(domain string) []DNSRecord {
	return []DNSRecord{
		{Type: "CNAME", Name: "*." + domain, Value: m.relayHost},
		{Type: "CNAME", Name: domain, Value: m.relayHost},
	}
}
```

And a minimal `Remove` (referenced by `TestSetEnvManaged`; full teardown assertions come in Task 7):

```go
// Remove tears down the custom domain. Shared-domain URLs are untouched.
func (m *Manager) Remove() error {
	if m.envDomain != "" {
		return ErrEnvManaged
	}
	dc, err := m.st.GetDomainConfig()
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	m.teardown(dc)
	return m.st.DeleteDomainConfig()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/domain/ -v -race`
Expected: PASS (run with `-race`: the manager is concurrent by design).

- [ ] **Step 5: `make verify`, then commit**

```bash
git add internal/domain/
git commit -m "feat(agent): domain manager — issuance state machine to live activation

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: `internal/domain` — Status (dns_ok) + Remove teardown

**Files:**
- Modify: `internal/domain/domain.go`
- Test: `internal/domain/domain_test.go`

**Interfaces:**
- Produces: `Status()` gains `DNSOK` via the `resolve` seam; `Remove()` behavior verified (relay cleared → routes removed → files gone → row gone).

- [ ] **Step 1: Write the failing tests**

Append to `internal/domain/domain_test.go`:

```go
func TestStatusDNSOK(t *testing.T) {
	m, st, _, _, _ := newTestManager(t, &fakeIssuer{})
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusActive)

	// Probe and relay host resolve to the same address → dns_ok.
	m.resolve = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "piper-probe.example.com", "relay.example.net":
			return []net.IP{net.ParseIP("203.0.113.7")}, nil
		}
		return nil, errors.New("nxdomain")
	}
	s, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !s.DNSOK {
		t.Fatal("want dns_ok=true when probe matches relay")
	}
	if s.DNSTokenSet != true || s.Domain != "example.com" || s.Source != "api" {
		t.Fatalf("status = %+v", s)
	}
	want := []DNSRecord{
		{Type: "CNAME", Name: "*.example.com", Value: "relay.example.net"},
		{Type: "CNAME", Name: "example.com", Value: "relay.example.net"},
	}
	if len(s.DNSRecords) != 2 || s.DNSRecords[0] != want[0] || s.DNSRecords[1] != want[1] {
		t.Fatalf("dns_records = %+v", s.DNSRecords)
	}

	// Probe resolving elsewhere → not ok.
	m.resolve = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "piper-probe.example.com" {
			return []net.IP{net.ParseIP("198.51.100.9")}, nil
		}
		return []net.IP{net.ParseIP("203.0.113.7")}, nil
	}
	s, _ = m.Status()
	if s.DNSOK {
		t.Fatal("want dns_ok=false on mismatch")
	}
}

func TestStatusUnconfigured(t *testing.T) {
	m, _, _, _, _ := newTestManager(t, &fakeIssuer{})
	s, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "" || s.Domain != "" || s.Source != "api" {
		t.Fatalf("unconfigured status = %+v", s)
	}
}

func TestRemoveTearsDown(t *testing.T) {
	m, st, proxy, relay, dataDir := newTestManager(t, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment("blog", "img", "ctr", 40001, "running", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusActive)

	if err := m.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := relay.last(); got != "" {
		t.Fatalf("relay last notify = %q, want cleared", got)
	}
	proxy.mu.Lock()
	removed := append([]string(nil), proxy.removed...)
	proxy.mu.Unlock()
	found := false
	for _, h := range removed {
		found = found || h == "blog.example.com"
	}
	if !found {
		t.Fatalf("removed routes = %v, want blog.example.com", removed)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "domain")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cert dir survives Remove: %v", err)
	}
	if _, err := st.GetDomainConfig(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("config survives Remove: %v", err)
	}
	// Removing again is a no-op.
	if err := m.Remove(); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify the dns_ok test fails**

Run: `go test ./internal/domain/ -run 'TestStatusDNSOK|TestRemoveTearsDown|TestStatusUnconfigured' -v`
Expected: `TestStatusDNSOK` FAILS (no `DNSOK` computation yet); the others may already pass from Task 6's minimal versions.

- [ ] **Step 3: Implement dns_ok**

In `Status()` (API-managed branch), after building `st`, add:

```go
	st.DNSOK = m.dnsOK(dc.Domain)
```

And add:

```go
// dnsOK reports whether a wildcard lookup under domain reaches the relay:
// piper-probe.<domain> (any label matches the user's wildcard record) must
// resolve to an address the relay host also resolves to. Traffic readiness —
// independent of issuance, which needs only the DNS API token.
func (m *Manager) dnsOK(domain string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	probe, err := m.resolve(ctx, "piper-probe."+domain)
	if err != nil {
		return false
	}
	relay, err := m.resolve(ctx, m.relayHost)
	if err != nil {
		return false
	}
	for _, p := range probe {
		for _, r := range relay {
			if p.Equal(r) {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/domain/ -v -race`
Expected: PASS.

- [ ] **Step 5: `make verify`, then commit**

```bash
git add internal/domain/
git commit -m "feat(agent): domain status — dns records, dns_ok probe, teardown

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: `internal/domain` — restart Resume, renewals, env-managed RunEnv

**Files:**
- Modify: `internal/domain/domain.go` (or a new `internal/domain/lifecycle.go` if domain.go passes ~400 lines)
- Test: `internal/domain/domain_test.go`

**Interfaces:**
- Produces (piperd calls these in Task 11):
  - `func (m *Manager) Resume()` — active + valid disk cert → re-arm without re-issuance; issuing/failed or damaged cert → re-enter issue loop.
  - `func (m *Manager) StartRenewals(ctx context.Context)` — blocking loop, run in a goroutine; 12h ticks, 30-day window.
  - `func (m *Manager) RunEnv(ctx context.Context, iss Issuer) error` — env-managed path: initial obtain + load + background renew (replaces piperd's `setupRelayTLS` ACME branch).

- [ ] **Step 1: Write the failing tests**

Append to `internal/domain/domain_test.go`:

```go
func TestResumeActiveReloadsWithoutReissuing(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, proxy, _, _ := newTestManager(t, iss)
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment("blog", "img", "ctr", 40001, "running", ""); err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-restart state: active row + valid disk cert.
	certPEM, keyPEM := selfSignedPEM(t, time.Now().Add(60*24*time.Hour), "*.example.com", "example.com")
	if err := m.writeCert(certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDomainConfig("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateDomainStatus(StatusActive, "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	m.Resume()

	if iss.obtainCalls() != 0 {
		t.Fatalf("Resume re-issued (%d obtains), want disk reload", iss.obtainCalls())
	}
	proxy.mu.Lock()
	replaced := proxy.replaced
	proxy.mu.Unlock()
	if replaced == 0 {
		t.Fatal("Resume did not reload the cert into Caddy")
	}
	if p, ok := proxy.route("blog.example.com"); !ok || p != 40001 {
		t.Fatalf("Resume route = %d,%v", p, ok)
	}
}

func TestResumeDamagedCertReissues(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, _, _, _ := newTestManager(t, iss)
	if err := st.SetDomainConfig("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateDomainStatus(StatusActive, "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.writeCert([]byte("garbage"), []byte("garbage")); err != nil {
		t.Fatal(err)
	}

	m.Resume()

	waitStatus(t, st, StatusActive)
	if iss.obtainCalls() == 0 {
		t.Fatal("damaged disk cert must degrade to re-issuance")
	}
}

func TestRenewCheckReissuesNearExpiry(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, proxy, _, _ := newTestManager(t, iss)
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dcBefore := waitStatus(t, st, StatusActive)
	callsAfterIssue := iss.obtainCalls()
	proxy.mu.Lock()
	replacedAfterIssue := proxy.replaced
	proxy.mu.Unlock()

	// Not due yet: 90-day cert, check "now".
	m.renewCheck(time.Now())
	if iss.obtainCalls() != callsAfterIssue {
		t.Fatal("renewed a cert that is not due")
	}

	// Due: pretend it's 20 days before expiry (inside the 30-day window).
	m.renewCheck(dcBefore.CertNotAfter.Add(-20 * 24 * time.Hour))
	if iss.obtainCalls() != callsAfterIssue+1 {
		t.Fatalf("obtains = %d, want one renewal", iss.obtainCalls())
	}
	proxy.mu.Lock()
	replaced := proxy.replaced
	proxy.mu.Unlock()
	if replaced != replacedAfterIssue+1 {
		t.Fatal("renewal did not swap the cert in Caddy")
	}
	dcAfter, _ := st.GetDomainConfig()
	if !dcAfter.CertNotAfter.After(dcBefore.CertNotAfter) {
		t.Fatal("cert_not_after not advanced by renewal")
	}
}

func TestRenewFailureKeepsServing(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, _, _, _ := newTestManager(t, iss)
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitStatus(t, st, StatusActive)

	iss.mu.Lock()
	iss.failures = iss.calls + 100 // all future obtains fail
	iss.mu.Unlock()
	m.renewCheck(dc.CertNotAfter.Add(-20 * 24 * time.Hour))

	dcAfter, _ := st.GetDomainConfig()
	if dcAfter.Status != StatusActive {
		t.Fatalf("status = %q, want active (old cert still serves)", dcAfter.Status)
	}
	if dcAfter.Error == "" {
		t.Fatal("renewal failure not recorded in error")
	}
}

func TestRunEnvIssuesAndRenews(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	proxy := &fakeProxy{}
	iss := &fakeIssuer{}
	m := New(Options{Store: st, Proxy: proxy, EnvDomain: "env.example.com",
		Issuer: func(string, string) (Issuer, error) { return iss, nil }})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.RunEnv(ctx, iss); err != nil {
		t.Fatalf("RunEnv: %v", err)
	}
	if iss.obtainCalls() != 1 {
		t.Fatalf("obtains = %d, want 1 initial issuance", iss.obtainCalls())
	}
	proxy.mu.Lock()
	replaced := proxy.replaced
	proxy.mu.Unlock()
	if replaced != 1 {
		t.Fatal("initial env cert not loaded")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/domain/ -run 'TestResume|TestRenew|TestRunEnv' -v`
Expected: FAIL — `Resume`/`renewCheck`/`RunEnv` undefined.

- [ ] **Step 3: Implement**

Append to `internal/domain/domain.go`:

```go
const (
	renewInterval = 12 * time.Hour
	renewWindow   = 30 * 24 * time.Hour
)

// Resume restores state after a restart: an active config re-arms from the
// disk cert (no re-issuance); issuing/failed configs — or an active one whose
// disk cert is damaged — re-enter the issue loop. The relay is not re-notified:
// it re-derives the mapping from its own store at session registration.
func (m *Manager) Resume() {
	if m.envDomain != "" {
		return
	}
	dc, err := m.st.GetDomainConfig()
	if err != nil {
		return
	}
	if dc.Status == StatusActive {
		certPEM, keyPEM, err := m.readCert()
		if err == nil && certCovers(certPEM, dc.Domain, time.Now()) {
			if err := m.arm(dc, certPEM, keyPEM); err == nil {
				return
			}
		}
		// Damaged or missing disk cert: degrade to re-issuance, not a crash.
		_ = m.st.UpdateDomainStatus(StatusIssuing, "", time.Time{})
	}
	go m.issueLoop(dc.Domain)
}

// StartRenewals renews the API-managed cert: every renewInterval, when the
// disk cert is within renewWindow of expiry, re-obtain and hot-swap. Blocks
// until ctx ends; run it in a goroutine.
func (m *Manager) StartRenewals(ctx context.Context) {
	t := time.NewTicker(renewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.renewCheck(time.Now())
		}
	}
}

func (m *Manager) renewCheck(now time.Time) {
	dc, err := m.st.GetDomainConfig()
	if err != nil || dc.Status != StatusActive {
		return
	}
	certPEM, _, err := m.readCert()
	if err != nil {
		return
	}
	due, err := certs.NeedsRenewal(certPEM, renewWindow, now)
	if err != nil || !due {
		return
	}
	if err := m.reissue(dc); err != nil {
		// Old cert keeps serving until expiry; surface the error, stay active.
		_ = m.st.UpdateDomainStatus(StatusActive, "renew: "+err.Error(), dc.CertNotAfter)
	}
}

// reissue obtains a fresh cert for an already-active config and swaps it in.
func (m *Manager) reissue(dc store.DomainConfig) error {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	iss, err := m.newIssuer(dc.DNSProvider, dc.DNSToken)
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := iss.Obtain([]string{"*." + dc.Domain, dc.Domain})
	if err != nil {
		return err
	}
	if err := m.writeCert(certPEM, keyPEM); err != nil {
		return err
	}
	if err := m.proxy.ReplaceCert(string(certPEM), string(keyPEM)); err != nil {
		return err
	}
	notAfter, err := certs.NotAfter(certPEM)
	if err != nil {
		return err
	}
	return m.st.UpdateDomainStatus(StatusActive, "", notAfter)
}

// RunEnv drives the env-managed (PIPER_BASE_DOMAIN) BYO path: issue the
// wildcard cert for the env domain now, then renew in the background — the
// fold-in of piperd's former setupRelayTLS/renewLoop pair. The Caddy manager
// was started WithHTTPS in this mode, so no EnsureHTTPS is needed.
func (m *Manager) RunEnv(ctx context.Context, iss Issuer) error {
	certPEM, keyPEM, err := iss.Obtain([]string{"*." + m.envDomain, m.envDomain})
	if err != nil {
		return err
	}
	if err := m.proxy.ReplaceCert(string(certPEM), string(keyPEM)); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(renewInterval)
		defer t.Stop()
		m.runEnvRenew(ctx, iss, certPEM, t.C, time.Now)
	}()
	return nil
}

func (m *Manager) runEnvRenew(ctx context.Context, iss Issuer, certPEM []byte, ticks <-chan time.Time, now func() time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			due, err := certs.NeedsRenewal(certPEM, renewWindow, now())
			if err != nil || !due {
				continue
			}
			newCert, newKey, err := iss.Obtain([]string{"*." + m.envDomain, m.envDomain})
			if err != nil {
				log.Printf("domain: env renew: %v", err)
				continue
			}
			if err := m.proxy.ReplaceCert(string(newCert), string(newKey)); err != nil {
				log.Printf("domain: env renew load: %v", err)
				continue
			}
			certPEM = newCert
		}
	}
}
```

Add `"log"` to the imports.

- [ ] **Step 4: Run the whole package**

Run: `go test ./internal/domain/ -v -race`
Expected: PASS.

- [ ] **Step 5: `make verify`, then commit**

```bash
git add internal/domain/
git commit -m "feat(agent): domain resume, renewal loop, env-managed RunEnv

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 9: `deploy` — route new deploys on the custom domain too

**Files:**
- Modify: `internal/deploy/deploy.go`
- Test: `internal/deploy/deploy_test.go`

**Interfaces:**
- Consumes: `store.GetDomainConfig` (Task 1), `RouteSetter`.
- Produces: `RouteSetter` gains `UpsertRouteTLS(host string, upstreamHostPort int) error`; `Deploy` adds an `<app>.<custom>` TLS route when a custom domain is active. (PR previews stay shared-domain only — the spec scopes dual-hosting to production deploys.)

- [ ] **Step 1: Write the failing test**

Extend the `fakeCaddy` type in `internal/deploy/deploy_test.go` with the new method (match its existing field style — it records upserts; add a parallel record):

```go
func (f *fakeCaddy) UpsertRouteTLS(host string, port int) error {
	f.tlsRoutes = append(f.tlsRoutes, fmt.Sprintf("%s->%d", host, port))
	return nil
}
```

(add field `tlsRoutes []string` to the struct), then append the test:

```go
func TestDeployRoutesCustomDomainWhenActive(t *testing.T) {
	// Arrange a working deploy exactly like the package's happy-path test, then
	// mark a custom domain active and assert the extra TLS route.
	st, rt, caddy, d := newTestDeployer(t) // use the file's existing setup helper/pattern
	if err := st.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateDomainStatus("active", "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	dep := deploySampleApp(t, d) // the file's existing "deploy the fake app" steps

	want := fmt.Sprintf("blog.shop.dev->%d", dep.HostPort)
	found := false
	for _, r := range caddy.tlsRoutes {
		found = found || r == want
	}
	if !found {
		t.Fatalf("tlsRoutes = %v, want %s", caddy.tlsRoutes, want)
	}
	_ = rt
}

func TestDeploySkipsCustomDomainWhenAbsent(t *testing.T) {
	_, _, caddy, d := newTestDeployer(t)
	deploySampleApp(t, d)
	if len(caddy.tlsRoutes) != 0 {
		t.Fatalf("tlsRoutes = %v, want none without an active custom domain", caddy.tlsRoutes)
	}
}
```

**Note:** `newTestDeployer` / `deploySampleApp` stand for the file's existing arrangement (real temp store + fake runtime + fakeCaddy + `New(...)`, then `Deploy` of app "blog"). Reuse the exact helpers/inline setup the existing tests use — copy from the nearest passing test rather than inventing new scaffolding.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/deploy/ -run TestDeployRoutesCustomDomain -v`
Expected: FAIL — `UpsertRouteTLS` not in `RouteSetter`, then route not added.

- [ ] **Step 3: Implement**

In `internal/deploy/deploy.go`, extend the interface:

```go
type RouteSetter interface {
	UpsertRoute(host string, upstreamHostPort int) error
	UpsertRouteTLS(host string, upstreamHostPort int) error
	RemoveRoute(host string) error
}
```

In `Deploy`, after the existing `d.routes.UpsertRoute(host, run.HostPort)` succeeds:

```go
	// An active BYO custom domain (#102) serves the app at <app>.<custom> over
	// the box-terminated :443 alongside the primary host.
	if dc, err := d.store.GetDomainConfig(); err == nil && dc.Status == "active" {
		if err := d.routes.UpsertRouteTLS(appName+"."+dc.Domain, run.HostPort); err != nil {
			return store.Deployment{}, fmt.Errorf("route custom domain: %w", err)
		}
	}
```

- [ ] **Step 4: Run the package**

Run: `go test ./internal/deploy/ -v`
Expected: PASS (existing tests updated only by the fake's new method).

- [ ] **Step 5: `make verify`, then commit**

```bash
git add internal/deploy/
git commit -m "feat(deploy): dual-host routing when a custom domain is active

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 10: `api` — `GET/PUT/DELETE /v1/domain`

**Files:**
- Modify: `internal/api/api.go`
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `domain.Status`, `domain.ErrEnvManaged`, `ErrInvalidDomain`, `ErrUnsupportedProvider`, `ErrTokenRequired`.
- Produces:
  - `type DomainManager interface { Set(domain, provider, token string) (domain.Status, error); Status() (domain.Status, error); Remove() error }`
  - `api.New` signature gains a trailing `dom DomainManager` parameter (nil ⇒ LAN box, endpoints answer 409). **Update all callers:** `cmd/piperd/main.go:243` (pass the manager — wired properly in Task 11; pass `nil` for now to keep the build green) and every `New(` call in `internal/api` tests (pass `nil`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/api/api_test.go`:

```go
type fakeDomainManager struct {
	status  domain.Status
	setErr  error
	gotSet  []string
	removed bool
}

func (f *fakeDomainManager) Set(d, p, tok string) (domain.Status, error) {
	f.gotSet = []string{d, p, tok}
	if f.setErr != nil {
		return domain.Status{}, f.setErr
	}
	return f.status, nil
}
func (f *fakeDomainManager) Status() (domain.Status, error) { return f.status, nil }
func (f *fakeDomainManager) Remove() error                  { f.removed = true; return nil }

func TestDomainEndpoints(t *testing.T) {
	fdm := &fakeDomainManager{status: domain.Status{
		Domain: "shop.dev", DNSProvider: "cloudflare", DNSTokenSet: true,
		Source: "api", Status: "issuing",
		DNSRecords: []domain.DNSRecord{{Type: "CNAME", Name: "*.shop.dev", Value: "relay.example.net"}},
	}}
	h := New(newTestStore(t), &fakeDeployer{}, "piper.localhost", "", nil, fdm)

	// PUT kicks Set with the body fields.
	put := httptest.NewRequest(http.MethodPut, "/v1/domain",
		strings.NewReader(`{"domain":"shop.dev","dns_provider":"cloudflare","dns_token":"cf-tok"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(fdm.gotSet) != 3 || fdm.gotSet[0] != "shop.dev" || fdm.gotSet[2] != "cf-tok" {
		t.Fatalf("Set called with %v", fdm.gotSet)
	}

	// GET returns the status; the token value must never appear.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/domain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"dns_token_set":true`) {
		t.Fatalf("GET body missing dns_token_set: %s", body)
	}
	if strings.Contains(body, `"dns_token":`) || strings.Contains(body, "cf-tok") {
		t.Fatalf("GET leaks the dns token: %s", body)
	}

	// DELETE removes.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/domain", nil))
	if rec.Code != http.StatusNoContent || !fdm.removed {
		t.Fatalf("DELETE = %d, removed = %v", rec.Code, fdm.removed)
	}
}

func TestDomainEndpointErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"env-managed", domain.ErrEnvManaged, http.StatusConflict},
		{"invalid domain", domain.ErrInvalidDomain, http.StatusBadRequest},
		{"bad provider", domain.ErrUnsupportedProvider, http.StatusBadRequest},
		{"empty token", domain.ErrTokenRequired, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := New(newTestStore(t), &fakeDeployer{}, "piper.localhost", "", nil,
				&fakeDomainManager{setErr: tc.err})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/domain",
				strings.NewReader(`{"domain":"x.dev","dns_provider":"cloudflare","dns_token":"t"}`)))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestDomainEndpointsWithoutRelay(t *testing.T) {
	h := New(newTestStore(t), &fakeDeployer{}, "piper.localhost", "", nil, nil)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/v1/domain", strings.NewReader(`{}`)))
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s without relay = %d, want 409", m, rec.Code)
		}
	}
}
```

Add `"github.com/piperbox/piper/internal/domain"` to imports. Update `newTestHandler` and every other `New(` call in the package's tests to pass a trailing `nil`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/api/ -v`
Expected: FAIL to build — `New` has the wrong arity / `DomainManager` undefined.

- [ ] **Step 3: Implement**

In `internal/api/api.go`:

```go
// DomainManager is the domain-config surface (#102). Nil when the box has no
// relay configured: the endpoints then answer 409.
type DomainManager interface {
	Set(domain, provider, token string) (domain.Status, error)
	Status() (domain.Status, error)
	Remove() error
}
```

Change the signature:

```go
func New(s *store.Store, d Deployerer, baseDomain, githubAPIBase string, onGitHubApp func(), dom DomainManager) http.Handler {
```

Add the handlers inside `New` (import `"github.com/piperbox/piper/internal/domain"`):

```go
	noRelay := func(w http.ResponseWriter) bool {
		if dom == nil {
			http.Error(w, "domain config requires a relay: connect this box to a relay first", http.StatusConflict)
			return true
		}
		return false
	}
	mux.HandleFunc("GET /v1/domain", func(w http.ResponseWriter, r *http.Request) {
		if noRelay(w) {
			return
		}
		st, err := dom.Status()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("PUT /v1/domain", func(w http.ResponseWriter, r *http.Request) {
		if noRelay(w) {
			return
		}
		var in struct {
			Domain      string `json:"domain"`
			DNSProvider string `json:"dns_provider"`
			DNSToken    string `json:"dns_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		st, err := dom.Set(in.Domain, in.DNSProvider, in.DNSToken)
		switch {
		case errors.Is(err, domain.ErrEnvManaged):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case errors.Is(err, domain.ErrInvalidDomain),
			errors.Is(err, domain.ErrUnsupportedProvider),
			errors.Is(err, domain.ErrTokenRequired):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("DELETE /v1/domain", func(w http.ResponseWriter, r *http.Request) {
		if noRelay(w) {
			return
		}
		if err := dom.Remove(); errors.Is(err, domain.ErrEnvManaged) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
```

Update `cmd/piperd/main.go:243` to pass `nil` as the new last argument (Task 11 wires the real manager).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api/ ./cmd/piperd/ -v`
Expected: PASS.

- [ ] **Step 5: `make verify`, then commit**

```bash
git add internal/api/ cmd/piperd/
git commit -m "feat(agent): GET/PUT/DELETE /v1/domain on the control API

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 11: `piperd` wiring — manager construction, env fold-in, test issuer, docs

**Files:**
- Modify: `cmd/piperd/main.go`, `cmd/piperd/main_test.go`
- Create: `docs/custom-domains.md`
- Modify: `README.md` (one link line, if it has a docs section)

**Interfaces:**
- Consumes: everything above.
- Produces: a running piperd where
  - relay + terminated ⇒ API-managed domain: `domain.New` + `SetRelay(tc)` + `Resume()` + `go StartRenewals(ctx)`; `api.New` gets the manager.
  - relay + non-terminated ⇒ env-managed: `Options.EnvDomain = cfg.BaseDomain`; static `TLSCertFile` branch unchanged; otherwise `RunEnv(ctx, envIssuer)` replaces `setupRelayTLS`'s ACME branch. `setupRelayTLS`, `renewLoop`, `runRenewLoop`, `certificateManager`, `certificateReplacer` are deleted from main.go (their behavior now lives in `domain`); `newDNSProvider` stays (env issuer construction).
  - LAN ⇒ no manager; `api.New(..., nil)`.
  - `PIPER_TEST_ISSUER=selfsigned` (e2e hook) short-circuits the issuer factory.

- [ ] **Step 1: Delete the moved renew-loop test, keep the rest**

In `cmd/piperd/main_test.go`: delete `TestRunRenewLoopReplacesCertificate`, `fakeCertificateManager`, `fakeCertificateReplacer`, and `expiringCert` (that behavior is tested in `internal/domain` since Task 8). Keep the `newDNSProvider` and shutdown/provision tests.

- [ ] **Step 2: Rewire main.go**

In `cmd/piperd/main.go`:

1. Delete `setupRelayTLS`, `renewLoop`, `runRenewLoop`, `certificateManager`, `certificateReplacer`.
2. Add the e2e issuer hook and env-issuer builder:

```go
// testSelfSignedIssuer is an e2e hook (PIPER_TEST_ISSUER=selfsigned): it
// issues a self-signed wildcard cert instead of ACME so end-to-end tests can
// exercise the domain-config flow without a CA or real DNS.
type testSelfSignedIssuer struct{}

func (testSelfSignedIssuer) Obtain(domains []string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM, nil
}

// newEnvIssuer builds the env-managed issuer: DNS provider by name with creds
// from the provider's own env vars (the pre-#102 path), ACME account key
// persisted in the data dir.
func newEnvIssuer(cfg config.Config) (domain.Issuer, error) {
	if os.Getenv("PIPER_TEST_ISSUER") == "selfsigned" {
		return testSelfSignedIssuer{}, nil
	}
	provider, err := newDNSProvider(cfg.DNSProvider)
	if err != nil {
		return nil, err
	}
	key, err := certs.LoadOrCreateAccountKey(filepath.Join(cfg.DataDir, "acme_account.key"))
	if err != nil {
		return nil, err
	}
	return certs.New(certs.Config{
		Email: cfg.ACMEEmail, CADirURL: cfg.ACMECA,
		DNSProvider: provider, AccountKey: key,
	})
}
```

(new imports: `crypto/x509`, `crypto/x509/pkix`, `encoding/pem`, `math/big`, `github.com/piperbox/piper/internal/domain`; `crypto/ecdsa`, `crypto/elliptic`, `crypto/rand` are already imported.)

3. Construct the manager before the relay branch (after `dep := deploy.New(...)`):

```go
	var domMgr *domain.Manager
	if cfg.RelayAddr != "" {
		relayHost := cfg.RelayAddr
		if h, _, err := net.SplitHostPort(cfg.RelayAddr); err == nil {
			relayHost = h
		}
		opts := domain.Options{
			Store: st, Proxy: caddy.NewClient(cfg.CaddyAdmin),
			DataDir: cfg.DataDir, RelayHost: relayHost, HTTPSListen: ":443",
			Issuer: func(provider, token string) (domain.Issuer, error) {
				if os.Getenv("PIPER_TEST_ISSUER") == "selfsigned" {
					return testSelfSignedIssuer{}, nil
				}
				key, err := certs.LoadOrCreateAccountKey(filepath.Join(cfg.DataDir, "acme_account.key"))
				if err != nil {
					return nil, err
				}
				return certs.NewCloudflareIssuer(cfg.ACMEEmail, cfg.ACMECA, token, key)
			},
		}
		if !cfg.Terminated {
			opts.EnvDomain = cfg.BaseDomain // env-managed BYO: API writes are 409
		}
		domMgr = domain.New(opts)
	}
```

4. In the relay branch, replace the `setupRelayTLS(ctx, cfg)` call (non-terminated arm) with:

```go
			if cfg.TLSCertFile != "" {
				certPEM, err := os.ReadFile(cfg.TLSCertFile)
				if err != nil {
					log.Fatalf("relay tls: %v", err)
				}
				keyPEM, err := os.ReadFile(cfg.TLSKeyFile)
				if err != nil {
					log.Fatalf("relay tls: %v", err)
				}
				if err := caddy.NewClient(cfg.CaddyAdmin).LoadCert(string(certPEM), string(keyPEM)); err != nil {
					log.Fatalf("relay tls: %v", err)
				}
			} else {
				iss, err := newEnvIssuer(cfg)
				if err != nil {
					log.Fatalf("relay tls: %v", err)
				}
				if err := domMgr.RunEnv(ctx, iss); err != nil {
					log.Fatalf("relay tls: %v", err)
				}
			}
```

5. After `tc := &agent.TunnelClient{}` (both arms), wire and resume:

```go
		domMgr.SetRelay(tc)
		if cfg.Terminated {
			domMgr.Resume()
			go domMgr.StartRenewals(ctx)
		}
```

6. Pass the manager to the API (replacing the Task-10 `nil`):

```go
	var dm api.DomainManager
	if domMgr != nil {
		dm = domMgr
	}
	handler := api.RequireToken(st, api.New(st, dep, cfg.BaseDomain, "", func() {
		if wh != nil {
			wh.start()
		}
	}, dm))
```

(The typed-nil guard matters: passing a nil `*domain.Manager` directly would make the interface non-nil.)

- [ ] **Step 3: Build + test**

Run: `go build ./... && go test ./cmd/piperd/ -v`
Expected: builds; piperd package tests PASS.

- [ ] **Step 4: Write the precedence docs**

Create `docs/custom-domains.md`:

```markdown
# Custom domains (BYO)

A box serves apps on a base domain. Two ways to configure it:

## Via the control API (dashboard / `curl`) — relay free-tier boxes

    PUT /v1/domain          {"domain":"example.com","dns_provider":"cloudflare","dns_token":"<token>"}
    GET /v1/domain          → status, DNS records to create, dns_ok, cert_not_after
    DELETE /v1/domain       → remove the custom domain

The box issues a wildcard cert via ACME DNS-01 (Let's Encrypt) using the
Cloudflare API token, terminates TLS itself, and asks the relay to splice
`*.example.com` SNI down its tunnel. Your existing shared-domain URLs
(`<hash>-<user>.<apex>`) keep working alongside.

Create the DNS records `GET /v1/domain` lists (wildcard + apex → the relay
host). Issuance starts immediately — records are needed for traffic, not for
the cert. `dns_ok` flips true once the wildcard resolves to the relay.

Secrets never leave the box: the DNS token is write-only (`dns_token_set`
signals presence), and the cert's private key and ACME account key live in
piperd's data dir with 0600 permissions.

## Via environment variables — self-managed boxes

`PIPER_BASE_DOMAIN` + `PIPER_DNS_PROVIDER` (creds via the provider's own env
vars, e.g. `CLOUDFLARE_DNS_API_TOKEN`), or a static `PIPER_TLS_CERT_FILE` /
`PIPER_TLS_KEY_FILE` pair. Unchanged from before.

## Precedence

**env > API > none.** A box whose base domain comes from the environment
(non-terminated relay mode) reports `"source":"env"` on `GET /v1/domain` and
answers `409` to `PUT`/`DELETE` — unset the env config to manage the domain
remotely. LAN-only boxes (no relay) answer `409` to all `/v1/domain` calls.
```

If `README.md` has a docs/links section, add `- [Custom domains](docs/custom-domains.md)`; otherwise skip.

- [ ] **Step 5: `make verify`, then commit**

```bash
git add cmd/piperd/ docs/custom-domains.md README.md
git commit -m "feat(agent): wire domain manager into piperd; document config precedence

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 12: e2e — free-tier box adds a custom domain; PROGRESS.md

**Files:**
- Create: `test/e2e/domain_test.go`
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: everything; reuses `writeSelfSigned`, `waitPort`, and the login/connect/deploy scaffolding from `test/e2e/relay_terminated_test.go` (copy its structure; extract shared helpers only if trivially clean).

- [ ] **Step 1: Write the e2e test**

Create `test/e2e/domain_test.go`. It follows `TestRelayTerminatedSelfService` **exactly** through "deploy succeeded" (relay with `PIPER_RELAY_APEX=public.localhost` + wildcard cert on `127.0.0.1:8443`, tunnel `127.0.0.1:7000`, API `127.0.0.1:8080`, fake approve; `piper login`; `piper connect`; `piperd token create`; start piperd — **adding `"PIPER_TEST_ISSUER=selfsigned"` to piperd's env**; `piper create blog`; retry `piper deploy blog`), then:

```go
	// ---- Custom domain via the control API (#102) ----
	custom := "shop.localhost"

	// PUT /v1/domain on the box's local control API.
	put := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:8088/v1/domain",
			strings.NewReader(`{"domain":"`+custom+`","dns_provider":"cloudflare","dns_token":"fake-for-selfsigned"}`))
		req.Header.Set("Authorization", "Bearer "+apiToken)
		return http.DefaultClient.Do(req)
	}
	resp, err := put()
	if err != nil {
		t.Fatalf("PUT /v1/domain: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /v1/domain = %d: %s", resp.StatusCode, b)
	}

	// Poll GET /v1/domain until active; assert the token never leaks.
	deadline = time.Now().Add(30 * time.Second)
	var domBody string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8088/v1/domain", nil)
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			gb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			domBody = string(gb)
			if strings.Contains(domBody, `"status":"active"`) {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(domBody, `"status":"active"`) {
		t.Fatalf("domain never became active: %s", domBody)
	}
	if strings.Contains(domBody, "fake-for-selfsigned") {
		t.Fatalf("GET /v1/domain leaks the dns token: %s", domBody)
	}
	if !strings.Contains(domBody, `"dns_records"`) || !strings.Contains(domBody, `"*.`+custom+`"`) {
		t.Fatalf("GET /v1/domain missing guided-setup records: %s", domBody)
	}

	// Visitor on the custom domain: TLS SNI blog.shop.localhost → relay:8443
	// splices passthrough → box :443 terminates. E2E TLS: relay never decrypts.
	var customResp string
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		d := &tls.Dialer{Config: &tls.Config{ServerName: "blog." + custom, InsecureSkipVerify: true}}
		conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
		if err == nil {
			fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: blog.%s\r\nConnection: close\r\n\r\n", custom)
			cb, _ := io.ReadAll(conn)
			conn.Close()
			if len(cb) > 0 {
				customResp = string(cb)
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if customResp == "" {
		t.Fatal("no response on the custom domain through the relay")
	}

	// Coexistence: the shared-domain URL still serves.
	hostname := terminatedHostname(t, relayData)
	d := &tls.Dialer{Config: &tls.Config{ServerName: hostname, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
	if err != nil {
		t.Fatalf("shared-domain dial after custom domain: %v", err)
	}
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostname)
	sb, _ := io.ReadAll(conn)
	conn.Close()
	if len(sb) == 0 {
		t.Fatal("shared-domain URL broke after adding the custom domain")
	}
```

Name the test `TestRelayCustomDomainSelfService`, gate on `RUN_E2E=1` like its siblings. Note piperd binds :80 and :443 in this test (as the existing e2e already binds :80/:443 via Caddy).

- [ ] **Step 2: Run it**

Run: `RUN_E2E=1 go test ./test/e2e/ -run TestRelayCustomDomainSelfService -v -timeout 10m`
Expected: PASS (needs Docker; Caddy embedded).

- [ ] **Step 3: Update PROGRESS.md**

Under the Plan-2 public-relay onboarding sub-list, add:

```markdown
  - ✅ domain-config API — BYO base domain + DNS creds settable remotely, live cert issuance + relay splice, shared-domain coexistence — [#102](https://github.com/piperbox/piper/issues/102)
```

- [ ] **Step 4: Full gate + commit**

Run: `make verify` and (if Docker available) `RUN_E2E=1 go test ./test/e2e/ -timeout 20m`
Expected: all green.

```bash
git add test/e2e/ PROGRESS.md
git commit -m "test(e2e): free-tier box adds BYO custom domain via the control API

Part of #102

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Completion

Open the PR into `main`:

```bash
git push -u origin ozykhan/domain-config-api
gh pr create --base main --title "[agent] Domain-config API: manage BYO base domain + cert issuance remotely" \
  --body "Implements the domain-config API per docs/superpowers/specs/2026-07-10-domain-config-api-design.md.

- PUT/GET/DELETE /v1/domain on the authenticated control API (works via the relay control stream)
- internal/domain manager: DNS-01 issuance → live Caddy activation → relay SNI splice, resume + renewals
- Coexistence: shared-domain URLs keep serving; custom domain adds <app>.<domain> on box-terminated :443
- Relay: set-domain control op, custom_domain uniqueness, reconnect rebind
- Env precedence documented (env > API > none); DNS token write-only; no key material leaves the box

Closes #102"
```

Squash-merge once CI is green.
