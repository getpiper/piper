# Public Relay Onboarding — Plan 2: `piper login` + `piper connect` CLI

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the `piper` CLI a two-verb self-service onboarding flow — `piper login` (Google device flow → relay **account credential**) and `piper connect` (claim **this box**: mint an enrollment token and install it into `piperd`) — consuming the relay control API that Plan 1 already ships.

**Architecture:** A small CLI-side HTTP client package (`internal/relayclient`) talks to the relay's `/v1/login/device`, `/v1/login/poll`, and `/v1/enroll` endpoints. `piper login` (overloaded: no `--token` ⇒ device flow) prints the verification URL + code, polls to completion, and stores the account credential in the CLI config. `piper connect` uses that credential to enroll, then writes a persisted `relay.json` into piperd's data dir; `piperd` loads that file at startup (env still overrides) and dials the tunnel exactly as it does today. No new piperd endpoint and no runtime tunnel management.

**Tech Stack:** Go 1.26, stdlib `net/http` + `net/http/httptest`, `encoding/json`, `flag`. No new dependencies.

This is **Plan 2 of 3** for the slice specced in [`docs/superpowers/specs/2026-07-07-public-relay-onboarding-design.md`](../specs/2026-07-07-public-relay-onboarding-design.md). Plan 1 (landed) = relay accounts + OAuth device-flow + self-service enrollment backend. Plan 3 = assigned hostnames + relay-terminated wildcard app-TLS path.

## Global Constraints

- **No cgo.** All builds pass with `CGO_ENABLED=0`; `make cross` (linux/arm64) must stay green. No new dependencies.
- **Module path** `github.com/piperbox/piper`.
- **TDD.** Every task is failing-test-first. Run `make test` before each commit; it must pass.
- **Layering.** `internal/relayclient` imports only stdlib. `cmd/piper` imports `internal/relayclient` + `internal/config`. `internal/config` imports only stdlib. Nothing imports "up".
- **Default relay control-API base URL** is `https://api.public.getpiper.co` (overridable with `piper login --relay <url>`).
- **`piper connect` prints the restart command; it does NOT auto-restart piperd.**
- **piperd config precedence:** environment variables override the persisted `relay.json` (which overrides built-in defaults).
- Commit messages are conventional-commit style ending with `Co-Authored-By: Claude {current model} <noreply@anthropic.com>`, and reference `Part of #49`.

## Relay control-API contract (from Plan 1 — do not re-implement, only consume)

- `POST /v1/login/device` → `200 {"user_code","verification_uri","device_code","interval","expires_in"}`.
- `POST /v1/login/poll` body `{"device_code":"..."}` → `200 {"account_credential","username"}` on success, `202 {"status":"authorization_pending"}` while pending, `400` on unknown/failed device code.
- `POST /v1/enroll` header `Authorization: Bearer <account_credential>` → `200 {"enrollment_token","base_domain","tunnel_endpoint"}`, `401` for bad/disabled credential, `429` when over the per-account cap.

## Scope & boundary with Plan 3

Plan 2 delivers `login` → `connect` → a persisted `relay.json` → piperd dials the tunnel with its account-bound base domain (the existing outbound-tunnel path). **A live HTTPS URL on the shared domain is Plan 3** (assigned per-app hostnames + relay-terminated wildcard TLS). Until Plan 3 lands, a self-enrolled free-tier box connects and registers its base domain but is not yet served end-to-end on `*.public.getpiper.co`; piperd's existing relay-mode TLS (`setupRelayTLS`) still expects a wildcard cert path (static PEM or DNS-01), unchanged by this plan. The Plan-2 automated tests therefore stop at the CLI ↔ fake-relay boundary and piperd's config-load precedence; the full `login → connect → deploy → curl the assigned hostname` loopback e2e is built in Plan 3 alongside relay-termination.

## File Structure

