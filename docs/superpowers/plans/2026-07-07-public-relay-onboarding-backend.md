# Public Relay Onboarding — Plan 1: Relay accounts, OAuth device-flow & self-service enrollment

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `piper-relay` a self-service backend: a caller authenticates with Google (OAuth device flow), the relay creates a tenant **account**, hands back an **account credential**, and mints an **account-bound enrollment token** for a box — with per-account caps and an operator kill-switch. No operator `piper-relay enroll` required.

**Architecture:** Extend the existing `internal/relay` package (today a lean SNI-passthrough server keyed by `base_domain`) with a SQLite `accounts` layer and a small HTTP control API. Google is reached through a `Verifier` interface so all logic is unit-tested against a fake IdP; the real Google device flow is a thin adapter over `golang.org/x/oauth2` + `github.com/coreos/go-oidc/v3`. The existing tunnel/router/SNI path is **untouched** — a self-enrolled agent gets a relay-assigned single-label base domain and behaves like any Plan-2 agent until Plan 3 adds relay-terminated app TLS.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure-Go driver), `golang.org/x/oauth2` + `golang.org/x/oauth2/google`, `github.com/coreos/go-oidc/v3/oidc`, `github.com/google/uuid`, stdlib `net/http` + `net/http/httptest`.

This is **Plan 1 of 3** for the slice specced in [`docs/superpowers/specs/2026-07-07-public-relay-onboarding-design.md`](../specs/2026-07-07-public-relay-onboarding-design.md). Plan 2 = `piper login`/`piper connect` CLI; Plan 3 = assigned hostnames + relay-terminated wildcard app path.

## Global Constraints

- **No cgo.** All builds pass with `CGO_ENABLED=0`; SQLite is `modernc.org/sqlite` only. `make cross` (arm64) must stay green.
- **Module path** `github.com/piperbox/piper`.
- **TDD.** Every task is failing-test-first. Run `make test` before each commit; it must pass.
- **Layering.** `relay` is cloud-side and imports only `internal/tunnel` + stdlib + the OAuth/OIDC libs. It must not import `store`, `deploy`, `api`, `runtime`, or `caddy`.
- **Dependencies already vendored** (indirect today; these tasks promote them to direct — no `go get` of new modules): `golang.org/x/oauth2 v0.36.0`, `github.com/coreos/go-oidc/v3 v3.17.0`, `github.com/google/uuid v1.6.0`.
- **Secrets are hashed at rest.** Account credentials and enrollment tokens are stored only as `sha256` hex, matching the existing `hashToken` pattern in `internal/relay/store.go`.
- **Free-tier apex default** `public.getpiper.co`; **default per-account agent cap** `3`.
- Commit messages are conventional-commit style ending with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`, and reference `Part of #49`.

## File Structure

- Modify `internal/relay/schema.sql` — add `accounts` + `account_creds` tables; `agents.account_id`.
- Modify `internal/relay/store.go` — guarded migration for `agents.account_id`; `Apex`/`MaxAgents` fields + `Configure`; `Authenticate` rejects agents of disabled accounts.
- Create `internal/relay/accounts.go` — account + credential + account-bound enrollment store methods; username derivation; kill-switch.
- Create `internal/relay/accounts_test.go` — tests for the above.
- Create `internal/relay/verifier.go` — `Verifier` interface, `DeviceAuth`/`Identity` types, `ErrAuthPending`, and an in-package fake.
- Create `internal/relay/verifier_google.go` — Google adapter over `oauth2` + `oidc` (built-tag-free; network calls only in the real methods).
- Create `internal/relay/api.go` — `NewAPI(*Store, Verifier) http.Handler`: `/v1/login/device`, `/v1/login/poll`, `/v1/enroll`.
- Create `internal/relay/api_test.go` — httptest coverage of the three routes against the fake verifier.
- Modify `cmd/piper-relay/main.go` — construct the Google verifier from env, serve the API, add an `admin disable` subcommand.

---

### Task 1: `accounts` schema + account upsert with unique username

**Files:**
- Modify: `internal/relay/schema.sql`
- Modify: `internal/relay/store.go` (migration + fields)
- Create: `internal/relay/accounts.go`
- Create: `internal/relay/accounts_test.go`

**Interfaces:**
- Consumes: existing `Store{db *sql.DB}`, `hashToken`, `Open` from `store.go`.
- Produces:
  - `type Account struct { ID, Username string; Disabled bool }`
  - `func (s *Store) UpsertAccount(googleSub, email string) (Account, error)` — idempotent by `googleSub`; derives a unique DNS-safe username on first sight.
  - `func deriveUsername(email string) string` — local-part → lowercase `[a-z0-9-]`, trimmed, capped at 30 chars.

- [ ] **Step 1: Write the failing test**

Add to `internal/relay/accounts_test.go`:

```go
package relay

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestUpsertAccountIsIdempotentBySub(t *testing.T) {
	st := openTestStore(t)

	a1, err := st.UpsertAccount("google-sub-1", "Alice.Smith@gmail.com")
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if a1.Username != "alice-smith" {
		t.Fatalf("username = %q, want alice-smith", a1.Username)
	}
	if a1.ID == "" {
		t.Fatal("empty account id")
	}

	a2, err := st.UpsertAccount("google-sub-1", "Alice.Smith@gmail.com")
	if err != nil {
		t.Fatalf("second UpsertAccount: %v", err)
	}
	if a2.ID != a1.ID {
		t.Fatalf("re-upsert made a new account: %s != %s", a2.ID, a1.ID)
	}
}

func TestUpsertAccountDisambiguatesUsername(t *testing.T) {
	st := openTestStore(t)
	a1, _ := st.UpsertAccount("sub-a", "bob@x.com")
	a2, _ := st.UpsertAccount("sub-b", "bob@y.com")
	if a1.Username != "bob" {
		t.Fatalf("first username = %q, want bob", a1.Username)
	}
	if a2.Username == a1.Username {
		t.Fatalf("second username not disambiguated: %q", a2.Username)
	}
	if a2.Username != "bob-2" {
		t.Fatalf("second username = %q, want bob-2", a2.Username)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestUpsertAccount -v`
