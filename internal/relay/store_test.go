package relay

import (
	"database/sql"
	"errors"
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

func TestOpenSetsBusyTimeout(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var timeout int
	if err := st.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

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

func TestControlTokenRoundTrip(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-ct", "ct")
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

// A modified agent must not be able to claim relay-managed names as its
// "custom domain": another agent's base domain (SNI hijack), the apex, a
// DNS-label parent/child of either, or its own base domain.
func TestSetCustomDomainRejectsRelayNamespace(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Configure("public.getpiper.co", 3, 10, 5)
	if _, err := st.Enroll("alice", "alice.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enroll("bob", "bob.example.com"); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{
		"alice.example.com",      // another agent's base domain
		"shop.alice.example.com", // subdomain of it
		"example.com",            // DNS-label parent of enrolled bases
		"bob.example.com",        // the requester's own base domain
		"public.getpiper.co",     // the relay apex
		"x.public.getpiper.co",   // subdomain of the apex (incl. api.<apex>)
		"getpiper.co",            // parent of the apex
	} {
		if _, err := st.SetCustomDomain("bob.example.com", d); !errors.Is(err, ErrDomainReserved) {
			t.Errorf("SetCustomDomain(%q) err = %v, want ErrDomainReserved", d, err)
		}
	}
	for _, d := range []string{
		"Not.A.Domain", // uppercase
		"no-dots",
		"-bad.example.dev",
		"shop..dev",
	} {
		if _, err := st.SetCustomDomain("bob.example.com", d); !errors.Is(err, ErrInvalidDomain) {
			t.Errorf("SetCustomDomain(%q) err = %v, want ErrInvalidDomain", d, err)
		}
	}
	// Nothing above may have stuck.
	if got, _ := st.CustomDomain("bob.example.com"); got != "" {
		t.Fatalf("custom domain = %q, want none", got)
	}
	// A legitimate unrelated domain still works.
	if _, err := st.SetCustomDomain("bob.example.com", "shop.dev"); err != nil {
		t.Fatalf("legit domain rejected: %v", err)
	}
	// "Suffix" means DNS labels, not raw strings: xalice.example.comx shares a
	// raw suffix with alice.example.com but is a different DNS name.
	if _, err := st.SetCustomDomain("bob.example.com", "xalice.example.comx"); err != nil {
		t.Fatalf("raw-suffix lookalike rejected: %v", err)
	}
}

func TestSetCustomDomainRejectsDisabledAccount(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A healthy account may set its custom domain.
	if _, err := st.SetCustomDomain(en.BaseDomain, "shop.dev"); err != nil {
		t.Fatalf("healthy account rejected: %v", err)
	}
	// Once disabled, it may not re-route to a new domain.
	if err := st.DisableAccount(acc.Username); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetCustomDomain(en.BaseDomain, "other.dev"); !errors.Is(err, ErrBadCredential) {
		t.Fatalf("disabled account: err = %v, want ErrBadCredential", err)
	}
}

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

// The partial unique index closes the SELECT-then-UPDATE FCFS race at the
// schema level: even a write that skips the pre-check cannot duplicate a
// custom domain. Cleared rows (” / NULL) stay unconstrained.
func TestCustomDomainUniqueIndex(t *testing.T) {
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
	if _, err := st.SetCustomDomain("alice.example.com", "shop.dev"); err != nil {
		t.Fatal(err)
	}
	// Bypass the pre-check: the index itself must refuse the duplicate.
	if _, err := st.db.Exec(
		`UPDATE agents SET custom_domain='shop.dev' WHERE base_domain='bob.example.com'`); err == nil {
		t.Fatal("duplicate custom_domain accepted; unique index missing")
	}
	// Two cleared agents may coexist.
	if _, err := st.SetCustomDomain("alice.example.com", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(
		`UPDATE agents SET custom_domain='' WHERE base_domain='bob.example.com'`); err != nil {
		t.Fatalf("empty custom_domain must not collide: %v", err)
	}
}
