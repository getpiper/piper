package relay

import (
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
	st.Configure("public.getpiper.co", 3, 10)
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
