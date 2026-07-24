# Relay Organizations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Orgs on the relay ([#104](https://github.com/piperbox/piper/issues/104)): an org is a login-less account that owns boxes/apps; members see and drive its boxes through the existing control plane; owners manage membership via GitHub-username invites.

**Architecture:** Per the approved spec (`docs/superpowers/specs/2026-07-11-relay-organizations-design.md`), an org is an `accounts` row with `type='org'`, `NULL github_id`, and no credentials — so agents, hostnames, quotas, and the kill-switch reuse `accounts.id` unchanged. New tables `org_members` and `org_invites`; the control proxy's owner check becomes owner-or-member (still `404` on failure — no existence leak); `/v1/enroll` gains an optional owner-gated `org` slug. All work is in `internal/relay/`.

**Tech Stack:** Go (1.26), `modernc.org/sqlite` (pure Go — **no cgo**), stdlib `net/http` ServeMux with method+path patterns (`r.PathValue`).

## Global Constraints

- `CGO_ENABLED=0` everywhere; only `modernc.org/sqlite` for SQLite.
- Module path `github.com/piperbox/piper`.
- Work on branch `ozykhan/relay-orgs-design` (already holds the spec commit); PR into `main`, squash-merge.
- One commit per task, conventional-commit style, each ending with:
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`
- Reference the issue in commits/PR body: `Part of #104`.
- Run tests with `go test ./internal/relay/ -run <Name> -v`; run `make verify` before claiming done (gofmt → vet → tests → arm64 cross-build).
- All tests are white-box (package `relay`), using the existing `openTestStore(t)` helper from `accounts_test.go`.
- Role strings are exactly `"owner"` and `"member"`. Account type strings are exactly `"user"` and `"org"`.
- Error → HTTP status mapping used across API tasks: `ErrNoOrg`/`ErrNotMember`/`ErrNoInvite` → 404, `ErrLastOwner`/`ErrOrgHasAgents`/`ErrAlreadyMember` → 409, non-owner on owner-gated endpoint → 403, bad body → 400.

---

### Task 1: Schema — org tables and the accounts rebuild migration

The `accounts` table gains `type` and `github_login` and relaxes `github_id` to nullable-unique. SQLite cannot drop `NOT NULL` in place, so legacy DBs are rebuilt (create-copy-drop-rename); fresh DBs get the new `schema.sql`. New tables `org_members` and `org_invites`.

**Files:**
- Modify: `internal/relay/schema.sql`
- Modify: `internal/relay/store.go` (add `migrateAccounts`, call it from `Open`)
- Test: `internal/relay/store_test.go`

**Interfaces:**
- Consumes: existing `Open(path string) (*Store, error)`.
- Produces: `accounts` columns `type TEXT NOT NULL DEFAULT 'user'`, `github_login TEXT`, `github_id TEXT UNIQUE` (nullable); tables `org_members(org_id, account_id, role, created_at)` PK `(org_id, account_id)` and `org_invites(org_id, github_login, invited_by, created_at)` PK `(org_id, github_login)`. Later tasks rely on these exact names.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/store_test.go` (add imports `"database/sql"` and `"path/filepath"` if missing; the modernc driver is already blank-imported by the package):

```go
// legacyAccountsDDL is the pre-#104 accounts shape: github_id NOT NULL, no
// type/github_login. Used to prove Open migrates old DBs in place.
const legacyAccountsDDL = `
CREATE TABLE accounts (
    id          TEXT PRIMARY KEY,
    github_id   TEXT NOT NULL UNIQUE,
    username    TEXT NOT NULL UNIQUE,
    disabled    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);`

func TestOpenMigratesLegacyAccountsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(legacyAccountsDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO accounts(id, github_id, username, disabled, created_at)
		 VALUES('acc-1','583231','alice',0,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on legacy DB: %v", err)
	}
	defer st.Close()

	// Existing row survives the rebuild and re-login resolves the same account.
	acc, err := st.UpsertAccount("583231", "alice")
	if err != nil {
		t.Fatalf("UpsertAccount after migration: %v", err)
	}
	if acc.ID != "acc-1" {
		t.Fatalf("account id = %q, want acc-1 (migration must preserve rows)", acc.ID)
	}
	// Migrated rows are type 'user'.
	var typ string
	if err := st.db.QueryRow(`SELECT type FROM accounts WHERE id='acc-1'`).Scan(&typ); err != nil {
		t.Fatalf("type column missing after migration: %v", err)
	}
	if typ != "user" {
		t.Fatalf("migrated type = %q, want user", typ)
	}
	// github_id is nullable now: an org-shaped row inserts cleanly.
	if _, err := st.db.Exec(
		`INSERT INTO accounts(id, username, type, created_at)
		 VALUES('org-1','someorg','org','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert NULL-github_id row: %v", err)
	}
}

func TestOpenCreatesOrgTables(t *testing.T) {
	st := openTestStore(t)
	for _, table := range []string{"org_members", "org_invites"} {
		var n int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestOpenMigratesLegacyAccountsTable|TestOpenCreatesOrgTables' -v`
Expected: FAIL — `no such table: org_members` and `type column missing` / `NOT NULL constraint failed` on the org-shaped insert.

- [ ] **Step 3: Implement**

Replace the `accounts` block in `internal/relay/schema.sql` and append the org tables (leave `agents`, `account_creds`, `hostnames`, and the index untouched):

```sql
CREATE TABLE IF NOT EXISTS accounts (
    id           TEXT PRIMARY KEY,
    github_id    TEXT UNIQUE,
    github_login TEXT,
    username     TEXT NOT NULL UNIQUE,
    type         TEXT NOT NULL DEFAULT 'user',
    disabled     INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL
);
```

```sql
CREATE TABLE IF NOT EXISTS org_members (
    org_id     TEXT NOT NULL REFERENCES accounts(id),
    account_id TEXT NOT NULL REFERENCES accounts(id),
    role       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (org_id, account_id)
);

CREATE TABLE IF NOT EXISTS org_invites (
    org_id       TEXT NOT NULL REFERENCES accounts(id),
    github_login TEXT NOT NULL,
    invited_by   TEXT NOT NULL REFERENCES accounts(id),
    created_at   TEXT NOT NULL,
    PRIMARY KEY (org_id, github_login)
);
```

In `internal/relay/store.go`, add after `ensureAgentColumn`:

```go
// migrateAccounts rebuilds a legacy accounts table (github_id NOT NULL, no
// type/github_login) into the org-aware shape. SQLite cannot relax NOT NULL in
// place, so: create-copy-drop-rename. Legacy is detected by the missing "type"
// column; migrated rows are type 'user' with github_login left NULL (refreshed
// at their next login).
func migrateAccounts(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(accounts)`)
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
		if name == "type" {
			return nil // already the new shape
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE accounts_new (
			id           TEXT PRIMARY KEY,
			github_id    TEXT UNIQUE,
			github_login TEXT,
			username     TEXT NOT NULL UNIQUE,
			type         TEXT NOT NULL DEFAULT 'user',
			disabled     INTEGER NOT NULL DEFAULT 0,
			created_at   TEXT NOT NULL
		)`,
		`INSERT INTO accounts_new (id, github_id, username, disabled, created_at)
			SELECT id, github_id, username, disabled, created_at FROM accounts`,
		`DROP TABLE accounts`,
		`ALTER TABLE accounts_new RENAME TO accounts`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

In `Open`, call it right after the schema is applied (before the `ensureAgentColumn` loop):

```go
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateAccounts(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate accounts: %w", err)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS, including the whole existing suite (the migration must not break anything).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/schema.sql internal/relay/store.go internal/relay/store_test.go
git commit -m "feat(relay): org-aware accounts schema + legacy rebuild migration

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Account plumbing — store and refresh `github_login`

Invites are matched by the raw GitHub login (the derived `username` slug is munged and can't match). Store it at signup, refresh it at every login (GitHub logins can be renamed), and expose it on `Account`.

**Files:**
- Modify: `internal/relay/accounts.go` (`Account` struct, `UpsertAccount`, `AuthenticateAccount`)
- Test: `internal/relay/accounts_test.go`

**Interfaces:**
- Consumes: Task 1's `github_login` column.
- Produces: `Account{ID, Username, GithubLogin string, Disabled bool}`; `UpsertAccount(githubID, login string) (Account, error)` now inserts `type='user'` + `github_login` and refreshes `github_login` on re-login; `AuthenticateAccount(cred string) (Account, error)` fills `GithubLogin`. Task 5 matches invites on lowercased `GithubLogin`.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/accounts_test.go`:

```go
func TestUpsertAccountStoresAndRefreshesGithubLogin(t *testing.T) {
	st := openTestStore(t)

	a1, err := st.UpsertAccount("gh-1", "Alice-Smith")
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if a1.GithubLogin != "Alice-Smith" {
		t.Fatalf("GithubLogin = %q, want Alice-Smith", a1.GithubLogin)
	}

	// GitHub login renamed: re-login refreshes the stored login.
	a2, err := st.UpsertAccount("gh-1", "alice-renamed")
	if err != nil {
		t.Fatalf("re-login UpsertAccount: %v", err)
	}
	if a2.GithubLogin != "alice-renamed" {
		t.Fatalf("refreshed GithubLogin = %q, want alice-renamed", a2.GithubLogin)
	}
	var stored string
	if err := st.db.QueryRow(`SELECT github_login FROM accounts WHERE github_id='gh-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "alice-renamed" {
		t.Fatalf("stored github_login = %q, want alice-renamed", stored)
	}

	// AuthenticateAccount surfaces the login too (needed for invite matching).
	cred, _ := st.MintAccountCredential(a1.ID)
	got, err := st.AuthenticateAccount(cred)
	if err != nil {
		t.Fatal(err)
	}
	if got.GithubLogin != "alice-renamed" {
		t.Fatalf("authenticated GithubLogin = %q, want alice-renamed", got.GithubLogin)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestUpsertAccountStoresAndRefreshesGithubLogin -v`
Expected: FAIL — `a1.GithubLogin undefined` (compile error) until the field exists, then empty-string mismatches.

- [ ] **Step 3: Implement**

In `internal/relay/accounts.go`:

```go
// Account is a relay tenant. One account owns many agents.
type Account struct {
	ID          string
	Username    string
	GithubLogin string // raw GitHub login, refreshed at every login; "" pre-migration
	Disabled    bool
}
```

Rewrite `UpsertAccount`'s lookup-and-insert to carry the login:

```go
func (s *Store) UpsertAccount(githubID, login string) (Account, error) {
	var acc Account
	var disabled int
	var storedLogin sql.NullString
	err := s.db.QueryRow(
		`SELECT id, username, disabled, github_login FROM accounts WHERE github_id=?`, githubID).
		Scan(&acc.ID, &acc.Username, &disabled, &storedLogin)
	if err == nil {
		acc.Disabled = disabled != 0
		acc.GithubLogin = login
		if storedLogin.String != login {
			// GitHub logins can be renamed; keep the invite-matching login fresh.
			if _, err := s.db.Exec(`UPDATE accounts SET github_login=? WHERE id=?`, login, acc.ID); err != nil {
				return Account{}, err
			}
		}
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
			`INSERT INTO accounts(id, github_id, github_login, username, type, disabled, created_at)
			 VALUES(?,?,?,?,'user',0,?)`,
			id, githubID, login, username, now)
		if err == nil {
			return Account{ID: id, Username: username, GithubLogin: login}, nil
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

Update `AuthenticateAccount` to select the login:

```go
func (s *Store) AuthenticateAccount(cred string) (Account, error) {
	var acc Account
	var disabled int
	var gl sql.NullString
	err := s.db.QueryRow(
		`SELECT a.id, a.username, a.github_login, a.disabled
		   FROM account_creds c JOIN accounts a ON a.id = c.account_id
		  WHERE c.token_hash = ?`, hashToken(cred)).
		Scan(&acc.ID, &acc.Username, &gl, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrBadCredential
	}
	if err != nil {
		return Account{}, err
	}
	if disabled != 0 {
		return Account{}, ErrBadCredential
	}
	acc.GithubLogin = gl.String
	acc.Disabled = false
	return acc, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS (full suite).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/accounts.go internal/relay/accounts_test.go
git commit -m "feat(relay): store and refresh the raw GitHub login on accounts

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: Org creation — `CreateOrg`, `OrgsForAccount`, `OrgRole`, inertness guards

**Files:**
- Create: `internal/relay/orgs.go`
- Create: `internal/relay/orgs_test.go`
- Modify: `internal/relay/accounts.go` (`MintAccountCredential` org guard)

**Interfaces:**
- Consumes: Task 1 schema; `deriveUsername`, `isUniqueViolation`, `ErrBadCredential` from `accounts.go`.
- Produces (later tasks use these exact signatures):
  - `type Org struct { ID, Slug, Role string }`
  - `var ErrNoOrg = errors.New("no such org")`
  - `CreateOrg(creatorID, name string) (Org, error)` — creator becomes sole owner
  - `OrgsForAccount(accountID string) ([]Org, error)`
  - `OrgRole(slug, accountID string) (orgID, role string, err error)` — `ErrNoOrg` when the org doesn't exist **or** the caller is not a member (no existence leak)

- [ ] **Step 1: Write the failing test**

Create `internal/relay/orgs_test.go`:

```go
package relay

import (
	"errors"
	"testing"
)

func TestCreateOrgMakesCreatorSoleOwner(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")

	org, err := st.CreateOrg(alice.ID, "Acme Robotics")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org.Slug != "acme-robotics" {
		t.Fatalf("slug = %q, want acme-robotics", org.Slug)
	}
	if org.ID == "" || org.ID == alice.ID {
		t.Fatalf("org id = %q, want a fresh account id", org.ID)
	}

	orgs, err := st.OrgsForAccount(alice.ID)
	if err != nil {
		t.Fatalf("OrgsForAccount: %v", err)
	}
	if len(orgs) != 1 || orgs[0].Slug != "acme-robotics" || orgs[0].Role != "owner" {
		t.Fatalf("orgs = %+v, want [acme-robotics owner]", orgs)
	}
}

func TestCreateOrgSlugSharesUsernameNamespace(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	// A user already holds "bob": the org gets bob-2, exactly like a
	// colliding user signup would.
	st.UpsertAccount("gh-bob", "bob")

	org, err := st.CreateOrg(alice.ID, "Bob")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org.Slug != "bob-2" {
		t.Fatalf("slug = %q, want bob-2", org.Slug)
	}
}

func TestOrgRoleHidesOrgFromNonMembers(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	mallory, _ := st.UpsertAccount("gh-mallory", "mallory")
	org, _ := st.CreateOrg(alice.ID, "acme")

	orgID, role, err := st.OrgRole(org.Slug, alice.ID)
	if err != nil || orgID != org.ID || role != "owner" {
		t.Fatalf("member OrgRole = (%q,%q,%v), want (%q, owner, nil)", orgID, role, err, org.ID)
	}
	// Non-member, nonexistent org, and a *user* slug are indistinguishable.
	if _, _, err := st.OrgRole(org.Slug, mallory.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("non-member err = %v, want ErrNoOrg", err)
	}
	if _, _, err := st.OrgRole("nope", alice.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("nonexistent err = %v, want ErrNoOrg", err)
	}
	if _, _, err := st.OrgRole("mallory", alice.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("user-slug err = %v, want ErrNoOrg", err)
	}
}

func TestOrgStaysInertAsPrincipal(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// An org can never hold a credential.
	if _, err := st.MintAccountCredential(org.ID); err == nil {
		t.Fatal("MintAccountCredential(org) succeeded, want error")
	}
	// An org cannot create an org.
	if _, err := st.CreateOrg(org.ID, "suborg"); err == nil {
		t.Fatal("CreateOrg(by org) succeeded, want error")
	}
}

func TestOrgAgentQuotaIsIndependent(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 2, 10)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// Alice fills her personal cap.
	for i := 0; i < 2; i++ {
		if _, err := st.EnrollForAccount(alice.ID); err != nil {
			t.Fatalf("personal enroll %d: %v", i, err)
		}
	}
	if _, err := st.EnrollForAccount(alice.ID); err != ErrQuotaExceeded {
		t.Fatalf("over personal cap err = %v, want ErrQuotaExceeded", err)
	}
	// The org's cap is its own; its base domain carries the org slug.
	en, err := st.EnrollForAccount(org.ID)
	if err != nil {
		t.Fatalf("org enroll: %v", err)
	}
	if want := "-acme.public.getpiper.co"; !strings.HasSuffix(en.BaseDomain, want) {
		t.Fatalf("org base domain = %q, want suffix %q", en.BaseDomain, want)
	}
}
```

Add `"strings"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestCreateOrg|TestOrgRole|TestOrgStaysInert|TestOrgAgentQuota' -v`
Expected: FAIL — compile errors: `st.CreateOrg undefined`, `ErrNoOrg` undefined.

- [ ] **Step 3: Implement**

Create `internal/relay/orgs.go`:

```go
package relay

import (
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Org is one org membership from the caller's point of view.
type Org struct {
	ID   string
	Slug string
	Role string // the caller's role: "owner" | "member"
}

// ErrNoOrg is returned when an org doesn't exist or the caller is not a
// member — deliberately indistinguishable, so org existence never leaks.
var ErrNoOrg = errors.New("no such org")

// CreateOrg creates an org account (type='org', no GitHub identity, no
// credentials) with a slug derived from name — unique in the same username
// namespace user slugs live in, since both become DNS-label components — and
// makes the creator its sole owner.
func (s *Store) CreateOrg(creatorID, name string) (Org, error) {
	var ctype string
	err := s.db.QueryRow(`SELECT type FROM accounts WHERE id=?`, creatorID).Scan(&ctype)
	if errors.Is(err, sql.ErrNoRows) {
		return Org{}, ErrBadCredential
	}
	if err != nil {
		return Org{}, err
	}
	if ctype != "user" {
		return Org{}, errors.New("only user accounts create orgs")
	}

	base := deriveUsername(name)
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.Begin()
	if err != nil {
		return Org{}, err
	}
	defer tx.Rollback()
	for i := 1; ; i++ {
		slug := base
		if i > 1 {
			slug = base + "-" + strconv.Itoa(i)
		}
		_, err := tx.Exec(
			`INSERT INTO accounts(id, github_id, github_login, username, type, disabled, created_at)
			 VALUES(?,NULL,NULL,?,'org',0,?)`, id, slug, now)
		if err == nil {
			if _, err := tx.Exec(
				`INSERT INTO org_members(org_id, account_id, role, created_at) VALUES(?,?,'owner',?)`,
				id, creatorID, now); err != nil {
				return Org{}, err
			}
			if err := tx.Commit(); err != nil {
				return Org{}, err
			}
			return Org{ID: id, Slug: slug, Role: "owner"}, nil
		}
		if isUniqueViolation(err) {
			continue // slug taken (user or org); try the next suffix
		}
		return Org{}, err
	}
}

// OrgsForAccount lists the orgs accountID belongs to, oldest membership first.
func (s *Store) OrgsForAccount(accountID string) ([]Org, error) {
	rows, err := s.db.Query(
		`SELECT o.id, o.username, m.role
		   FROM org_members m JOIN accounts o ON o.id = m.org_id
		  WHERE m.account_id = ? ORDER BY m.rowid`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orgs []Org
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Slug, &o.Role); err != nil {
			return nil, err
		}
		orgs = append(orgs, o)
	}
	return orgs, rows.Err()
}

// OrgRole resolves slug to the org's account id and accountID's role in it.
// ErrNoOrg both when no such org exists and when the caller is not a member,
// so a non-member can't probe org existence.
func (s *Store) OrgRole(slug, accountID string) (orgID, role string, err error) {
	err = s.db.QueryRow(
		`SELECT o.id, m.role
		   FROM accounts o JOIN org_members m ON m.org_id = o.id AND m.account_id = ?
		  WHERE o.username = ? AND o.type = 'org'`, accountID, slug).
		Scan(&orgID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNoOrg
	}
	if err != nil {
		return "", "", err
	}
	return orgID, role, nil
}
```

In `internal/relay/accounts.go`, guard `MintAccountCredential` (insert at the top of the function):

```go
	// Orgs are inert principals: they never hold credentials, so they can
	// never authenticate (belt-and-braces on top of the NULL github_id).
	var typ string
	if err := s.db.QueryRow(`SELECT type FROM accounts WHERE id=?`, accountID).Scan(&typ); err != nil {
		return "", err
	}
	if typ != "user" {
		return "", errors.New("only user accounts hold credentials")
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS (full suite — the mint guard must not break existing login tests).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/orgs.go internal/relay/orgs_test.go internal/relay/accounts.go
git commit -m "feat(relay): org accounts — create, list, role resolution, inertness guards

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Membership — `Members`, `SetMemberRole`, `RemoveMember`, last-owner guards

**Files:**
- Modify: `internal/relay/orgs.go`
- Test: `internal/relay/orgs_test.go`

**Interfaces:**
- Consumes: Task 3's `CreateOrg`/`OrgRole`.
- Produces:
  - `type Member struct { Username, Role string }`
  - `var ErrNotMember = errors.New("not a member")`
  - `var ErrLastOwner = errors.New("an org must keep at least one owner")`
  - `Members(orgID string) ([]Member, error)`
  - `SetMemberRole(orgID, username, role string) error`
  - `RemoveMember(orgID, username string) error`
- Test-only helper produced here and reused by later tasks: `addMember(t, st, orgID, accountID, role)` (direct insert, so membership tests don't depend on the invite flow of Task 5).

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/orgs_test.go`:

```go
// addMember inserts a membership row directly; org/membership tests must not
// depend on the invite flow.
func addMember(t *testing.T, st *Store, orgID, accountID, role string) {
	t.Helper()
	if _, err := st.db.Exec(
		`INSERT INTO org_members(org_id, account_id, role, created_at)
		 VALUES(?,?,?,'2026-01-01T00:00:00Z')`, orgID, accountID, role); err != nil {
		t.Fatal(err)
	}
}

func TestMembersListsUsernamesAndRoles(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	members, err := st.Members(org.ID)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 2 ||
		members[0] != (Member{Username: "alice", Role: "owner"}) ||
		members[1] != (Member{Username: "bob", Role: "member"}) {
		t.Fatalf("members = %+v, want [alice/owner bob/member]", members)
	}
}

func TestSetMemberRolePromotesAndGuardsLastOwner(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	// Sole owner cannot demote themselves.
	if err := st.SetMemberRole(org.ID, "alice", "member"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demote sole owner err = %v, want ErrLastOwner", err)
	}
	// Promote bob, then alice may step down.
	if err := st.SetMemberRole(org.ID, "bob", "owner"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := st.SetMemberRole(org.ID, "alice", "member"); err != nil {
		t.Fatalf("demote after promote: %v", err)
	}
	// Unknown target.
	if err := st.SetMemberRole(org.ID, "nobody", "member"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("unknown member err = %v, want ErrNotMember", err)
	}
}

func TestRemoveMemberGuardsLastOwner(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	if err := st.RemoveMember(org.ID, "alice"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("remove sole owner err = %v, want ErrLastOwner", err)
	}
	if err := st.RemoveMember(org.ID, "bob"); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if err := st.RemoveMember(org.ID, "bob"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("re-remove err = %v, want ErrNotMember", err)
	}
	// Removal is real: bob no longer lists the org.
	orgs, _ := st.OrgsForAccount(bob.ID)
	if len(orgs) != 0 {
		t.Fatalf("bob still in orgs: %+v", orgs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestMembers|TestSetMemberRole|TestRemoveMember' -v`
Expected: FAIL — compile errors on `Member`, `st.Members`, `ErrLastOwner`, `ErrNotMember`.

- [ ] **Step 3: Implement**

Append to `internal/relay/orgs.go`:

```go
// Member is one row of an org's member list.
type Member struct {
	Username string
	Role     string
}

// ErrNotMember is returned when the target username has no membership row.
var ErrNotMember = errors.New("not a member")

// ErrLastOwner is returned when a role change or removal would leave the org
// with no owner.
var ErrLastOwner = errors.New("an org must keep at least one owner")

// Members lists an org's members, oldest first.
func (s *Store) Members(orgID string) ([]Member, error) {
	rows, err := s.db.Query(
		`SELECT a.username, m.role
		   FROM org_members m JOIN accounts a ON a.id = m.account_id
		  WHERE m.org_id = ? ORDER BY m.rowid`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Username, &m.Role); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// memberForUpdate resolves username's membership row inside tx and, when the
// change would drop an owner, enforces the last-owner rule.
func memberForUpdate(tx *sql.Tx, orgID, username string, dropsOwner func(cur string) bool) (targetID string, err error) {
	var cur string
	err = tx.QueryRow(
		`SELECT a.id, m.role
		   FROM org_members m JOIN accounts a ON a.id = m.account_id
		  WHERE m.org_id = ? AND a.username = ?`, orgID, username).
		Scan(&targetID, &cur)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotMember
	}
	if err != nil {
		return "", err
	}
	if dropsOwner(cur) {
		var owners int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM org_members WHERE org_id = ? AND role = 'owner'`, orgID).
			Scan(&owners); err != nil {
			return "", err
		}
		if owners <= 1 {
			return "", ErrLastOwner
		}
	}
	return targetID, nil
}

// SetMemberRole changes username's role in the org. Demoting the last owner is
// ErrLastOwner; an unknown target is ErrNotMember.
func (s *Store) SetMemberRole(orgID, username, role string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	targetID, err := memberForUpdate(tx, orgID, username,
		func(cur string) bool { return cur == "owner" && role != "owner" })
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE org_members SET role=? WHERE org_id=? AND account_id=?`, role, orgID, targetID); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveMember deletes username's membership. Removing the last owner is
// ErrLastOwner; an unknown target is ErrNotMember. The member's personal
// account and boxes are untouched.
func (s *Store) RemoveMember(orgID, username string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	targetID, err := memberForUpdate(tx, orgID, username,
		func(cur string) bool { return cur == "owner" })
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM org_members WHERE org_id=? AND account_id=?`, orgID, targetID); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/orgs.go internal/relay/orgs_test.go
git commit -m "feat(relay): org membership — list, role changes, last-owner guards

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 5: Invites — store lifecycle

Invites are keyed `(org_id, lowercased github_login)`. They match a user by the current `github_login`, so inviting someone who has never logged in works, and a GitHub rename means the invite follows the login (same trust call GitHub's own org invites make).

**Files:**
- Modify: `internal/relay/orgs.go`
- Test: `internal/relay/orgs_test.go`

**Interfaces:**
- Consumes: Tasks 2–4 (`Account.GithubLogin`, `CreateOrg`, `addMember` test helper).
- Produces:
  - `var ErrAlreadyMember = errors.New("already a member")`
  - `var ErrNoInvite = errors.New("no such invite")`
  - `CreateInvite(orgID, githubLogin, inviterID string) error` — duplicate invite is idempotent nil
  - `OrgInvites(orgID string) ([]string, error)` — pending logins
  - `RevokeInvite(orgID, githubLogin string) error`
  - `InvitesForAccount(accountID string) ([]string, error)` — org slugs
  - `AcceptInvite(accountID, orgSlug string) error` — membership as `"member"`, invite consumed
  - `DeclineInvite(accountID, orgSlug string) error`

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/orgs_test.go`:

```go
func TestInviteLifecycle(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "Bob-Builder")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// Invite by GitHub username, any case; duplicate is idempotent.
	if err := st.CreateInvite(org.ID, "BOB-builder", alice.ID); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if err := st.CreateInvite(org.ID, "bob-builder", alice.ID); err != nil {
		t.Fatalf("duplicate invite: %v, want nil (idempotent)", err)
	}
	pending, err := st.OrgInvites(org.ID)
	if err != nil || len(pending) != 1 || pending[0] != "bob-builder" {
		t.Fatalf("OrgInvites = %v (%v), want [bob-builder]", pending, err)
	}
	mine, err := st.InvitesForAccount(bob.ID)
	if err != nil || len(mine) != 1 || mine[0] != "acme" {
		t.Fatalf("InvitesForAccount = %v (%v), want [acme]", mine, err)
	}

	// Accept: membership as member, invite consumed.
	if err := st.AcceptInvite(bob.ID, "acme"); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	orgs, _ := st.OrgsForAccount(bob.ID)
	if len(orgs) != 1 || orgs[0].Role != "member" {
		t.Fatalf("bob's orgs = %+v, want [acme member]", orgs)
	}
	if pending, _ := st.OrgInvites(org.ID); len(pending) != 0 {
		t.Fatalf("invite not consumed: %v", pending)
	}
	if err := st.AcceptInvite(bob.ID, "acme"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("re-accept err = %v, want ErrNoInvite", err)
	}

	// Inviting an existing member is refused.
	if err := st.CreateInvite(org.ID, "Bob-Builder", alice.ID); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("invite member err = %v, want ErrAlreadyMember", err)
	}
}

func TestInviteBeforeFirstLogin(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// Invited before ever logging into the relay.
	if err := st.CreateInvite(org.ID, "Newbie", alice.ID); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	newbie, _ := st.UpsertAccount("gh-newbie", "Newbie")
	mine, err := st.InvitesForAccount(newbie.ID)
	if err != nil || len(mine) != 1 || mine[0] != "acme" {
		t.Fatalf("InvitesForAccount = %v (%v), want [acme]", mine, err)
	}
	if err := st.AcceptInvite(newbie.ID, "acme"); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
}

func TestDeclineAndRevokeInvite(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")

	st.CreateInvite(org.ID, "bob", alice.ID)
	if err := st.DeclineInvite(bob.ID, "acme"); err != nil {
		t.Fatalf("DeclineInvite: %v", err)
	}
	if orgs, _ := st.OrgsForAccount(bob.ID); len(orgs) != 0 {
		t.Fatalf("decline created membership: %+v", orgs)
	}
	if err := st.DeclineInvite(bob.ID, "acme"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("re-decline err = %v, want ErrNoInvite", err)
	}

	st.CreateInvite(org.ID, "bob", alice.ID)
	if err := st.RevokeInvite(org.ID, "BOB"); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}
	if err := st.RevokeInvite(org.ID, "bob"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("re-revoke err = %v, want ErrNoInvite", err)
	}
	// A consumed/revoked invite no longer accepts.
	if err := st.AcceptInvite(bob.ID, "acme"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("accept revoked err = %v, want ErrNoInvite", err)
	}
	// Accepting a nonexistent org is the same error (no existence leak).
	if err := st.AcceptInvite(bob.ID, "nope"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("accept unknown org err = %v, want ErrNoInvite", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestInvite|TestDeclineAndRevoke' -v`
Expected: FAIL — compile errors on the new methods and errors.

- [ ] **Step 3: Implement**

Append to `internal/relay/orgs.go` (add `"strings"` to its imports):

```go
// ErrAlreadyMember is returned when inviting someone who is already a member.
var ErrAlreadyMember = errors.New("already a member")

// ErrNoInvite is returned when no matching pending invite exists — including
// for a nonexistent org, so accept/decline don't leak org existence.
var ErrNoInvite = errors.New("no such invite")

// CreateInvite records a pending invite for a GitHub username (stored
// lowercased; matching is case-insensitive). Inviting an existing member is
// ErrAlreadyMember; re-inviting the same login is idempotent. The username is
// not validated against GitHub — a typo'd invite sits pending until revoked.
func (s *Store) CreateInvite(orgID, githubLogin, inviterID string) error {
	login := strings.ToLower(githubLogin)
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM org_members m JOIN accounts a ON a.id = m.account_id
		  WHERE m.org_id = ? AND lower(a.github_login) = ?`, orgID, login).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrAlreadyMember
	}
	_, err := s.db.Exec(
		`INSERT INTO org_invites(org_id, github_login, invited_by, created_at) VALUES(?,?,?,?)`,
		orgID, login, inviterID, time.Now().UTC().Format(time.RFC3339Nano))
	if isUniqueViolation(err) {
		return nil // an identical pending invite already exists
	}
	return err
}

// OrgInvites lists an org's pending invite logins, oldest first.
func (s *Store) OrgInvites(orgID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT github_login FROM org_invites WHERE org_id = ? ORDER BY rowid`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logins []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		logins = append(logins, l)
	}
	return logins, rows.Err()
}

// RevokeInvite withdraws a pending invite. ErrNoInvite if none matches.
func (s *Store) RevokeInvite(orgID, githubLogin string) error {
	res, err := s.db.Exec(
		`DELETE FROM org_invites WHERE org_id = ? AND github_login = ?`,
		orgID, strings.ToLower(githubLogin))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoInvite
	}
	return nil
}

// InvitesForAccount lists the org slugs holding a pending invite for the
// account's current GitHub login. An account with no stored login (pre-org
// migration, hasn't re-logged in) has no matchable invites.
func (s *Store) InvitesForAccount(accountID string) ([]string, error) {
	var login sql.NullString
	if err := s.db.QueryRow(
		`SELECT github_login FROM accounts WHERE id = ?`, accountID).Scan(&login); err != nil {
		return nil, err
	}
	if !login.Valid || login.String == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT o.username FROM org_invites i JOIN accounts o ON o.id = i.org_id
		  WHERE i.github_login = ? ORDER BY i.rowid`, strings.ToLower(login.String))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

// takeInvite consumes (deletes) the pending invite matching accountID's
// current GitHub login in the org named orgSlug, returning the org id.
// Any miss — unknown org, no stored login, no invite — is ErrNoInvite.
func takeInvite(tx *sql.Tx, accountID, orgSlug string) (orgID string, err error) {
	err = tx.QueryRow(
		`SELECT id FROM accounts WHERE username = ? AND type = 'org'`, orgSlug).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoInvite
	}
	if err != nil {
		return "", err
	}
	var login sql.NullString
	if err := tx.QueryRow(
		`SELECT github_login FROM accounts WHERE id = ?`, accountID).Scan(&login); err != nil {
		return "", err
	}
	if !login.Valid || login.String == "" {
		return "", ErrNoInvite
	}
	res, err := tx.Exec(
		`DELETE FROM org_invites WHERE org_id = ? AND github_login = ?`,
		orgID, strings.ToLower(login.String))
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNoInvite
	}
	return orgID, nil
}

