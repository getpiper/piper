package relay

import (
	"encoding/json"
	"net/http"
	"strings"
)

// registerOrgRoutes wires the org-management surface (#104). All handlers
// authenticate the account credential; org-scoped ones resolve the {slug}
// through OrgRole, whose ErrNoOrg (unknown org OR non-member) maps to 404 so
// org existence never leaks.
func (a *api) registerOrgRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/orgs", a.orgCreate)
	mux.HandleFunc("GET /v1/orgs", a.orgList)
}

func (a *api) orgCreate(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	org, err := a.st.CreateOrg(acc.ID, req.Name)
	if err != nil {
		http.Error(w, "org create failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"org": org.Slug, "role": "owner"})
}

func (a *api) orgList(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	orgs, err := a.st.OrgsForAccount(acc.ID)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]string, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, map[string]string{"org": o.Slug, "role": o.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"orgs": out})
}
