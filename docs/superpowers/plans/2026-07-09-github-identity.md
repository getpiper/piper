# GitHub Identity for Relay Accounts — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Switch relay account identity from Google to a getpiper-owned GitHub OAuth app — device flow for `piper login`, authorization-code flow for the browser — and remove the Google flow ([#99]).

**Architecture:** A plain-HTTP `GitHubVerifier` (no `x/oauth2`, no `go-oidc`) replaces `GoogleVerifier` behind the unchanged `Verifier` interface, so the CLI and device endpoints keep their exact wire shape. A small new `WebVerifier` interface (`AuthCodeURL`/`Exchange`) plus two relay endpoints (`GET /v1/login/web`, `GET /v1/login/callback`) give the browser a relay-hosted authorization-code flow that returns the same account credential in a URL fragment. Usernames derive from the GitHub login; the `accounts` table keys on `github_id`.

**Tech Stack:** Go stdlib only (`net/http`, `net/http/httptest` for fakes). Spec: `docs/superpowers/specs/2026-07-09-github-identity-design.md`.

## Global Constraints

- **No cgo** — everything must build with `CGO_ENABLED=0` (`make cross` proves it).
- Branch: `ozykhan/github-identity` (already created; the spec is its first commit). Never commit to `main`.
- Commits: conventional-commit style, one per plan task step-group, ending with `Co-Authored-By: Claude {current model} <noreply@anthropic.com>`.
- Reference the issue as `Part of #99` in commit bodies.
- Run tests with `go test ./internal/relay/ ./cmd/... -count=1` per task; run `make verify` at the end (Task 5).
- Wire shape of `POST /v1/login/device` and `POST /v1/login/poll` must not change (the CLI depends on it).
- No new OAuth scopes: the GitHub authorize/device requests carry **no `scope` parameter**.
- Schema change is a plain rename in `schema.sql` — **no migration code** (pre-release decision; operator drops `accounts`/`account_creds` on the hosted relay).

---

### Task 1: Accounts keyed by `github_id`, usernames from GitHub login

**Files:**
- Modify: `internal/relay/schema.sql` (accounts table, line 13)
- Modify: `internal/relay/accounts.go` (`deriveUsername`, `UpsertAccount`)
- Test: `internal/relay/accounts_test.go`
- Modify (mechanical, second arg becomes a login): `internal/relay/hostnames_test.go:17`, `internal/relay/server_test.go:28`, `internal/relay/proxy_test.go:65,70`, `internal/relay/store_test.go:67`, `cmd/piper-relay/main_test.go:16`, `test/e2e/relay_terminated_test.go:251`

**Interfaces:**
- Consumes: existing `Store` (`internal/relay/store.go`), `openTestStore` helper.
- Produces: `func (s *Store) UpsertAccount(githubID, login string) (Account, error)` — same signature shape as today, but the second argument is a GitHub login, not an email; username = sanitized login. Later tasks call it as `a.st.UpsertAccount(id.Subject, id.Login)`.

- [ ] **Step 1: Update the account tests to speak GitHub**

In `internal/relay/accounts_test.go`, replace `TestUpsertAccountIsIdempotentBySub` and `TestUpsertAccountDisambiguatesUsername` with:

```go
func TestUpsertAccountIsIdempotentByGitHubID(t *testing.T) {
	st := openTestStore(t)

	a1, err := st.UpsertAccount("583231", "Alice-Smith")
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if a1.Username != "alice-smith" {
		t.Fatalf("username = %q, want alice-smith", a1.Username)
	}
	if a1.ID == "" {
		t.Fatal("empty account id")
	}

	a2, err := st.UpsertAccount("583231", "Alice-Smith")
	if err != nil {
		t.Fatalf("second UpsertAccount: %v", err)
	}
	if a2.ID != a1.ID {
		t.Fatalf("re-upsert made a new account: %s != %s", a2.ID, a1.ID)
	}
}

func TestUpsertAccountDisambiguatesUsername(t *testing.T) {
	st := openTestStore(t)
	// Two different GitHub accounts can collide on the derived username
	// (e.g. after a rename freed the login for someone else).
	a1, _ := st.UpsertAccount("gh-a", "bob")
	a2, _ := st.UpsertAccount("gh-b", "bob")
	if a1.Username != "bob" {
		t.Fatalf("first username = %q, want bob", a1.Username)
	}
	if a2.Username != "bob-2" {
		t.Fatalf("second username = %q, want bob-2", a2.Username)
	}
}

func TestUpsertAccountCapsLongLogin(t *testing.T) {
	st := openTestStore(t)
	// GitHub logins go up to 39 chars; usernames cap at 30 to keep the
	// eventual "<hash>-<username>.<apex>" DNS label under 63 chars.
	long := "a-very-long-github-login-name-indeed-x" // 39 chars
	acc, err := st.UpsertAccount("gh-long", long)
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if len(acc.Username) > 30 {
		t.Fatalf("username %q is %d chars, want <= 30", acc.Username, len(acc.Username))
	}
}
```

In the same file, update the remaining `UpsertAccount` calls to pass logins (usernames stay the same, so no other assertions change):
- `st.UpsertAccount("sub-1", "carol@x.com")` → `st.UpsertAccount("sub-1", "carol")`
- `st.UpsertAccount("sub-1", "dave@x.com")` → `st.UpsertAccount("sub-1", "dave")`
- `st.UpsertAccount("sub-1", "erin@x.com")` → `st.UpsertAccount("sub-1", "erin")`
- `st.UpsertAccount("sub-1", "frank@x.com")` → `st.UpsertAccount("sub-1", "frank")`
- `st.UpsertAccount("sub-1", "grace@x.com")` → `st.UpsertAccount("sub-1", "grace")`

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestUpsertAccount' -count=1 -v`
Expected: FAIL — `TestUpsertAccountIsIdempotentByGitHubID` gets username `alice-smith`? No: `deriveUsername("Alice-Smith")` today treats the whole string as an email local part and lowercases to `alice-smith`, so that one may pass; `TestUpsertAccountCapsLongLogin` passes too. The **real** red comes from the schema rename below, so treat this step as: tests compile and the suite still runs against `google_sub`. The failing signal for the rename is Step 4's grep + Step 5. (If everything passes here, that's expected — proceed.)