- Create `internal/relayclient/relayclient.go` — CLI-side HTTP client for the relay control API: `New`, `LoginDevice`, `LoginPoll`, `Enroll`, response types, and sentinel errors.
- Create `internal/relayclient/relayclient_test.go` — httptest coverage of the three calls + sentinel mapping.
- Modify `internal/config/config.go` — extend `ClientConfig` (`RelayAPI`, `AccountCredential`); export `DefaultDataDir`; add the `RelayFile` type + `SaveRelayFile`/`LoadRelayFile`; wire `LoadRelayFile` into `Load()` with env-over-file precedence via a `firstNonEmpty` helper.
- Modify `internal/config/config_test.go` — `ClientConfig` round-trips the new fields; relay-file round-trip + missing; `Load()` reads the file when env is unset and env overrides it.
- Create `cmd/piper/relayonboard.go` — the device-flow `login` path (`relayLogin`) and the `connect` command, plus the `pollSleep` test seam and the `defaultRelayAPI` constant.
- Create `cmd/piper/relayonboard_test.go` — device-flow login + connect against a fake relay.
- Modify `cmd/piper/main.go` — `login` dispatch overload (no `--token` ⇒ device flow, add `--relay`); new `connect` case; updated `usage`.
- Modify `cmd/piper/login_test.go` — replace `TestLoginRequiresToken` (no-token now starts the device flow, not exit 2).
- Modify `README.md` + `PROGRESS.md` — quickstart for `piper login`/`connect`; mark the CLI onboarding surface landed.

---

### Task 1: `relayclient` — CLI-side relay control-API client

**Files:**
- Create: `internal/relayclient/relayclient.go`
- Create: `internal/relayclient/relayclient_test.go`

**Interfaces:**
- Consumes: nothing (stdlib only).
- Produces:
  - `type DeviceAuth struct { UserCode, VerificationURI, DeviceCode string; Interval, ExpiresIn int }`
  - `type Account struct { AccountCredential, Username string }`
  - `type Enrollment struct { EnrollmentToken, BaseDomain, TunnelEndpoint string }`
  - `var ErrAuthPending = errors.New("authorization_pending")`
  - `var ErrBadCredential = errors.New("relay rejected account credential")`
  - `var ErrQuotaExceeded = errors.New("account agent quota exceeded")`
  - `func New(base string) *Client`
  - `func (c *Client) LoginDevice() (DeviceAuth, error)`
  - `func (c *Client) LoginPoll(deviceCode string) (Account, error)` — `ErrAuthPending` on 202.
  - `func (c *Client) Enroll(accountCredential string) (Enrollment, error)` — `ErrBadCredential` on 401, `ErrQuotaExceeded` on 429.

- [ ] **Step 1: Write the failing test**

Create `internal/relayclient/relayclient_test.go`:

```go
package relayclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoginDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/login/device" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_code": "ABCD-EFGH", "verification_uri": "https://relay.test/device",
			"device_code": "dev-1", "interval": 5, "expires_in": 300,
		})
	}))
	defer srv.Close()

	da, err := New(srv.URL).LoginDevice()
	if err != nil {
		t.Fatalf("LoginDevice: %v", err)
	}
	if da.UserCode != "ABCD-EFGH" || da.VerificationURI != "https://relay.test/device" ||
		da.DeviceCode != "dev-1" || da.Interval != 5 || da.ExpiresIn != 300 {
		t.Fatalf("device auth = %+v", da)
	}
}

func TestLoginPollPendingThenSuccess(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ DeviceCode string `json:"device_code"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.DeviceCode != "dev-1" {
			t.Errorf("device_code = %q", body.DeviceCode)
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"account_credential": "cred-xyz", "username": "alice",
		})
	}))
	defer srv.Close()
	c := New(srv.URL)

	if _, err := c.LoginPoll("dev-1"); err != ErrAuthPending {
		t.Fatalf("first poll err = %v, want ErrAuthPending", err)
	}
	acc, err := c.LoginPoll("dev-1")
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if acc.AccountCredential != "cred-xyz" || acc.Username != "alice" {
		t.Fatalf("account = %+v", acc)
	}
}

func TestEnroll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cred-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"enrollment_token": "enr-1", "base_domain": "ab12-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
		})
	}))
	defer srv.Close()

	en, err := New(srv.URL).Enroll("cred-xyz")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if en.EnrollmentToken != "enr-1" || en.BaseDomain != "ab12-alice.public.getpiper.co" ||
		en.TunnelEndpoint != "relay.getpiper.co:7000" {
		t.Fatalf("enrollment = %+v", en)
	}
}

func TestEnrollErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{{http.StatusUnauthorized, ErrBadCredential}, {http.StatusTooManyRequests, ErrQuotaExceeded}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.code)
		}))
		_, err := New(srv.URL).Enroll("whatever")
		srv.Close()
		if err != tc.want {
			t.Fatalf("code %d err = %v, want %v", tc.code, err, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relayclient/ -v`
Expected: FAIL — compile error, package `relayclient` / `New` undefined.

- [ ] **Step 3: Implement the client**

Create `internal/relayclient/relayclient.go`:

```go
// Package relayclient is the piper CLI's HTTP client for the relay control API:
// the Google device-flow login and account-bound box enrollment. It is the
// CLI-side counterpart of the relay's internal/relay control handlers.
package relayclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DeviceAuth is the relay's response to starting a device-flow login.
type DeviceAuth struct {
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	DeviceCode      string `json:"device_code"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

// Account is the completed-login result: a relay account credential + username.
type Account struct {
	AccountCredential string `json:"account_credential"`
	Username          string `json:"username"`
}

// Enrollment is the result of claiming a box: an enrollment token, the assigned
// base domain, and the relay tunnel endpoint the box should dial.
type Enrollment struct {
	EnrollmentToken string `json:"enrollment_token"`
	BaseDomain      string `json:"base_domain"`
	TunnelEndpoint  string `json:"tunnel_endpoint"`
}

// ErrAuthPending means the user has not yet completed the device flow.
var ErrAuthPending = errors.New("authorization_pending")

// ErrBadCredential means the relay rejected the account credential (unknown or
// a disabled account).
var ErrBadCredential = errors.New("relay rejected account credential")

// ErrQuotaExceeded means the account is already at its agent cap.
var ErrQuotaExceeded = errors.New("account agent quota exceeded")

// Client talks to a relay's control API rooted at base.
type Client struct {
	base string
	http *http.Client
}

// New returns a Client for the relay control API at base (e.g.
// https://api.public.getpiper.co).
func New(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) post(path string, body any, auth string) (*http.Response, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	return c.http.Do(req)
}

// LoginDevice starts a device-flow login and returns the user code + URL to show.
func (c *Client) LoginDevice() (DeviceAuth, error) {
	resp, err := c.post("/v1/login/device", nil, "")
	if err != nil {
		return DeviceAuth{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DeviceAuth{}, fmt.Errorf("relay device login: %s", resp.Status)
	}
	var da DeviceAuth
	if err := json.NewDecoder(resp.Body).Decode(&da); err != nil {
		return DeviceAuth{}, err
	}
	return da, nil
}

// LoginPoll polls once for completion of the device flow. It returns
// ErrAuthPending while the user has not finished, or the Account on success.
func (c *Client) LoginPoll(deviceCode string) (Account, error) {
	resp, err := c.post("/v1/login/poll", map[string]string{"device_code": deviceCode}, "")
	if err != nil {
		return Account{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var acc Account
		if err := json.NewDecoder(resp.Body).Decode(&acc); err != nil {
			return Account{}, err
		}
		return acc, nil
	case http.StatusAccepted:
		return Account{}, ErrAuthPending
	default:
		return Account{}, fmt.Errorf("relay login poll: %s", resp.Status)
	}
}

// Enroll claims a box for the account behind accountCredential, returning the
// enrollment token, assigned base domain, and tunnel endpoint.
func (c *Client) Enroll(accountCredential string) (Enrollment, error) {
	resp, err := c.post("/v1/enroll", nil, accountCredential)
	if err != nil {
		return Enrollment{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var en Enrollment
		if err := json.NewDecoder(resp.Body).Decode(&en); err != nil {
			return Enrollment{}, err
		}
		return en, nil
	case http.StatusUnauthorized:
		return Enrollment{}, ErrBadCredential
	case http.StatusTooManyRequests:
		return Enrollment{}, ErrQuotaExceeded
	default:
		return Enrollment{}, fmt.Errorf("relay enroll: %s", resp.Status)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/relayclient/ -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add internal/relayclient/relayclient.go internal/relayclient/relayclient_test.go
git commit -m "feat(cli): relay control-API client (device login + enroll)

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 2: `ClientConfig` gains relay fields

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: existing `ClientConfig`, `LoadClient`, `SaveClient` in `config.go`.
- Produces: `ClientConfig` with two new persisted fields:
  - `RelayAPI string` (json `relay_api`)
  - `AccountCredential string` (json `account_credential`)

  `LoadClient`/`SaveClient` already round-trip the struct as JSON, so the new fields persist with no change to those functions.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestClientConfigRoundTripsRelayFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClient(ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: "https://api.public.getpiper.co",
		AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}
	cc, err := LoadClient()
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cc.RelayAPI != "https://api.public.getpiper.co" || cc.AccountCredential != "cred-xyz" {
		t.Fatalf("cc = %+v", cc)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestClientConfigRoundTripsRelayFields -v`
Expected: FAIL — compile error, `RelayAPI` / `AccountCredential` unknown fields.

- [ ] **Step 3: Add the fields**

In `internal/config/config.go`, replace the `ClientConfig` struct:

```go
// ClientConfig is the piper CLI's saved credentials/target. Addr/Token are the
// LAN path (bearer to piperd); RelayAPI/AccountCredential are the relay path
// (device-flow login), written by `piper login` and read by `piper connect`.
type ClientConfig struct {
	Addr              string `json:"addr"`
	Token             string `json:"token"`
	RelayAPI          string `json:"relay_api,omitempty"`
	AccountCredential string `json:"account_credential,omitempty"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestClientConfigRoundTripsRelayFields -v`
Expected: PASS. Then `go test ./internal/config/ -v` — whole package still green.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(cli): persist relay API + account credential in ClientConfig

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 3: piperd persisted `relay.json` + env-over-file precedence

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: existing `Load`, `env`, `defaultDataDir` in `config.go`.
- Produces:
  - `type RelayFile struct { RelayAddr, RelayToken, BaseDomain string }` (json `relay_addr`/`relay_token`/`base_domain`)
  - `func DefaultDataDir() string` — exported; piperd's data-dir default (`~/.piper/piperd`), reused by `piper connect`.
  - `func SaveRelayFile(dataDir string, rf RelayFile) error` — writes `<dataDir>/relay.json`, 0600, creating the dir.
  - `func LoadRelayFile(dataDir string) (RelayFile, bool, error)` — reads it; `found=false` (no error) when absent.
  - `Load()` now fills `RelayAddr`/`RelayToken`/`BaseDomain` from `relay.json` when the corresponding env var is empty (env wins; then file; then the built-in default for `BaseDomain`).

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestRelayFileRoundTripAndMissing(t *testing.T) {
	dir := t.TempDir()
	if _, found, err := LoadRelayFile(dir); err != nil || found {
		t.Fatalf("missing relay file: found=%v err=%v", found, err)
	}
	rf := RelayFile{RelayAddr: "relay:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co"}
	if err := SaveRelayFile(dir, rf); err != nil {
		t.Fatalf("SaveRelayFile: %v", err)
	}
	got, found, err := LoadRelayFile(dir)
	if err != nil || !found {
		t.Fatalf("LoadRelayFile: found=%v err=%v", found, err)
	}
	if got != rf {
		t.Fatalf("relay file = %+v, want %+v", got, rf)
	}
}

func TestLoadReadsRelayFileWhenEnvUnset(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	cfg := Load()
	if cfg.RelayAddr != "relay:7000" || cfg.RelayToken != "enr-1" ||
		cfg.BaseDomain != "ab12-alice.public.getpiper.co" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadEnvOverridesRelayFile(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "override:9000")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	cfg := Load()
	if cfg.RelayAddr != "override:9000" {
		t.Fatalf("RelayAddr = %q, want env override", cfg.RelayAddr)
	}
	if cfg.RelayToken != "enr-1" { // env unset ⇒ file value
		t.Fatalf("RelayToken = %q, want file value", cfg.RelayToken)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run 'TestRelayFile|TestLoadReadsRelayFile|TestLoadEnvOverridesRelayFile' -v`
Expected: FAIL — `RelayFile` / `SaveRelayFile` / `LoadRelayFile` undefined.

- [ ] **Step 3: Implement the relay file + precedence**

In `internal/config/config.go`, add the exported data-dir accessor next to `defaultDataDir` (keep `defaultDataDir` and delegate, so no other call site changes):

```go
// DefaultDataDir is piperd's data-dir default (~/.piper/piperd) when
// PIPER_DATA_DIR is unset. `piper connect` reuses it to write relay.json to the
// same place piperd reads it.
func DefaultDataDir() string { return defaultDataDir() }
```

Add the relay-file type and I/O (place after `SaveClient`):

```go
// RelayFile is the persisted relay enrollment written by `piper connect` and
// read by piperd at startup. Environment variables override these values.
type RelayFile struct {
	RelayAddr  string `json:"relay_addr"`
	RelayToken string `json:"relay_token"`
	BaseDomain string `json:"base_domain"`
}

func relayFilePath(dataDir string) string { return filepath.Join(dataDir, "relay.json") }

// SaveRelayFile writes rf to <dataDir>/relay.json with 0600 perms, creating the
// directory if needed.
func SaveRelayFile(dataDir string, rf RelayFile) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(relayFilePath(dataDir), data, 0o600)
}

// LoadRelayFile reads <dataDir>/relay.json. A missing file is not an error:
// found is false and rf is the zero value.
func LoadRelayFile(dataDir string) (RelayFile, bool, error) {
	var rf RelayFile
	data, err := os.ReadFile(relayFilePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return rf, false, nil
	}
	if err != nil {
		return rf, false, err
	}
	if err := json.Unmarshal(data, &rf); err != nil {
		return rf, false, err
	}
	return rf, true, nil
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
```

Then replace `Load()` so it resolves the data dir once, loads the relay file, and applies env-over-file precedence for the three relay-derived fields:

```go
// Load builds a Config from env vars and the persisted relay.json, applying
// defaults. Env vars override relay.json, which overrides built-in defaults.
func Load() Config {
	dataDir := env("PIPER_DATA_DIR", DefaultDataDir())
	rf, _, _ := LoadRelayFile(dataDir) // best-effort: a corrupt file yields zero values

	return Config{
		APIAddr:     env("PIPER_API_ADDR", "127.0.0.1:8088"),
		WebhookAddr: env("PIPER_WEBHOOK_ADDR", "127.0.0.1:8089"),
		DataDir:     dataDir,
		BaseDomain:  firstNonEmpty(os.Getenv("PIPER_BASE_DOMAIN"), rf.BaseDomain, "piper.localhost"),
		CaddyAdmin:  env("PIPER_CADDY_ADMIN", "http://127.0.0.1:2019"),

		RelayAddr:   firstNonEmpty(os.Getenv("PIPER_RELAY_ADDR"), rf.RelayAddr),
		RelayToken:  firstNonEmpty(os.Getenv("PIPER_RELAY_TOKEN"), rf.RelayToken),
		ACMEEmail:   env("PIPER_ACME_EMAIL", ""),
		ACMECA:      env("PIPER_ACME_CA", ""),
		DNSProvider: env("PIPER_DNS_PROVIDER", ""),
		TLSCertFile: env("PIPER_TLS_CERT_FILE", ""),
		TLSKeyFile:  env("PIPER_TLS_KEY_FILE", ""),
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run 'TestRelayFile|TestLoadReadsRelayFile|TestLoadEnvOverridesRelayFile' -v`
Expected: PASS. Then `go test ./internal/config/ -v` — confirm the existing `TestLoadRelayFields` (env-based) and defaults tests still pass.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(agent): load persisted relay.json at startup (env overrides)

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 4: `piper login` device-flow path (overload)

**Files:**
- Create: `cmd/piper/relayonboard.go`
- Create: `cmd/piper/relayonboard_test.go`
- Modify: `cmd/piper/main.go` (login dispatch overload)
- Modify: `cmd/piper/login_test.go` (replace `TestLoginRequiresToken`)

**Interfaces:**
- Consumes: `relayclient.New`/`LoginDevice`/`LoginPoll`/`ErrAuthPending` (Task 1); `config.LoadClient`/`SaveClient`/`ClientConfig` (Task 2); the existing `openBrowserFn` seam in `cmd/piper/main.go`.
- Produces:
  - `const defaultRelayAPI = "https://api.public.getpiper.co"`
  - `var pollSleep = time.Sleep` (test seam)
  - `func relayLogin(relayAPI string, stdout, stderr io.Writer) int`
  - `main.go` `login` case: `--token` present ⇒ existing LAN `login`; else `relayLogin(*relay, ...)` with a new `--relay` flag defaulting to `defaultRelayAPI`.

- [ ] **Step 1: Write the failing test**

Create `cmd/piper/relayonboard_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/config"
)

func TestRelayLoginStoresCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	// No real sleeps or browser during the poll loop.
	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	openBrowserFn = func(string) error { return nil }
	defer func() { openBrowserFn = openBrowser }()

	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_code": "ABCD-EFGH", "verification_uri": "https://relay.test/device",
				"device_code": "dev-1", "interval": 1, "expires_in": 300,
			})
		case "/v1/login/poll":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"account_credential": "cred-xyz", "username": "alice",
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--relay", srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.RelayAPI != srv.URL || cc.AccountCredential != "cred-xyz" {
		t.Fatalf("cc = %+v", cc)
	}
}
```

Also replace `TestLoginRequiresToken` in `cmd/piper/login_test.go` (the no-token path now starts the device flow instead of erroring). Delete the old test and add, in its place:

```go
func TestLoginTokenPathStillValidates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	// --token present ⇒ LAN path, which rejects a bad token with exit 1.
	if code := run([]string{"login", "--addr", srv.URL, "--token", "bad"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/piper/ -run 'TestRelayLogin|TestLoginTokenPathStillValidates' -v`
Expected: FAIL — `relayLogin`/`pollSleep`/`defaultRelayAPI` undefined (and `run` still routes no-`--relay` login through the old path).

- [ ] **Step 3: Implement `relayLogin` + the dispatch overload**

Create `cmd/piper/relayonboard.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
)

// defaultRelayAPI is the hosted public relay's control API. Override with
// `piper login --relay <url>` for a self-hosted relay.
const defaultRelayAPI = "https://api.public.getpiper.co"

// pollSleep is the device-flow poll delay; a seam so tests don't really sleep.
var pollSleep = time.Sleep

// relayLogin runs the Google device flow against the relay, printing the
// verification URL + user code, polling to completion, and storing the returned
// account credential (and relay API base) in the CLI config.
func relayLogin(relayAPI string, stdout, stderr io.Writer) int {
	rc := relayclient.New(relayAPI)
	da, err := rc.LoginDevice()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "To log in, open:\n\n    %s\n\nand enter the code: %s\n\n", da.VerificationURI, da.UserCode)
	_ = openBrowserFn(da.VerificationURI)

	interval := da.Interval
	if interval <= 0 {
		interval = 5
	}
	expires := da.ExpiresIn
	if expires <= 0 {
		expires = 300
	}
	deadline := time.Now().Add(time.Duration(expires) * time.Second)
	for {
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "error: login timed out; run `piper login` again")
			return 1
		}
		pollSleep(time.Duration(interval) * time.Second)
		acc, err := rc.LoginPoll(da.DeviceCode)
		if errors.Is(err, relayclient.ErrAuthPending) {
			continue
		}
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cc, err := config.LoadClient()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cc.RelayAPI = relayAPI
		cc.AccountCredential = acc.AccountCredential
		if err := config.SaveClient(cc); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "logged in to relay as %s\n", acc.Username)
		return 0
	}
}
```

In `cmd/piper/main.go`, replace the `login` case so no `--token` routes to the device flow:

```go
	case "login":
		fs := flag.NewFlagSet("login", flag.ContinueOnError)
		fs.SetOutput(stderr)
		token := fs.String("token", "", "API token from `piperd token create` (LAN login)")
		addr := fs.String("addr", "", "piperd address (LAN login)")
		relay := fs.String("relay", defaultRelayAPI, "relay control API base URL (device-flow login)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *token != "" {
			return login(*addr, *token, stdout, stderr)
		}
		return relayLogin(*relay, stdout, stderr)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/piper/ -run 'TestRelayLogin|TestLogin' -v`
Expected: PASS (new device-flow test, the retained LAN tests, and `TestLoginTokenPathStillValidates`).

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/relayonboard.go cmd/piper/relayonboard_test.go cmd/piper/main.go cmd/piper/login_test.go
git commit -m "feat(cli): piper login device flow (relay account credential)

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 5: `piper connect` — enroll + install relay.json

**Files:**
- Modify: `cmd/piper/relayonboard.go` (add `connect`)
- Modify: `cmd/piper/relayonboard_test.go` (add connect tests)
- Modify: `cmd/piper/main.go` (`connect` case + usage)

**Interfaces:**
- Consumes: `config.LoadClient` (Task 2), `config.SaveRelayFile`/`RelayFile`/`DefaultDataDir` (Task 3), `relayclient.New`/`Enroll`/`ErrBadCredential`/`ErrQuotaExceeded` (Task 1).
- Produces:
  - `func connect(dataDir string, stdout, stderr io.Writer) int`
  - `main.go` `connect` case: resolves the piperd data dir the same way piperd does (`--data-dir` flag defaulting to `PIPER_DATA_DIR` or `config.DefaultDataDir()`), then calls `connect`.
  - `usage` updated to list `connect`.

- [ ] **Step 1: Write the failing test**

Add to `cmd/piper/relayonboard_test.go`:

```go
func TestConnectRequiresLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", t.TempDir()}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !bytes.Contains(errb.Bytes(), []byte("piper login")) {
		t.Fatalf("stderr = %q, want a `piper login` hint", errb.String())
	}
}

func TestConnectEnrollsAndWritesRelayFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" || r.Header.Get("Authorization") != "Bearer cred-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"enrollment_token": "enr-1", "base_domain": "ab12-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
		})
	}))
	defer srv.Close()

	// Prior `piper login` state.
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
	rf, found, err := config.LoadRelayFile(dataDir)
	if err != nil || !found {
		t.Fatalf("relay file: found=%v err=%v", found, err)
	}
	want := config.RelayFile{RelayAddr: "relay.getpiper.co:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co"}
	if rf != want {
		t.Fatalf("relay file = %+v, want %+v", rf, want)
	}
}

func TestConnectQuotaExceeded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", t.TempDir()}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !bytes.Contains(errb.Bytes(), []byte("quota")) {
		t.Fatalf("stderr = %q, want a quota message", errb.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/piper/ -run TestConnect -v`
Expected: FAIL — `connect` undefined and the `connect` dispatch case is missing (`run` returns usage / exit 2).

- [ ] **Step 3: Implement `connect` + dispatch + usage**

Add to `cmd/piper/relayonboard.go`:

```go
// connect claims this box: it enrolls with the relay using the stored account
// credential and writes a relay.json into piperd's data dir. piperd reads that
// file at startup and dials the tunnel; connect does not restart piperd.
func connect(dataDir string, stdout, stderr io.Writer) int {
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if cc.RelayAPI == "" || cc.AccountCredential == "" {
		fmt.Fprintln(stderr, "error: not logged in to a relay; run `piper login` first")
		return 1
	}
	en, err := relayclient.New(cc.RelayAPI).Enroll(cc.AccountCredential)
	switch {
	case errors.Is(err, relayclient.ErrBadCredential):
		fmt.Fprintln(stderr, "error: relay rejected your account credential; run `piper login` again")
		return 1
	case errors.Is(err, relayclient.ErrQuotaExceeded):
		fmt.Fprintln(stderr, "error: account agent quota exceeded; remove an existing box or upgrade")
		return 1
	case err != nil:
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := config.SaveRelayFile(dataDir, config.RelayFile{
		RelayAddr:  en.TunnelEndpoint,
		RelayToken: en.EnrollmentToken,
		BaseDomain: en.BaseDomain,
	}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "box claimed: %s\nrestart piperd to connect, e.g.:\n\n    sudo systemctl restart piperd\n", en.BaseDomain)
	return 0
}
```

In `cmd/piper/main.go`, add a `connect` case to the dispatch switch (after `login`):

```go
	case "connect":
		fs := flag.NewFlagSet("connect", flag.ContinueOnError)
		fs.SetOutput(stderr)
		def := os.Getenv("PIPER_DATA_DIR")
		if def == "" {
			def = config.DefaultDataDir()
		}
		dataDir := fs.String("data-dir", def, "piperd data directory (where relay.json is written)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		return connect(*dataDir, stdout, stderr)
```

Update `usage` to include `connect`:

```go
func usage(w io.Writer) int {
	fmt.Fprintln(w, "usage: piper <version|login|connect|create|deploy|list|app|github> [args]")
	return 2
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/piper/ -run TestConnect -v`
Expected: PASS. Then `go test ./cmd/piper/ -v` — whole CLI package green.

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/relayonboard.go cmd/piper/relayonboard_test.go cmd/piper/main.go
git commit -m "feat(cli): piper connect — claim box, install relay.json

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 6: Docs + full-plan verification

**Files:**
- Modify: `README.md`
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: the CLI surface from Tasks 1–5. No new code.

- [ ] **Step 1: Add a `piper login` / `piper connect` quickstart to README**

In `README.md`, add a short section (place it near the existing relay/onboarding docs; match the surrounding heading style):

```markdown
### Join the public relay (self-service)

On a box running `piperd`:

```bash
piper login          # opens a Google device-flow login; stores your account credential
piper connect        # claims this box on the relay and writes ~/.piper/piperd/relay.json
sudo systemctl restart piperd   # piperd reads relay.json at startup and dials the tunnel
```

`piper login --relay <url>` targets a self-hosted relay instead of the default
`https://api.public.getpiper.co`. Environment variables (`PIPER_RELAY_ADDR`,
`PIPER_RELAY_TOKEN`, `PIPER_BASE_DOMAIN`) still override `relay.json`.

> A live HTTPS URL on the shared `*.public.getpiper.co` domain arrives with the
> relay-terminated app path (next plan); `connect` today gets your box onto the
> tunnel with its assigned base domain.
```

- [ ] **Step 2: Update PROGRESS.md**

Add/adjust the relay onboarding line so the CLI surface is marked landed, linking `#49` (keep entries terse — one line + issue ref, matching the file's existing style). Example:

```markdown
- `piper login` / `piper connect` self-service onboarding CLI — device-flow login + box claim, writes piperd `relay.json` [#49]
```

- [ ] **Step 3: Full verification**

Run: `make test`
Expected: all packages pass (Docker/e2e skip cleanly if Docker absent).
Run: `make cross`
Expected: linux/arm64 build succeeds (no-cgo intact).

- [ ] **Step 4: Commit**

```bash
git add README.md PROGRESS.md
git commit -m "docs: piper login/connect self-service onboarding quickstart

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage** (against `2026-07-07-public-relay-onboarding-design.md`, `piper` CLI items):

- `piper login` — device flow, print URL + code, poll, store account credential in CLI config → Tasks 1, 2, 4. ✅
- Coexists with the existing `piper login --token <t> --addr <box>` LAN path → Task 4 (overload: `--token` ⇒ LAN, else device flow). ✅
- `piper connect` — requires a prior login (errors without a stored credential), requests an enrollment token, installs token + relay endpoint into piperd → Tasks 1, 3, 5. ✅
- Device-flow failures (expiry, denial, network) surfaced by the CLI; box unchanged → Task 4 (timeout deadline, poll-error exit) + Task 5 (enroll error mapping, no relay.json written on failure). ✅
- Quota exceeded → clear actionable error → Task 5 (`429` → message). ✅
- Kill-switch (disabled account) mid-flow → `login`/`enroll` return the relay's `401`, mapped to a re-login hint → Tasks 1, 5. ✅
- piperd accepts the installed enrollment token and dials the tunnel → Task 3 (loads `relay.json`; the existing `RunTunnelClient` path is unchanged). ✅
- **Out of scope (Plan 3, intentionally):** assigned per-app hostnames, relay-terminated wildcard TLS, hostname registration over the control tunnel, and the full deploy-and-curl loopback e2e. Documented in *Scope & boundary with Plan 3*. ✅

**Placeholder scan:** No `TBD`/`TODO`/"add error handling" — every code and test step is complete. The README/PROGRESS edits in Task 6 show the exact prose to add. ✅

**Type consistency:** `DeviceAuth{UserCode,VerificationURI,DeviceCode,Interval,ExpiresIn}`, `Account{AccountCredential,Username}`, `Enrollment{EnrollmentToken,BaseDomain,TunnelEndpoint}`, `RelayFile{RelayAddr,RelayToken,BaseDomain}`, and the new `ClientConfig` fields `RelayAPI`/`AccountCredential` are used identically across tasks. Sentinels `ErrAuthPending`/`ErrBadCredential`/`ErrQuotaExceeded` and the `pollSleep`/`openBrowserFn` seams are referenced consistently. `connect` maps `TunnelEndpoint→RelayAddr` and `EnrollmentToken→RelayToken` when writing `RelayFile`, matching piperd's `Load()` consumption in Task 3. ✅

**Layering:** `relayclient` (stdlib only) ← `cmd/piper` → `config` (stdlib only). No upward imports; no new dependencies (`make cross` unaffected). ✅

## Next plans

- **Plan 3 — assigned hostnames + relay-terminated app path:** per-app `<hash>-<username>.<apex>` hostnames registered over the control tunnel; the relay `:443` SNI branch (terminate under `*.public.getpiper.co` vs. passthrough); piperd-side forwarded-HTTP handling; and the full `login → connect → deploy → curl the assigned hostname` loopback e2e with a fake Google IdP.
