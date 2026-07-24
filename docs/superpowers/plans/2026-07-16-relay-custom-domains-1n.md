# Relay Custom Domains 1:N Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The relay holds many custom domains per agent, each starting `pending` (routable immediately, evictable after 1h if unconfirmed) and flipping `active` when the box confirms cert issuance — with the v0.1.0 `set-domain` op kept working as a compat shim.

**Architecture:** A new `custom_domains` SQLite table (domain PK = structural FCFS) replaces the single `agents.custom_domain` column, which is migrated in as `active` rows then cleared. Three new control ops (`add-domain`, `remove-domain`, `domain-active`) ride the existing authenticated `KindControl` stream; the router is untouched (already per-domain). Expiry is lazy: rival claims evict, reconnect re-derivation filters.

**Tech Stack:** Go, `modernc.org/sqlite` (pure Go — **no cgo, ever**), existing `internal/tunnel` mux.

**Spec:** `docs/superpowers/specs/2026-07-16-relay-per-app-custom-domains-design.md` · **Issue:** #227 (epic #224)

## Global Constraints

- `CGO_ENABLED=0` everywhere; `make verify` (gofmt → vet → test → arm64 cross-build) must pass before the work is done.
- Branch `ozykhan/relay-custom-domains` (already created, spec committed). One commit per task, conventional-commit style, ending with `Co-Authored-By: Claude {current model} <noreply@anthropic.com>`.
- Status strings for custom_domains rows are exactly `"pending"` and `"active"`.
- Pending TTL is exactly `const pendingTTL = time.Hour`.
- Per-agent domain cap defaults to **5**, counting live rows (active + unexpired pending), operator-configurable via `Configure` / `PIPER_RELAY_MAX_DOMAINS`.
- Layering: `internal/relay` never imports `internal/agent`; `internal/agent` never imports `internal/relay`. Shared shapes live in `internal/tunnel`.
- All timestamps stored as `time.Now().UTC().Format(time.RFC3339Nano)` (existing convention). **Never compare stored timestamps lexically in SQL** — RFC3339Nano trims trailing zeros, so ordering is unreliable; parse in Go instead (the table is ≤5 rows/agent).

---

### Task 1: `custom_domains` table + column migration

**Files:**
- Modify: `internal/relay/schema.sql`
- Modify: `internal/relay/store.go` (inside `Open`, after the `agents_custom_domain_unique` index creation, ~line 99)
- Test: `internal/relay/domains_test.go` (create)

**Interfaces:**
- Produces: the `custom_domains(domain PK, agent_base, status, created_at)` table, guaranteed present and column-migrated after any `Open`. Later tasks read/write it.

- [ ] **Step 1: Write the failing migration test**

Create `internal/relay/domains_test.go`:

```go
package relay

import (
	"path/filepath"
	"testing"
)

// Opening a store that predates custom_domains (#227) must fold the legacy
// agents.custom_domain column into the new table as an active row, then
// clear the column — clearing is what stops the copy (which re-runs on every
// Open) from resurrecting a domain the agent later removes.
func TestOpenMigratesCustomDomainColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enroll("alice", "alice.example.com"); err != nil {
		t.Fatal(err)
	}
	// Simulate a v0.1.0 row: domain lives in the agents column.
	if _, err := st.db.Exec(
		`UPDATE agents SET custom_domain='shop.dev' WHERE base_domain='alice.example.com'`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var agent, status string
	if err := st.db.QueryRow(
		`SELECT agent_base, status FROM custom_domains WHERE domain='shop.dev'`).
		Scan(&agent, &status); err != nil {
		t.Fatalf("migrated row: %v", err)
	}
	if agent != "alice.example.com" || status != "active" {
		t.Fatalf("migrated row = %s/%s, want alice.example.com/active", agent, status)
	}
	var col string
	if err := st.db.QueryRow(
		`SELECT custom_domain FROM agents WHERE base_domain='alice.example.com'`).Scan(&col); err != nil {
		t.Fatal(err)
	}
	if col != "" {
		t.Fatalf("agents.custom_domain = %q after migration, want cleared", col)
	}

	// Removal must survive a re-Open: delete the row, reopen, row stays gone.
	if _, err := st.db.Exec(`DELETE FROM custom_domains WHERE domain='shop.dev'`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM custom_domains`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("re-Open resurrected %d custom_domains rows, want 0", n)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/relay/ -run TestOpenMigratesCustomDomainColumn -v`
Expected: FAIL with `no such table: custom_domains`

- [ ] **Step 3: Add the table and the migration**

Append to `internal/relay/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS custom_domains (
    domain      TEXT PRIMARY KEY,
    agent_base  TEXT NOT NULL,
    status      TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
```

(The `domain` primary key is the FCFS backstop — no partial unique index needed, unlike the legacy column.)

In `internal/relay/store.go`, inside `Open`, directly after the `agents_custom_domain_unique` index `db.Exec` block (before `return &Store{db: db}, nil`), add:

```go
	// #227: per-app custom domains live in custom_domains; fold the legacy
	// single agents.custom_domain column in as an active row (those boxes
	// proved ownership via DNS-01 under #102), then clear the column.
	// Clearing is correctness, not tidiness: this copy re-runs on every
	// Open, so a stale column value would resurrect a domain the agent has
	// since removed. One-way; the column and its index stay, unused.
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO custom_domains(domain, agent_base, status, created_at)
		    SELECT custom_domain, base_domain, 'active', ?
		    FROM agents WHERE custom_domain IS NOT NULL AND custom_domain != ''`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate custom domains: %w", err)
	}
	if _, err := db.Exec(
		`UPDATE agents SET custom_domain='' WHERE custom_domain IS NOT NULL AND custom_domain != ''`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate custom domains: %w", err)
	}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/relay/ -run TestOpenMigratesCustomDomainColumn -v`
Expected: PASS

- [ ] **Step 5: Run the whole relay package** (the migration touches every `Open`)

Run: `go test ./internal/relay/`
Expected: PASS. Note: `TestSetCustomDomain`, `TestSetDomainControlOp` etc. still pass — they exercise the legacy column path, which is untouched until Task 4.

- [ ] **Step 6: Commit**

```bash
git add internal/relay/schema.sql internal/relay/store.go internal/relay/domains_test.go
git commit -m "feat(relay): custom_domains table + one-way column migration

Part of #227.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 2: `AddCustomDomain` + `CustomDomains` (claim, cap, TTL, eviction)

**Files:**
- Create: `internal/relay/domains.go`
- Modify: `internal/relay/store.go` (Store struct, `Configure`, `Open`)
- Modify: `cmd/piper-relay/main.go:149-153` (Configure call)
- Modify: every test calling `st.Configure(` (mechanical, see Step 4)
- Test: `internal/relay/domains_test.go`

**Interfaces:**
- Consumes: `custom_domains` table (Task 1); existing `customDomainRE`, `domainClaimable`, `ErrBadToken`, `ErrDomainTaken`, `ErrInvalidDomain`, `ErrQuotaExceeded` (all in `internal/relay` already).
- Produces (later tasks call these exactly):
  - `func (s *Store) AddCustomDomain(baseDomain, domain string) error`
  - `func (s *Store) CustomDomains(baseDomain string) ([]string, error)`
  - `const pendingTTL = time.Hour`
  - `Store.nowFunc func() time.Time` (test hook, set by `Open`)
  - `func (s *Store) Configure(apex string, maxAgents, maxApps, maxDomains int)` — **note the new 4th parameter**

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/domains_test.go`:

```go
func openDomainsStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Enroll("alice", "alice.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enroll("bob", "bob.example.com"); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAddCustomDomainClaimAndList(t *testing.T) {
	st := openDomainsStore(t)
	if err := st.AddCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.AddCustomDomain("alice.example.com", "blog.dev"); err != nil {
		t.Fatalf("second add: %v", err)
	}
	got, err := st.CustomDomains("alice.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "blog.dev" || got[1] != "shop.dev" {
		t.Fatalf("CustomDomains = %v, want [blog.dev shop.dev]", got)
	}
	// FCFS: a live pending claim blocks a rival.
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("rival claim: err = %v, want ErrDomainTaken", err)
	}
	// Validation and identity errors mirror SetCustomDomain's.
	if err := st.AddCustomDomain("alice.example.com", "Bad_Domain"); !errors.Is(err, ErrInvalidDomain) {
		t.Fatalf("malformed: %v", err)
	}
	if err := st.AddCustomDomain("alice.example.com", "bob.example.com"); !errors.Is(err, ErrDomainReserved) {
		t.Fatalf("relay namespace: %v", err)
	}
	if err := st.AddCustomDomain("nobody.example.com", "x.dev"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("unknown agent: %v", err)
	}
}

// An expired pending claim is squat, not ownership: a rival claim evicts it.
// Re-adding your own pending domain refreshes the window.
func TestAddCustomDomainExpiryAndEviction(t *testing.T) {
	st := openDomainsStore(t)
	now := time.Now()
	st.nowFunc = func() time.Time { return now }

	if err := st.AddCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatal(err)
	}
	// Not yet expired: rival still blocked at TTL-1s.
	now = now.Add(pendingTTL - time.Second)
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("rival before expiry: %v", err)
	}
	// Refresh resets alice's window.
	if err := st.AddCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	now = now.Add(pendingTTL - time.Second)
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("rival after refresh: %v", err)
	}
	// Past the (refreshed) TTL the claim is evictable.
	now = now.Add(2 * time.Second)
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); err != nil {
		t.Fatalf("evicting expired squat: %v", err)
	}
	if got, _ := st.CustomDomains("bob.example.com"); len(got) != 1 || got[0] != "shop.dev" {
		t.Fatalf("bob's domains = %v", got)
	}
	if got, _ := st.CustomDomains("alice.example.com"); len(got) != 0 {
		t.Fatalf("alice still lists %v after eviction", got)
	}
	// Expired pending rows are filtered from the list (reconnect re-derive).
	now = now.Add(pendingTTL + time.Second)
	if got, _ := st.CustomDomains("bob.example.com"); len(got) != 0 {
		t.Fatalf("expired pending still listed: %v", got)
	}
}

func TestAddCustomDomainCap(t *testing.T) {
	st := openDomainsStore(t)
	st.Configure("public.getpiper.co", 3, 10, 2)
	for i, d := range []string{"one.dev", "two.dev"} {
		if err := st.AddCustomDomain("alice.example.com", d); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if err := st.AddCustomDomain("alice.example.com", "three.dev"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over cap: err = %v, want ErrQuotaExceeded", err)
	}
	// The cap is per agent, not global.
	if err := st.AddCustomDomain("bob.example.com", "three.dev"); err != nil {
		t.Fatalf("bob under cap: %v", err)
	}
	// Re-adding an existing domain is not a new claim and must not hit the cap.
	if err := st.AddCustomDomain("alice.example.com", "one.dev"); err != nil {
		t.Fatalf("re-add at cap: %v", err)
	}
}
```

Add `"errors"` and `"time"` to the imports of `domains_test.go`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestAddCustomDomain' -v`
Expected: FAIL to compile — `st.AddCustomDomain undefined`, `st.nowFunc undefined`, wrong `Configure` arity.

- [ ] **Step 3: Extend Store config, then implement**

In `internal/relay/store.go`:

1. Add fields to the struct (after `maxApps int`): `maxDomains int` and `nowFunc func() time.Time`.
2. Replace `Configure` with:

```go
// Configure sets the free-tier apex, the per-account agent cap (EnrollForAccount),
// the per-account app cap (RegisterHostname), and the per-agent custom-domain
// cap (AddCustomDomain). Safe to call once after Open.
func (s *Store) Configure(apex string, maxAgents, maxApps, maxDomains int) {
	s.apex = apex
	s.maxAgents = maxAgents
	s.maxApps = maxApps
	s.maxDomains = maxDomains
}
```

3. In `Open`, change the final return to seed the clock hook: `return &Store{db: db, nowFunc: time.Now}, nil`.

Create `internal/relay/domains.go`:

```go
package relay

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Per-app BYO custom domains (#227). Each row is one domain claimed by one
// agent, pending until the box confirms cert issuance. A pending claim is
// routable immediately — that is what lets the TLS-ALPN-01 challenge reach
// the box before any cert exists — but expires after pendingTTL if never
// confirmed, so an unproven claim can squat a name for at most an hour.
// Expiry is lazy: rival claims evict, and CustomDomains filters at reconnect
// re-derivation; there is no background sweeper.

// pendingTTL is how long an unconfirmed pending claim holds a domain.
const pendingTTL = time.Hour

// ErrDomainNotFound is returned when an agent confirms a domain it does not hold.
var ErrDomainNotFound = errors.New("domain not registered to this agent")

func (s *Store) maxDomainsOrDefault() int {
	if s.maxDomains <= 0 {
		return 5
	}
	return s.maxDomains
}

// liveAt reports whether a custom_domains row still counts: active rows
// always, pending rows only within pendingTTL of their claim. Timestamps are
// parsed here rather than compared in SQL — RFC3339Nano trims trailing
// zeros, so lexical order is unreliable. Unparsable rows count as expired.
func liveAt(status, createdAt string, now time.Time) bool {
	if status == "active" {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	return err == nil && now.Sub(t) < pendingTTL
}

// AddCustomDomain claims domain for the agent enrolled at baseDomain as a
// pending custom domain. First-come-first-served: a domain live under
// another agent is ErrDomainTaken, but an expired pending claim is evicted.
// Re-adding your own pending domain refreshes its TTL window (an operator
// retrying resets their clock); re-adding your own active domain is a no-op.
// New claims count toward the per-agent cap, pending included — pending rows
// are the squattable kind.
func (s *Store) AddCustomDomain(baseDomain, domain string) error {
	if !customDomainRE.MatchString(domain) {
		return ErrInvalidDomain
	}
	if err := s.domainClaimable(domain); err != nil {
		return err
	}
	now := s.nowFunc().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM agents WHERE base_domain=?`, baseDomain).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBadToken
		}
		return err
	}
	var owner, status, created string
	err = tx.QueryRow(
		`SELECT agent_base, status, created_at FROM custom_domains WHERE domain=?`, domain).
		Scan(&owner, &status, &created)
	switch {
	case err == nil && owner == baseDomain:
		if status == "pending" {
			if _, err := tx.Exec(`UPDATE custom_domains SET created_at=? WHERE domain=?`,
				now.Format(time.RFC3339Nano), domain); err != nil {
				return err
			}
		}
		return tx.Commit() // own active row: no-op re-add
	case err == nil:
		if liveAt(status, created, now) {
			return ErrDomainTaken
		}
		// Expired pending claim by another agent: evict and claim below.
		if _, err := tx.Exec(`DELETE FROM custom_domains WHERE domain=?`, domain); err != nil {
			return err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	live, err := countLive(tx, baseDomain, now)
	if err != nil {
		return err
	}
	if live >= s.maxDomainsOrDefault() {
		return ErrQuotaExceeded
	}
	if _, err := tx.Exec(
		`INSERT INTO custom_domains(domain, agent_base, status, created_at) VALUES(?, ?, 'pending', ?)`,
		domain, baseDomain, now.Format(time.RFC3339Nano)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrDomainTaken // PK backstop: lost the FCFS race
		}
		return err
	}
	return tx.Commit()
}