- [ ] **Step 3: Rename the schema column and update accounts.go**

`internal/relay/schema.sql` — accounts table becomes:

```sql
CREATE TABLE IF NOT EXISTS accounts (
    id          TEXT PRIMARY KEY,
    github_id   TEXT NOT NULL UNIQUE,
    username    TEXT NOT NULL UNIQUE,
    disabled    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);
```

`internal/relay/accounts.go` — replace `deriveUsername` and `UpsertAccount`:

```go
// deriveUsername turns a GitHub login into a DNS-safe label component:
// lowercased, every rune outside [a-z0-9-] replaced by '-', trimmed of
// leading/trailing '-', and capped at 30 chars so the eventual
// "<hash>-<username>.<apex>" label stays under DNS's 63-char limit.
// (GitHub logins are already <= 39 chars of [A-Za-z0-9-], so this is
// nearly a lowercase passthrough.)
func deriveUsername(login string) string {
	login = strings.ToLower(login)
	var b strings.Builder
	for _, r := range login {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	u := strings.Trim(b.String(), "-")
	if u == "" {
		u = "user"
	}
	if len(u) > 30 {
		u = strings.Trim(u[:30], "-")
	}
	return u
}

// UpsertAccount returns the account for githubID, creating it (with a unique
// username derived from the GitHub login) on first sight. Idempotent by
// githubID.
func (s *Store) UpsertAccount(githubID, login string) (Account, error) {
	var acc Account
	var disabled int
	err := s.db.QueryRow(`SELECT id, username, disabled FROM accounts WHERE github_id=?`, githubID).
		Scan(&acc.ID, &acc.Username, &disabled)
	if err == nil {
		acc.Disabled = disabled != 0
		return acc, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Account{}, err
	}

	base := deriveUsername(login)
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 1; ; i++ {
		username := base
		if i > 1 {
			username = base + "-" + strconv.Itoa(i)
		}
		_, err := s.db.Exec(
			`INSERT INTO accounts(id, github_id, username, disabled, created_at) VALUES(?,?,?,0,?)`,
			id, githubID, username, now)
		if err == nil {
			return Account{ID: id, Username: username}, nil
		}
		if isUniqueViolation(err) {
			// Another account already holds this username; try the next suffix.
			// (A racing insert of the same github_id is vanishingly unlikely on a
			// single relay; the SELECT above handles the common re-login path.)
			continue
		}
		return Account{}, err
	}
}
```

(The email local-part split — `strings.IndexByte(email, '@')` — is gone; everything else in the file is untouched.)

- [ ] **Step 4: Update the mechanical call sites**

- `internal/relay/hostnames_test.go:17`: `st.UpsertAccount("google-sub-1", "alice@example.com")` → `st.UpsertAccount("gh-1", "alice")`
- `internal/relay/server_test.go:28`: `st.UpsertAccount("sub-1", "alice@example.com")` → `st.UpsertAccount("sub-1", "alice")`
- `internal/relay/proxy_test.go:65`: `st.UpsertAccount("sub-alice", "alice@x.com")` → `st.UpsertAccount("sub-alice", "alice")`
- `internal/relay/proxy_test.go:70`: `st.UpsertAccount("sub-mallory", "mallory@x.com")` → `st.UpsertAccount("sub-mallory", "mallory")`
- `internal/relay/store_test.go:67`: `st.UpsertAccount("sub-ct", "ct@example.com")` → `st.UpsertAccount("sub-ct", "ct")`
- `cmd/piper-relay/main_test.go:16`: `st.UpsertAccount("sub-1", "leo@x.com")` → `st.UpsertAccount("sub-1", "leo")`
- `test/e2e/relay_terminated_test.go:251`: in the raw SQL, `INSERT INTO accounts(id, google_sub, username, disabled, created_at)` → `INSERT INTO accounts(id, github_id, username, disabled, created_at)`

Then verify nothing still references the old column: `grep -rn "google_sub" .` → no matches outside `docs/`.

- [ ] **Step 5: Run the package tests**

