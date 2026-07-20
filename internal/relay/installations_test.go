package relay

import (
	"errors"
	"testing"
)

func TestLinkInstallationBindsToSenderAccount(t *testing.T) {
	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatalf("LinkInstallation: %v", err)
	}

	got, err := st.AccountForInstallation("55")
	if err != nil {
		t.Fatalf("AccountForInstallation: %v", err)
	}
	if got != acc.ID {
		t.Fatalf("account = %q, want %q", got, acc.ID)
	}

	inst, err := st.InstallationForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationForAccount: %v", err)
	}
	if inst != "55" {
		t.Fatalf("installation = %q, want 55", inst)
	}
}

func TestLinkInstallationIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.UpsertAccount("1001", "alice"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
			t.Fatalf("LinkInstallation #%d: %v", i, err)
		}
	}
}

func TestLinkInstallationUnknownSender(t *testing.T) {
	st := openTestStore(t)
	err := st.LinkInstallation("55", "9999", "user", "nobody")
	if !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("err = %v, want ErrUnknownAccount", err)
	}
}

func TestAccountForInstallationUnknown(t *testing.T) {
	st := openTestStore(t)
	_, err := st.AccountForInstallation("404")
	if !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("err = %v, want ErrNoInstallation", err)
	}
}

func TestUnlinkInstallation(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.UpsertAccount("1001", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnlinkInstallation("55"); err != nil {
		t.Fatalf("UnlinkInstallation: %v", err)
	}
	if _, err := st.AccountForInstallation("55"); !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("err = %v, want ErrNoInstallation", err)
	}
}
