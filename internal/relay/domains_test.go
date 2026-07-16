package relay

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
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