// AcceptInvite consumes accountID's pending invite to orgSlug and adds the
// membership as "member" (owners promote afterwards).
func (s *Store) AcceptInvite(accountID, orgSlug string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	orgID, err := takeInvite(tx, accountID, orgSlug)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO org_members(org_id, account_id, role, created_at) VALUES(?,?,'member',?)`,
		orgID, accountID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

// DeclineInvite consumes the invite without creating a membership.
func (s *Store) DeclineInvite(accountID, orgSlug string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := takeInvite(tx, accountID, orgSlug); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/orgs.go internal/relay/orgs_test.go
git commit -m "feat(relay): org invites — create, list, revoke, accept, decline

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 6: Authz primitives — `CanControl` and `AgentsVisibleTo`

`AgentsForAccount` (personal agents only) is replaced by `AgentsVisibleTo` (personal + org agents, each tagged with the owner slug). Its only non-test consumer is the control proxy list (`proxy.go:46`), updated in Task 7 — this task swaps the store method and rewrites the store test.

**Files:**
- Modify: `internal/relay/orgs.go` (add the two methods)
- Modify: `internal/relay/accounts.go` (delete `AgentsForAccount`)
- Modify: `internal/relay/proxy.go` (switch the list call — minimal, to keep the package compiling; the owner field lands in Task 7)
- Test: `internal/relay/orgs_test.go`, `internal/relay/accounts_test.go`

**Interfaces:**
- Consumes: Tasks 3–4.
- Produces:
  - `CanControl(callerID, ownerID string) (bool, error)` — true iff caller **is** the owner or is a member (any role) of the owning org
  - `type OwnedAgent struct { BaseDomain, Owner string }` (`Owner` = owning account/org slug)
  - `AgentsVisibleTo(accountID string) ([]OwnedAgent, error)` — enrollment order
  - `AgentsForAccount` **no longer exists**

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/orgs_test.go`:

```go
func TestCanControlOwnerAndOrgMember(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	mallory, _ := st.UpsertAccount("gh-mallory", "mallory")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	cases := []struct {
		name             string
		caller, owner    string
		want             bool
	}{
		{"self", alice.ID, alice.ID, true},
		{"org owner", alice.ID, org.ID, true},
		{"org member", bob.ID, org.ID, true},
		{"non-member", mallory.ID, org.ID, false},
		{"other user's box", mallory.ID, alice.ID, false},
	}
	for _, c := range cases {
		got, err := st.CanControl(c.caller, c.owner)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("CanControl(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAgentsVisibleToMergesPersonalAndOrg(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	personal, _ := st.EnrollForAccount(bob.ID)
	orgEn, _ := st.EnrollForAccount(org.ID)
	st.EnrollForAccount(alice.ID) // alice's personal box: invisible to bob

	got, err := st.AgentsVisibleTo(bob.ID)
	if err != nil {
		t.Fatalf("AgentsVisibleTo: %v", err)
	}
	want := []OwnedAgent{
		{BaseDomain: personal.BaseDomain, Owner: "bob"},
		{BaseDomain: orgEn.BaseDomain, Owner: "acme"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("AgentsVisibleTo = %+v, want %+v", got, want)
	}

	// An account with nothing visible lists empty, not an error.
	carol, _ := st.UpsertAccount("gh-carol", "carol")
	if got, err := st.AgentsVisibleTo(carol.ID); err != nil || len(got) != 0 {
		t.Fatalf("empty AgentsVisibleTo = %+v (%v), want none", got, err)
	}
}
```

In `internal/relay/accounts_test.go`, delete `TestAgentsForAccountListsOnlyOwnAgents` (superseded by `TestAgentsVisibleToMergesPersonalAndOrg`, which keeps its cross-tenant and empty-list assertions).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestCanControl|TestAgentsVisibleTo' -v`
Expected: FAIL — compile errors on `CanControl`, `OwnedAgent`, `AgentsVisibleTo`.

- [ ] **Step 3: Implement**

Append to `internal/relay/orgs.go`:

```go
// CanControl reports whether caller may drive agents owned by owner: the
// owner itself, or any member of the owning org. Role does not matter here —
// owners and members both drive; role only gates org management.
func (s *Store) CanControl(callerID, ownerID string) (bool, error) {
	if callerID == ownerID {
		return true, nil
	}
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM org_members WHERE org_id = ? AND account_id = ?`,
		ownerID, callerID).Scan(&n)
	return n > 0, err
}

