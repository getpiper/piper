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

func TestInviteLifecycle(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "Bob-Builder")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// Invite by GitHub username, any case; duplicate is idempotent.
	if err := st.CreateInvite(org.ID, "BOB-builder", alice.ID); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if err := st.CreateInvite(org.ID, "bob-builder", alice.ID); err != nil {
		t.Fatalf("duplicate invite: %v, want nil (idempotent)", err)
	}
	pending, err := st.OrgInvites(org.ID)
	if err != nil || len(pending) != 1 || pending[0] != "bob-builder" {
		t.Fatalf("OrgInvites = %v (%v), want [bob-builder]", pending, err)
	}
	mine, err := st.InvitesForAccount(bob.ID)
	if err != nil || len(mine) != 1 || mine[0] != "acme" {
		t.Fatalf("InvitesForAccount = %v (%v), want [acme]", mine, err)
	}

	// Accept: membership as member, invite consumed.
	if err := st.AcceptInvite(bob.ID, "acme"); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	orgs, _ := st.OrgsForAccount(bob.ID)
	if len(orgs) != 1 || orgs[0].Role != "member" {
		t.Fatalf("bob's orgs = %+v, want [acme member]", orgs)
	}
	if pending, _ := st.OrgInvites(org.ID); len(pending) != 0 {
		t.Fatalf("invite not consumed: %v", pending)
	}
	if err := st.AcceptInvite(bob.ID, "acme"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("re-accept err = %v, want ErrNoInvite", err)
	}

	// Inviting an existing member is refused.
	if err := st.CreateInvite(org.ID, "Bob-Builder", alice.ID); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("invite member err = %v, want ErrAlreadyMember", err)
	}
}

func TestInviteBeforeFirstLogin(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, _ := st.CreateOrg(alice.ID, "acme")

	// Invited before ever logging into the relay.
	if err := st.CreateInvite(org.ID, "Newbie", alice.ID); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	newbie, _ := st.UpsertAccount("gh-newbie", "Newbie")
	mine, err := st.InvitesForAccount(newbie.ID)
	if err != nil || len(mine) != 1 || mine[0] != "acme" {
		t.Fatalf("InvitesForAccount = %v (%v), want [acme]", mine, err)
	}
	if err := st.AcceptInvite(newbie.ID, "acme"); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
}

func TestDeclineAndRevokeInvite(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	bob, _ := st.UpsertAccount("gh-bob", "bob")
	org, _ := st.CreateOrg(alice.ID, "acme")

	st.CreateInvite(org.ID, "bob", alice.ID)
	if err := st.DeclineInvite(bob.ID, "acme"); err != nil {
		t.Fatalf("DeclineInvite: %v", err)
	}
	if orgs, _ := st.OrgsForAccount(bob.ID); len(orgs) != 0 {
		t.Fatalf("decline created membership: %+v", orgs)
	}
	if err := st.DeclineInvite(bob.ID, "acme"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("re-decline err = %v, want ErrNoInvite", err)
	}

	st.CreateInvite(org.ID, "bob", alice.ID)
	if err := st.RevokeInvite(org.ID, "BOB"); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}
	if err := st.RevokeInvite(org.ID, "bob"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("re-revoke err = %v, want ErrNoInvite", err)
	}
	// A consumed/revoked invite no longer accepts.
	if err := st.AcceptInvite(bob.ID, "acme"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("accept revoked err = %v, want ErrNoInvite", err)
	}
	// Accepting a nonexistent org is the same error (no existence leak).
	if err := st.AcceptInvite(bob.ID, "nope"); !errors.Is(err, ErrNoInvite) {
		t.Fatalf("accept unknown org err = %v, want ErrNoInvite", err)
	}
}