Run: `go test ./internal/relay/ ./cmd/... -count=1`
Expected: PASS (e2e tests skip without the full stack; that's fine).

- [ ] **Step 6: Commit**

```bash
git add internal/relay/schema.sql internal/relay/accounts.go internal/relay/accounts_test.go \
  internal/relay/hostnames_test.go internal/relay/server_test.go internal/relay/proxy_test.go \
  internal/relay/store_test.go cmd/piper-relay/main_test.go test/e2e/relay_terminated_test.go
git commit -m "feat(relay): key accounts on github_id, derive usernames from GitHub login

Part of #99

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 2: GitHub device-flow verifier replaces Google

**Files:**
- Create: `internal/relay/verifier_github.go`
- Create: `internal/relay/verifier_github_test.go`
- Delete: `internal/relay/verifier_google.go`
- Modify: `internal/relay/verifier.go` (Identity: `Email` → `Login`; comment tweaks)
- Modify: `internal/relay/verifier_test.go` (Email → Login; drop the Google test)
- Modify: `internal/relay/api.go:71` (`id.Email` → `id.Login`)
- Modify: `internal/relay/api_test.go:48` (Identity literal)
- Modify: `cmd/piper-relay/main.go:129-141` (env wiring)
- Modify: `go.mod` / `go.sum` (via `go mod tidy`)

**Interfaces:**
- Consumes: `Verifier` interface, `DeviceAuth`, `ErrAuthPending` (unchanged, `internal/relay/verifier.go`); `UpsertAccount(githubID, login string)` from Task 1.
- Produces:
  - `type Identity struct { Subject string /* GitHub numeric user id, decimal string */; Login string }`
  - `func NewGitHubVerifier(clientID, clientSecret string) *GitHubVerifier` — implements `Verifier`. No error return (no OIDC discovery).
  - Unexported seams later tasks and tests rely on: `oauthBase`, `apiBase` string fields (test override), `sleep func(time.Duration)` field (defaults `time.Sleep`), and `fetchUser(ctx, token) (Identity, error)` + `postForm(ctx, url, form, out) error` helpers reused by Task 3's `Exchange`.

- [ ] **Step 1: Update Identity and the fake, and fix their users**

`internal/relay/verifier.go`:
- `Identity` becomes:

```go
// Identity is the verified subject of a completed login.
type Identity struct {
	Subject string // stable IdP user id (GitHub numeric id, as a decimal string)
	Login   string // GitHub login; source of the derived username
}
```

- In the `NewAutoApproveVerifier` doc comment, replace “without a real Google IdP” with “without a real GitHub IdP” and “no real Google client ID” with “no real GitHub client ID”; its signature becomes `NewAutoApproveVerifier(sub, login string)` with body `f.auto = &Identity{Subject: sub, Login: login}`.

`internal/relay/verifier_test.go`:
- In `TestFakeVerifierStartPollApprove`: `Identity{Subject: "sub-1", Email: "heidi@x.com"}` → `Identity{Subject: "sub-1", Login: "heidi"}` and the assertion `id.Email != "heidi@x.com"` → `id.Login != "heidi"`.
- Delete `TestGoogleVerifierPollUnknownHandle` (replaced in the new file below).

`internal/relay/api.go:71`: `a.st.UpsertAccount(id.Subject, id.Email)` → `a.st.UpsertAccount(id.Subject, id.Login)`.

`internal/relay/api_test.go:48`: `fv.Approve(dev.DeviceCode, Identity{Subject: "sub-1", Email: "ivan@x.com"})` → `fv.Approve(dev.DeviceCode, Identity{Subject: "sub-1", Login: "ivan"})` (the `Username != "ivan"` assertion at line 62 keeps passing).

`cmd/piper-relay/main.go` verifier wiring (replaces the Google block; note `NewAutoApproveVerifier` now takes a login):

```go
	// Self-service login needs a GitHub OAuth app; without one the relay runs
	// operator-enroll-only (existing behaviour) and login completes only via
	// test approval.
	var v relay.Verifier
	if id := env("PIPER_RELAY_GITHUB_CLIENT_ID", ""); id != "" {
		v = relay.NewGitHubVerifier(id, env("PIPER_RELAY_GITHUB_CLIENT_SECRET", ""))
	} else if env("PIPER_RELAY_FAKE_APPROVE", "") == "1" {
		log.Print("piper-relay: PIPER_RELAY_FAKE_APPROVE=1 — device login auto-approves (TEST ONLY)")
		v = relay.NewAutoApproveVerifier("e2e-sub", "e2e")
	} else {
		log.Print("piper-relay: no PIPER_RELAY_GITHUB_CLIENT_ID; self-service login disabled")
		v = relay.NewFakeVerifier() // login routes exist but complete only via test approval
	}
```

Then delete `internal/relay/verifier_google.go` (`git rm internal/relay/verifier_google.go`).

- [ ] **Step 2: Write the failing GitHub verifier tests**

Create `internal/relay/verifier_github_test.go`:

```go
package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeGitHub fakes github.com (device code + token) and api.github.com (/user)
// on one httptest server. Poll responses are scripted via tokenResponses.
type fakeGitHub struct {
	t *testing.T

	mu             sync.Mutex
	tokenResponses []map[string]any // popped one per access_token poll
	tokenForms     []map[string]string
	userCalls      int
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("client_id") != "test-client" {
			f.t.Errorf("device/code client_id = %q", r.FormValue("client_id"))
		}
		if r.FormValue("scope") != "" {
			f.t.Errorf("device/code sent scope %q, want none", r.FormValue("scope"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-1", "user_code": "ABCD-1234",
			"verification_uri": "https://github.test/login/device",
			"expires_in":       900, "interval": 5,
		})
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for k := range r.Form {
			form[k] = r.FormValue(k)
		}
		f.mu.Lock()
		f.tokenForms = append(f.tokenForms, form)
		var resp map[string]any
		if len(f.tokenResponses) > 0 {
			resp = f.tokenResponses[0]
			f.tokenResponses = f.tokenResponses[1:]
		} else {
			resp = map[string]any{"error": "authorization_pending"}
		}
		f.mu.Unlock()
		// GitHub returns poll errors in 200-OK bodies.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gho_tok" {
			f.t.Errorf("/user Authorization = %q", got)
		}
		f.mu.Lock()
		f.userCalls++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 583231, "login": "Octo-Cat"})
	})
	return mux
}

// newTestGitHubVerifier points a GitHubVerifier at the fake and makes sleeps
// instant, recording requested durations.
func newTestGitHubVerifier(t *testing.T, fake *fakeGitHub) (*GitHubVerifier, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	v := NewGitHubVerifier("test-client", "test-secret")
	v.oauthBase = srv.URL
	v.apiBase = srv.URL
	var slept []time.Duration
	var mu sync.Mutex
	v.sleep = func(d time.Duration) { mu.Lock(); slept = append(slept, d); mu.Unlock() }
	return v, &slept
}