Expected: FAIL — compile error, `st.UpsertAccount` undefined.

- [ ] **Step 3: Add schema**

Append to `internal/relay/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS accounts (
    id          TEXT PRIMARY KEY,
    google_sub  TEXT NOT NULL UNIQUE,
    username    TEXT NOT NULL UNIQUE,
    disabled    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS account_creds (
    token_hash  TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    created_at  TEXT NOT NULL
);
```

- [ ] **Step 4: Add the `agents.account_id` migration to `Open`**

In `internal/relay/store.go`, after the `db.Exec(schema)` block in `Open`, insert:

```go
	if err := ensureAgentAccountColumn(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate agents: %w", err)
	}
```

Then add at the bottom of `store.go`:

```go
// ensureAgentAccountColumn adds agents.account_id if an older DB predates it.
// CREATE TABLE IF NOT EXISTS can't alter an existing table, so we add the column
// idempotently and tolerate the "duplicate column" error on already-migrated DBs.
func ensureAgentAccountColumn(db *sql.DB) error {
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
		if name == "account_id" {
			return nil // already migrated
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE agents ADD COLUMN account_id TEXT`)
	return err
}
```

- [ ] **Step 5: Implement account upsert + username derivation**

Create `internal/relay/accounts.go`:

```go
package relay

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Account is a relay tenant. One account owns many agents.
type Account struct {
	ID       string
	Username string
	Disabled bool
}