// countLive counts the agent's live rows inside tx (cap enforcement).
func countLive(tx *sql.Tx, baseDomain string, now time.Time) (int, error) {
	rows, err := tx.Query(
		`SELECT status, created_at FROM custom_domains WHERE agent_base=?`, baseDomain)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var st, ca string
		if err := rows.Scan(&st, &ca); err != nil {
			return 0, err
		}
		if liveAt(st, ca, now) {
			n++
		}
	}
	return n, rows.Err()
}

// CustomDomains returns the agent's live custom domains — active plus
// unexpired pending, sorted — for reconnect re-derivation. Expired pending
// rows are filtered, so a squat mapping dies at the next reconnect even if
// never contested.
func (s *Store) CustomDomains(baseDomain string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT domain, status, created_at FROM custom_domains WHERE agent_base=? ORDER BY domain`,
		baseDomain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.nowFunc().UTC()
	var out []string
	for rows.Next() {
		var d, st, ca string
		if err := rows.Scan(&d, &st, &ca); err != nil {
			return nil, err
		}
		if liveAt(st, ca, now) {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Fix the `Configure` callers (compile break is deliberate)**

Run: `grep -rn '\.Configure(' --include='*.go' .`

- `cmd/piper-relay/main.go:149-153` — add the fourth argument:

```go
	st.Configure(
		env("PIPER_RELAY_APEX", "public.getpiper.dev"),
		atoiOr(env("PIPER_RELAY_MAX_AGENTS", "3"), 3),
		atoiOr(env("PIPER_RELAY_MAX_APPS", "10"), 10),
		atoiOr(env("PIPER_RELAY_MAX_DOMAINS", "5"), 5),
	)
```

- Every test hit (e.g. `internal/relay/server_test.go:26` `st.Configure("public.getpiper.co", 3, 10)`): append `, 5` (or `, 0` where the test doesn't care — use `5` everywhere for uniformity). Do NOT change any other argument.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/relay/ ./cmd/... -run '.' 2>&1 | tail -20` then `go test ./internal/relay/ -run 'TestAddCustomDomain' -v`
Expected: everything compiles; the three new tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/relay/domains.go internal/relay/domains_test.go internal/relay/store.go cmd/piper-relay/main.go internal/relay/server_test.go $(grep -rl '\.Configure(' --include='*_test.go' internal cmd)
git commit -m "feat(relay): AddCustomDomain/CustomDomains — pending claims with TTL, cap, lazy eviction

Part of #227.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 3: `ConfirmCustomDomain` + `RemoveCustomDomain`

**Files:**
- Modify: `internal/relay/domains.go`
- Test: `internal/relay/domains_test.go`

**Interfaces:**
- Consumes: `custom_domains` rows created by `AddCustomDomain` (Task 2).
- Produces (Task 6 calls these exactly):
  - `func (s *Store) ConfirmCustomDomain(baseDomain, domain string) error` — `ErrDomainNotFound` when the agent doesn't hold the row
  - `func (s *Store) RemoveCustomDomain(baseDomain, domain string) error` — idempotent, nil when nothing to delete

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/domains_test.go`:

```go
func TestConfirmCustomDomain(t *testing.T) {
	st := openDomainsStore(t)
	now := time.Now()
	st.nowFunc = func() time.Time { return now }
	if err := st.AddCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatal(err)
	}
	// Only the holder may confirm.
	if err := st.ConfirmCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("rival confirm: err = %v, want ErrDomainNotFound", err)
	}
	if err := st.ConfirmCustomDomain("alice.example.com", "nope.dev"); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("missing row: err = %v, want ErrDomainNotFound", err)
	}
	// Confirm ignores pending age: eviction is the only claim-killer, so a
	// slow issuance (>TTL) still confirms if nobody contested the name.
	now = now.Add(pendingTTL + time.Minute)
	if err := st.ConfirmCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("confirm after TTL: %v", err)
	}
	// Active rows never expire from the list.
	now = now.Add(24 * time.Hour)
	if got, _ := st.CustomDomains("alice.example.com"); len(got) != 1 || got[0] != "shop.dev" {
		t.Fatalf("active domain listed = %v", got)
	}
	// Idempotent on active.
	if err := st.ConfirmCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
	// An active row blocks rivals forever.
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("rival vs active: %v", err)
	}
}