// waitDone polls the verifier until the flow completes or times out.
func waitDone(t *testing.T, v *GitHubVerifier, handle string) (Identity, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		id, err := v.Poll(context.Background(), handle)
		if err != ErrAuthPending {
			return id, err
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("flow never completed")
	return Identity{}, nil
}

func TestGitHubDeviceFlowApproved(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "authorization_pending"},
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v, _ := newTestGitHubVerifier(t, fake)

	handle, da, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if da.UserCode != "ABCD-1234" || da.VerificationURI != "https://github.test/login/device" ||
		da.Interval != 5 || da.ExpiresIn != 900 {
		t.Fatalf("DeviceAuth = %+v", da)
	}

	id, err := waitDone(t, v, handle)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if id.Subject != "583231" || id.Login != "Octo-Cat" {
		t.Fatalf("identity = %+v", id)
	}
	// The poll used the device grant.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.tokenForms) == 0 ||
		fake.tokenForms[0]["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" ||
		fake.tokenForms[0]["device_code"] != "dc-1" {
		t.Fatalf("token forms = %+v", fake.tokenForms)
	}
}

func TestGitHubDeviceFlowSlowDown(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "slow_down"},
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v, slept := newTestGitHubVerifier(t, fake)

	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := waitDone(t, v, handle); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// First sleep at the server interval (5s), then slow_down adds 5s (GitHub semantics).
	if len(*slept) < 2 || (*slept)[0] != 5*time.Second || (*slept)[1] != 10*time.Second {
		t.Fatalf("sleeps = %v, want [5s 10s ...]", *slept)
	}
}

func TestGitHubDeviceFlowDenied(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "access_denied"},
	}}
	v, _ := newTestGitHubVerifier(t, fake)

	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := waitDone(t, v, handle); err == nil || err == ErrAuthPending {
		t.Fatalf("denied flow err = %v, want terminal error", err)
	}
}

func TestGitHubVerifierPollUnknownHandle(t *testing.T) {
	v := NewGitHubVerifier("test-client", "test-secret")
	if _, err := v.Poll(context.Background(), "never-started"); err == nil {
		t.Fatal("Poll(unknown) succeeded, want error")
	}
}
```

- [ ] **Step 3: Run the new tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestGitHub' -count=1 -v`
Expected: FAIL to compile — `undefined: GitHubVerifier`, `undefined: NewGitHubVerifier`.

- [ ] **Step 4: Implement the GitHub verifier**

Create `internal/relay/verifier_github.go`:

```go
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHubVerifier brokers GitHub's OAuth device authorization grant and, for the
// browser, the authorization-code exchange. It holds the relay's GitHub client
// secret so callers never see it. GitHub's device flow returns no ID token —
// identity comes from GET /user with the granted access token, which is used
// once and discarded. Each Start spawns a background goroutine that polls
// GitHub until the user approves, the code expires, or the process exits; Poll
// reports progress without blocking.
type GitHubVerifier struct {
	clientID, clientSecret string
	oauthBase              string // https://github.com; tests override
	apiBase                string // https://api.github.com; tests override
	httpc                  *http.Client
	sleep                  func(time.Duration) // poll delay seam; tests override

	mu    sync.Mutex
	flows map[string]*githubFlow
}

type githubFlow struct {
	done bool
	id   Identity
	err  error
}

func NewGitHubVerifier(clientID, clientSecret string) *GitHubVerifier {
	return &GitHubVerifier{
		clientID:     clientID,
		clientSecret: clientSecret,
		oauthBase:    "https://github.com",
		apiBase:      "https://api.github.com",
		httpc:        &http.Client{Timeout: 15 * time.Second},
		sleep:        time.Sleep,
		flows:        map[string]*githubFlow{},
	}
}

// deviceCodeResponse / tokenResponse mirror GitHub's JSON. GitHub reports poll
// errors ("authorization_pending", "slow_down", ...) as fields in 200-OK
// bodies, not RFC-style 4xx responses.
type githubTokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
}

func (g *GitHubVerifier) Start(ctx context.Context) (string, DeviceAuth, error) {
	var res struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
	}
	err := g.postForm(ctx, g.oauthBase+"/login/device/code",
		url.Values{"client_id": {g.clientID}}, &res)
	if err != nil {
		return "", DeviceAuth{}, err
	}
	if res.Error != "" || res.DeviceCode == "" {
		return "", DeviceAuth{}, fmt.Errorf("github device code: %q", res.Error)
	}

	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	handle := hex.EncodeToString(raw)

	fl := &githubFlow{}
	g.mu.Lock()
	g.flows[handle] = fl
	g.mu.Unlock()

	go g.pollUntilDone(res.DeviceCode, res.Interval, res.ExpiresIn, fl)

	return handle, DeviceAuth{
		UserCode:        res.UserCode,
		VerificationURI: res.VerificationURI,
		Interval:        res.Interval,
		ExpiresIn:       res.ExpiresIn,
	}, nil
}

// pollUntilDone polls GitHub's token endpoint at the server-given interval,
// stretching by 5s on slow_down (GitHub's documented semantics), until the
// grant resolves or the device code's lifetime elapses.
func (g *GitHubVerifier) pollUntilDone(deviceCode string, interval, expiresIn int, fl *githubFlow) {
	finish := func(id Identity, err error) {
		g.mu.Lock()
		fl.done, fl.id, fl.err = true, id, err
		g.mu.Unlock()
	}
	if interval <= 0 {
		interval = 5
	}
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	ctx := context.Background()
	for {
		if time.Now().After(deadline) {
			finish(Identity{}, errors.New("device code expired"))
			return
		}
		g.sleep(time.Duration(interval) * time.Second)

		var tr githubTokenResponse
		err := g.postForm(ctx, g.oauthBase+"/login/oauth/access_token", url.Values{
			"client_id":   {g.clientID},
			"device_code": {deviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}, &tr)
		if err != nil {
			finish(Identity{}, err)
			return
		}
		switch tr.Error {
		case "":
			if tr.AccessToken == "" {
				finish(Identity{}, errors.New("github token response missing access_token"))
				return
			}
			finish(g.fetchUser(ctx, tr.AccessToken))
			return
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5
			continue
		default: // expired_token, access_denied, incorrect_device_code, ...
			finish(Identity{}, fmt.Errorf("github device flow: %s", tr.Error))
			return
		}
	}
}

// fetchUser resolves an access token to the GitHub identity behind it.
func (g *GitHubVerifier) fetchUser(ctx context.Context, token string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiBase+"/user", nil)
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return Identity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("github /user: status %d", resp.StatusCode)
	}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return Identity{}, err
	}
	if u.ID == 0 || u.Login == "" {
		return Identity{}, errors.New("github /user: missing id or login")
	}
	return Identity{Subject: strconv.FormatInt(u.ID, 10), Login: u.Login}, nil
}

// postForm POSTs a form and decodes the JSON response into out. GitHub encodes
// protocol errors inside 200-OK bodies, so only transport/HTTP-level failures
// are errors here; callers inspect the decoded error field.
func (g *GitHubVerifier) postForm(ctx context.Context, u string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github: POST %s: status %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *GitHubVerifier) Poll(_ context.Context, handle string) (Identity, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fl, ok := g.flows[handle]
	if !ok {
		return Identity{}, errors.New("unknown handle")
	}
	if !fl.done {
		return Identity{}, ErrAuthPending
	}
	return fl.id, fl.err
}
```