// deriveUsername turns an email into a DNS-safe label component: the local part,
// lowercased, with every rune outside [a-z0-9-] replaced by '-', trimmed of
// leading/trailing '-', and capped at 30 chars so the eventual
// "<hash>-<username>.<apex>" label stays under DNS's 63-char limit.
func deriveUsername(email string) string {
	local := email
	if i := strings.IndexByte(email, '@'); i >= 0 {
		local = email[:i]
	}
	local = strings.ToLower(local)
	var b strings.Builder
	for _, r := range local {
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

// UpsertAccount returns the account for googleSub, creating it (with a unique
// derived username) on first sight. Idempotent by googleSub.
func (s *Store) UpsertAccount(googleSub, email string) (Account, error) {
	var acc Account
	var disabled int
	err := s.db.QueryRow(`SELECT id, username, disabled FROM accounts WHERE google_sub=?`, googleSub).
		Scan(&acc.ID, &acc.Username, &disabled)
	if err == nil {
		acc.Disabled = disabled != 0
		return acc, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Account{}, err
	}

	base := deriveUsername(email)
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 1; ; i++ {
		username := base
		if i > 1 {
			username = base + "-" + itoa(i)
		}
		_, err := s.db.Exec(
			`INSERT INTO accounts(id, google_sub, username, disabled, created_at) VALUES(?,?,?,0,?)`,
			id, googleSub, username, now)
		if err == nil {
			return Account{ID: id, Username: username}, nil
		}
		if isUniqueViolation(err) {
			// Another account already holds this username; try the next suffix.
			// (A racing insert of the same google_sub is vanishingly unlikely on a
			// single relay; the SELECT above handles the common re-login path.)
			continue
		}
		return Account{}, err
	}
}

func itoa(i int) string {
	// small non-negative ints only; avoids importing strconv for one call site
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/relay/ -run TestUpsertAccount -v`
Expected: PASS (both cases).

- [ ] **Step 7: Commit**

```bash
git add internal/relay/schema.sql internal/relay/store.go internal/relay/accounts.go internal/relay/accounts_test.go
git commit -m "feat(relay): accounts table + Google-sub-keyed upsert with unique username

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Account credentials — mint & authenticate (with kill-switch)

**Files:**
- Modify: `internal/relay/accounts.go`
- Modify: `internal/relay/accounts_test.go`

**Interfaces:**
- Consumes: `Account`, `hashToken`, `Store` from Tasks 1 / `store.go`.
- Produces:
  - `var ErrBadCredential = errors.New("bad credential")`
  - `func (s *Store) MintAccountCredential(accountID string) (string, error)` — returns a plaintext credential once; stores only its hash.
  - `func (s *Store) AuthenticateAccount(cred string) (Account, error)` — resolves a credential to its `Account`; returns `ErrBadCredential` for unknown creds **or** disabled accounts.
  - `func (s *Store) DisableAccount(username string) error`

- [ ] **Step 1: Write the failing test**

Add to `internal/relay/accounts_test.go`:

```go
func TestMintAndAuthenticateCredential(t *testing.T) {
	st := openTestStore(t)
	acc, _ := st.UpsertAccount("sub-1", "carol@x.com")

	cred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatalf("MintAccountCredential: %v", err)
	}
	if cred == "" {
		t.Fatal("empty credential")
	}

	got, err := st.AuthenticateAccount(cred)
	if err != nil {
		t.Fatalf("AuthenticateAccount: %v", err)
	}
	if got.ID != acc.ID || got.Username != acc.Username {
		t.Fatalf("account = %+v, want %+v", got, acc)
	}

	if _, err := st.AuthenticateAccount("nope"); err != ErrBadCredential {
		t.Fatalf("bad cred err = %v, want ErrBadCredential", err)
	}
}

func TestDisabledAccountCredentialRejected(t *testing.T) {
	st := openTestStore(t)
	acc, _ := st.UpsertAccount("sub-1", "dave@x.com")
	cred, _ := st.MintAccountCredential(acc.ID)

	if err := st.DisableAccount(acc.Username); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}
	if _, err := st.AuthenticateAccount(cred); err != ErrBadCredential {
		t.Fatalf("disabled cred err = %v, want ErrBadCredential", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestMintAndAuthenticateCredential|TestDisabledAccountCredentialRejected' -v`
Expected: FAIL — `MintAccountCredential` / `ErrBadCredential` undefined.

- [ ] **Step 3: Implement**

Add to `internal/relay/accounts.go` (add `crypto/rand` and `encoding/hex` to its imports):

```go
// ErrBadCredential is returned for an unknown account credential or one whose
// account has been disabled by the operator kill-switch.
var ErrBadCredential = errors.New("bad credential")

// MintAccountCredential issues a fresh random credential for accountID and stores
// only its hash. The plaintext is returned once, to the caller.
func (s *Store) MintAccountCredential(accountID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	cred := hex.EncodeToString(raw)
	_, err := s.db.Exec(
		`INSERT INTO account_creds(token_hash, account_id, created_at) VALUES(?,?,?)`,
		hashToken(cred), accountID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return cred, nil
}

// AuthenticateAccount resolves a plaintext credential to its Account. A disabled
// account is treated as unauthenticated (ErrBadCredential).
func (s *Store) AuthenticateAccount(cred string) (Account, error) {
	var acc Account
	var disabled int
	err := s.db.QueryRow(
		`SELECT a.id, a.username, a.disabled
		   FROM account_creds c JOIN accounts a ON a.id = c.account_id
		  WHERE c.token_hash = ?`, hashToken(cred)).
		Scan(&acc.ID, &acc.Username, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrBadCredential
	}
	if err != nil {
		return Account{}, err
	}
	if disabled != 0 {
		return Account{}, ErrBadCredential
	}
	acc.Disabled = false
	return acc, nil
}

// DisableAccount flips the kill-switch for an account by username. Its
// credentials stop authenticating and its agents stop connecting.
func (s *Store) DisableAccount(username string) error {
	res, err := s.db.Exec(`UPDATE accounts SET disabled=1 WHERE username=?`, username)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("no such account")
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/relay/ -run 'TestMintAndAuthenticateCredential|TestDisabledAccountCredentialRejected' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/accounts.go internal/relay/accounts_test.go
git commit -m "feat(relay): account credentials + operator kill-switch

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Account-bound enrollment with per-account cap

**Files:**
- Modify: `internal/relay/store.go` (`Apex`/`MaxAgents` fields, `Configure`, `Authenticate` join)
- Modify: `internal/relay/accounts.go` (`EnrollForAccount`)
- Modify: `internal/relay/accounts_test.go`

**Interfaces:**
- Consumes: `Account`, existing `Enroll`/`Authenticate`/`Agent` in `store.go`.
- Produces:
  - `func (s *Store) Configure(apex string, maxAgents int)` — sets the free-tier apex + per-account agent cap.
  - `type Enrollment struct { Token, BaseDomain string }`
  - `var ErrQuotaExceeded = errors.New("account agent quota exceeded")`
  - `func (s *Store) EnrollForAccount(accountID string) (Enrollment, error)` — assigns `<hash>-<username>.<apex>`, binds the agent to the account, enforces the cap.
  - `Authenticate` now rejects agents whose owning account is disabled.

- [ ] **Step 1: Write the failing test**

Add to `internal/relay/accounts_test.go`:

```go
import "strings" // add alongside existing imports if not present

func TestEnrollForAccountAssignsLabelAndBindsAccount(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3)
	acc, _ := st.UpsertAccount("sub-1", "erin@x.com")

	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatalf("EnrollForAccount: %v", err)
	}
	if en.Token == "" {
		t.Fatal("empty enrollment token")
	}
	if !strings.HasSuffix(en.BaseDomain, "-erin.public.getpiper.co") {
		t.Fatalf("base domain = %q, want <hash>-erin.public.getpiper.co", en.BaseDomain)
	}
	// The enrollment token authenticates as an agent bound to this base domain.
	ag, err := st.Authenticate(en.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ag.BaseDomain != en.BaseDomain {
		t.Fatalf("agent base = %q, want %q", ag.BaseDomain, en.BaseDomain)
	}
}

func TestEnrollForAccountEnforcesCap(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 2)
	acc, _ := st.UpsertAccount("sub-1", "frank@x.com")

	for i := 0; i < 2; i++ {
		if _, err := st.EnrollForAccount(acc.ID); err != nil {
			t.Fatalf("enroll %d: %v", i, err)
		}
	}
	if _, err := st.EnrollForAccount(acc.ID); err != ErrQuotaExceeded {
		t.Fatalf("over-cap err = %v, want ErrQuotaExceeded", err)
	}
}

func TestAuthenticateRejectsDisabledAccountAgent(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3)
	acc, _ := st.UpsertAccount("sub-1", "grace@x.com")
	en, _ := st.EnrollForAccount(acc.ID)

	if err := st.DisableAccount(acc.Username); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}
	if _, err := st.Authenticate(en.Token); err != ErrBadToken {
		t.Fatalf("disabled agent auth err = %v, want ErrBadToken", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestEnrollForAccount|TestAuthenticateRejectsDisabled' -v`
Expected: FAIL — `Configure`/`EnrollForAccount`/`ErrQuotaExceeded` undefined.

- [ ] **Step 3: Add config fields + defaults + disabled-account join to `store.go`**

In `internal/relay/store.go`, extend the `Store` struct and add `Configure`:

```go
type Store struct {
	db        *sql.DB
	apex      string
	maxAgents int
}

// Configure sets the free-tier apex domain and the per-account agent cap used by
// EnrollForAccount. Safe to call once after Open.
func (s *Store) Configure(apex string, maxAgents int) {
	s.apex = apex
	s.maxAgents = maxAgents
}

func (s *Store) apexOrDefault() string {
	if s.apex == "" {
		return "public.getpiper.co"
	}
	return s.apex
}

func (s *Store) maxAgentsOrDefault() int {
	if s.maxAgents <= 0 {
		return 3
	}
	return s.maxAgents
}
```

Then replace the `Authenticate` query so a disabled owning account rejects the agent:

```go
// Authenticate resolves a plaintext token to its Agent, or ErrBadToken. An agent
// whose owning account has been disabled is rejected as ErrBadToken.
func (s *Store) Authenticate(token string) (Agent, error) {
	var ag Agent
	var disabled sql.NullInt64
	err := s.db.QueryRow(
		`SELECT ag.name, ag.base_domain, acc.disabled
		   FROM agents ag LEFT JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.token_hash = ?`, hashToken(token)).
		Scan(&ag.Name, &ag.BaseDomain, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrBadToken
	}
	if err != nil {
		return Agent{}, err
	}
	if disabled.Valid && disabled.Int64 != 0 {
		return Agent{}, ErrBadToken
	}
	return ag, nil
}
```

- [ ] **Step 4: Implement `EnrollForAccount`**

Add to `internal/relay/accounts.go`:

```go
// Enrollment is the result of a self-service claim: an enrollment token plus the
// single-label base domain the relay assigned the agent under the apex.
type Enrollment struct {
	Token      string
	BaseDomain string
}

// ErrQuotaExceeded is returned when an account is already at its agent cap.
var ErrQuotaExceeded = errors.New("account agent quota exceeded")

// EnrollForAccount mints an enrollment token for a new agent bound to accountID,
// assigning it "<hash>-<username>.<apex>". Enforces the per-account agent cap.
func (s *Store) EnrollForAccount(accountID string) (Enrollment, error) {
	var username string
	if err := s.db.QueryRow(`SELECT username FROM accounts WHERE id=?`, accountID).Scan(&username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Enrollment{}, ErrBadCredential
		}
		return Enrollment{}, err
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE account_id=?`, accountID).Scan(&count); err != nil {
		return Enrollment{}, err
	}
	if count >= s.maxAgentsOrDefault() {
		return Enrollment{}, ErrQuotaExceeded
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for attempt := 0; attempt < 5; attempt++ {
		hash := make([]byte, 4)
		if _, err := rand.Read(hash); err != nil {
			return Enrollment{}, err
		}
		base := hex.EncodeToString(hash) + "-" + username + "." + s.apexOrDefault()

		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return Enrollment{}, err
		}
		tok := hex.EncodeToString(raw)

		_, err := s.db.Exec(
			`INSERT INTO agents(name, token_hash, base_domain, account_id, created_at) VALUES(?,?,?,?,?)`,
			base, hashToken(tok), base, accountID, now)
		if err == nil {
			return Enrollment{Token: tok, BaseDomain: base}, nil
		}
		if isUniqueViolation(err) {
			continue // hash collided with an existing base_domain; retry
		}
		return Enrollment{}, err
	}
	return Enrollment{}, errors.New("could not assign a unique base domain")
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/relay/ -run 'TestEnrollForAccount|TestAuthenticateRejectsDisabled' -v`
Expected: PASS. Then `go test ./internal/relay/ -v` to confirm the existing `TestEnrollAndAuthenticate` / `TestEnrollRejectsDuplicateBaseDomain` still pass (the `Authenticate` query change is behaviour-preserving for operator-enrolled agents).

- [ ] **Step 6: Commit**

```bash
git add internal/relay/store.go internal/relay/accounts.go internal/relay/accounts_test.go
git commit -m "feat(relay): account-bound enrollment with per-account cap

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: `Verifier` interface + in-package fake IdP

**Files:**
- Create: `internal/relay/verifier.go`
- Create: `internal/relay/verifier_test.go`

**Interfaces:**
- Produces:
  - `type DeviceAuth struct { UserCode, VerificationURI string; Interval, ExpiresIn int }`
  - `type Identity struct { Subject, Email string }`
  - `var ErrAuthPending = errors.New("authorization_pending")`
  - `type Verifier interface { Start(ctx context.Context) (handle string, d DeviceAuth, err error); Poll(ctx context.Context, handle string) (Identity, error) }`
  - `type FakeVerifier struct { ... }` with `func NewFakeVerifier() *FakeVerifier`, and a test control method `func (f *FakeVerifier) Approve(handle string, id Identity)`.

- [ ] **Step 1: Write the failing test**

Create `internal/relay/verifier_test.go`:

```go
package relay

import (
	"context"
	"testing"
)

func TestFakeVerifierStartPollApprove(t *testing.T) {
	f := NewFakeVerifier()
	ctx := context.Background()

	handle, d, err := f.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if handle == "" || d.UserCode == "" || d.VerificationURI == "" {
		t.Fatalf("empty device auth: %q %+v", handle, d)
	}

	if _, err := f.Poll(ctx, handle); err != ErrAuthPending {
		t.Fatalf("pre-approval Poll err = %v, want ErrAuthPending", err)
	}

	f.Approve(handle, Identity{Subject: "sub-1", Email: "heidi@x.com"})
	id, err := f.Poll(ctx, handle)
	if err != nil {
		t.Fatalf("post-approval Poll: %v", err)
	}
	if id.Subject != "sub-1" || id.Email != "heidi@x.com" {
		t.Fatalf("identity = %+v", id)
	}

	if _, err := f.Poll(ctx, "unknown-handle"); err == nil {
		t.Fatal("Poll(unknown) succeeded, want error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestFakeVerifier -v`
Expected: FAIL — `NewFakeVerifier` / `ErrAuthPending` undefined.

- [ ] **Step 3: Implement interface + fake**

Create `internal/relay/verifier.go`:

```go
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
)

// DeviceAuth is what a caller shows the user to complete an OAuth device flow.
type DeviceAuth struct {
	UserCode        string
	VerificationURI string
	Interval        int // seconds between polls
	ExpiresIn       int // seconds until the device code expires
}

// Identity is the verified subject of a completed login.
type Identity struct {
	Subject string // stable IdP user id (Google "sub")
	Email   string
}

// ErrAuthPending means the user has not yet completed the device flow.
var ErrAuthPending = errors.New("authorization_pending")

// Verifier brokers an OAuth device flow with an identity provider. Start begins a
// flow and returns an opaque handle; Poll reports ErrAuthPending until the user
// finishes, then the verified Identity.
type Verifier interface {
	Start(ctx context.Context) (handle string, d DeviceAuth, err error)
	Poll(ctx context.Context, handle string) (Identity, error)
}

// FakeVerifier is an in-memory Verifier for tests. Approve completes a flow.
type FakeVerifier struct {
	mu       sync.Mutex
	approved map[string]Identity
	started  map[string]bool
}

func NewFakeVerifier() *FakeVerifier {
	return &FakeVerifier{approved: map[string]Identity{}, started: map[string]bool{}}
}

func (f *FakeVerifier) Start(context.Context) (string, DeviceAuth, error) {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	handle := hex.EncodeToString(raw)
	f.mu.Lock()
	f.started[handle] = true
	f.mu.Unlock()
	return handle, DeviceAuth{
		UserCode:        "FAKE-CODE",
		VerificationURI: "https://example.test/device",
		Interval:        1,
		ExpiresIn:       300,
	}, nil
}

func (f *FakeVerifier) Poll(_ context.Context, handle string) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started[handle] {
		return Identity{}, errors.New("unknown handle")
	}
	if id, ok := f.approved[handle]; ok {
		return id, nil
	}
	return Identity{}, ErrAuthPending
}

// Approve marks a started handle complete with the given identity (test helper).
func (f *FakeVerifier) Approve(handle string, id Identity) {
	f.mu.Lock()
	f.approved[handle] = id
	f.mu.Unlock()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/relay/ -run TestFakeVerifier -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/verifier.go internal/relay/verifier_test.go
git commit -m "feat(relay): Verifier interface + fake IdP for device flow

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Google device-flow verifier adapter

**Files:**
- Create: `internal/relay/verifier_google.go`
- Modify: `internal/relay/verifier_test.go`

**Interfaces:**
- Consumes: `Verifier`, `DeviceAuth`, `Identity`, `ErrAuthPending` (Task 4).
- Produces:
  - `func NewGoogleVerifier(ctx context.Context, clientID, clientSecret string) (*GoogleVerifier, error)` — satisfies `Verifier`.

**Note on testing:** the real flow calls `accounts.google.com` and `oauth2.googleapis.com`, so the network path is exercised manually / in Plan-3 e2e, not in unit tests. This task's unit test only asserts the adapter constructs and that `Poll` on an unknown handle errors (no network). All device-flow *behaviour* is covered by the `FakeVerifier` in Tasks 4/6.

- [ ] **Step 1: Write the failing test**

Add to `internal/relay/verifier_test.go`:

```go
func TestGoogleVerifierPollUnknownHandle(t *testing.T) {
	v, err := NewGoogleVerifier(context.Background(), "client-id.apps.googleusercontent.com", "secret")
	if err != nil {
		t.Fatalf("NewGoogleVerifier: %v", err)
	}
	if _, err := v.Poll(context.Background(), "never-started"); err == nil {
		t.Fatal("Poll(unknown) succeeded, want error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestGoogleVerifier -v`
Expected: FAIL — `NewGoogleVerifier` undefined.

- [ ] **Step 3: Implement the adapter**

Create `internal/relay/verifier_google.go`:

```go
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"

	oidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GoogleVerifier brokers Google's OAuth 2.0 device authorization grant and
// verifies the returned ID token. It holds the relay's Google client secret so
// callers never see it. Each Start spawns a background goroutine that blocks on
// Google's polling until the user approves, expires, or the process exits; Poll
// reports progress without blocking.
type GoogleVerifier struct {
	cfg      *oauth2.Config
	verifier *oidc.IDTokenVerifier

	mu    sync.Mutex
	flows map[string]*googleFlow
}

type googleFlow struct {
	done bool
	id   Identity
	err  error
}

func NewGoogleVerifier(ctx context.Context, clientID, clientSecret string) (*GoogleVerifier, error) {
	provider, err := oidc.NewProvider(ctx, "https://accounts.google.com")
	if err != nil {
		return nil, err
	}
	return &GoogleVerifier{
		cfg: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     google.Endpoint,
			Scopes:       []string{oidc.ScopeOpenID, "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		flows:    map[string]*googleFlow{},
	}, nil
}

func (g *GoogleVerifier) Start(ctx context.Context) (string, DeviceAuth, error) {
	da, err := g.cfg.DeviceAuth(ctx)
	if err != nil {
		return "", DeviceAuth{}, err
	}
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	handle := hex.EncodeToString(raw)

	fl := &googleFlow{}
	g.mu.Lock()
	g.flows[handle] = fl
	g.mu.Unlock()

	// Block on Google's poll loop in the background; DeviceAccessToken honours the
	// server's interval and returns on approval or expiry.
	go func() {
		tok, err := g.cfg.DeviceAccessToken(context.Background(), da)
		id, verr := g.identityFromToken(context.Background(), tok, err)
		g.mu.Lock()
		fl.done, fl.id, fl.err = true, id, verr
		g.mu.Unlock()
	}()

	return handle, DeviceAuth{
		UserCode:        da.UserCode,
		VerificationURI: da.VerificationURI,
		Interval:        int(da.Interval),
		ExpiresIn:       int(da.Expiry.Unix()),
	}, nil
}

func (g *GoogleVerifier) identityFromToken(ctx context.Context, tok *oauth2.Token, tokErr error) (Identity, error) {
	if tokErr != nil {
		return Identity{}, tokErr
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return Identity{}, errors.New("no id_token in token response")
	}
	idt, err := g.verifier.Verify(ctx, rawID)
	if err != nil {
		return Identity{}, err
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := idt.Claims(&claims); err != nil {
		return Identity{}, err
	}
	return Identity{Subject: claims.Sub, Email: claims.Email}, nil
}

func (g *GoogleVerifier) Poll(_ context.Context, handle string) (Identity, error) {
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

- [ ] **Step 4: Tidy modules & run test**

Run: `go mod tidy` (promotes `golang.org/x/oauth2` and `github.com/coreos/go-oidc/v3` to direct requires — no new downloads).
Run: `go test ./internal/relay/ -run TestGoogleVerifier -v`
Expected: PASS. (Construction hits Google's discovery document once; if the sandbox is offline, note it and rely on `FakeVerifier` coverage — do not add network mocks.)

- [ ] **Step 5: Commit**

```bash
git add internal/relay/verifier_google.go internal/relay/verifier_test.go go.mod go.sum
git commit -m "feat(relay): Google device-flow verifier adapter (oauth2 + oidc)

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: HTTP control API — login device + poll

**Files:**
- Create: `internal/relay/api.go`
- Create: `internal/relay/api_test.go`

**Interfaces:**
- Consumes: `Store`, `Verifier`, `Identity`, `ErrAuthPending` (Tasks 1–5); `UpsertAccount`, `MintAccountCredential`.
- Produces:
  - `func NewAPI(st *Store, v Verifier) http.Handler`
  - `POST /v1/login/device` → `200 {"user_code","verification_uri","device_code","interval","expires_in"}` (the opaque `handle` is returned to the client as `device_code`).
  - `POST /v1/login/poll` body `{"device_code"}` → `200 {"account_credential","username"}` on success, `202 {"status":"authorization_pending"}` while pending, `400` on unknown handle.

- [ ] **Step 1: Write the failing test**

Create `internal/relay/api_test.go`:

```go
package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAPI(t *testing.T) (http.Handler, *Store, *FakeVerifier) {
	t.Helper()
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3)
	fv := NewFakeVerifier()
	return NewAPI(st, fv), st, fv
}

func TestLoginDeviceThenPoll(t *testing.T) {
	api, _, fv := newTestAPI(t)

	// Start device flow.
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/device", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("device status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var dev struct {
		UserCode   string `json:"user_code"`
		DeviceCode string `json:"device_code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &dev); err != nil {
		t.Fatal(err)
	}
	if dev.UserCode == "" || dev.DeviceCode == "" {
		t.Fatalf("empty device response: %+v", dev)
	}

	// Poll before approval → 202 pending.
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"`+dev.DeviceCode+`"}`)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("pending poll status = %d, want 202", rr.Code)
	}

	// Approve, then poll → 200 with a credential.
	fv.Approve(dev.DeviceCode, Identity{Subject: "sub-1", Email: "ivan@x.com"})
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"`+dev.DeviceCode+`"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("success poll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var ok struct {
		AccountCredential string `json:"account_credential"`
		Username          string `json:"username"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ok); err != nil {
		t.Fatal(err)
	}
	if ok.AccountCredential == "" || ok.Username != "ivan" {
		t.Fatalf("poll success body = %+v", ok)
	}
}