func TestRemoveCustomDomain(t *testing.T) {
	st := openDomainsStore(t)
	if err := st.AddCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatal(err)
	}
	// Another agent's remove must not touch the row.
	if err := st.RemoveCustomDomain("bob.example.com", "shop.dev"); err != nil {
		t.Fatalf("rival remove errored: %v", err)
	}
	if got, _ := st.CustomDomains("alice.example.com"); len(got) != 1 {
		t.Fatalf("rival remove deleted the row: %v", got)
	}
	if err := st.RemoveCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got, _ := st.CustomDomains("alice.example.com"); len(got) != 0 {
		t.Fatalf("still listed after remove: %v", got)
	}
	// Idempotent: removing again is a no-op, and the name is free for others.
	if err := st.RemoveCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatalf("re-remove: %v", err)
	}
	if err := st.AddCustomDomain("bob.example.com", "shop.dev"); err != nil {
		t.Fatalf("claim after remove: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestConfirmCustomDomain|TestRemoveCustomDomain' -v`
Expected: FAIL to compile — `ConfirmCustomDomain`/`RemoveCustomDomain` undefined.

- [ ] **Step 3: Implement**

Append to `internal/relay/domains.go`:

```go
// ConfirmCustomDomain flips the agent's own claim to active: the box reports
// it holds the issued cert (#229 sends this after TLS-ALPN-01 completes).
// Pending age is deliberately not checked — eviction by a rival claim is the
// only thing that kills a claim, so a slow issuance still confirms if nobody
// contested the name. Idempotent on active rows.
func (s *Store) ConfirmCustomDomain(baseDomain, domain string) error {
	res, err := s.db.Exec(
		`UPDATE custom_domains SET status='active' WHERE domain=? AND agent_base=?`,
		domain, baseDomain)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDomainNotFound
	}
	return nil
}

// RemoveCustomDomain drops the agent's own claim on domain. Idempotent —
// removing a domain the agent does not hold is a no-op, so teardown retries
// are safe.
func (s *Store) RemoveCustomDomain(baseDomain, domain string) error {
	_, err := s.db.Exec(
		`DELETE FROM custom_domains WHERE domain=? AND agent_base=?`, domain, baseDomain)
	return err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run 'TestConfirmCustomDomain|TestRemoveCustomDomain' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/relay/domains.go internal/relay/domains_test.go
git commit -m "feat(relay): ConfirmCustomDomain + RemoveCustomDomain

Part of #227.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 4: rewrite `SetCustomDomain` as the compat shim over `custom_domains`

**Files:**
- Modify: `internal/relay/domains.go` (new shim lives here)
- Modify: `internal/relay/store.go` (delete old `SetCustomDomain` ~lines 226-279 and `CustomDomain` ~lines 281-292)
- Modify: `internal/relay/store_test.go` (`TestSetCustomDomain` ~line 104; delete `TestCustomDomainUniqueIndex` ~line 272)
- Modify: `internal/relay/server_test.go:252,290` (two `st.CustomDomain(base)` call sites)

**Interfaces:**
- Consumes: `liveAt`, table, validation helpers (Tasks 1-3).
- Produces: `func (s *Store) SetCustomDomain(baseDomain, domain string) (string, error)` — **same signature as today**; replaces all the agent's rows with one `active` row (empty = clear all), returns the previous single domain. The single-value getter `CustomDomain` is **removed** (no production caller after Task 6; tests move to `CustomDomains`).

- [ ] **Step 1: Update the tests first**

In `internal/relay/store_test.go`:

1. Replace the two `st.CustomDomain(...)` getter call sites:

   - `store_test.go:121` (in `TestSetCustomDomain`):

```go
	got, err := st.CustomDomains("alice.example.com")
	if err != nil || len(got) != 1 || got[0] != "shop.dev" {
		t.Fatalf("CustomDomains = %v, %v", got, err)
	}
```

   - `store_test.go:188` (in `TestSetCustomDomainRejectsRelayNamespace`):

```go
	if got, _ := st.CustomDomains("bob.example.com"); len(got) != 0 {
		t.Fatalf("custom domains = %v after rejected claims, want none", got)
	}
```

2. Append two shim-specific assertions at the end of `TestSetCustomDomain`:

```go
	// The shim replaces ALL rows: per-app claims vanish when set-domain lands.
	if err := st.AddCustomDomain("alice.example.com", "extra.dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetCustomDomain("alice.example.com", "final.dev"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.CustomDomains("alice.example.com"); len(got) != 1 || got[0] != "final.dev" {
		t.Fatalf("after shim replace-all: %v", got)
	}
	// Shim rows are active immediately (DNS-01 proof predates the call): a
	// rival cannot evict them even after the pending TTL.
	var status string
	if err := st.db.QueryRow(
		`SELECT status FROM custom_domains WHERE domain='final.dev'`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("shim row status = %q, %v, want active", status, err)
	}
```

3. Delete `TestCustomDomainUniqueIndex` (~line 272) entirely — it exercises the retired column's partial unique index via raw `UPDATE agents SET custom_domain=...`; the FCFS backstop is now the `custom_domains` PK, covered by `TestAddCustomDomainClaimAndList`.

In `internal/relay/server_test.go`:

- Line ~252 (`TestSetDomainControlOp`): replace

```go
	if got, _ := st.CustomDomain(base); got != "shop.dev" {
		t.Fatalf("stored custom domain = %q", got)
	}
```

with

```go
	if got, _ := st.CustomDomains(base); len(got) != 1 || got[0] != "shop.dev" {
		t.Fatalf("stored custom domains = %v", got)
	}
```

- Line ~290 (`TestSetDomainControlOpRejectsHijack`): replace

```go
	if got, _ := st.CustomDomain(base); got != "" {
		t.Fatalf("custom domain = %q after rejected claims, want none", got)
	}
```

with

```go
	if got, _ := st.CustomDomains(base); len(got) != 0 {
		t.Fatalf("custom domains = %v after rejected claims, want none", got)
	}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestSetCustomDomain|TestSetDomainControlOp' -v`
Expected: FAIL — old `SetCustomDomain` writes the column, so `CustomDomains` (table) returns nothing.

- [ ] **Step 3: Replace the implementation**

Delete from `internal/relay/store.go`: the whole `SetCustomDomain` function (~lines 226-279) and the whole `CustomDomain` function (~lines 281-292). Leave `ErrDomainTaken`/`ErrInvalidDomain`/`ErrDomainReserved`, `customDomainRE`, `dnsOverlap`, `domainClaimable` where they are — `domains.go` uses them.

Append to `internal/relay/domains.go`:

```go
// SetCustomDomain is the v0.1.0 box-wide BYO op (#102), kept as a compat
// shim over custom_domains: the deployed public relay serves boxes that
// re-arm their domain with set-domain on every reconnect. Old semantics
// preserved exactly — replace ALL of the agent's rows with one active row
// (those boxes proved ownership via DNS-01 before calling; empty domain
// clears), returning the previous single domain so handleControl's
// unregister-previous logic is unchanged. A mixed agent holding N per-app
// rows and sending set-domain cannot occur in shipped combinations — nothing
// calls the per-app ops until #229, and #229 removes this op's caller — so
// replace-all is safe.
func (s *Store) SetCustomDomain(baseDomain, domain string) (string, error) {
	if domain != "" {
		if !customDomainRE.MatchString(domain) {
			return "", ErrInvalidDomain
		}
		if err := s.domainClaimable(domain); err != nil {
			return "", err
		}
	}
	now := s.nowFunc().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM agents WHERE base_domain=?`, baseDomain).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrBadToken
		}
		return "", err
	}
	if domain != "" {
		var owner, status, created string
		err := tx.QueryRow(
			`SELECT agent_base, status, created_at FROM custom_domains WHERE domain=?`, domain).
			Scan(&owner, &status, &created)
		switch {
		case err == nil && owner != baseDomain && liveAt(status, created, now):
			return "", ErrDomainTaken
		case err == nil && owner != baseDomain:
			// Expired pending squat: evict.
			if _, err := tx.Exec(`DELETE FROM custom_domains WHERE domain=?`, domain); err != nil {
				return "", err
			}
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return "", err
		}
	}
	var prev sql.NullString
	if err := tx.QueryRow(
		`SELECT domain FROM custom_domains WHERE agent_base=? ORDER BY domain LIMIT 1`,
		baseDomain).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if _, err := tx.Exec(`DELETE FROM custom_domains WHERE agent_base=?`, baseDomain); err != nil {
		return "", err
	}
	if domain != "" {
		if _, err := tx.Exec(
			`INSERT INTO custom_domains(domain, agent_base, status, created_at) VALUES(?, ?, 'active', ?)`,
			domain, baseDomain, now.Format(time.RFC3339Nano)); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return "", ErrDomainTaken
			}
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return prev.String, nil
}
```

- [ ] **Step 4: Fix the reconnect re-derive call site (compile break)**

`internal/relay/server.go:109-111` still calls the deleted `st.CustomDomain`. Replace:

```go
		if cd, err := st.CustomDomain(sess.BaseDomain); err == nil && cd != "" {
			router.RegisterCustom(cd, sess)
		}