- [ ] **Step 5: Run the relay + cmd tests**

Run: `go test ./internal/relay/ ./cmd/... -count=1`
Expected: PASS.

- [ ] **Step 6: Drop the dead dependencies**

Run: `go mod tidy`, then `grep -E "go-oidc|golang.org/x/oauth2" go.mod` — neither should remain as a **direct** dependency (an `// indirect` `x/oauth2` line may survive via other modules; that's fine).
Run: `go build ./...`
Expected: builds clean.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "feat(relay): GitHub device-flow verifier replaces Google

Identity is now {github numeric id, login}; the relay polls GitHub's
device grant server-side and resolves identity via GET /user (no OIDC,
no scopes). go-oidc and direct x/oauth2 deps drop out.

Part of #99

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 3: Web-flow verifier methods (`AuthCodeURL` / `Exchange`)

**Files:**
- Modify: `internal/relay/verifier.go` (add `WebVerifier` interface; extend `FakeVerifier`)
- Modify: `internal/relay/verifier_github.go` (add the two methods)
- Test: `internal/relay/verifier_github_test.go`, `internal/relay/verifier_test.go`

**Interfaces:**
- Consumes: `GitHubVerifier` fields/helpers from Task 2 (`oauthBase`, `clientID`, `clientSecret`, `postForm`, `fetchUser`, `githubTokenResponse`).
- Produces:
  - `type WebVerifier interface { AuthCodeURL(state string) string; Exchange(ctx context.Context, code string) (Identity, error) }`
  - `*GitHubVerifier` and `*FakeVerifier` both implement it.
  - Test helper on the fake: `func (f *FakeVerifier) GrantCode(code string, id Identity)` — Task 4's API tests call this.

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/verifier_github_test.go`:

```go
func TestGitHubAuthCodeURL(t *testing.T) {
	v := NewGitHubVerifier("test-client", "test-secret")
	got := v.AuthCodeURL("state-123")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("AuthCodeURL not a URL: %v", err)
	}
	if u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
		t.Fatalf("authorize URL = %q", got)
	}
	q := u.Query()
	if q.Get("client_id") != "test-client" || q.Get("state") != "state-123" {
		t.Fatalf("authorize query = %q", u.RawQuery)
	}
	if q.Get("scope") != "" {
		t.Fatalf("authorize URL carries scope %q, want none", q.Get("scope"))
	}
}

