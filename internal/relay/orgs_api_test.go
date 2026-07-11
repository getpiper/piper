package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// orgAPIFixture: an API over a store with two logged-in users.
func orgAPIFixture(t *testing.T) (api http.Handler, st *Store, aliceCred, bobCred string) {
	t.Helper()
	st = openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10)
	alice, err := st.UpsertAccount("sub-alice", "alice")
	if err != nil {
		t.Fatal(err)
	}
	aliceCred, _ = st.MintAccountCredential(alice.ID)
	bob, err := st.UpsertAccount("sub-bob", "bob")
	if err != nil {
		t.Fatal(err)
	}
	bobCred, _ = st.MintAccountCredential(bob.ID)
	api = NewAPI(st, NewFakeVerifier())
	return
}

// apiReq performs one JSON request against the API.
func apiReq(t *testing.T, api http.Handler, method, path, cred, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	return rr
}

func TestOrgCreateAndList(t *testing.T) {
	api, _, aliceCred, bobCred := orgAPIFixture(t)

	// Auth gates.
	if rr := apiReq(t, api, "POST", "/v1/orgs", "", `{"name":"acme"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no cred: %d, want 401", rr.Code)
	}
	if rr := apiReq(t, api, "POST", "/v1/orgs", aliceCred, `{}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty name: %d, want 400", rr.Code)
	}

	rr := apiReq(t, api, "POST", "/v1/orgs", aliceCred, `{"name":"Acme Robotics"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: %d (body %s)", rr.Code, rr.Body.String())
	}
	var created struct {
		Org  string `json:"org"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Org != "acme-robotics" || created.Role != "owner" {
		t.Fatalf("created = %+v, want acme-robotics/owner", created)
	}

	// Creator lists it; a stranger's list stays empty (and non-null).
	rr = apiReq(t, api, "GET", "/v1/orgs", aliceCred, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	var list struct {
		Orgs []struct {
			Org  string `json:"org"`
			Role string `json:"role"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Orgs) != 1 || list.Orgs[0].Org != "acme-robotics" || list.Orgs[0].Role != "owner" {
		t.Fatalf("list = %+v, want [acme-robotics owner]", list.Orgs)
	}

	rr = apiReq(t, api, "GET", "/v1/orgs", bobCred, "")
	list.Orgs = nil
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Orgs == nil || len(list.Orgs) != 0 {
		t.Fatalf("bob's list = %+v, want empty non-null array", list.Orgs)
	}
}

// orgWithMember: alice owns "acme", bob is a plain member.
func orgWithMember(t *testing.T, st *Store, aliceCred, bobCred string) (orgID string) {
	t.Helper()
	alice, err := st.AuthenticateAccount(aliceCred)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.AuthenticateAccount(bobCred)
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.CreateOrg(alice.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	addMember(t, st, org.ID, bob.ID, "member")
	return org.ID
}

func TestOrgMembersEndpointRoleMatrix(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	orgWithMember(t, st, aliceCred, bobCred)
	mallory, _ := st.UpsertAccount("sub-mallory", "mallory")
	malloryCred, _ := st.MintAccountCredential(mallory.ID)

	// Any member reads the list; a non-member gets 404, not 403 (no leak).
	rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", bobCred, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("member list: %d", rr.Code)
	}
	var list struct {
		Members []struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"members"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Members) != 2 || list.Members[0].Username != "alice" || list.Members[0].Role != "owner" ||
		list.Members[1].Username != "bob" || list.Members[1].Role != "member" {
		t.Fatalf("members = %+v", list.Members)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", malloryCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("non-member list: %d, want 404", rr.Code)
	}
	if rr := apiReq(t, api, "GET", "/v1/orgs/ghost/members", aliceCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown org list: %d, want 404", rr.Code)
	}

	// Promote: owner-only; members get 403; bad role 400; unknown target 404.
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", bobCred, `{"role":"owner"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("member promote: %d, want 403", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", aliceCred, `{"role":"admin"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad role: %d, want 400", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/nobody", aliceCred, `{"role":"member"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown target: %d, want 404", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", aliceCred, `{"role":"owner"}`); rr.Code != http.StatusOK {
		t.Fatalf("promote: %d", rr.Code)
	}

	// Last-owner guard surfaces as 409 (bob demoted back first).
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/bob", aliceCred, `{"role":"member"}`); rr.Code != http.StatusOK {
		t.Fatalf("demote bob: %d", rr.Code)
	}
	if rr := apiReq(t, api, "PUT", "/v1/orgs/acme/members/alice", aliceCred, `{"role":"member"}`); rr.Code != http.StatusConflict {
		t.Fatalf("demote last owner: %d, want 409", rr.Code)
	}
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/members/alice", aliceCred, ""); rr.Code != http.StatusConflict {
		t.Fatalf("remove last owner: %d, want 409", rr.Code)
	}
}

func TestOrgMemberRemovalAndSelfLeave(t *testing.T) {
	api, st, aliceCred, bobCred := orgAPIFixture(t)
	orgWithMember(t, st, aliceCred, bobCred)

	// A member cannot remove someone else...
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/members/alice", bobCred, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("member removes other: %d, want 403", rr.Code)
	}
	// ...but may leave.
	if rr := apiReq(t, api, "DELETE", "/v1/orgs/acme/members/bob", bobCred, ""); rr.Code != http.StatusOK {
		t.Fatalf("self-leave: %d", rr.Code)
	}
	// Gone: the org now 404s for bob.
	if rr := apiReq(t, api, "GET", "/v1/orgs/acme/members", bobCred, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("after leave: %d, want 404", rr.Code)
	}
}
