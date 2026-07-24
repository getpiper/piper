# Multi-installation repo picker (relay) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the relay expose every GitHub App installation an account holds, labelled by its real target, and mint git tokens from the installation that actually owns the repo — fixing issue #321.

**Architecture:** Add a plural `InstallationsForAccount` store query returning each installation's `{id, target_type, target_login}`. Reshape `/v1/github/status` to return an `installations[]` array and `/v1/github/repos` to select by `?installation_id=` (authz-checked). Resolve token minting by matching the repo owner to an installation's `target_login`. Delete the now-superseded single-result `InstallationForAccount`.

**Tech Stack:** Go (`CGO_ENABLED=0`), `modernc.org/sqlite`, standard `net/http`, table-free `httptest` unit tests.

## Global Constraints

- **No cgo.** All builds must pass with `CGO_ENABLED=0`. (`make cross` proves the arm64 build.)
- **Module path** `github.com/piperbox/piper`.
- **Pre-1.x compat policy:** break wire/response shapes in place — no shim, no version negotiation, no SQLite migration. The dashboard is updated in tandem.
- **Deployment status strings** unchanged; not touched here.
- **Verification gate:** `make verify` (gofmt → `go vet` → `go test ./...` → `make cross`) must pass before the branch is done.
- Branch already exists: `ozykhan/multi-installation-repo-picker` (design doc committed). Reference `#321` in commits; `Closes #321` goes in the PR body.
- Commit trailer: `Co-Authored-By: Claude {current model} <noreply@anthropic.com>`.

---

## Ordering note (why the deletion is last)

`InstallationForAccount` has four callers today: `ghtoken.go`, `api.go` (×2: `githubStatus`, `githubRepos`), and `weblogin_cli.go`. Tasks 1–4 add the plural method and migrate the first three callers; Task 5 migrates the last caller and deletes the old method. Every task leaves the tree building and `go test ./...` green.

---

## Task 1: Store — `Installation` type + `InstallationsForAccount`

**Files:**
- Modify: `internal/relay/installations.go` (add type + method; leave `InstallationForAccount` in place for now)
- Test: `internal/relay/installations_test.go`

**Interfaces:**
- Produces:
  - `type Installation struct { ID string; TargetType string; TargetLogin string }` with JSON tags `installation_id` / `target_type` / `target_login`.
  - `func (s *Store) InstallationsForAccount(accountID string) ([]Installation, error)` — all installations for the account, newest-first (`created_at DESC`), empty slice (not error) when none.

- [ ] **Step 1: Write the failing tests**

Add to `internal/relay/installations_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run TestInstallationsForAccount -v`
Expected: FAIL — `undefined: Installation` / `st.InstallationsForAccount undefined`.

- [ ] **Step 3: Add the type and method**

In `internal/relay/installations.go`, add above the existing `InstallationForAccount` (keep that method):

```go
// Installation is one GitHub App installation linked to an account, carrying
// the display identity of its target — the user or org the App is installed on
// (github_installations.target_type / target_login).
type Installation struct {
	ID          string `json:"installation_id"`
	TargetType  string `json:"target_type"`
	TargetLogin string `json:"target_login"`
}

// InstallationsForAccount lists every installation linked to the account,
// newest first. Empty (not an error) when the account has none.
func (s *Store) InstallationsForAccount(accountID string) ([]Installation, error) {
	rows, err := s.db.Query(
		`SELECT installation_id, target_type, target_login FROM github_installations
		  WHERE account_id=? ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Installation
	for rows.Next() {
		var in Installation
		if err := rows.Scan(&in.ID, &in.TargetType, &in.TargetLogin); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run TestInstallationsForAccount -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/installations.go internal/relay/installations_test.go
git commit -m "$(cat <<'EOF'
feat(relay): enumerate all installations for an account (#321)

Part of #321.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Token minting resolves the installation by repo owner

**Files:**
- Modify: `internal/relay/ghtoken.go` (`GitHubTokenFor` + `strings` import)
- Test: `internal/relay/ghtoken_test.go`

**Interfaces:**
- Consumes: `Store.InstallationsForAccount` (Task 1); existing `normalizeRepo`, `Store.AgentAccount`, `GitHubApp.RepoToken`, `ErrNoInstallation`.
- Produces: `GitHubTokenFor` now mints from the installation whose `target_login` case-insensitively equals the owner segment of `owner/name`; `ErrNoInstallation` when none matches.

**Why owner-match is correct:** a GitHub App installation lives on exactly one account and can only access repos owned by that account, so for `owner/name` the minting installation is the one whose `target_login == owner`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/relay/ghtoken_test.go`:

```go
// TestGitHubTokenForPicksInstallationByRepoOwner: an account holding a personal
// install (id 55) and a getpiper-org install (id 66) mints a token for
// getpiper/app from the org installation, not the most-recent one.
func TestGitHubTokenForPicksInstallationByRepoOwner(t *testing.T) {
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_ok","expires_at":"2026-07-20T12:00:00Z"}`))
	}))
	defer srv.Close()

	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("66", "1001", "org", "getpiper"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "app", "getpiper/app", "main"); err != nil {
		t.Fatal(err)
	}

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.GitHubTokenFor(context.Background(), app, agent, "getpiper/app"); err != nil {
		t.Fatalf("GitHubTokenFor: %v", err)
	}
	if hit != "/app/installations/66/access_tokens" {
		t.Fatalf("minted from %q, want installation 66 (getpiper org)", hit)
	}
}