func TestGitHubExchange(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v, _ := newTestGitHubVerifier(t, fake)

	id, err := v.Exchange(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "583231" || id.Login != "Octo-Cat" {
		t.Fatalf("identity = %+v", id)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	f := fake.tokenForms[0]
	if f["client_id"] != "test-client" || f["client_secret"] != "test-secret" || f["code"] != "code-1" {
		t.Fatalf("exchange form = %+v", f)
	}
}

func TestGitHubExchangeBadCode(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "bad_verification_code"},
	}}
	v, _ := newTestGitHubVerifier(t, fake)
	if _, err := v.Exchange(context.Background(), "nope"); err == nil {
		t.Fatal("Exchange(bad code) succeeded, want error")
	}
}
```

Add `"net/url"` to that file's imports.

Append to `internal/relay/verifier_test.go`:

```go
func TestFakeVerifierWebFlow(t *testing.T) {
	f := NewFakeVerifier()

	if got := f.AuthCodeURL("st-1"); !strings.Contains(got, "state=st-1") {
		t.Fatalf("AuthCodeURL = %q, want state embedded", got)
	}

	if _, err := f.Exchange(context.Background(), "unknown-code"); err == nil {
		t.Fatal("Exchange(unknown) succeeded, want error")
	}

	f.GrantCode("code-1", Identity{Subject: "sub-1", Login: "heidi"})
	id, err := f.Exchange(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Login != "heidi" {
		t.Fatalf("identity = %+v", id)
	}

	// Both verifiers satisfy WebVerifier.
	var _ WebVerifier = f
	var _ WebVerifier = NewGitHubVerifier("id", "secret")
}
```

Add `"strings"` to that file's imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestGitHubAuthCodeURL|TestGitHubExchange|TestFakeVerifierWebFlow' -count=1 -v`
Expected: FAIL to compile — `undefined: WebVerifier`, missing methods.

- [ ] **Step 3: Implement**

In `internal/relay/verifier.go`, after the `Verifier` interface:

```go
// WebVerifier brokers the browser authorization-code flow with the identity
// provider: AuthCodeURL is where /v1/login/web redirects the browser, and
// Exchange resolves the code GitHub posts back to /v1/login/callback.
type WebVerifier interface {
	AuthCodeURL(state string) string
	Exchange(ctx context.Context, code string) (Identity, error)
}
```

Extend `FakeVerifier` (new field + methods; initialize the map in `NewFakeVerifier`):

```go
type FakeVerifier struct {
	mu       sync.Mutex
	approved map[string]Identity
	started  map[string]bool
	codes    map[string]Identity // web-flow codes granted via GrantCode
	auto     *Identity           // when set, Poll auto-approves any started handle (test-only)
}

func NewFakeVerifier() *FakeVerifier {
	return &FakeVerifier{
		approved: map[string]Identity{},
		started:  map[string]bool{},
		codes:    map[string]Identity{},
	}
}

func (f *FakeVerifier) AuthCodeURL(state string) string {
	return "https://github.example.test/login/oauth/authorize?state=" + url.QueryEscape(state)
}

func (f *FakeVerifier) Exchange(_ context.Context, code string) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.codes[code]; ok {
		return id, nil
	}
	return Identity{}, errors.New("bad code")
}

// GrantCode makes a web-flow code exchangeable for id (test helper).
func (f *FakeVerifier) GrantCode(code string, id Identity) {
	f.mu.Lock()
	f.codes[code] = id
	f.mu.Unlock()
}
```

Add `"net/url"` to `verifier.go`'s imports.

In `internal/relay/verifier_github.go`:

```go
// AuthCodeURL is the GitHub authorize URL for the browser flow. No
// redirect_uri parameter: the OAuth app's single registered callback URL
// (the relay's /v1/login/callback) is used.
func (g *GitHubVerifier) AuthCodeURL(state string) string {
	return g.oauthBase + "/login/oauth/authorize?client_id=" +
		url.QueryEscape(g.clientID) + "&state=" + url.QueryEscape(state)
}

// Exchange resolves an authorization code to the GitHub identity behind it.
func (g *GitHubVerifier) Exchange(ctx context.Context, code string) (Identity, error) {
	var tr githubTokenResponse
	err := g.postForm(ctx, g.oauthBase+"/login/oauth/access_token", url.Values{
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"code":          {code},
	}, &tr)
	if err != nil {
		return Identity{}, err
	}
	if tr.Error != "" || tr.AccessToken == "" {
		return Identity{}, fmt.Errorf("github code exchange: %q", tr.Error)
	}
	return g.fetchUser(ctx, tr.AccessToken)
}
```

Note: `TestGitHubAuthCodeURL` asserts host `github.com` — it uses `NewGitHubVerifier` directly (no base override), so the default `oauthBase` satisfies it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/verifier.go internal/relay/verifier_github.go \
  internal/relay/verifier_github_test.go internal/relay/verifier_test.go
git commit -m "feat(relay): authorization-code exchange on the GitHub verifier

Part of #99

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 4: Relay web-login endpoints

**Files:**
- Modify: `internal/relay/api.go`
- Test: `internal/relay/api_test.go`
- Modify: `internal/relay/proxy_test.go:81` (call-site arity)
- Modify: `cmd/piper-relay/main.go` (call-site signature + `PIPER_RELAY_WEB_REDIRECTS`)

**Interfaces:**
- Consumes: `WebVerifier`, `FakeVerifier.GrantCode` (Task 3); `UpsertAccount`/`MintAccountCredential` (Task 1).
- Produces:
  - `func NewAPIWithTunnel(st *Store, v Verifier, tunnelEndpoint string, router *Router, webRedirects []string) http.Handler` — **breaking signature change**; `NewAPI(st, v)` keeps its shape (passes `nil`).
  - `GET /v1/login/web?redirect_uri=…` → 302 to the IdP (sets `piper_login_state` cookie) | 400 disallowed redirect | 503 not configured.
  - `GET /v1/login/callback?code&state` → 302 to `{redirect_uri}#credential=…&username=…` | 400 bad state | 502 exchange failure | 503 not configured.

- [ ] **Step 1: Write the failing API tests**

Append to `internal/relay/api_test.go`:

```go
// startWebLogin drives GET /v1/login/web and returns the minted state and the
// state cookie. The FakeVerifier's AuthCodeURL embeds the state, so it's
// recoverable from the redirect Location.
func startWebLogin(t *testing.T, api http.Handler, redirectURI string) (state string, cookie *http.Cookie) {
	t.Helper()
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape(redirectURI), nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("web login status = %d, body = %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	state = loc.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in redirect %q", loc)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "piper_login_state" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no piper_login_state cookie set")
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags = %+v, want HttpOnly Secure SameSite=Lax", cookie)
	}
	return state, cookie
}

func newWebTestAPI(t *testing.T) (http.Handler, *FakeVerifier) {
	t.Helper()
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	fv := NewFakeVerifier()
	api := NewAPIWithTunnel(st, fv, "", nil, []string{"https://dash.getpiper.co/"})
	return api, fv
}

func TestWebLoginCallbackHappyPath(t *testing.T) {
	api, fv := newWebTestAPI(t)
	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")

	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})
	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body = %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != "https://dash.getpiper.co/auth" {
		t.Fatalf("redirect target = %q", got)
	}
	frag, err := url.ParseQuery(loc.Fragment)
	if err != nil {
		t.Fatalf("bad fragment %q: %v", loc.Fragment, err)
	}
	if frag.Get("credential") == "" || frag.Get("username") != "ivan" {
		t.Fatalf("fragment = %q", loc.Fragment)
	}
}

func TestWebLoginRejectsDisallowedRedirect(t *testing.T) {
	api, _ := newWebTestAPI(t)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape("https://evil.example/auth"), nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("disallowed redirect status = %d, want 400", rr.Code)
	}
}

func TestWebLoginCallbackStateSingleUse(t *testing.T) {
	api, fv := newWebTestAPI(t)
	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")
	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})

	do := func() int {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		api.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := do(); c != http.StatusFound {
		t.Fatalf("first callback = %d, want 302", c)
	}
	if c := do(); c != http.StatusBadRequest {
		t.Fatalf("replayed callback = %d, want 400", c)
	}
}

func TestWebLoginCallbackRejectsCookieMismatch(t *testing.T) {
	api, fv := newWebTestAPI(t)
	state, _ := startWebLogin(t, api, "https://dash.getpiper.co/auth")
	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})

	// No cookie at all.
	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("cookieless callback = %d, want 400", rr.Code)
	}

	// Wrong cookie value.
	req = httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	req.AddCookie(&http.Cookie{Name: "piper_login_state", Value: "someone-else"})
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("wrong-cookie callback = %d, want 400", rr.Code)
	}
}

func TestWebLoginCallbackExchangeFailure(t *testing.T) {
	api, _ := newWebTestAPI(t) // no GrantCode → Exchange fails
	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=bad&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("failed-exchange callback = %d, want 502", rr.Code)
	}
}

func TestWebLoginNotConfigured(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	api := NewAPI(st, NewFakeVerifier()) // no webRedirects

	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape("https://dash.getpiper.co/auth"), nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured web login = %d, want 503", rr.Code)
	}
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/login/callback?code=x&state=y", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured callback = %d, want 503", rr.Code)
	}
}
```

Add `"net/url"` to `api_test.go`'s imports. Also update the existing call sites for the new arity:
- `internal/relay/api_test.go:80,114,128`: `NewAPIWithTunnel(st, NewFakeVerifier(), "relay.getpiper.co:7000", nil)` → `NewAPIWithTunnel(st, NewFakeVerifier(), "relay.getpiper.co:7000", nil, nil)` (and the `"relay:7000"` variants likewise).
- `internal/relay/proxy_test.go:81`: `NewAPIWithTunnel(st, NewFakeVerifier(), "", router)` → `NewAPIWithTunnel(st, NewFakeVerifier(), "", router, nil)`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestWebLogin' -count=1 -v`
Expected: FAIL to compile — `NewAPIWithTunnel` arity, then (after arity fix attempt) 404s from missing routes.

- [ ] **Step 3: Implement the endpoints**

Rewrite `internal/relay/api.go`'s constructor/struct and add the handlers:

```go
// NewAPI returns the account API without a tunnel endpoint, control proxy, or
// web login (tests / LAN). Use NewAPIWithTunnel in production.
func NewAPI(st *Store, v Verifier) http.Handler { return NewAPIWithTunnel(st, v, "", nil, nil) }

// NewAPIWithTunnel is the full account-facing API: device login, browser
// (authorization-code) login, enroll, and — when router is non-nil — the
// /agents/ control proxy (#73). webRedirects is the allowlist of redirect_uri
// prefixes for the browser flow; empty disables web login (503).
func NewAPIWithTunnel(st *Store, v Verifier, tunnelEndpoint string, router *Router, webRedirects []string) http.Handler {
	a := &api{st: st, v: v, tunnelEndpoint: tunnelEndpoint,
		webRedirects: webRedirects, webStates: map[string]webState{}}
	if wv, ok := v.(WebVerifier); ok {
		a.webv = wv
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/login/device", a.loginDevice)
	mux.HandleFunc("POST /v1/login/poll", a.loginPoll)
	mux.HandleFunc("GET /v1/login/web", a.loginWeb)
	mux.HandleFunc("GET /v1/login/callback", a.loginCallback)
	mux.HandleFunc("POST /v1/enroll", a.enroll)
	if router != nil {
		mux.Handle("/agents/", NewControlProxy(st, router))
	}
	return mux
}

type api struct {
	st             *Store
	v              Verifier
	webv           WebVerifier // nil ⇒ web login disabled
	tunnelEndpoint string
	webRedirects   []string // allowed redirect_uri prefixes; empty ⇒ web login disabled

	mu        sync.Mutex
	webStates map[string]webState // state → pending browser flow
}

// webState is a pending browser login: where to send the credential, and how
// long the state stays redeemable.
type webState struct {
	redirectURI string
	expires     time.Time
}

const stateCookie = "piper_login_state"

// webLoginEnabled gates both browser endpoints: a WebVerifier must be wired
// and at least one redirect prefix allowed.
func (a *api) webLoginEnabled() bool { return a.webv != nil && len(a.webRedirects) > 0 }

func (a *api) redirectAllowed(uri string) bool {
	if uri == "" {
		return false
	}
	for _, p := range a.webRedirects {
		if strings.HasPrefix(uri, p) {
			return true
		}
	}
	return false
}

// loginWeb starts the browser flow: bind a fresh state to the validated
// redirect_uri (server-side map + browser cookie), then hand the browser to
// the IdP.
func (a *api) loginWeb(w http.ResponseWriter, r *http.Request) {
	if !a.webLoginEnabled() {
		http.Error(w, "web login not configured", http.StatusServiceUnavailable)
		return
	}
	ru := r.URL.Query().Get("redirect_uri")
	if !a.redirectAllowed(ru) {
		http.Error(w, "redirect_uri not allowed", http.StatusBadRequest)
		return
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	state := hex.EncodeToString(raw)
	a.mu.Lock()
	a.webStates[state] = webState{redirectURI: ru, expires: time.Now().Add(10 * time.Minute)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, MaxAge: 600, Path: "/v1/login",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.webv.AuthCodeURL(state), http.StatusFound)
}

// loginCallback finishes the browser flow: state must match both the
// server-side map (single-use, unexpired) and the browser's cookie
// (login-CSRF guard); then code → identity → account → credential, delivered
// in the URL fragment so it never reaches server logs.
func (a *api) loginCallback(w http.ResponseWriter, r *http.Request) {
	if !a.webLoginEnabled() {
		http.Error(w, "web login not configured", http.StatusServiceUnavailable)
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	c, err := r.Cookie(stateCookie)
	if state == "" || code == "" || err != nil || c.Value != state {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	ws, ok := a.webStates[state]
	delete(a.webStates, state) // single use
	a.mu.Unlock()
	if !ok || time.Now().After(ws.expires) {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	id, err := a.webv.Exchange(r.Context(), code)
	if err != nil {
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}
	acc, err := a.st.UpsertAccount(id.Subject, id.Login)
	if err != nil {
		http.Error(w, "account error", http.StatusInternalServerError)
		return
	}
	cred, err := a.st.MintAccountCredential(acc.ID)
	if err != nil {
		http.Error(w, "credential error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r,
		ws.redirectURI+"#credential="+url.QueryEscape(cred)+"&username="+url.QueryEscape(acc.Username),
		http.StatusFound)
}
```

Add `"crypto/rand"`, `"encoding/hex"`, `"net/url"`, `"strings"`, `"sync"`, `"time"` to `api.go`'s imports as needed. `loginDevice`, `loginPoll`, `enroll`, `bearerToken`, `writeJSON` are untouched.

Update `cmd/piper-relay/main.go`:

```go
	// Browser (dashboard) login: allowed redirect_uri prefixes, comma-separated.
	// Empty — or a missing client secret — leaves web login disabled (503).
	var webRedirects []string
	for _, p := range strings.Split(env("PIPER_RELAY_WEB_REDIRECTS", ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			webRedirects = append(webRedirects, p)
		}
	}
	if len(webRedirects) > 0 && env("PIPER_RELAY_GITHUB_CLIENT_SECRET", "") == "" {
		log.Print("piper-relay: PIPER_RELAY_WEB_REDIRECTS set but no PIPER_RELAY_GITHUB_CLIENT_SECRET; web login disabled")
		webRedirects = nil
	}

	router := relay.NewRouter()
	apiHandler := relay.NewAPIWithTunnel(st, v, tunnelPublic, router, webRedirects)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ ./cmd/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/api.go internal/relay/api_test.go cmd/piper-relay/main.go
git commit -m "feat(relay): browser authorization-code login endpoints

GET /v1/login/web validates redirect_uri against PIPER_RELAY_WEB_REDIRECTS,
binds a single-use state (server map + CSRF cookie), and hands the browser
to GitHub; GET /v1/login/callback exchanges the code and returns the account
credential to the dashboard in the URL fragment.

Part of #99

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 5: Docs sweep, PROGRESS.md, full verify

**Files:**
- Modify: `README.md:68,96`
- Modify: `internal/relayclient/relayclient.go:2` (comment)
- Modify: `cmd/piper/relayonboard.go:20` (comment)
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: everything landed in Tasks 1–4.
- Produces: no code — documentation + the green `make verify` gate.

- [ ] **Step 1: Sweep the Google mentions**

- `README.md:68`: `piper login          # opens a Google device-flow login; stores your account credential` → `piper login          # opens a GitHub device-flow login; stores your account credential`
- `README.md:96`: `piper login                  # Google device-flow; stores your account credential` → `piper login                  # GitHub device-flow; stores your account credential`
- `internal/relayclient/relayclient.go:2`: “the Google device-flow login” → “the GitHub device-flow login”
- `cmd/piper/relayonboard.go:20`: “relayLogin runs the Google device flow against the relay” → “relayLogin runs the GitHub device flow against the relay”

Check for stragglers: `grep -rn -i google --include="*.go" --include="*.md" . | grep -v docs/superpowers | grep -v github.com/google/uuid` → only historical spec/plan docs may remain.

- [ ] **Step 2: Update PROGRESS.md**

Under the public-relay onboarding section (near line 42), add one line:

```markdown
  - ✅ GitHub identity — relay accounts on GitHub OAuth (device flow for `piper login`, relay-hosted authorization-code flow for the browser); Google flow removed — [#99](https://github.com/piperbox/piper/issues/99)
```

- [ ] **Step 3: Run the full verify gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests pass, arm64 cross-build succeeds. Fix anything it flags before committing.

- [ ] **Step 4: Commit**

```bash
git add README.md internal/relayclient/relayclient.go cmd/piper/relayonboard.go PROGRESS.md
git commit -m "docs: GitHub identity sweep — README, comments, PROGRESS

Part of #99

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

- [ ] **Step 5: Open the PR**

```bash
git push -u origin ozykhan/github-identity
gh pr create --base main --title "[relay] GitHub identity: device + web login flows replace Google" --body "$(cat <<'EOF'
Switches relay account identity from Google to a getpiper-owned GitHub OAuth app, per docs/superpowers/specs/2026-07-09-github-identity-design.md:

- `GitHubVerifier` (plain HTTP, no scopes): server-side device flow for `piper login` — unchanged wire shape, so the CLI didn't change; identity from `GET /user` (numeric id + login).
- Browser flow: `GET /v1/login/web` (redirect_uri allowlist via `PIPER_RELAY_WEB_REDIRECTS`, single-use state + CSRF cookie) → GitHub → `GET /v1/login/callback` → credential delivered in the URL fragment.
- Usernames derive from the GitHub login; `accounts.google_sub` → `github_id` (no migration — pre-release).
- Google verifier deleted; `go-oidc` and direct `x/oauth2` deps dropped.

**Hosted-relay deploy notes:** create the GitHub OAuth app (device flow enabled; callback `https://api.public.getpiper.co/v1/login/callback`), set `PIPER_RELAY_GITHUB_CLIENT_ID`/`PIPER_RELAY_GITHUB_CLIENT_SECRET` (+ `PIPER_RELAY_WEB_REDIRECTS` when the dashboard exists), and drop the `accounts`/`account_creds` tables (`agents` untouched).

Closes #99

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

[#99]: https://github.com/piperbox/piper/issues/99
