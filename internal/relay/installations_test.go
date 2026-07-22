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

	insts, err := st.InstallationsForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationsForAccount: %v", err)
	}
	if len(insts) != 1 || insts[0].ID != "55" {
		t.Fatalf("installations = %+v, want single id 55", insts)
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

func TestInstallationsForAccountReturnsAllNewestFirst(t *testing.T) {
	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("66", "1001", "org", "getpiper"); err != nil {
		t.Fatal(err)
	}

	got, err := st.InstallationsForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationsForAccount: %v", err)
	}
	want := []Installation{
		{ID: "66", TargetType: "org", TargetLogin: "getpiper"},
		{ID: "55", TargetType: "user", TargetLogin: "alice"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("installations = %+v, want %+v", got, want)
	}
}

func TestInstallationsForAccountEmpty(t *testing.T) {
	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.InstallationsForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationsForAccount: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("installations = %+v, want empty", got)
	}
}
