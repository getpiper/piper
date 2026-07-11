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