// TestGitHubTokenForNoInstallationForRepoOwner: bound repo whose owner has no
// linked installation → ErrNoInstallation.
func TestGitHubTokenForNoInstallationForRepoOwner(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "app", "getpiper/app", "main"); err != nil {
		t.Fatal(err)
	}
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.GitHubTokenFor(context.Background(), app, agent, "getpiper/app")
	if !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("err = %v, want ErrNoInstallation", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run TestGitHubTokenForPicksInstallationByRepoOwner -v`
Expected: FAIL — token minted from installation 55 (`/app/installations/55/access_tokens`), not 66, because `GitHubTokenFor` still picks most-recent.

- [ ] **Step 3: Switch `GitHubTokenFor` to owner-match**

In `internal/relay/ghtoken.go`, add `"strings"` to the import block, then replace the tail of `GitHubTokenFor` (the `accountID` → `RepoToken` section, currently lines 36-44):

```go
	accountID, _, err := s.AgentAccount(agentName)
	if err != nil {
		return "", time.Time{}, err
	}
	// The installation that can mint a token for owner/name is the one whose
	// target_login is that owner — an installation only reaches its own
	// account's repos. This replaces most-recent-wins, which broke deploys
	// from any non-newest installation.
	insts, err := s.InstallationsForAccount(accountID)
	if err != nil {
		return "", time.Time{}, err
	}
	owner, _, _ := strings.Cut(normalizeRepo(repo), "/")
	for _, in := range insts {
		if strings.EqualFold(in.TargetLogin, owner) {
			return app.RepoToken(ctx, in.ID, repo)
		}
	}
	return "", time.Time{}, ErrNoInstallation
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run TestGitHubTokenFor -v`
Expected: PASS — including the pre-existing `TestGitHubTokenForReturnsTokenWhenBoundAndLinked` (repo `alice/blog`, install 55 target_login `alice`) and `TestGitHubTokenForRejectsUnboundRepoWithNonNilApp`.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/ghtoken.go internal/relay/ghtoken_test.go
git commit -m "$(cat <<'EOF'
fix(relay): mint git tokens from the installation owning the repo (#321)

Part of #321.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: `githubStatus` returns the installations array

**Files:**
- Modify: `internal/relay/api.go` (`githubStatus` + its doc comment)
- Test: `internal/relay/api_test.go`

**Interfaces:**
- Consumes: `Store.InstallationsForAccount` (Task 1), `Installation` (Task 1).
- Produces: `GET /v1/github/status` → `{ "github_app": bool, "installations": [{installation_id,target_type,target_login}], "install_url": string }`. `installed` and `account` are removed.

- [ ] **Step 1: Update the test decode struct and rewrite the status tests**

In `internal/relay/api_test.go`, replace the `ghStatus` struct (currently ~lines 546-551):

```go
type ghStatus struct {
	GitHubApp     bool           `json:"github_app"`
	Installations []Installation `json:"installations"`
	InstallURL    string         `json:"install_url"`
}
```

Replace `TestGitHubStatusInstalled` with:

```go
func TestGitHubStatusInstalled(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st)
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("66", "1001", "org", "getpiper"); err != nil {
		t.Fatal(err)
	}

	rec := getStatus(t, statusAPI(t, st), cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got ghStatus
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	wantInst := []Installation{
		{ID: "66", TargetType: "org", TargetLogin: "getpiper"},
		{ID: "55", TargetType: "user", TargetLogin: "alice"},
	}
	if !got.GitHubApp || got.InstallURL != "https://github.com/apps/piper-relay/installations/new" ||
		len(got.Installations) != len(wantInst) ||
		got.Installations[0] != wantInst[0] || got.Installations[1] != wantInst[1] {
		t.Fatalf("status = %+v, want github_app + %+v", got, wantInst)
	}
}

// TestGitHubStatusLabelsOrgInstallByOrgLogin is the #321 Gap-2 regression: a
// personal login whose only installation targets an org must report the org as
// the installation's target_login, not the logged-in username.
func TestGitHubStatusLabelsOrgInstallByOrgLogin(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st) // account "1001", login "alice"
	if err := st.LinkInstallation("66", "1001", "org", "getpiper"); err != nil {
		t.Fatal(err)
	}
	rec := getStatus(t, statusAPI(t, st), cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got ghStatus
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Installations) != 1 ||
		got.Installations[0].TargetLogin != "getpiper" || got.Installations[0].TargetType != "org" {
		t.Fatalf("installations = %+v, want single org getpiper (not login alice)", got.Installations)
	}
}
```

Replace `TestGitHubStatusNotInstalled` body's assertion (the `want`/`got != want` block) with:

```go
	if !got.GitHubApp || got.InstallURL != "https://github.com/apps/piper-relay/installations/new" ||
		len(got.Installations) != 0 {
		t.Fatalf("status = %+v, want github_app + no installations", got)
	}
