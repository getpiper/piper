package relay

import (
	"errors"
	"strings"
	"testing"
)

func TestCreateOrgMakesCreatorSoleOwner(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")

	org, err := st.CreateOrg(alice.ID, "Acme Robotics")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org.Slug != "acme-robotics" {
		t.Fatalf("slug = %q, want acme-robotics", org.Slug)
	}
	if org.ID == "" || org.ID == alice.ID {
		t.Fatalf("org id = %q, want a fresh account id", org.ID)
	}

	orgs, err := st.OrgsForAccount(alice.ID)
	if err != nil {
		t.Fatalf("OrgsForAccount: %v", err)
	}
	if len(orgs) != 1 || orgs[0].Slug != "acme-robotics" || orgs[0].Role != "owner" {
		t.Fatalf("orgs = %+v, want [acme-robotics owner]", orgs)
	}
}

func TestCreateOrgSlugSharesUsernameNamespace(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	// A user already holds "bob": the org gets bob-2, exactly like a
	// colliding user signup would.
	st.UpsertAccount("gh-bob", "bob")

	org, err := st.CreateOrg(alice.ID, "Bob")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if org.Slug != "bob-2" {
		t.Fatalf("slug = %q, want bob-2", org.Slug)
	}
}

func TestOrgRoleHidesOrgFromNonMembers(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	mallory, _ := st.UpsertAccount("gh-mallory", "mallory")
	org, _ := st.CreateOrg(alice.ID, "acme")

	orgID, role, err := st.OrgRole(org.Slug, alice.ID)
	if err != nil || orgID != org.ID || role != "owner" {
		t.Fatalf("member OrgRole = (%q,%q,%v), want (%q, owner, nil)", orgID, role, err, org.ID)
	}
	// Non-member, nonexistent org, and a *user* slug are indistinguishable.
	if _, _, err := st.OrgRole(org.Slug, mallory.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("non-member err = %v, want ErrNoOrg", err)
	}
	if _, _, err := st.OrgRole("nope", alice.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("nonexistent err = %v, want ErrNoOrg", err)
	}
	if _, _, err := st.OrgRole("mallory", alice.ID); !errors.Is(err, ErrNoOrg) {
		t.Fatalf("user-slug err = %v, want ErrNoOrg", err)
	}
}

func TestOrgStaysInertAsPrincipal(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// An org can never hold a credential.
	if _, err := st.MintAccountCredential(org.ID); err == nil {
		t.Fatal("MintAccountCredential(org) succeeded, want error")
	}
	// An org cannot create an org.
	if _, err := st.CreateOrg(org.ID, "suborg"); err == nil {
		t.Fatal("CreateOrg(by org) succeeded, want error")
	}
}

func TestOrgAgentQuotaIsIndependent(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 2, 10)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// Alice fills her personal cap.
	for i := 0; i < 2; i++ {
		if _, err := st.EnrollForAccount(alice.ID); err != nil {
			t.Fatalf("personal enroll %d: %v", i, err)
		}
	}
	if _, err := st.EnrollForAccount(alice.ID); err != ErrQuotaExceeded {
		t.Fatalf("over personal cap err = %v, want ErrQuotaExceeded", err)
	}
	// The org's cap is its own; its base domain carries the org slug.
	en, err := st.EnrollForAccount(org.ID)
	if err != nil {
		t.Fatalf("org enroll: %v", err)
	}
	if want := "-acme.public.getpiper.co"; !strings.HasSuffix(en.BaseDomain, want) {
		t.Fatalf("org base domain = %q, want suffix %q", en.BaseDomain, want)
	}
}

// addMember inserts a membership row directly; org/membership tests must not
// depend on the invite flow.
func addMember(t *testing.T, st *Store, orgID, accountID, role string) {
	t.Helper()
	if _, err := st.db.Exec(
		`INSERT INTO org_members(org_id, account_id, role, created_at)
		 VALUES(?,?,?,'2026-01-01T00:00:00Z')`, orgID, accountID, role); err != nil {
		t.Fatal(err)
	}
}

func TestMembersListsUsernamesAndRoles(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	members, err := st.Members(org.ID)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 2 ||
		members[0] != (Member{Username: "alice", Role: "owner"}) ||
		members[1] != (Member{Username: "bob", Role: "member"}) {
		t.Fatalf("members = %+v, want [alice/owner bob/member]", members)
	}
}

func TestSetMemberRolePromotesAndGuardsLastOwner(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	// Sole owner cannot demote themselves.
	if err := st.SetMemberRole(org.ID, "alice", "member"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demote sole owner err = %v, want ErrLastOwner", err)
	}
	// Promote bob, then alice may step down.
	if err := st.SetMemberRole(org.ID, "bob", "owner"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := st.SetMemberRole(org.ID, "alice", "member"); err != nil {
		t.Fatalf("demote after promote: %v", err)
	}
	// Unknown target.
	if err := st.SetMemberRole(org.ID, "nobody", "member"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("unknown member err = %v, want ErrNotMember", err)
	}
}

func TestRemoveMemberGuardsLastOwner(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")
	addMember(t, st, org.ID, bob.ID, "member")

	if err := st.RemoveMember(org.ID, "alice"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("remove sole owner err = %v, want ErrLastOwner", err)
	}
	if err := st.RemoveMember(org.ID, "bob"); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if err := st.RemoveMember(org.ID, "bob"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("re-remove err = %v, want ErrNotMember", err)
	}
	// Removal is real: bob no longer lists the org.
	orgs, _ := st.OrgsForAccount(bob.ID)
	if len(orgs) != 0 {
		t.Fatalf("bob still in orgs: %+v", orgs)
	}
}