// OwnedAgent is one row of an account's visible-agents list.
type OwnedAgent struct {
	BaseDomain string
	Owner      string // owning account/org slug
}

// AgentsVisibleTo returns the agents accountID may drive — its own plus those
// of every org it belongs to — in enrollment order, tagged with the owner slug.
func (s *Store) AgentsVisibleTo(accountID string) ([]OwnedAgent, error) {
	rows, err := s.db.Query(
		`SELECT ag.base_domain, acc.username
		   FROM agents ag JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.account_id = ?
		     OR ag.account_id IN (SELECT org_id FROM org_members WHERE account_id = ?)
		  ORDER BY ag.rowid`, accountID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []OwnedAgent
	for rows.Next() {
		var a OwnedAgent
		if err := rows.Scan(&a.BaseDomain, &a.Owner); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}
```

Delete `AgentsForAccount` (and its doc comment) from `internal/relay/accounts.go`.

In `internal/relay/proxy.go`, replace the list block's store call so the package compiles (full handler update comes in Task 7):

```go
			visible, err := st.AgentsVisibleTo(acc.ID)
			if err != nil {
				http.Error(w, "list failed", http.StatusInternalServerError)
				return
			}
			agents := make([]map[string]any, 0, len(visible))
			for _, a := range visible {
				_, connected := router.Lookup(a.BaseDomain)
				agents = append(agents, map[string]any{"agent": a.BaseDomain, "connected": connected})
			}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS (full suite — existing proxy list tests still pass since the response shape is unchanged so far).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/orgs.go internal/relay/orgs_test.go internal/relay/accounts.go internal/relay/accounts_test.go internal/relay/proxy.go
git commit -m "feat(relay): CanControl + AgentsVisibleTo replace AgentsForAccount

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 7: Control proxy — org-scoped authz and `owner` in the agents list

This is #104's acceptance criterion: a member drives the org's boxes through the existing control plane; a non-member gets `404`.

**Files:**
- Modify: `internal/relay/proxy.go`
- Test: `internal/relay/proxy_test.go`

**Interfaces:**
- Consumes: Task 6's `CanControl` and `AgentsVisibleTo`.
- Produces: `/agents` rows are now `{"agent", "owner", "connected"}`; per-agent authz is owner-or-member. Nothing else changes: Token B injection, liveness, 503-offline, 404 shapes all stay.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/proxy_test.go`:

```go
// orgProxyFixture: alice owns org "acme" with an enrolled agent; bob is a
// member, mallory a stranger.
func orgProxyFixture(t *testing.T) (api http.Handler, st *Store, router *Router, bobCred, malloryCred, base string) {
	t.Helper()
	st = openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	alice, err := st.UpsertAccount("sub-alice", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.UpsertAccount("sub-bob", "bob")
	if err != nil {
		t.Fatal(err)
	}
	bobCred, _ = st.MintAccountCredential(bob.ID)
	mallory, err := st.UpsertAccount("sub-mallory", "mallory")
	if err != nil {
		t.Fatal(err)
	}
	malloryCred, _ = st.MintAccountCredential(mallory.ID)

	org, err := st.CreateOrg(alice.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	addMember(t, st, org.ID, bob.ID, "member")
	en, err := st.EnrollForAccount(org.ID)
	if err != nil {
		t.Fatal(err)
	}
	base = en.BaseDomain
	router = NewRouter()
	api = NewAPIWithTunnel(st, NewFakeVerifier(), "", router, nil)
	return
}

func TestControlProxyOrgMemberDrivesBox(t *testing.T) {
	api, st, router, bobCred, malloryCred, base := orgProxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)
	if err := st.SetControlToken(base, "boxtok"); err != nil {
		t.Fatal(err)
	}

	// A plain member drives the org's box end-to-end, Token B injected.
	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", bobCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("member request: %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "auth=Bearer boxtok") {
		t.Fatalf("Token B not injected for member: %q", rr.Body.String())
	}

	// A non-member gets 404 — indistinguishable from an unknown agent.
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("non-member: %d, want 404", rr.Code)
	}
	// Liveness is equally gated.
	if rr := proxyGet(t, api, "/agents/"+base, malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("non-member liveness: %d, want 404", rr.Code)
	}
}

func TestControlProxyDisabledOrgSeversMembers(t *testing.T) {
	api, st, router, bobCred, _, base := orgProxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)

	if err := st.DisableAccount("acme"); err != nil {
		t.Fatal(err)
	}
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", bobCred); rr.Code != http.StatusNotFound {
		t.Fatalf("disabled org: %d, want 404", rr.Code)
	}
}

