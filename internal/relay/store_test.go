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