```

with:

```go
		// Re-derive every live custom domain (active + unexpired pending);
		// expired pending squats are filtered by the store, so they also die
		// here even if never contested by a rival claim (#227).
		if domains, err := st.CustomDomains(sess.BaseDomain); err == nil {
			for _, d := range domains {
				router.RegisterCustom(d, sess)
			}
		}
```

(This is Task 6 territory conceptually, but the compile break forces it now; Task 6 adds the tests that pin it.)

- [ ] **Step 5: Run the package to verify everything passes**

Run: `go test ./internal/relay/ -v -run 'Domain' && go test ./internal/relay/`
Expected: PASS, including the untouched `TestSetDomainControlOpRejectsHijack` (shim validates identically).

- [ ] **Step 6: Commit**

```bash
git add internal/relay/domains.go internal/relay/store.go internal/relay/store_test.go internal/relay/server_test.go internal/relay/server.go
git commit -m "refactor(relay): SetCustomDomain becomes a compat shim over custom_domains

The legacy agents.custom_domain column is no longer written; the single-value
CustomDomain getter and the column's unique-index test retire with it.

Part of #227.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 5: tunnel ops + `TunnelClient` wrappers

**Files:**
- Modify: `internal/tunnel/tunnel.go:125` (Op doc comment only — the struct already carries `Domain`)
- Modify: `internal/agent/tunnelclient.go` (after `SetCustomDomain`, ~line 109)
- Test: `internal/agent/tunnelclient_test.go`