func TestControlProxyListIncludesOrgAgentsWithOwner(t *testing.T) {
	api, st, router, bobCred, _, base := orgProxyFixture(t)
	relaySess, _ := pipeSession(t, base)
	router.Register(relaySess)

	// Bob also has a personal box.
	acc, err := st.AuthenticateAccount(bobCred)
	if err != nil {
		t.Fatal(err)
	}
	personal, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}

	rr := proxyGet(t, api, "/agents", bobCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d (body %s)", rr.Code, rr.Body.String())
	}
	var list struct {
		Agents []struct {
			Agent     string `json:"agent"`
			Owner     string `json:"owner"`
			Connected bool   `json:"connected"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Agents) != 2 {
		t.Fatalf("listed %d agents, want 2: %+v", len(list.Agents), list.Agents)
	}
	if list.Agents[0].Agent != base || list.Agents[0].Owner != "acme" || !list.Agents[0].Connected {
		t.Errorf("agent[0] = %+v, want %s owner=acme connected", list.Agents[0], base)
	}
	if list.Agents[1].Agent != personal.BaseDomain || list.Agents[1].Owner != "bob" || list.Agents[1].Connected {
		t.Errorf("agent[1] = %+v, want %s owner=bob offline", list.Agents[1], personal.BaseDomain)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestControlProxyOrg -v` then `go test ./internal/relay/ -run 'TestControlProxyDisabledOrg|TestControlProxyListIncludesOrg' -v`
Expected: FAIL — member gets 404 (owner-only check), list rows lack `owner`.

- [ ] **Step 3: Implement**

In `internal/relay/proxy.go`:

Replace the per-agent authz check:

```go
		ownerID, _, err := st.AgentAccount(base)
		if err != nil {
			// Unknown agent and disabled owner both 404: no existence leak.
			http.NotFound(w, r)
			return
		}
		if ok, err := st.CanControl(acc.ID, ownerID); err != nil || !ok {
			http.NotFound(w, r)
			return
		}
```

Add `"owner"` to the list rows (from Task 6's interim block):

```go
				agents = append(agents, map[string]any{"agent": a.BaseDomain, "owner": a.Owner, "connected": connected})
```

Update `NewControlProxy`'s doc comment: change "authorizes that the account owns the agent" to "authorizes that the account owns the agent or is a member of the owning org (#104)".

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS (full suite — the pre-org proxy tests must keep passing untouched).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/proxy.go internal/relay/proxy_test.go
git commit -m "feat(relay): org members drive org boxes through the control proxy

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 8: API — `authAccount` helper, `POST /v1/orgs`, `GET /v1/orgs`

**Files:**
- Create: `internal/relay/orgs_api.go`
- Create: `internal/relay/orgs_api_test.go`
- Modify: `internal/relay/api.go` (extract `authAccount` from `enroll`; register org routes)

**Interfaces:**
- Consumes: Task 3 store methods; `writeJSON`, `bearerToken` from `api.go`.
- Produces:
  - `(a *api) authAccount(w, r) (Account, bool)` — 401/500 written on false; reused by every org handler and Tasks 9–12
  - `(a *api) registerOrgRoutes(mux *http.ServeMux)` — called from `NewAPIWithTunnel`; later tasks add routes inside it
  - `POST /v1/orgs {"name"} → 200 {"org","role":"owner"}`; empty/missing name → 400
  - `GET /v1/orgs → 200 {"orgs":[{"org","role"}]}` (empty array, never null)
  - Test helpers `orgAPIFixture` and `apiReq` (below) reused by Tasks 9–12

- [ ] **Step 1: Write the failing test**

Create `internal/relay/orgs_api_test.go`:

```go
package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// orgAPIFixture: an API over a store with two logged-in users.
func orgAPIFixture(t *testing.T) (api http.Handler, st *Store, aliceCred, bobCred string) {
	t.Helper()
	st = openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	alice, err := st.UpsertAccount("sub-alice", "alice")
	if err != nil {
		t.Fatal(err)
	}
	aliceCred, _ = st.MintAccountCredential(alice.ID)
	bob, err := st.UpsertAccount("sub-bob", "bob")
	if err != nil {
		t.Fatal(err)
	}
	bobCred, _ = st.MintAccountCredential(bob.ID)
	api = NewAPI(st, NewFakeVerifier())
	return
}

// apiReq performs one JSON request against the API.
func apiReq(t *testing.T, api http.Handler, method, path, cred, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	return rr
}

func TestOrgCreateAndList(t *testing.T) {
	api, _, aliceCred, bobCred := orgAPIFixture(t)

	// Auth gates.
	if rr := apiReq(t, api, "POST", "/v1/orgs", "", `{"name":"acme"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no cred: %d, want 401", rr.Code)
	}
	if rr := apiReq(t, api, "POST", "/v1/orgs", aliceCred, `{}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty name: %d, want 400", rr.Code)
	}

	rr := apiReq(t, api, "POST", "/v1/orgs", aliceCred, `{"name":"Acme Robotics"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: %d (body %s)", rr.Code, rr.Body.String())
	}
	var created struct {
		Org  string `json:"org"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Org != "acme-robotics" || created.Role != "owner" {
		t.Fatalf("created = %+v, want acme-robotics/owner", created)
	}

	// Creator lists it; a stranger's list stays empty (and non-null).
	rr = apiReq(t, api, "GET", "/v1/orgs", aliceCred, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	var list struct {
		Orgs []struct {
			Org  string `json:"org"`
			Role string `json:"role"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Orgs) != 1 || list.Orgs[0].Org != "acme-robotics" || list.Orgs[0].Role != "owner" {
		t.Fatalf("list = %+v, want [acme-robotics owner]", list.Orgs)
	}

	rr = apiReq(t, api, "GET", "/v1/orgs", bobCred, "")
	list.Orgs = nil
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Orgs == nil || len(list.Orgs) != 0 {
		t.Fatalf("bob's list = %+v, want empty non-null array", list.Orgs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestOrgCreateAndList -v`
Expected: FAIL — 404s (routes not registered).

- [ ] **Step 3: Implement**

In `internal/relay/api.go`, extract the auth preamble (add `"errors"` to imports if absent — it's already there):

```go
// authAccount authenticates the request's bearer account credential, writing
// the error response itself when it fails.
func (a *api) authAccount(w http.ResponseWriter, r *http.Request) (Account, bool) {
	cred, ok := bearerToken(r)
	if !ok {
		http.Error(w, "missing bearer credential", http.StatusUnauthorized)
		return Account{}, false
	}
	acc, err := a.st.AuthenticateAccount(cred)
	if errors.Is(err, ErrBadCredential) {
		http.Error(w, "bad credential", http.StatusUnauthorized)
		return Account{}, false
	}
	if err != nil {
		http.Error(w, "auth error", http.StatusInternalServerError)
		return Account{}, false
	}
	return acc, true
}
```

Rewrite `enroll`'s preamble to use it:

```go
func (a *api) enroll(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	en, err := a.st.EnrollForAccount(acc.ID)
	// ... rest unchanged
```

Register the org routes in `NewAPIWithTunnel` (after the `/v1/enroll` line):

```go
	a.registerOrgRoutes(mux)
```

Create `internal/relay/orgs_api.go`:

```go
package relay

import (
	"encoding/json"
	"net/http"
	"strings"
)

// registerOrgRoutes wires the org-management surface (#104). All handlers
// authenticate the account credential; org-scoped ones resolve the {slug}
// through OrgRole, whose ErrNoOrg (unknown org OR non-member) maps to 404 so
// org existence never leaks.
func (a *api) registerOrgRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/orgs", a.orgCreate)
	mux.HandleFunc("GET /v1/orgs", a.orgList)
}

func (a *api) orgCreate(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	org, err := a.st.CreateOrg(acc.ID, req.Name)
	if err != nil {
		http.Error(w, "org create failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"org": org.Slug, "role": "owner"})
}

func (a *api) orgList(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgs, err := a.st.OrgsForAccount(acc.ID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]string, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, map[string]string{"org": o.Slug, "role": o.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"orgs": out})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS (existing enroll tests confirm the `authAccount` refactor is behavior-preserving).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/orgs_api.go internal/relay/orgs_api_test.go internal/relay/api.go
git commit -m "feat(relay): org API — create and list orgs, shared authAccount helper

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 9: API — members endpoints

**Files:**
- Modify: `internal/relay/orgs_api.go`
- Test: `internal/relay/orgs_api_test.go`

**Interfaces:**
- Consumes: Task 4 store methods; Task 8's `authAccount`, `orgAPIFixture`, `apiReq`, and Task 4's `addMember`.
- Produces:
  - `(a *api) orgForMember(w, r, accID) (orgID, role string, ok bool)` — 404 on ErrNoOrg; reused by Tasks 10–12
  - `GET /v1/orgs/{slug}/members → 200 {"members":[{"username","role"}]}` (any member)
  - `PUT /v1/orgs/{slug}/members/{username} {"role"} → 200` (owner; bad role → 400; ErrNotMember → 404; ErrLastOwner → 409; non-owner → 403)
  - `DELETE /v1/orgs/{slug}/members/{username} → 200` (owner, or the member removing themselves)

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/orgs_api_test.go`:

```go
// orgWithMember: alice owns "acme", bob is a plain member.
func orgWithMember(t *testing.T, st *Store, aliceCred, bobCred string) (orgID string) {
	t.Helper()
	alice, err := st.AuthenticateAccount(aliceCred)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.AuthenticateAccount(bobCred)
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.CreateOrg(alice.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	addMember(t, st, org.ID, bob.ID, "member")
	return org.ID
}

func TestOrgMembersEndpointRoleMatrix(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	orgWithMember(t, st, aliceCred, bobCred)
	mallory, _ := st.UpsertAccount("sub-mallory", "mallory")
	malloryCred, _ := st.MintAccountCredential(mallory.ID)

	// Any member reads the list; a non-member gets 404, not 403 (no leak).
	rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", bobCred, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("member list: %d", rr.Code)
	}
	var list struct {
		Members []struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"members"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Members) != 2 || list.Members[0].Username != "alice" || list.Members[0].Role != "owner" ||
		list.Members[1].Username != "bob" || list.Members[1].Role != "member" {
		t.Fatalf("members = %+v", list.Members)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", malloryCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("non-member list: %d, want 404", rr.Code)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/ghost/members", aliceCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown org list: %d, want 404", rr.Code)
	}

	// Promote: owner-only; members get 403; bad role 400; unknown target 404.
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", bobCred, `{"role":"owner"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("member promote: %d, want 403", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", aliceCred, `{"role":"admin"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad role: %d, want 400", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/nobody", aliceCred, `{"role":"member"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown target: %d, want 404", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", aliceCred, `{"role":"owner"}`); rr.Code != http.StatusOK {
		t.Fatalf("promote: %d", rr.Code)
	}

	// Last-owner guard surfaces as 409 (bob demoted back first).
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", aliceCred, `{"role":"member"}`); rr.Code != http.StatusOK {
		t.Fatalf("demote bob: %d", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/alice", aliceCred, `{"role":"member"}`); rr.Code != http.StatusConflict {
		t.Fatalf("demote last owner: %d, want 409", rr.Code)
	}
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/members/alice", aliceCred, ""); rr.Code != http.StatusConflict {
		t.Fatalf("remove last owner: %d, want 409", rr.Code)
	}
}

func TestOrgMemberRemovalAndSelfLeave(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	orgWithMember(t, st, aliceCred, bobCred)

	// A member cannot remove someone else...
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/members/alice", bobCred, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("member removes other: %d, want 403", rr.Code)
	}
	// ...but may leave.
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/members/bob", bobCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("self-leave: %d", rr.Code)
	}
	// Gone: the org now 404s for bob.
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", bobCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("after leave: %d, want 404", rr.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestOrgMembersEndpoint|TestOrgMemberRemoval' -v`
Expected: FAIL — 404s from unregistered routes.

- [ ] **Step 3: Implement**

In `internal/relay/orgs_api.go`, add routes to `registerOrgRoutes`:

```go
	mux.HandleFunc("GET /v1/orgs/{slug}/members", a.orgMembers)
	mux.HandleFunc("PUT /v1/orgs/{slug}/members/{username}", a.orgSetRole)
	mux.HandleFunc("DELETE /v1/orgs/{slug}/members/{username}", a.orgRemoveMember)
```

Add the handlers and shared resolver (add `"errors"` to the file's imports):

```go
// orgForMember resolves {slug} for the authenticated caller. Non-members and
// unknown orgs both 404 (ErrNoOrg is deliberately ambiguous).
func (a *api) orgForMember(w http.ResponseWriter, r *http.Request, accID string) (orgID, role string, ok bool) {
	orgID, role, err := a.st.OrgRole(r.PathValue("slug"), accID)
	if errors.Is(err, ErrNoOrg) {
		http.NotFound(w, r)
		return "", "", false
	}
	if err != nil {
		http.Error(w, "org error", http.StatusInternalServerError)
		return "", "", false
	}
	return orgID, role, true
}

func (a *api) orgMembers(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, _, ok := a.orgForMember(w, r, acc.ID)
	if !ok {
		return
	}
	members, err := a.st.Members(orgID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]string, 0, len(members))
	for _, m := range members {
		out = append(out, map[string]string{"username": m.Username, "role": m.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

// writeMembershipErr maps the shared membership store errors onto HTTP.
func writeMembershipErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotMember):
		http.NotFound(w, r)
	case errors.Is(err, ErrLastOwner):
		http.Error(w, "an org must keep at least one owner", http.StatusConflict)
	default:
		http.Error(w, "membership error", http.StatusInternalServerError)
	}
}

func (a *api) orgSetRole(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, role, ok := a.orgForMember(w, r, acc.ID)
	if !ok {
		return
	}
	if role != "owner" {
		http.Error(w, "owner role required", http.StatusForbidden)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Role != "owner" && req.Role != "member") {
		http.Error(w, `role must be "owner" or "member"`, http.StatusBadRequest)
		return
	}
	if err := a.st.SetMemberRole(orgID, r.PathValue("username"), req.Role); err != nil {
		writeMembershipErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": r.PathValue("username"), "role": req.Role})
}

func (a *api) orgRemoveMember(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, role, ok := a.orgForMember(w, r, acc.ID)
	if !ok {
		return
	}
	target := r.PathValue("username")
	// Owners remove anyone; a plain member may only remove themselves (leave).
	if role != "owner" && target != acc.Username {
		http.Error(w, "owner role required", http.StatusForbidden)
		return
	}
	if err := a.st.RemoveMember(orgID, target); err != nil {
		writeMembershipErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"removed": target})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/orgs_api.go internal/relay/orgs_api_test.go
git commit -m "feat(relay): org members API — list, role changes, removal, self-leave

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 10: API — invites endpoints

**Files:**
- Modify: `internal/relay/orgs_api.go`
- Test: `internal/relay/orgs_api_test.go`

**Interfaces:**
- Consumes: Task 5 store methods; Task 9's `orgForMember`.
- Produces:
  - `POST /v1/orgs/{slug}/invites {"github_username"} → 200` (owner; empty → 400; ErrAlreadyMember → 409)
  - `GET /v1/orgs/{slug}/invites → 200 {"invites":["login",…]}` (owner)
  - `DELETE /v1/orgs/{slug}/invites/{login} → 200` (owner; ErrNoInvite → 404)
  - `GET /v1/invites → 200 {"invites":[{"org":slug},…]}` (any account)
  - `POST /v1/invites/{slug}/accept → 200`, `POST /v1/invites/{slug}/decline → 200` (ErrNoInvite → 404)

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/orgs_api_test.go`:

```go
func TestInviteEndpointsFullFlow(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	alice, _ := st.AuthenticateAccount(aliceCred)
	if _, err := st.CreateOrg(alice.ID, "acme"); err != nil {
		t.Fatal(err)
	}

	// Owner-only invite creation; members/strangers can't see the surface.
	if rr := apiReq(t, api, "POST", "/v1/orgs/acme/invites", bobCred, `{"github_username":"x"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("non-member invites: %d, want 404", rr.Code)
	}
	if rr := apiReq(t, api, "POST", "/v1/orgs/acme/invites", aliceCred, `{}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty username: %d, want 400", rr.Code)
	}
	if rr := apiReq(t, api, "POST", "/v1/orgs/acme/invites", aliceCred, `{"github_username":"Bob"}`); rr.Code != http.StatusOK {
		t.Fatalf("invite: %d (body %s)", rr.Code, rr.Body.String())
	}

	// Owner sees it pending; the invitee sees it under /v1/invites.
	rr := apiReq(t, api, "GET", "/v1/orgs/acme/invites", aliceCred, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"bob"`) {
		t.Fatalf("pending list: %d %s", rr.Code, rr.Body.String())
	}
	rr = apiReq(t, api, "GET", "/v1/invites", bobCred, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("my invites: %d", rr.Code)
	}
	var mine struct {
		Invites []struct {
			Org string `json:"org"`
		} `json:"invites"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&mine); err != nil {
		t.Fatal(err)
	}
	if len(mine.Invites) != 1 || mine.Invites[0].Org != "acme" {
		t.Fatalf("my invites = %+v, want [acme]", mine.Invites)
	}

	// Accept → member; invite consumed; re-accept 404.
	if rr := apiReq(t, api, "POST", "/v1/invites/acme/accept", bobCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("accept: %d", rr.Code)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", bobCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("bob not a member after accept: %d", rr.Code)
	}
	if rr := apiReq(t, api, "POST", "/v1/invites/acme/accept", bobCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("re-accept: %d, want 404", rr.Code)
	}
	// Inviting an existing member → 409.
	if rr := apiReq(t, api, "POST", "/v1/orgs/acme/invites", aliceCred, `{"github_username":"bob"}`); rr.Code != http.StatusConflict {
		t.Fatalf("invite member: %d, want 409", rr.Code)
	}
}

func TestInviteDeclineAndRevokeEndpoints(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	alice, _ := st.AuthenticateAccount(aliceCred)
	if _, err := st.CreateOrg(alice.ID, "acme"); err != nil {
		t.Fatal(err)
	}

	apiReq(t, api, "POST", "/v1/orgs/acme/invites", aliceCred, `{"github_username":"bob"}`)
	if rr := apiReq(t, api, "POST", "/v1/invites/acme/decline", bobCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("decline: %d", rr.Code)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", bobCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("decline must not add membership: %d, want 404", rr.Code)
	}

	apiReq(t, api, "POST", "/v1/orgs/acme/invites", aliceCred, `{"github_username":"bob"}`)
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/invites/bob", aliceCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rr.Code)
	}
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/invites/bob", aliceCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("re-revoke: %d, want 404", rr.Code)
	}
	// Accepting a nonexistent org's invite → 404 (no existence probe).
	if rr := apiReq(t, api, "POST", "/v1/invites/ghost/accept", bobCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("accept unknown org: %d, want 404", rr.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestInviteEndpoints|TestInviteDeclineAndRevoke' -v`
Expected: FAIL — 404s from unregistered routes (note: the non-member 404 assertion may pass vacuously until routes exist; the 200/400/409 assertions are the failing signal).

- [ ] **Step 3: Implement**

Add routes to `registerOrgRoutes`:

```go
	mux.HandleFunc("POST /v1/orgs/{slug}/invites", a.orgInvite)
	mux.HandleFunc("GET /v1/orgs/{slug}/invites", a.orgInvitesList)
	mux.HandleFunc("DELETE /v1/orgs/{slug}/invites/{login}", a.orgRevokeInvite)
	mux.HandleFunc("GET /v1/invites", a.myInvites)
	mux.HandleFunc("POST /v1/invites/{slug}/accept", a.inviteAccept)
	mux.HandleFunc("POST /v1/invites/{slug}/decline", a.inviteDecline)
```

Add the handlers:

```go
// requireOwner is orgForMember plus the owner gate shared by the owner-only
// management endpoints.
func (a *api) requireOwner(w http.ResponseWriter, r *http.Request, accID string) (orgID string, ok bool) {
	orgID, role, ok := a.orgForMember(w, r, accID)
	if !ok {
		return "", false
	}
	if role != "owner" {
		http.Error(w, "owner role required", http.StatusForbidden)
		return "", false
	}
	return orgID, true
}

func (a *api) orgInvite(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, ok := a.requireOwner(w, r, acc.ID)
	if !ok {
		return
	}
	var req struct {
		GithubUsername string `json:"github_username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.GithubUsername) == "" {
		http.Error(w, "github_username required", http.StatusBadRequest)
		return
	}
	err := a.st.CreateInvite(orgID, req.GithubUsername, acc.ID)
	if errors.Is(err, ErrAlreadyMember) {
		http.Error(w, "already a member", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "invite failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"invited": strings.ToLower(req.GithubUsername)})
}

func (a *api) orgInvitesList(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, ok := a.requireOwner(w, r, acc.ID)
	if !ok {
		return
	}
	logins, err := a.st.OrgInvites(orgID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	if logins == nil {
		logins = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": logins})
}

func (a *api) orgRevokeInvite(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, ok := a.requireOwner(w, r, acc.ID)
	if !ok {
		return
	}
	err := a.st.RevokeInvite(orgID, r.PathValue("login"))
	if errors.Is(err, ErrNoInvite) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"revoked": strings.ToLower(r.PathValue("login"))})
}

func (a *api) myInvites(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	slugs, err := a.st.InvitesForAccount(acc.ID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]string, 0, len(slugs))
	for _, s := range slugs {
		out = append(out, map[string]string{"org": s})
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

func (a *api) inviteAccept(w http.ResponseWriter, r *http.Request) {
	a.consumeInvite(w, r, a.st.AcceptInvite, "accepted")
}

func (a *api) inviteDecline(w http.ResponseWriter, r *http.Request) {
	a.consumeInvite(w, r, a.st.DeclineInvite, "declined")
}

func (a *api) consumeInvite(w http.ResponseWriter, r *http.Request, act func(accountID, orgSlug string) error, verb string) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	slug := r.PathValue("slug")
	err := act(acc.ID, slug)
	if errors.Is(err, ErrNoInvite) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "invite error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{verb: slug})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/orgs_api.go internal/relay/orgs_api_test.go
git commit -m "feat(relay): invites API — org-side manage, caller-side accept/decline

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 11: API — enroll into an org

**Files:**
- Modify: `internal/relay/api.go` (`enroll`)
- Test: `internal/relay/orgs_api_test.go`

**Interfaces:**
- Consumes: Task 3's `OrgRole`; Task 8's `authAccount`.
- Produces: `POST /v1/enroll` accepts an optional JSON body `{"org":"<slug>"}`. With `org`: caller must be an **owner** (member → 403, non-member/unknown → 404); the box binds to the org's account and gets `<hash>-<orgslug>.<apex>`. Without a body: personal enrollment, byte-for-byte unchanged.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/orgs_api_test.go`:

```go
func TestEnrollIntoOrg(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	orgWithMember(t, st, aliceCred, bobCred) // alice owner, bob member

	// Owner enrolls into the org: base domain carries the org slug.
	rr := apiReq(t, api, "POST", "/v1/enroll", aliceCred, `{"org":"acme"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("org enroll: %d (body %s)", rr.Code, rr.Body.String())
	}
	var en struct {
		BaseDomain string `json:"base_domain"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&en); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(en.BaseDomain, "-acme.") {
		t.Fatalf("base domain = %q, want the acme org slug", en.BaseDomain)
	}

	// A plain member may not enroll (owners manage the footprint).
	if rr := apiReq(t, api, "POST", "/v1/enroll", bobCred, `{"org":"acme"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("member org enroll: %d, want 403", rr.Code)
	}
	// Unknown org and non-member are indistinguishable 404s.
	if rr := apiReq(t, api, "POST", "/v1/enroll", aliceCred, `{"org":"ghost"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown org enroll: %d, want 404", rr.Code)
	}

	// No body: personal enrollment still works exactly as before.
	rr = apiReq(t, api, "POST", "/v1/enroll", aliceCred, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("personal enroll: %d", rr.Code)
	}
	en.BaseDomain = ""
	if err := json.NewDecoder(rr.Body).Decode(&en); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(en.BaseDomain, "-alice.") {
		t.Fatalf("personal base domain = %q, want the alice slug", en.BaseDomain)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestEnrollIntoOrg -v`
Expected: FAIL — org enroll returns the *personal* base domain (`-alice.`), body ignored.

- [ ] **Step 3: Implement**

In `internal/relay/api.go`, rewrite `enroll` (after the Task 8 refactor):

```go
func (a *api) enroll(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	// Optional body: {"org":"<slug>"} enrolls the box into an org the caller
	// owns. No/empty body is personal enrollment, unchanged.
	var req struct {
		Org string `json:"org"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	targetID := acc.ID
	if req.Org != "" {
		orgID, role, err := a.st.OrgRole(req.Org, acc.ID)
		if errors.Is(err, ErrNoOrg) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "org error", http.StatusInternalServerError)
			return
		}
		if role != "owner" {
			http.Error(w, "owner role required", http.StatusForbidden)
			return
		}
		targetID = orgID
	}
	en, err := a.st.EnrollForAccount(targetID)
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS (existing enroll tests prove the no-body path is unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/api.go internal/relay/orgs_api_test.go
git commit -m "feat(relay): enroll a box into an org (owner-gated org param)

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 12: Org deletion — store + API

**Files:**
- Modify: `internal/relay/orgs.go`, `internal/relay/orgs_api.go`
- Test: `internal/relay/orgs_test.go`, `internal/relay/orgs_api_test.go`

**Interfaces:**
- Consumes: Tasks 3–10.
- Produces:
  - `var ErrOrgHasAgents = errors.New("org still owns agents")`
  - `DeleteOrg(orgID string) error` — removes the org account row, memberships, invites, and hostname rows; refused while agents exist
  - `DELETE /v1/orgs/{slug} → 200` (owner; ErrOrgHasAgents → 409; non-owner → 403; non-member/unknown → 404)

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/orgs_test.go`:

```go
func TestDeleteOrgRefusedWhileAgentsExist(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")
	st.CreateInvite(org.ID, "someone", alice.ID)

	if _, err := st.EnrollForAccount(org.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteOrg(org.ID); !errors.Is(err, ErrOrgHasAgents) {
		t.Fatalf("delete with agents err = %v, want ErrOrgHasAgents", err)
	}

	// Clear the agent, then deletion sweeps members, invites, and the slug.
	if _, err := st.db.Exec(`DELETE FROM agents WHERE account_id=?`, org.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteOrg(org.ID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	if orgs, _ := st.OrgsForAccount(alice.ID); len(orgs) != 0 {
		t.Fatalf("membership survived delete: %+v", orgs)
	}
	if _, _, err := st.OrgRole("acme", alice.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("org survived delete: %v", err)
	}
	// The slug is free again.
	if _, err := st.CreateOrg(alice.ID, "acme"); err != nil {
		t.Fatalf("slug not freed: %v", err)
	}
}
```

Append to `internal/relay/orgs_api_test.go`:

```go
func TestOrgDeleteEndpoint(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	orgWithMember(t, st, aliceCred, bobCred)

	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme", bobCred, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("member delete: %d, want 403", rr.Code)
	}
	alice, _ := st.AuthenticateAccount(aliceCred)
	org, _, err := st.OrgRole("acme", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnrollForAccount(org); err != nil {
		t.Fatal(err)
	}
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme", aliceCred, ""); rr.Code != http.StatusConflict {
		t.Fatalf("delete with agents: %d, want 409", rr.Code)
	}
	if _, err := st.db.Exec(`DELETE FROM agents WHERE account_id=?`, org); err != nil {
		t.Fatal(err)
	}
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme", aliceCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("delete: %d", rr.Code)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", aliceCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("org survived: %d, want 404", rr.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestDeleteOrg|TestOrgDeleteEndpoint' -v`
Expected: FAIL — `st.DeleteOrg undefined`.

- [ ] **Step 3: Implement**

Append to `internal/relay/orgs.go`:

```go
// ErrOrgHasAgents is returned when deleting an org that still owns agents —
// boxes must be re-homed or retired first, never orphaned.
var ErrOrgHasAgents = errors.New("org still owns agents")

// DeleteOrg removes an empty org: its memberships, pending invites, hostname
// rows, and the account row itself. Refused while the org owns agents.
func (s *Store) DeleteOrg(orgID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var agents int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE account_id = ?`, orgID).Scan(&agents); err != nil {
		return err
	}
	if agents > 0 {
		return ErrOrgHasAgents
	}
	for _, stmt := range []string{
		`DELETE FROM org_invites WHERE org_id = ?`,
		`DELETE FROM org_members WHERE org_id = ?`,
		`DELETE FROM hostnames WHERE account_id = ?`,
		`DELETE FROM accounts WHERE id = ? AND type = 'org'`,
	} {
		if _, err := tx.Exec(stmt, orgID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

In `internal/relay/orgs_api.go`, register and add the handler:

```go
	mux.HandleFunc("DELETE /v1/orgs/{slug}", a.orgDelete)
```

```go
func (a *api) orgDelete(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgID, ok := a.requireOwner(w, r, acc.ID)
	if !ok {
		return
	}
	err := a.st.DeleteOrg(orgID)
	if errors.Is(err, ErrOrgHasAgents) {
		http.Error(w, "org still owns agents", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("slug")})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/orgs.go internal/relay/orgs_api.go internal/relay/orgs_test.go internal/relay/orgs_api_test.go
git commit -m "feat(relay): delete empty orgs, refused while agents exist

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 13: Verify, PROGRESS.md, PR

**Files:**
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: everything above.
- Produces: a green `make verify`, an updated progress map, and an open PR.

- [ ] **Step 1: Run the full gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, all tests pass, arm64 cross-build succeeds. Fix anything it flags before proceeding (gofmt failures are `make fmt`).

- [ ] **Step 2: Update PROGRESS.md**

Add one line to the relay section of `PROGRESS.md` (match the file's existing one-line + issue-link style; do not restate details):

```markdown
- Organizations: org accounts, membership + invites, org-scoped control authz [#104]
```

- [ ] **Step 3: Commit and push**

```bash
git add PROGRESS.md
git commit -m "docs: record relay organizations in PROGRESS

Part of #104.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
git push -u origin ozykhan/relay-orgs-design
```

- [ ] **Step 4: Open the PR**

```bash
gh pr create --base main --title "[relay] Organizations: org model, membership, and org-scoped agent authz" --body "$(cat <<'EOF'
Implements organizations on the relay per the approved design doc
(docs/superpowers/specs/2026-07-11-relay-organizations-design.md, included).

- An org is a login-less account (type='org'): agents, hostnames, quotas, and
  the kill-switch reuse accounts.id unchanged.
- org_members (owner/member) + org_invites (by GitHub username, invite+accept,
  works pre-first-login); accounts store the raw github_login, refreshed at login.
- Control-proxy authz is now owner-or-member, still 404 on failure (no
  existence leak); Token B stays per-agent and validated at piperd.
- /v1/enroll takes an optional owner-gated org slug; /agents rows carry owner.
- Org management API: create/list/delete, members, role changes, self-leave,
  invites accept/decline; last-owner and delete-only-when-empty guards.

Closes #104

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 5: Verify PR opened**

Run: `gh pr view --json url,title -q .url`
Expected: the PR URL prints.
