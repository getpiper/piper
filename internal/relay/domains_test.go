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
