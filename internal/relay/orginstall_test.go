package relay

import (
	"errors"
	"testing"
)

func TestSetOrgGitHubRequiresOrg(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("1001", "alice")
	// A user account is not an org.
	if err := st.SetOrgGitHub(alice.ID, "acme"); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("SetOrgGitHub on a user = %v, want ErrNoOrg", err)
	}
	org, _ := st.CreateOrg(alice.ID, "acme")
	if err := st.SetOrgGitHub(org.ID, "Acme-Inc"); err != nil {
		t.Fatalf("SetOrgGitHub: %v", err)
	}
}

// OrgForGitHubInstall matches by the declared login on the first install, then
// pins the stable GitHub org id so a later login rename still resolves.
func TestOrgForGitHubInstallResolvesByLoginThenID(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("1001", "alice") // owner ⇒ member
	org, _ := st.CreateOrg(alice.ID, "acme")
	if err := st.SetOrgGitHub(org.ID, "Acme-Inc"); err != nil {
		t.Fatal(err)
	}

	// First install: no id pinned yet, matches case-insensitively by login.
	got, err := st.OrgForGitHubInstall("5000", "acme-inc", "1001")
	if err != nil || got != org.ID {
		t.Fatalf("by-login = (%q,%v), want %q", got, err, org.ID)
	}
	// The stable id is now pinned: a later event resolves even if GitHub renamed
	// the org.
	got, err = st.OrgForGitHubInstall("5000", "renamed-on-github", "1001")
	if err != nil || got != org.ID {
		t.Fatalf("by-id = (%q,%v), want %q", got, err, org.ID)
	}
}

// The installing sender must belong to the Piper org — a non-member installing
// on a GitHub org whose login a Piper org happens to have declared must not
// bind that installation to the org.
func TestOrgForGitHubInstallRejectsNonMemberSender(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("1001", "alice")
	st.UpsertAccount("2002", "mallory") // a user, not a member of acme
	org, _ := st.CreateOrg(alice.ID, "acme")
	if err := st.SetOrgGitHub(org.ID, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OrgForGitHubInstall("5000", "acme", "2002"); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("non-member = %v, want ErrNoOrg", err)
	}
}

func TestOrgForGitHubInstallUnknownOrg(t *testing.T) {
	st := openTestStore(t)
	st.UpsertAccount("1001", "alice")
	if _, err := st.OrgForGitHubInstall("5000", "no-such-org", "1001"); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("unknown = %v, want ErrNoOrg", err)
	}
}
