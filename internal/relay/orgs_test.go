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
