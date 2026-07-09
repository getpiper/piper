package relay

import (
	"path/filepath"
	"strings"
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

func TestMintAndAuthenticateCredential(t *testing.T) {
	st := openTestStore(t)
	acc, _ := st.UpsertAccount("sub-1", "carol")

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
	acc, _ := st.UpsertAccount("sub-1", "dave")
	cred, _ := st.MintAccountCredential(acc.ID)

	if err := st.DisableAccount(acc.Username); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}
	if _, err := st.AuthenticateAccount(cred); err != ErrBadCredential {
		t.Fatalf("disabled cred err = %v, want ErrBadCredential", err)
	}
}

func TestEnrollForAccountAssignsLabelAndBindsAccount(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	acc, _ := st.UpsertAccount("sub-1", "erin")

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
	st.Configure("public.getpiper.co", 2, 10)
	acc, _ := st.UpsertAccount("sub-1", "frank")

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
	st.Configure("public.getpiper.co", 3, 10)
	acc, _ := st.UpsertAccount("sub-1", "grace")
	en, _ := st.EnrollForAccount(acc.ID)

	if err := st.DisableAccount(acc.Username); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}
	if _, err := st.Authenticate(en.Token); err != ErrBadToken {
		t.Fatalf("disabled agent auth err = %v, want ErrBadToken", err)
	}
}

func TestAgentsForAccountListsOnlyOwnAgents(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	alice, _ := st.UpsertAccount("sub-alice", "alice")
	bob, _ := st.UpsertAccount("sub-bob", "bob")

	var want []string
	for i := 0; i < 2; i++ {
		en, err := st.EnrollForAccount(alice.ID)
		if err != nil {
			t.Fatalf("enroll alice %d: %v", i, err)
		}
		want = append(want, en.BaseDomain)
	}
	if _, err := st.EnrollForAccount(bob.ID); err != nil {
		t.Fatalf("enroll bob: %v", err)
	}

	got, err := st.AgentsForAccount(alice.ID)
	if err != nil {
		t.Fatalf("AgentsForAccount: %v", err)
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("AgentsForAccount = %v, want %v (enrollment order)", got, want)
	}

	// An account with no agents lists empty, not an error.
	carol, _ := st.UpsertAccount("sub-carol", "carol")
	got, err = st.AgentsForAccount(carol.ID)
	if err != nil {
		t.Fatalf("AgentsForAccount(empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("AgentsForAccount(empty) = %v, want none", got)
	}
}