```

Replace `TestGitHubStatusNoAppConfigured`'s assertion (the `want`/`got != want` block) with:

```go
	if got.GitHubApp || got.InstallURL != "" || len(got.Installations) != 0 {
		t.Fatalf("status = %+v, want github_app:false + no installations", got)
	}
```

(`ghStatus` now holds a slice, so `got != want` no longer compiles — every status test must compare field-by-field.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run TestGitHubStatus -v`
Expected: FAIL — `githubStatus` still emits `installed`/`account` and no `installations`, so `TestGitHubStatusInstalled` / `...LabelsOrgInstallByOrgLogin` mismatch.

- [ ] **Step 3: Reshape `githubStatus`**

In `internal/relay/api.go`, replace `githubStatus` and its doc comment (currently ~lines 331-360):

```go
// githubStatus tells the web dashboard whether the relay holds a GitHub App and,
// if so, every installation linked to the caller's account — each with its own
// target identity (target_type / target_login), so the wizard's label always
// matches the repos an installation serves (#321). Plus the install-and-authorize
// URL to add an installation (#315). It never 404s on a missing installation:
// an empty list is the answer the Connect step needs, not an error — and it
// answers 200 even with no App configured so the dashboard learns
// github_app:false rather than reading a 503 as an outage.
func (a *api) githubStatus(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	resp := map[string]any{
		"github_app":    a.ghApp != nil,
		"installations": []Installation{},
		"install_url":   "",
	}
	if a.ghApp != nil {
		resp["install_url"] = a.ghApp.InstallURL()
		insts, err := a.st.InstallationsForAccount(acc.ID)
		if err != nil {
			http.Error(w, "lookup error", http.StatusInternalServerError)
			return
		}
		if insts != nil {
			resp["installations"] = insts
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
```