**Interfaces:**
- Consumes: `TunnelClient.control` (existing private helper), `tunnel.ControlRequest`.
- Produces (#229 will call these; nothing in this epic's relay side consumes them):
  - `func (c *TunnelClient) AddCustomDomain(domain string) error` → op `"add-domain"`
  - `func (c *TunnelClient) RemoveCustomDomain(domain string) error` → op `"remove-domain"`
  - `func (c *TunnelClient) ConfirmCustomDomain(domain string) error` → op `"domain-active"`

- [ ] **Step 1: Write the failing test**

Append to `internal/agent/tunnelclient_test.go` (mirrors `TestTunnelClientSetCustomDomain` at line 297, generalized over the three ops):

```go
// The per-app domain ops (#227) are thin control-stream wrappers; assert each
// sends the right op + domain and surfaces relay errors.
func TestTunnelClientDomainOps(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	relaySess := <-sessCh

	got := make(chan tunnel.ControlRequest, 3)
	go func() {
		for {
			kind, stream, err := relaySess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindControl {
				stream.Close()
				continue
			}
			var req tunnel.ControlRequest
			_ = tunnel.ReadMsg(stream, &req)
			got <- req
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
			stream.Close()
		}
	}()

	calls := []struct {
		name   string
		call   func(string) error
		wantOp string
	}{
		{"AddCustomDomain", c.AddCustomDomain, "add-domain"},
		{"ConfirmCustomDomain", c.ConfirmCustomDomain, "domain-active"},
		{"RemoveCustomDomain", c.RemoveCustomDomain, "remove-domain"},
	}
	for _, tc := range calls {
		var err error
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err = tc.call("shop.example.com"); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		req := <-got
		if req.Op != tc.wantOp || req.Domain != "shop.example.com" {
			t.Fatalf("%s sent %+v, want op=%s domain=shop.example.com", tc.name, req, tc.wantOp)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agent/ -run TestTunnelClientDomainOps -v`
Expected: FAIL to compile — the three methods are undefined.

- [ ] **Step 3: Implement**

In `internal/tunnel/tunnel.go`, update the Op field comment (line 125) to:

```go
	Op string `json:"op"` // "register" | "deregister" | "provision" | "set-domain" | "add-domain" | "remove-domain" | "domain-active"
```

In `internal/agent/tunnelclient.go`, after `SetCustomDomain` (~line 109), add:

```go
// AddCustomDomain claims domain on the relay as a pending per-app custom
// domain (#227): routable immediately so the TLS-ALPN-01 challenge can reach
// this box, evictable if not confirmed within the relay's pending TTL.
func (c *TunnelClient) AddCustomDomain(domain string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "add-domain", Domain: domain})
	return err
}

// RemoveCustomDomain drops this agent's claim on domain and its routing.
func (c *TunnelClient) RemoveCustomDomain(domain string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "remove-domain", Domain: domain})
	return err
}

// ConfirmCustomDomain reports that this box holds an issued cert for domain,
// flipping the relay's pending claim to active (permanent, reconnect-safe).
func (c *TunnelClient) ConfirmCustomDomain(domain string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "domain-active", Domain: domain})
	return err
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/agent/ -run TestTunnelClientDomainOps -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/tunnel.go internal/agent/tunnelclient.go internal/agent/tunnelclient_test.go
git commit -m "feat(agent): tunnel-client add/remove/confirm custom-domain ops

Part of #227.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

### Task 6: relay control handlers + routing lifecycle tests

**Files:**
- Modify: `internal/relay/server.go` (`handleControl`, after the `"set-domain"` case ~line 187)
- Test: `internal/relay/server_test.go`

**Interfaces:**
- Consumes: `AddCustomDomain`/`ConfirmCustomDomain`/`RemoveCustomDomain`/`CustomDomains` (Tasks 2-4), `Router.RegisterCustom`/`UnregisterCustom` (existing), ops from Task 5.
- Produces: the relay-side behavior #229 depends on — add routes-while-pending, confirm persists, remove unroutes, reconnect re-derives, rival eviction overwrites routing.

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/server_test.go`:

```go
// controlOp sends one control request over sess and returns the response,
// failing the test on transport errors or an unexpected error-ness.
func controlOp(t *testing.T, sess *tunnel.Session, req tunnel.ControlRequest, wantErr bool) tunnel.ControlResponse {
	t.Helper()
	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if err := tunnel.WriteMsg(cs, req); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	if wantErr && resp.Error == "" {
		t.Fatalf("%s %q accepted, want rejection", req.Op, req.Domain)
	}
	if !wantErr && resp.Error != "" {
		t.Fatalf("%s %q: %s", req.Op, req.Domain, resp.Error)
	}
	return resp
}

// A pending claim must route immediately: that is what lets the TLS-ALPN-01
// challenge reach the box before any cert exists (#227).
func TestAddDomainRoutesWhilePending(t *testing.T) {
	sess, tlsAddr, _, st := startTestRelay(t, nil, nil)

	got := make(chan byte, 1)
	go func() {
		for {
			kind, stream, err := sess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindPassthrough {
				stream.Close()
				continue
			}
			buf := make([]byte, 1)
			if _, err := io.ReadFull(stream, buf); err == nil {
				got <- buf[0]
			}
			stream.Close()
			return
		}
	}()

	controlOp(t, sess, tunnel.ControlRequest{Op: "add-domain", Domain: "shop.dev"}, false)
	if domains, _ := st.CustomDomains(sess.BaseDomain); len(domains) != 1 || domains[0] != "shop.dev" {
		t.Fatalf("stored domains = %v", domains)
	}

	conn, err := net.Dial("tcp", tlsAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tc := tls.Client(conn, &tls.Config{ServerName: "shop.dev", InsecureSkipVerify: true})
	go tc.Handshake() // never completes — only the ClientHello needs to travel
	select {
	case b := <-got:
		if b != 0x16 {
			t.Fatalf("first passthrough byte = %#x, want TLS record type 0x16", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no passthrough stream reached the agent for a pending domain")
	}
}

func TestDomainLifecycleControlOps(t *testing.T) {
	sess, _, base, st := startTestRelay(t, nil, nil)

	controlOp(t, sess, tunnel.ControlRequest{Op: "add-domain", Domain: "shop.dev"}, false)
	controlOp(t, sess, tunnel.ControlRequest{Op: "domain-active", Domain: "shop.dev"}, false)
	var status string
	if err := st.db.QueryRow(
		`SELECT status FROM custom_domains WHERE domain='shop.dev'`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("status = %q, %v, want active", status, err)
	}
	// Confirming a domain you don't hold is rejected.
	controlOp(t, sess, tunnel.ControlRequest{Op: "domain-active", Domain: "other.dev"}, true)
	// Malformed and relay-namespace domains are rejected on add.
	controlOp(t, sess, tunnel.ControlRequest{Op: "add-domain", Domain: "Bad_Domain"}, true)
	controlOp(t, sess, tunnel.ControlRequest{Op: "add-domain", Domain: base}, true)

	controlOp(t, sess, tunnel.ControlRequest{Op: "remove-domain", Domain: "shop.dev"}, false)
	if got, _ := st.CustomDomains(base); len(got) != 0 {
		t.Fatalf("domains after remove = %v", got)
	}
}

// Reconnect re-derives live domains (active + unexpired pending) and drops
// expired pending squats; a rival claim over an expired squat overwrites the
// router mapping in place.
func TestReconnectRederivesCustomDomains(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Configure("public.getpiper.co", 3, 10, 5)
	now := time.Now()
	st.nowFunc = func() time.Time { return now }
	tokA, err := st.Enroll("alice", "alice.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokB, err := st.Enroll("bob", "bob.example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Seed: one active, one fresh pending, one expired pending.
	if err := st.AddCustomDomain("alice.example.com", "active.dev"); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmCustomDomain("alice.example.com", "active.dev"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCustomDomain("alice.example.com", "squat.dev"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(pendingTTL + time.Second) // squat.dev expires
	if err := st.AddCustomDomain("alice.example.com", "fresh.dev"); err != nil {
		t.Fatal(err)
	}

	router := NewRouter()
	tunLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tunLn.Close()
	go acceptTunnels(tunLn, st, router)

	dial := func(tok, base string) *tunnel.Session {
		t.Helper()
		conn, err := net.Dial("tcp", tunLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		sess, err := tunnel.Dial(conn, tok, base)
		if err != nil {
			t.Fatal(err)
		}
		return sess
	}
	sessA := dial(tokA, "alice.example.com")
	defer sessA.Close()

	waitRouted := func(domain string, want *tunnel.Session) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if s, ok := router.Lookup(domain); ok && s == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s not routed to the expected session", domain)
	}
	waitRouted("active.dev", sessA)
	waitRouted("fresh.dev", sessA)
	if _, ok := router.Lookup("squat.dev"); ok {
		t.Fatal("expired pending domain routed after reconnect")
	}

	// Rival claim over the expired squat: bob's registration overwrites in place.
	sessB := dial(tokB, "bob.example.com")
	defer sessB.Close()
	controlOp(t, sessB, tunnel.ControlRequest{Op: "add-domain", Domain: "squat.dev"}, false)
	waitRouted("squat.dev", sessB)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestAddDomainRoutesWhilePending|TestDomainLifecycleControlOps|TestReconnectRederives' -v`
Expected: `TestReconnectRederivesCustomDomains` partially passes (the re-derive loop landed in Task 4 Step 4), but `add-domain`/`domain-active`/`remove-domain` come back `unknown op` — FAIL.

- [ ] **Step 3: Implement the control cases**

In `internal/relay/server.go`, `handleControl`, after the `"set-domain"` case (~line 187), add:

```go
	case "add-domain":
		// Per-app custom domain claim (#227): pending, routable immediately —
		// that is what lets the TLS-ALPN-01 challenge reach the box before any
		// cert exists. RegisterCustom overwrites any evicted squatter's mapping
		// (the router is keyed by domain), so its routing dies with the claim.
		if err := st.AddCustomDomain(sess.BaseDomain, req.Domain); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		router.RegisterCustom(req.Domain, sess)
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	case "domain-active":
		// The box reports it holds the issued cert; the claim becomes
		// permanent. Routing is already live, so the router is untouched.
		if err := st.ConfirmCustomDomain(sess.BaseDomain, req.Domain); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	case "remove-domain":
		if err := st.RemoveCustomDomain(sess.BaseDomain, req.Domain); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		router.UnregisterCustom(req.Domain)
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run 'TestAddDomainRoutesWhilePending|TestDomainLifecycleControlOps|TestReconnectRederives' -v`
Expected: PASS

- [ ] **Step 5: Full gate**

Run: `make verify`
Expected: gofmt clean, vet clean, all tests pass (Docker-dependent ones may skip), arm64 cross-build OK.

- [ ] **Step 6: Commit**

```bash
git add internal/relay/server.go internal/relay/server_test.go
git commit -m "feat(relay): add-domain/domain-active/remove-domain control ops

Pending claims route immediately for TLS-ALPN-01; reconnect re-derives live
domains and expired squats die by router overwrite or re-derive filtering.

Closes #227.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>"
```

---

## Post-plan checklist (not tasks — session wrap-up)

- Push and open the PR: `gh pr create --base main` with `Closes #227` and `Part of #224` in the body; squash-merge policy applies.
- The spec commit (`docs/superpowers/specs/2026-07-16-relay-per-app-custom-domains-design.md`) is already on this branch and rides along.
- `PROGRESS.md`: add a one-liner under the relay section linking `[#227]` when the PR merges.