func TestLoginPollUnknownHandle(t *testing.T) {
	api, _, _ := newTestAPI(t)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"nope"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown-handle poll status = %d, want 400", rr.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestLoginDevice|TestLoginPoll' -v`
Expected: FAIL — `NewAPI` undefined.

- [ ] **Step 3: Implement the API skeleton + login routes**

Create `internal/relay/api.go`:

```go
package relay

import (
	"encoding/json"
	"errors"
	"net/http"
)

// NewAPI returns the relay's self-service control API: device-flow login and
// account-bound enrollment. TLS termination for this handler is a deployment
// concern (front it with the api.<apex> cert); the handler itself is plain HTTP.
func NewAPI(st *Store, v Verifier) http.Handler {
	a := &api{st: st, v: v}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/login/device", a.loginDevice)
	mux.HandleFunc("POST /v1/login/poll", a.loginPoll)
	mux.HandleFunc("POST /v1/enroll", a.enroll) // implemented in Task 7
	return mux
}

type api struct {
	st *Store
	v  Verifier
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (a *api) loginDevice(w http.ResponseWriter, r *http.Request) {
	handle, d, err := a.v.Start(r.Context())
	if err != nil {
		http.Error(w, "device flow start failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_code":        d.UserCode,
		"verification_uri": d.VerificationURI,
		"device_code":      handle,
		"interval":         d.Interval,
		"expires_in":       d.ExpiresIn,
	})
}

func (a *api) loginPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceCode == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id, err := a.v.Poll(r.Context(), req.DeviceCode)
	if errors.Is(err, ErrAuthPending) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "authorization_pending"})
		return
	}
	if err != nil {
		http.Error(w, "unknown or failed device code", http.StatusBadRequest)
		return
	}
	acc, err := a.st.UpsertAccount(id.Subject, id.Email)
	if err != nil {
		http.Error(w, "account error", http.StatusInternalServerError)
		return
	}
	cred, err := a.st.MintAccountCredential(acc.ID)
	if err != nil {
		http.Error(w, "credential error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"account_credential": cred,
		"username":           acc.Username,
	})
}
```

**Note:** `a.enroll` is referenced here but implemented in Task 7. To keep this task compiling and its tests green, add a temporary stub at the end of `api.go` now; Task 7 replaces its body:

```go
func (a *api) enroll(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/relay/ -run 'TestLoginDevice|TestLoginPoll' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/api.go internal/relay/api_test.go
git commit -m "feat(relay): control API login device + poll (Google device flow)

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: HTTP control API — account-bound enroll

**Files:**
- Modify: `internal/relay/api.go` (replace the `enroll` stub)
- Modify: `internal/relay/api_test.go`

**Interfaces:**
- Consumes: `AuthenticateAccount`, `EnrollForAccount`, `ErrBadCredential`, `ErrQuotaExceeded`, `Store.apexOrDefault` (Tasks 2–3).
- Produces:
  - `POST /v1/enroll` with `Authorization: Bearer <account_credential>` → `200 {"enrollment_token","base_domain","tunnel_endpoint"}`; `401` for a bad/disabled credential; `429` when over cap.
  - `func NewAPIWithTunnel(st *Store, v Verifier, tunnelEndpoint string) http.Handler` — like `NewAPI` but advertises the relay's public tunnel address; `NewAPI` delegates with an empty endpoint.

- [ ] **Step 1: Write the failing test**

Add to `internal/relay/api_test.go`:

```go
func TestEnrollWithAccountCredential(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay.getpiper.co:7000")

	acc, _ := st.UpsertAccount("sub-1", "judy@x.com")
	cred, _ := st.MintAccountCredential(acc.ID)

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		EnrollmentToken string `json:"enrollment_token"`
		BaseDomain      string `json:"base_domain"`
		TunnelEndpoint  string `json:"tunnel_endpoint"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.EnrollmentToken == "" {
		t.Fatal("empty enrollment token")
	}
	if !strings.HasSuffix(out.BaseDomain, "-judy.public.getpiper.co") {
		t.Fatalf("base domain = %q", out.BaseDomain)
	}
	if out.TunnelEndpoint != "relay.getpiper.co:7000" {
		t.Fatalf("tunnel endpoint = %q", out.TunnelEndpoint)
	}
}

func TestEnrollRejectsBadCredential(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay:7000")

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad-cred enroll status = %d, want 401", rr.Code)
	}
}

func TestEnrollOverCapReturns429(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 1)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay:7000")
	acc, _ := st.UpsertAccount("sub-1", "ken@x.com")
	cred, _ := st.MintAccountCredential(acc.ID)

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
		req.Header.Set("Authorization", "Bearer "+cred)
		rr := httptest.NewRecorder()
		api.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := do(); c != http.StatusOK {
		t.Fatalf("first enroll = %d, want 200", c)
	}
	if c := do(); c != http.StatusTooManyRequests {
		t.Fatalf("over-cap enroll = %d, want 429", c)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestEnroll -v`
Expected: FAIL — `NewAPIWithTunnel` undefined and the stub returns 501.

- [ ] **Step 3: Implement enroll + tunnel-endpoint plumbing**

In `internal/relay/api.go`, replace `NewAPI`, the `api` struct, and the `enroll` stub:

```go
// NewAPI returns the control API without advertising a tunnel endpoint (tests /
// LAN). Use NewAPIWithTunnel in production to advertise the relay's tunnel addr.
func NewAPI(st *Store, v Verifier) http.Handler { return NewAPIWithTunnel(st, v, "") }

// NewAPIWithTunnel is NewAPI plus the public tunnel endpoint returned to agents
// on enroll so a freshly claimed box knows where to dial.
func NewAPIWithTunnel(st *Store, v Verifier, tunnelEndpoint string) http.Handler {
	a := &api{st: st, v: v, tunnelEndpoint: tunnelEndpoint}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/login/device", a.loginDevice)
	mux.HandleFunc("POST /v1/login/poll", a.loginPoll)
	mux.HandleFunc("POST /v1/enroll", a.enroll)
	return mux
}

type api struct {
	st             *Store
	v              Verifier
	tunnelEndpoint string
}
```

Then replace the `enroll` stub body with:

```go
func (a *api) enroll(w http.ResponseWriter, r *http.Request) {
	cred, ok := bearerToken(r)
	if !ok {
		http.Error(w, "missing bearer credential", http.StatusUnauthorized)
		return
	}
	acc, err := a.st.AuthenticateAccount(cred)
	if errors.Is(err, ErrBadCredential) {
		http.Error(w, "bad credential", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "auth error", http.StatusInternalServerError)
		return
	}
	en, err := a.st.EnrollForAccount(acc.ID)
	if errors.Is(err, ErrQuotaExceeded) {
		http.Error(w, "agent quota exceeded", http.StatusTooManyRequests)
		return
	}
	if err != nil {
		http.Error(w, "enroll error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"enrollment_token": en.Token,
		"base_domain":      en.BaseDomain,
		"tunnel_endpoint":  a.tunnelEndpoint,
	})
}

// bearerToken extracts a "Bearer <tok>" Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(p) || h[:len(p)] != p {
		return "", false
	}
	return h[len(p):], true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/relay/ -run TestEnroll -v`
Expected: PASS. Then `go test ./internal/relay/ -v` — whole package green.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/api.go internal/relay/api_test.go
git commit -m "feat(relay): control API account-bound enroll endpoint

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: Wire `piper-relay` — serve API, env config, admin kill-switch

**Files:**
- Modify: `cmd/piper-relay/main.go`
- Create: `cmd/piper-relay/main_test.go`

**Interfaces:**
- Consumes: `relay.Open`, `relay.Configure`, `relay.NewAPIWithTunnel`, `relay.NewGoogleVerifier`, `relay.Serve`, `relay.DisableAccount`.
- Produces: a `piper-relay` process that, alongside the existing tunnel+TLS listeners, serves the control API on `PIPER_RELAY_API_ADDR`, and a `piper-relay admin disable <username>` subcommand. Extract the arg handling into a testable `func runAdmin(st adminStore, args []string) error` so the disable path is unit-tested without a live server.

- [ ] **Step 1: Write the failing test**

Create `cmd/piper-relay/main_test.go`:

```go
package main

import (
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/relay"
)

func TestRunAdminDisable(t *testing.T) {
	st, err := relay.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acc, err := st.UpsertAccount("sub-1", "leo@x.com")
	if err != nil {
		t.Fatal(err)
	}

	if err := runAdmin(st, []string{"disable", acc.Username}); err != nil {
		t.Fatalf("runAdmin disable: %v", err)
	}
	// Disabling again on a real account still succeeds (idempotent-ish); an
	// unknown username must error.
	if err := runAdmin(st, []string{"disable", "no-such-user"}); err == nil {
		t.Fatal("runAdmin disable unknown user succeeded, want error")
	}
}

func TestRunAdminUsage(t *testing.T) {
	st, _ := relay.Open(filepath.Join(t.TempDir(), "relay.db"))
	defer st.Close()
	if err := runAdmin(st, []string{"disable"}); err == nil {
		t.Fatal("runAdmin with no username succeeded, want usage error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/piper-relay/ -v`
Expected: FAIL — `runAdmin` undefined.

- [ ] **Step 3: Implement `runAdmin` + API wiring in `main.go`**

In `cmd/piper-relay/main.go`, add the admin type + function (place above `main`):

```go
// adminStore is the slice of *relay.Store the admin subcommands need.
type adminStore interface {
	DisableAccount(username string) error
}

// runAdmin handles "piper-relay admin <cmd> ...". Currently: disable <username>.
func runAdmin(st adminStore, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: piper-relay admin disable <username>")
	}
	switch args[0] {
	case "disable":
		if len(args) != 2 {
			return fmt.Errorf("usage: piper-relay admin disable <username>")
		}
		return st.DisableAccount(args[1])
	default:
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}
```

Then, in `main`, after the store is opened and before the existing `enroll` block, add an `admin` dispatch:

```go
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := runAdmin(st, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
```

Finally, replace the serving tail of `main` (the `tlsAddr`/`tunnelAddr`/`relay.Serve` lines) with API wiring:

```go
	st.Configure(env("PIPER_RELAY_APEX", "public.getpiper.co"), atoiOr(env("PIPER_RELAY_MAX_AGENTS", "3"), 3))

	tlsAddr := env("PIPER_RELAY_TLS_ADDR", ":443")
	tunnelAddr := env("PIPER_RELAY_TUNNEL_ADDR", ":7000")
	apiAddr := env("PIPER_RELAY_API_ADDR", ":8080")
	tunnelPublic := env("PIPER_RELAY_TUNNEL_PUBLIC", "")

	// Self-service login needs a Google OAuth client; without one the relay runs
	// operator-enroll-only (existing behaviour) and the API 503s login routes.
	var v relay.Verifier
	if id := env("PIPER_RELAY_GOOGLE_CLIENT_ID", ""); id != "" {
		gv, err := relay.NewGoogleVerifier(context.Background(), id, env("PIPER_RELAY_GOOGLE_CLIENT_SECRET", ""))
		if err != nil {
			log.Fatalf("google verifier: %v", err)
		}
		v = gv
	} else {
		log.Print("piper-relay: no PIPER_RELAY_GOOGLE_CLIENT_ID; self-service login disabled")
		v = relay.NewFakeVerifier() // login routes exist but complete only via test approval
	}

	go func() {
		log.Printf("piper-relay: control API %s", apiAddr)
		if err := http.ListenAndServe(apiAddr, relay.NewAPIWithTunnel(st, v, tunnelPublic)); err != nil {
			log.Fatalf("control API: %v", err)
		}
	}()

	log.Printf("piper-relay: TLS %s, tunnel %s", tlsAddr, tunnelAddr)
	log.Fatal(relay.Serve(tlsAddr, tunnelAddr, st))
```

Add the needed imports to `main.go`: `"context"`, `"net/http"`, `"strconv"`, and a small helper:

```go
func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/piper-relay/ -v`
Expected: PASS.

- [ ] **Step 5: Full verification**

Run: `make test`
Expected: all packages pass (Docker/e2e skip cleanly if Docker absent).
Run: `make cross`
Expected: arm64 build succeeds (no-cgo intact).

- [ ] **Step 6: Commit**

```bash
git add cmd/piper-relay/main.go cmd/piper-relay/main_test.go
git commit -m "feat(relay): serve control API + admin disable kill-switch

Part of #49

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage** (against `2026-07-07-public-relay-onboarding-design.md`, In-scope items):

- Google-OAuth accounts → Tasks 1, 5, 6. ✅
- Device-flow self-service claim → Tasks 4–7 (device + poll + enroll). ✅
- Account credential (trust spec's "relay account credential") → Task 2. ✅
- Assigned single-label name `<hash>-<username>.<apex>` → Task 3 (as the agent's base domain; per-app hashing lands in Plan 3, which is where routing changes). ✅ (scoped)
- Per-account cap → Task 3 + Task 7 (429). ✅
- Operator kill-switch (disable account/hostname) → Tasks 2, 3 (agent rejected), 8 (CLI). *Hostname-level disable lands with the hostname registry in Plan 3; account-level disable is here.* ✅ (scoped)
- Relay-terminated wildcard app path, hostname registry, `piper login`/`connect` CLI → **Plans 2 & 3**, intentionally out of this plan. ✅

**Placeholder scan:** The `api.enroll` stub in Task 6 is explicitly a temporary compile-shim, replaced in Task 7 — flagged inline, not a lingering placeholder. No `TBD`/`TODO`/"add error handling" left. ✅

**Type consistency:** `Account{ID,Username,Disabled}`, `Enrollment{Token,BaseDomain}`, `Identity{Subject,Email}`, `DeviceAuth{UserCode,VerificationURI,Interval,ExpiresIn}` are used identically across tasks. `NewAPI`→`NewAPIWithTunnel` delegation is consistent (Task 6 introduces `NewAPI`; Task 7 redefines both together in the same file edit, so no dangling signature). Sentinels `ErrBadCredential`, `ErrQuotaExceeded`, `ErrAuthPending`, existing `ErrBadToken` used consistently. ✅

**Layering:** `relay` imports only stdlib, `internal/tunnel` (already), `oauth2`, `oidc`, `uuid`. No upward imports. ✅

## Next plans

- **Plan 2 — `piper login` + `piper connect` CLI:** `login` drives `/v1/login/device` + `/v1/login/poll`, stores the account credential in `~/.piper/piper/config.json` (extend `config.ClientConfig`); `connect` calls `/v1/enroll` and writes `RelayAddr`/`RelayToken` into piperd's config from the response.
- **Plan 3 — assigned hostnames + relay-terminated app path:** per-app `<hash>-<username>.<apex>` hostnames registered over the control tunnel; the relay `:443` SNI branch (terminate under `*.public.getpiper.co` vs. Plan-2 passthrough); agent-side forwarded-HTTP handling.
