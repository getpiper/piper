package relay

import (
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
