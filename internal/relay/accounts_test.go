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