(`InstallationsForAccount` returns a nil slice when empty; guarding with `if insts != nil` keeps the JSON `[]` rather than `null`.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run TestGitHubStatus -v`
Expected: PASS (Installed, LabelsOrgInstallByOrgLogin, NotInstalled, RequiresCredential, NoAppConfigured).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/api.go internal/relay/api_test.go
git commit -m "$(cat <<'EOF'
feat(relay): github status returns all installations by target identity (#321)

Part of #321.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `githubRepos` selects the installation by `?installation_id=`

**Files:**
- Modify: `internal/relay/api.go` (`githubRepos` + its doc comment)
- Test: `internal/relay/api_test.go` (`getRepos` helper, `ghAPIStub`, repos tests)

**Interfaces:**
- Consumes: existing `Store.AccountForInstallation`, `ErrNoInstallation`, `GitHubApp.Repos`.
- Produces: `GET /v1/github/repos?installation_id=<id>` → `{ "repos": [...] }`; `400` when the param is absent; `404` when the id is unknown or owned by another account.

- [ ] **Step 1: Update the `getRepos` helper, extend the stub, rewrite the repos tests**

In `internal/relay/api_test.go`, change the `getRepos` helper to take an installation id:

```go
func getRepos(t *testing.T, h http.Handler, cred, instID string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/github/repos"
	if instID != "" {
		target += "?installation_id=" + url.QueryEscape(instID)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
```

Extend `ghAPIStub` to mint access tokens for either installation id used below (55 or 66) — replace the `"/app/installations/55/access_tokens"` case:

```go
		case "/app/installations/55/access_tokens", "/app/installations/66/access_tokens":
			_, _ = w.Write([]byte(`{"token":"t","expires_at":"2026-07-20T12:00:00Z"}`))
```

Update the existing `TestGitHubReposListsInstallationRepos` call site (it links installation "55"):

```go
	rec := getRepos(t, reposAPI(t, st, gh), cred, "55")
```

Update `TestGitHubReposRequiresCredential`:

```go
	rec := getRepos(t, reposAPI(t, openTestStore(t), gh), "", "55")
```

Replace `TestGitHubReposWithoutInstallation` with the three cases the new contract needs:

```go
func TestGitHubReposRequiresInstallationID(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()
	st := openTestStore(t)
	cred := accountWithCred(t, st)
	rec := getRepos(t, reposAPI(t, st, gh), cred, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestGitHubReposUnknownInstallation(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()
	st := openTestStore(t)
	cred := accountWithCred(t, st)
	rec := getRepos(t, reposAPI(t, st, gh), cred, "999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestGitHubReposForeignInstallation: an installation owned by a different
// account must not be readable, reported as 404 (no existence leak).
func TestGitHubReposForeignInstallation(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()
	st := openTestStore(t)
	cred := accountWithCred(t, st) // account 1001 / alice
	if _, err := st.UpsertAccount("2002", "mallory"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("77", "2002", "user", "mallory"); err != nil {
		t.Fatal(err)
	}
	rec := getRepos(t, reposAPI(t, st, gh), cred, "77")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for foreign installation", rec.Code)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run TestGitHubRepos -v`
Expected: FAIL — `getRepos` now passes `installation_id`, but `githubRepos` ignores it and still resolves via `InstallationForAccount`, so `TestGitHubReposRequiresInstallationID` gets 404 (no installation) instead of 400, and `...ForeignInstallation` serves repos (200) instead of 404.

- [ ] **Step 3: Reshape `githubRepos`**

In `internal/relay/api.go`, replace `githubRepos` and its doc comment (currently ~lines 301-329):

```go
// githubRepos lists the repositories reachable through the installation named by
// ?installation_id=, which must belong to the caller (#321). No list is cached:
// it is read live through a fresh installation token, so a repository revoked in
// GitHub disappears here immediately. An unknown or foreign installation id is
// reported as 404 identically — the picker never learns another account's
// installation exists.
func (a *api) githubRepos(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	if a.ghApp == nil {
		http.Error(w, "relay has no github app configured", http.StatusServiceUnavailable)
		return
	}
	instID := r.URL.Query().Get("installation_id")
	if instID == "" {
		http.Error(w, "installation_id required", http.StatusBadRequest)
		return
	}
	owner, err := a.st.AccountForInstallation(instID)
	if errors.Is(err, ErrNoInstallation) || (err == nil && owner != acc.ID) {
		http.Error(w, "github app not installed for this account", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "lookup error", http.StatusInternalServerError)
		return
	}
	repos, err := a.ghApp.Repos(r.Context(), instID)
	if err != nil {
		log.Printf("relay: list repos for %s: %v", acc.Username, err)
		http.Error(w, "github unavailable", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run TestGitHubRepos -v`
Expected: PASS (ListsInstallationRepos, RequiresCredential, RequiresInstallationID, UnknownInstallation, ForeignInstallation).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/api.go internal/relay/api_test.go
git commit -m "$(cat <<'EOF'
feat(relay): github repos selects installation by ?installation_id= (#321)

Part of #321.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Migrate the last caller and delete `InstallationForAccount`

**Files:**
- Modify: `internal/relay/weblogin_cli.go` (existence check + drop the now-unused `errors` import)
- Modify: `internal/relay/installations.go` (delete `InstallationForAccount`)
- Test: `internal/relay/installations_test.go` (fix `TestLinkInstallationBindsToSenderAccount`)

**Interfaces:**
- Consumes: `Store.InstallationsForAccount` (Task 1).
- Produces: `InstallationForAccount` no longer exists in the package.

- [ ] **Step 1: Update the store test that referenced the deleted method**

In `internal/relay/installations_test.go`, in `TestLinkInstallationBindsToSenderAccount`, replace the `InstallationForAccount` block (the `inst, err := st.InstallationForAccount(acc.ID)` section) with:

```go
	insts, err := st.InstallationsForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationsForAccount: %v", err)
	}
	if len(insts) != 1 || insts[0].ID != "55" {
		t.Fatalf("installations = %+v, want single id 55", insts)
	}
```

- [ ] **Step 2: Migrate the CLI-login existence check**

In `internal/relay/weblogin_cli.go`, replace the existence check (currently lines 194-197):

```go
	installURL := ""
	if insts, _ := a.st.InstallationsForAccount(acc.ID); len(insts) == 0 {
		installURL = a.ghApp.InstallURL()
	}
```

Then remove the now-unused `"errors"` line from that file's import block (it was used only by the replaced `errors.Is`).

- [ ] **Step 3: Delete `InstallationForAccount`**

In `internal/relay/installations.go`, delete the `InstallationForAccount` method and its doc comment (the `// InstallationForAccount returns the installation an account's agents mint …` block through the closing brace).

- [ ] **Step 4: Verify the whole package builds and is green**

Run: `go build ./... && go test ./internal/relay/ -v`
Expected: builds with no "unused import" / "undefined" errors; all relay tests PASS. (A compile error here means a stray `InstallationForAccount` reference or the leftover `errors` import — fix and re-run.)

- [ ] **Step 5: Commit**

```bash
git add internal/relay/installations.go internal/relay/installations_test.go internal/relay/weblogin_cli.go
git commit -m "$(cat <<'EOF'
refactor(relay): drop single-result InstallationForAccount (#321)

Part of #321.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Full verification gate + PROGRESS + push + PR

**Files:**
- Modify: `PROGRESS.md` (one terse line if the GitHub-App wizard area is tracked there)

- [ ] **Step 1: Run the full gate**

Run: `make verify`
Expected: gofmt clean, `go vet` clean, `go test ./...` PASS, `make cross` (linux/arm64) builds. If gofmt flags anything, run `make fmt` and re-run `make verify`.

- [ ] **Step 2: Update PROGRESS.md if the area is listed**

Check `PROGRESS.md` for the relay GitHub-App / repo-picker line. If present, append `[#321]` or a one-line note that the picker now enumerates all installations; keep it terse (detail lives in the issue). If no matching line exists, skip — do not invent a new section.

- [ ] **Step 3: Commit any PROGRESS/gofmt changes**

```bash
git add -A
git commit -m "$(cat <<'EOF'
chore(relay): note multi-installation picker in progress (#321)

Part of #321.

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

(Skip this commit if Steps 1-2 produced no changes.)

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin ozykhan/multi-installation-repo-picker
gh pr create --base main \
  --title "[relay] repo picker: enumerate all installations, label by target, mint by owner" \
  --body "$(cat <<'EOF'
Fixes the relay side of the repo-picker confusion:

- `githubStatus` returns every installation linked to the account, each with its own `target_type`/`target_login`, so the wizard label matches the repos shown (Gap 2).
- `githubRepos?installation_id=` lets the picker choose which installation to deploy from, authz-checked (Gap 1).
- Token minting resolves the installation by repo owner, so deploys from a non-newest installation no longer mint from the wrong one.
- Drops the single-result `InstallationForAccount`.

Dashboard picker UI is a separate repo; #320 (multi-account login 404) is untracked here and stays open.

Closes #321

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

---

## Self-Review

**Spec coverage:**
- Gap 1 (one install, no selector) → Tasks 1 (`InstallationsForAccount`), 3 (`githubStatus` array), 4 (`githubRepos?installation_id=`). ✓
- Gap 2 (mislabelled with login) → Task 3 + the `TestGitHubStatusLabelsOrgInstallByOrgLogin` regression. ✓
- Gap 3 (token minting picks wrong install) → Task 2 owner-match. ✓
- Delete `InstallationForAccount`, migrate `weblogin_cli` → Task 5. ✓
- No schema change; pre-1.x break-in-place → honored (response/request shapes changed directly). ✓
- Verification gate + PR with `Closes #321` → Task 6. ✓

**Placeholder scan:** none — every code and test step shows full content; no TBD/TODO. ✓

**Type consistency:** `Installation{ID, TargetType, TargetLogin}` (json `installation_id`/`target_type`/`target_login`) defined in Task 1 and used verbatim in Tasks 2, 3, 5, and the `ghStatus` decode struct. `InstallationsForAccount(accountID string) ([]Installation, error)` signature identical across Tasks 1–5. `getRepos(t, h, cred, instID)` 4-arg helper defined and used consistently in Task 4. ✓
