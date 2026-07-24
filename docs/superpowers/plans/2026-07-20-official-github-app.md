# Official GitHub App Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a relay hold one GitHub App on behalf of its users, so a newcomer on a public relay reaches `git push` → deploy with a single GitHub consent screen and no manual App creation, while BYO per-box Apps keep working unchanged. (`piper login` stays device-flow: two short browser stops; the one-trip CLI login is follow-up [#291](https://github.com/piperbox/piper/issues/291).)

**Architecture:** Two provider modes behind the existing `source.Provider` seam. BYO is today's path. In brokered mode the relay verifies GitHub's webhook, resolves `installation → account → repo binding → agent`, re-signs the payload with a per-agent secret, and delivers it over a `tunnel.KindHTTP` stream to the box's Caddy `:80`, which routes `hooks.<base>` to the existing webhook listener. The agent, holding no GitHub credentials, asks the relay for repo-scoped installation tokens over the existing `KindControl` op protocol.

**Tech Stack:** Go (stdlib `net/http`, `crypto/rsa`, `crypto/hmac`), `modernc.org/sqlite` (pure Go), `hashicorp/yamux` via `internal/tunnel`, Caddy admin API.

**Spec:** [`docs/superpowers/specs/2026-07-20-official-github-app-design.md`](../specs/2026-07-20-official-github-app-design.md)

## Global Constraints

- **No cgo.** Every build must pass with `CGO_ENABLED=0`. No cgo SQLite drivers.
- **Module path** is `github.com/piperbox/piper`.
- **Layering:** `store` knows persistence, `runtime` knows Docker, `caddy` knows Caddy, `deploy` orchestrates, `api` is transport, `client` is the CLI's view. Nothing imports "up". `internal/relay` must not import `internal/source/github`; shared crypto goes in a neutral package.
- **No migrations, no compat shims** (pre-1.x policy in `CLAUDE.md`). `schema.sql` is always the complete current shape; edit `CREATE TABLE` in place.
- **Deployment status strings** are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`.
- **Defaults:** control API `127.0.0.1:8088`, Caddy admin `http://127.0.0.1:2019`, webhook listener `127.0.0.1:8089`, app container port `8080`.
- **Every task ends with `make verify` passing** (gofmt → vet → test → cross-compile) before its commit.
- **Commits** are conventional-commit style, one per task, ending with a co-author trailer naming the model doing the work (the commit blocks below show the current one):
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`
- **Branch:** all work lands on a branch off `main` via PR. Never commit to `main`.

## Scope note: org installs

This plan links an installation to **the account of the GitHub user who installed it**
(the webhook's `sender`), regardless of whether the target is a user or an organization.
`target_type` and `target_login` are recorded as display metadata only. Routing an
org-target installation to *org-owned* agents through `org_members` is a follow-up
(#290), not part of this plan. Every task below assumes user-account linkage.

## File Structure

**Created:**

| Path | Responsibility |
| --- | --- |
| `internal/ghjwt/ghjwt.go` | GitHub App JWT signing (RS256) + PEM parsing, shared by agent and relay |
| `internal/source/github/tokens.go` | `TokenSource` seam; `appTokenSource` (BYO) and `relayTokenSource` (brokered) |
| `internal/relay/installations.go` | `github_installations` persistence |
| `internal/relay/bindings.go` | `repo_bindings` persistence + token-brokering authz |
| `internal/relay/githubapp.go` | Relay-side GitHub App client: signature verify, repo-scoped tokens, repo listing |
| `internal/relay/ingress.go` | `POST /gh` webhook ingress |
| `internal/relay/delivery.go` | `KindHTTP` delivery with re-signing; `pending_events` park/drain |

**Modified:** `internal/relay/schema.sql`, `internal/relay/store.go`, `internal/relay/accounts.go`, `internal/relay/server.go`, `internal/relay/api.go`, `internal/tunnel/tunnel.go`, `internal/agent/tunnelclient.go`, `internal/source/github/{github,fetch,report}.go`, `internal/config/config.go`, `internal/api/api.go`, `cmd/piperd/main.go`, `cmd/piper-relay/main.go`, `cmd/piper/main.go`, `PROGRESS.md`, `docs/runbooks/git-deploy-e2e.md`.

---

### Task 1: `TokenSource` seam in the GitHub provider

Today `Provider` computes installation tokens from an App private key it owns. Brokered mode has no key. Extract the token acquisition behind an interface so both modes share `Fetch`, `Report` and `Parse`.

**Files:**
- Create: `internal/source/github/tokens.go`
- Create: `internal/source/github/tokens_test.go`
- Modify: `internal/source/github/github.go` (the `Provider` struct, `New`, and the `appJWT`/`installationToken` methods)
- Modify: `internal/source/github/fetch.go:19`
- Modify: `internal/source/github/report.go:15`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type TokenSource interface { Token(ctx context.Context, ev source.Event) (string, error) }`
  - `func New(cfg Config) (*Provider, error)` — unchanged signature, now builds an `appTokenSource`
  - `func NewWithTokens(cfg Config, ts TokenSource) *Provider` — brokered/test constructor; `cfg.PrivateKeyPEM` is ignored

- [ ] **Step 1: Write the failing test**

Create `internal/source/github/tokens_test.go`:

```go
package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/source"
)

// stubTokens records the event it was asked about and returns a canned token.
type stubTokens struct {
	tok     string
	gotRepo string
}

func (s *stubTokens) Token(_ context.Context, ev source.Event) (string, error) {
	s.gotRepo = ev.Repo
	return s.tok, nil
}

func TestProviderWithTokenSourceFetches(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write(makeTarball(t))
	}))
	defer srv.Close()

	ts := &stubTokens{tok: "brokered-tok"}
	p := NewWithTokens(Config{WebhookSecret: "s", APIBase: srv.URL}, ts)

	dir := t.TempDir()
	ev := source.Event{Repo: "alice/blog", SHA: "abc"}
	if err := p.Fetch(context.Background(), ev, dir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAuth != "token brokered-tok" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "token brokered-tok")
	}
	if ts.gotRepo != "alice/blog" {
		t.Fatalf("TokenSource saw repo %q, want alice/blog", ts.gotRepo)
	}
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		t.Fatalf("Dockerfile not extracted: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/github/ -run TestProviderWithTokenSourceFetches -v`
Expected: FAIL — `undefined: NewWithTokens`

- [ ] **Step 3: Create the token source**

Create `internal/source/github/tokens.go`:

```go
package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/piperbox/piper/internal/source"
)

// TokenSource yields a GitHub token authorized for ev's repository. BYO mints
// one from the App private key; brokered mode asks the relay for a repo-scoped
// token over the tunnel.
type TokenSource interface {
	Token(ctx context.Context, ev source.Event) (string, error)
}

// appTokenSource mints installation tokens directly from a GitHub App key.
type appTokenSource struct {
	appID   int64
	key     *rsa.PrivateKey
	apiBase string
	http    *http.Client
}

func (a *appTokenSource) Token(ctx context.Context, ev source.Event) (string, error) {
	jwt, err := a.appJWT(time.Now())
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiBase, ev.InstallationID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("installation token: %s: %s", resp.Status, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Token, nil
}
```

- [ ] **Step 4: Move the JWT and rewire `Provider`**

In `internal/source/github/github.go`, replace the `Provider` struct, `New`, `appJWT` and `installationToken` with:

```go
type Provider struct {
	secret  string
	apiBase string
	http    *http.Client
	tokens  TokenSource
}

func New(cfg Config) (*Provider, error) {
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse app private key: %w", err)
	}
	base := apiBaseOrDefault(cfg.APIBase)
	client := &http.Client{Timeout: 30 * time.Second}
	return &Provider{
		secret:  cfg.WebhookSecret,
		apiBase: base,
		http:    client,
		tokens:  &appTokenSource{appID: cfg.AppID, key: key, apiBase: base, http: client},
	}, nil
}

// NewWithTokens builds a Provider whose tokens come from ts. Brokered boxes
// hold no App key, so cfg.AppID and cfg.PrivateKeyPEM are ignored.
func NewWithTokens(cfg Config, ts TokenSource) *Provider {
	return &Provider{
		secret:  cfg.WebhookSecret,
		apiBase: apiBaseOrDefault(cfg.APIBase),
		http:    &http.Client{Timeout: 30 * time.Second},
		tokens:  ts,
	}
}

func apiBaseOrDefault(base string) string {
	if base == "" {
		base = defaultAPIBase
	}
	return strings.TrimRight(base, "/")
}
```

Move `appJWT` into `tokens.go` as a method on `*appTokenSource`, changing only its receiver and field references (`p.appID` → `a.appID`, `p.key` → `a.key`):

```go
// appJWT mints a short-lived GitHub App JWT (RS256) signed with the app key.
func (a *appTokenSource) appJWT(now time.Time) (string, error) {
	header := b64url([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%d"}`,
		now.Add(-30*time.Second).Unix(), now.Add(9*time.Minute).Unix(), a.appID)
	signingInput := header + "." + b64url([]byte(claims))
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(sig), nil
}
```

Keep `Config`, `parsePrivateKey`, `b64url` and `defaultAPIBase` in `github.go`. Remove imports that only the moved code used.

- [ ] **Step 5: Point Fetch and Report at the seam**

`internal/source/github/fetch.go:19` — replace:

```go
	token, err := p.installationToken(ctx, ev.InstallationID)
```

with:

```go
	token, err := p.tokens.Token(ctx, ev)
```

`internal/source/github/report.go:15` — make the identical replacement.

- [ ] **Step 6: Fix the existing JWT/token tests**

`internal/source/github/github_test.go` exercises `installationToken` and `appJWT` on `*Provider`. Retarget both to the token source. Replace each construction of the form `p, err := New(Config{...})` followed by `p.installationToken(ctx, 42)` with:

```go
	key, err := parsePrivateKey(testKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	a := &appTokenSource{appID: 7, key: key, apiBase: srv.URL, http: srv.Client()}
	tok, err := a.Token(context.Background(), source.Event{InstallationID: 42})
```

and each `p.appJWT(now)` with `a.appJWT(now)`. Add `"github.com/piperbox/piper/internal/source"` to that file's imports.

- [ ] **Step 7: Run the package tests**

Run: `go test ./internal/source/github/ -v`
Expected: PASS — including `TestProviderWithTokenSourceFetches` and the retargeted JWT tests.

- [ ] **Step 8: Verify and commit**

```bash
make verify
git add internal/source/github/
git commit -m "$(cat <<'EOF'
refactor(source): put installation tokens behind a TokenSource seam

Brokered boxes hold no GitHub App key, so token acquisition moves behind an
interface that Fetch and Report call. BYO behavior is unchanged.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Relay persistence for installations

**Files:**
- Modify: `internal/relay/schema.sql`
- Create: `internal/relay/installations.go`
- Create: `internal/relay/installations_test.go`

**Interfaces:**
- Consumes: `Store` and `hashToken` from `internal/relay/store.go`; `UpsertAccount` from `internal/relay/accounts.go`.
- Produces:
  - `var ErrNoInstallation = errors.New("no github installation")`
  - `func (s *Store) LinkInstallation(installationID, senderGithubID, targetType, targetLogin string) error`
  - `func (s *Store) UnlinkInstallation(installationID string) error`
  - `func (s *Store) AccountForInstallation(installationID string) (string, error)`
  - `func (s *Store) InstallationForAccount(accountID string) (string, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/relay/installations_test.go`:

```go
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
```

The package's store helper already exists: `openTestStore` (`internal/relay/accounts_test.go:9`) — use it, don't define a new one. `EnrollForAccount` works on an unconfigured store (`apexOrDefault`/`maxAgentsOrDefault` supply defaults), so no `Configure` call is needed in these tests.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestLinkInstallation -v`
Expected: FAIL — `undefined: st.LinkInstallation`

- [ ] **Step 3: Add the table**

Append to `internal/relay/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS github_installations (
    installation_id TEXT PRIMARY KEY,
    account_id      TEXT NOT NULL REFERENCES accounts(id),
    target_type     TEXT NOT NULL,
    target_login    TEXT NOT NULL,
    created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS github_installations_account
    ON github_installations(account_id);
```

- [ ] **Step 4: Implement the store methods**

Create `internal/relay/installations.go`:

```go
package relay

import (
	"database/sql"
	"errors"
	"time"
)

// ErrNoInstallation is returned when no GitHub App installation is on record
// for the requested installation id or account.
var ErrNoInstallation = errors.New("no github installation")

// LinkInstallation records a GitHub App installation against the account of the
// user who installed it (the webhook's sender). Target type and login are
// display metadata: an org-target install still links to the installing user.
//
// Idempotent by installation_id, because the OAuth redirect and the
// installation webhook race and either may land first.
func (s *Store) LinkInstallation(installationID, senderGithubID, targetType, targetLogin string) error {
	var accountID string
	err := s.db.QueryRow(`SELECT id FROM accounts WHERE github_id=?`, senderGithubID).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnknownAccount
	}
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO github_installations(installation_id, account_id, target_type, target_login, created_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(installation_id) DO UPDATE SET
		     account_id   = excluded.account_id,
		     target_type  = excluded.target_type,
		     target_login = excluded.target_login`,
		installationID, accountID, targetType, targetLogin,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// UnlinkInstallation drops an installation, e.g. on installation.deleted.
func (s *Store) UnlinkInstallation(installationID string) error {
	_, err := s.db.Exec(`DELETE FROM github_installations WHERE installation_id=?`, installationID)
	return err
}

// AccountForInstallation resolves an installation to its owning account id.
func (s *Store) AccountForInstallation(installationID string) (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT account_id FROM github_installations WHERE installation_id=?`,
		installationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoInstallation
	}
	return id, err
}

// InstallationForAccount returns the installation an account's agents mint
// tokens through. The most recent one wins if an account somehow holds several.
func (s *Store) InstallationForAccount(accountID string) (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT installation_id FROM github_installations
		  WHERE account_id=? ORDER BY created_at DESC LIMIT 1`, accountID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoInstallation
	}
	return id, err
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/relay/ -run 'Installation' -v`
Expected: PASS — five tests.

- [ ] **Step 6: Verify and commit**

```bash
make verify
git add internal/relay/schema.sql internal/relay/installations.go internal/relay/installations_test.go
git commit -m "$(cat <<'EOF'
feat(relay): persist GitHub App installations per account

Links an installation to the account of the user who installed it, idempotently
so the OAuth redirect and the installation webhook may arrive in either order.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Relay persistence for repo bindings

The relay must know which agent's app a repository deploys to, and must be able to answer the token-brokering authz question: may *this* agent mint a token for *this* repo?

**Files:**
- Modify: `internal/relay/schema.sql`
- Create: `internal/relay/bindings.go`
- Create: `internal/relay/bindings_test.go`

**Interfaces:**
- Consumes: `Store`, `Enroll` (from `store.go:96`), `UpsertAccount`, `EnrollForAccount`.
- Produces:
  - `type Binding struct { AgentName, App, Repo, Branch string }`
  - `func (s *Store) BindRepo(agentName, app, repo, branch string) error`
  - `func (s *Store) UnbindRepo(agentName, app string) error`
  - `func (s *Store) BindingsForRepo(accountID, repo string) ([]Binding, error)`
  - `func (s *Store) AgentBoundToRepo(agentName, repo string) (bool, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/relay/bindings_test.go`:

```go
package relay

import "testing"

// enrolledAgent creates an account and one agent under it, returning the
// account id and the agent's base domain (which is also its agents.name).
func enrolledAgent(t *testing.T, st *Store, githubID, login string) (string, string) {
	t.Helper()
	acc, err := st.UpsertAccount(githubID, login)
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return acc.ID, en.BaseDomain
}

func TestBindRepoAndLookupByRepo(t *testing.T) {
	st := openTestStore(t)
	accID, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.BindRepo(agent, "blog", "Alice/Blog", "main"); err != nil {
		t.Fatalf("BindRepo: %v", err)
	}

	// Repo matching is case-insensitive: GitHub preserves the owner's casing,
	// but the same repository must resolve however it is spelled.
	got, err := st.BindingsForRepo(accID, "alice/blog")
	if err != nil {
		t.Fatalf("BindingsForRepo: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d bindings, want 1", len(got))
	}
	if got[0].AgentName != agent || got[0].App != "blog" || got[0].Branch != "main" {
		t.Fatalf("binding = %+v", got[0])
	}
}

func TestBindRepoReplacesPerApp(t *testing.T) {
	st := openTestStore(t)
	accID, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.BindRepo(agent, "blog", "alice/old", "main"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "blog", "alice/new", "trunk"); err != nil {
		t.Fatal(err)
	}

	if got, _ := st.BindingsForRepo(accID, "alice/old"); len(got) != 0 {
		t.Fatalf("old repo still bound: %+v", got)
	}
	got, _ := st.BindingsForRepo(accID, "alice/new")
	if len(got) != 1 || got[0].Branch != "trunk" {
		t.Fatalf("new binding = %+v", got)
	}
}

func TestBindingsForRepoIsAccountScoped(t *testing.T) {
	st := openTestStore(t)
	_, aliceAgent := enrolledAgent(t, st, "1001", "alice")
	bobID, _ := enrolledAgent(t, st, "2002", "bob")

	if err := st.BindRepo(aliceAgent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}

	got, err := st.BindingsForRepo(bobID, "alice/blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("bob saw alice's binding: %+v", got)
	}
}

func TestAgentBoundToRepo(t *testing.T) {
	st := openTestStore(t)
	_, aliceAgent := enrolledAgent(t, st, "1001", "alice")
	_, bobAgent := enrolledAgent(t, st, "2002", "bob")

	if err := st.BindRepo(aliceAgent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}

	ok, err := st.AgentBoundToRepo(aliceAgent, "alice/blog")
	if err != nil || !ok {
		t.Fatalf("alice's agent bound = %v, err = %v; want true", ok, err)
	}
	ok, err = st.AgentBoundToRepo(bobAgent, "alice/blog")
	if err != nil || ok {
		t.Fatalf("bob's agent bound = %v, err = %v; want false", ok, err)
	}
}

func TestUnbindRepo(t *testing.T) {
	st := openTestStore(t)
	accID, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnbindRepo(agent, "blog"); err != nil {
		t.Fatalf("UnbindRepo: %v", err)
	}
	if got, _ := st.BindingsForRepo(accID, "alice/blog"); len(got) != 0 {
		t.Fatalf("binding survived unbind: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestBindRepo|TestBindings|TestAgentBound|TestUnbind' -v`
Expected: FAIL — `undefined: st.BindRepo`

- [ ] **Step 3: Add the table**

Append to `internal/relay/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS repo_bindings (
    agent_name TEXT NOT NULL REFERENCES agents(name),
    app        TEXT NOT NULL,
    repo       TEXT NOT NULL,
    branch     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (agent_name, app)
);

CREATE INDEX IF NOT EXISTS repo_bindings_repo ON repo_bindings(repo);
```

- [ ] **Step 4: Implement the store methods**

Create `internal/relay/bindings.go`:

```go
package relay

import (
	"strings"
	"time"
)

// Binding is one repo→app deploy route on a specific agent.
type Binding struct {
	AgentName string
	App       string
	Repo      string // "owner/name", lowercased
	Branch    string
}

// normalizeRepo lowercases an "owner/name" so lookups match however GitHub
// spelled the repository in a given payload.
func normalizeRepo(repo string) string { return strings.ToLower(strings.TrimSpace(repo)) }

// BindRepo records that agentName's app deploys from repo@branch. One binding
// per (agent, app): re-linking an app to a different repo replaces the old row.
func (s *Store) BindRepo(agentName, app, repo, branch string) error {
	_, err := s.db.Exec(
		`INSERT INTO repo_bindings(agent_name, app, repo, branch, created_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(agent_name, app) DO UPDATE SET
		     repo = excluded.repo, branch = excluded.branch`,
		agentName, app, normalizeRepo(repo), branch,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// UnbindRepo removes an app's binding. Removing an absent binding is not an error.
func (s *Store) UnbindRepo(agentName, app string) error {
	_, err := s.db.Exec(`DELETE FROM repo_bindings WHERE agent_name=? AND app=?`, agentName, app)
	return err
}

// BindingsForRepo returns every binding for repo among accountID's own agents.
// Scoping by account is what keeps one tenant's push from ever reaching
// another tenant's box, even if both bound the same repository name.
func (s *Store) BindingsForRepo(accountID, repo string) ([]Binding, error) {
	rows, err := s.db.Query(
		`SELECT b.agent_name, b.app, b.repo, b.branch
		   FROM repo_bindings b JOIN agents a ON a.name = b.agent_name
		  WHERE b.repo = ? AND a.account_id = ?`,
		normalizeRepo(repo), accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Binding
	for rows.Next() {
		var b Binding
		if err := rows.Scan(&b.AgentName, &b.App, &b.Repo, &b.Branch); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AgentBoundToRepo reports whether agentName has any binding for repo. This is
// the token-brokering authz check: a box may mint a token only for a repository
// it actually deploys, so one compromised box cannot read every repository the
// account granted the App.
func (s *Store) AgentBoundToRepo(agentName, repo string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM repo_bindings WHERE agent_name=? AND repo=?`,
		agentName, normalizeRepo(repo)).Scan(&n)
	return n > 0, err
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/relay/ -run 'TestBindRepo|TestBindings|TestAgentBound|TestUnbind' -v`
Expected: PASS — five tests.

- [ ] **Step 6: Verify and commit**

```bash
make verify
git add internal/relay/schema.sql internal/relay/bindings.go internal/relay/bindings_test.go
git commit -m "$(cat <<'EOF'
feat(relay): persist repo->app bindings per agent

Lets the relay route a push to the right box and answer the token-brokering
authz question: an agent may mint a token only for a repo it deploys. Lookups
are account-scoped so a repo name can never cross tenants.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Relay-side GitHub App client

The relay needs to verify GitHub's webhook signature, mint repo-scoped installation tokens, and list an installation's repositories. JWT signing now has two consumers (agent and relay), so extract it into a neutral package rather than importing across the layering boundary.

**Files:**
- Create: `internal/ghjwt/ghjwt.go`
- Create: `internal/ghjwt/ghjwt_test.go`
- Create: `internal/relay/githubapp.go`
- Create: `internal/relay/githubapp_test.go`
- Modify: `internal/source/github/tokens.go` (use `ghjwt.Sign`)
- Modify: `internal/source/github/github.go` (use `ghjwt.ParseKey`)
- Modify: `cmd/piper-relay/main.go` (read the new env vars)

**Interfaces:**
- Consumes: `TokenSource`/`appTokenSource` from Task 1.
- Produces:
  - `func ghjwt.ParseKey(pemStr string) (*rsa.PrivateKey, error)`
  - `func ghjwt.Sign(appID string, key *rsa.PrivateKey, now time.Time) (string, error)`
  - `type relay.GitHubAppConfig struct { AppID, PrivateKeyPEM, WebhookSecret, APIBase string }`
  - `func relay.NewGitHubApp(cfg GitHubAppConfig) (*GitHubApp, error)`
  - `func (g *GitHubApp) VerifySignature(header string, body []byte) bool`
  - `func (g *GitHubApp) RepoToken(ctx context.Context, installationID, repo string) (string, time.Time, error)`
  - `func (g *GitHubApp) Repos(ctx context.Context, installationID string) ([]string, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/ghjwt/ghjwt_test.go`:

```go
package ghjwt

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
}

func TestSignProducesThreeSegments(t *testing.T) {
	key, err := ParseKey(testKeyPEM(t))
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	tok, err := Sign("12345", key, time.Now())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if n := strings.Count(tok, "."); n != 2 {
		t.Fatalf("token has %d dots, want 2: %q", n, tok)
	}
}

func TestParseKeyRejectsGarbage(t *testing.T) {
	if _, err := ParseKey("not a pem"); err == nil {
		t.Fatal("ParseKey accepted garbage")
	}
}
```

Create `internal/relay/githubapp_test.go`:

```go
package relay

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

func relayTestKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
}

func TestVerifySignature(t *testing.T) {
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"hello":"world"}`)
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write(body)
	good := "sha256=" + hex.EncodeToString(m.Sum(nil))

	if !app.VerifySignature(good, body) {
		t.Fatal("valid signature rejected")
	}
	if app.VerifySignature("sha256=deadbeef", body) {
		t.Fatal("bad signature accepted")
	}
	if app.VerifySignature("", body) {
		t.Fatal("empty signature accepted")
	}
}

func TestRepoTokenIsScopedToOneRepo(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_scoped","expires_at":"2026-07-20T12:00:00Z"}`))
	}))
	defer srv.Close()

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, exp, err := app.RepoToken(context.Background(), "55", "Alice/Blog")
	if err != nil {
		t.Fatalf("RepoToken: %v", err)
	}
	if tok != "ghs_scoped" {
		t.Fatalf("token = %q", tok)
	}
	if exp.IsZero() {
		t.Fatal("expiry not parsed")
	}
	if gotPath != "/app/installations/55/access_tokens" {
		t.Fatalf("path = %q", gotPath)
	}
	repos, _ := gotBody["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "Blog" {
		t.Fatalf("repositories = %v, want [Blog]", gotBody["repositories"])
	}
	perms, _ := gotBody["permissions"].(map[string]any)
	if perms["contents"] != "read" || perms["deployments"] != "write" {
		t.Fatalf("permissions = %v", perms)
	}
}

func TestReposListsInstallationRepositories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/55/access_tokens" {
			_, _ = w.Write([]byte(`{"token":"t","expires_at":"2026-07-20T12:00:00Z"}`))
			return
		}
		if r.URL.Path != "/installation/repositories" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"repositories":[{"full_name":"alice/blog"},{"full_name":"alice/api"}]}`))
	}))
	defer srv.Close()

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := app.Repos(context.Background(), "55")
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if len(repos) != 2 || repos[0] != "alice/blog" || repos[1] != "alice/api" {
		t.Fatalf("repos = %v", repos)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/ghjwt/ ./internal/relay/ -run 'TestSign|TestParseKey|TestVerifySignature|TestRepoToken|TestRepos' -v`
Expected: FAIL — `no Go files in internal/ghjwt` and `undefined: NewGitHubApp`

- [ ] **Step 3: Create the shared JWT package**

Create `internal/ghjwt/ghjwt.go`:

```go
// Package ghjwt signs GitHub App JWTs (RS256) and parses App private keys. It
// has two consumers — the agent's BYO provider and the relay's brokered App —
// and depends on nothing else in the tree.
package ghjwt

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// ParseKey decodes a PKCS#1 or PKCS#8 RSA private key in PEM form.
func ParseKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Sign mints a short-lived App JWT: issued 30s in the past to tolerate clock
// skew, expiring in 9 minutes (GitHub's ceiling is 10).
func Sign(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := b64url([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%s"}`,
		now.Add(-30*time.Second).Unix(), now.Add(9*time.Minute).Unix(), appID)
	signingInput := header + "." + b64url([]byte(claims))
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(sig), nil
}
```

- [ ] **Step 4: Point the agent provider at it**

In `internal/source/github/tokens.go`, delete the `appJWT` method and change `appTokenSource.Token` to call:

```go
	jwt, err := ghjwt.Sign(strconv.FormatInt(a.appID, 10), a.key, time.Now())
```

In `internal/source/github/github.go`, replace the body of `parsePrivateKey` with `return ghjwt.ParseKey(pemStr)`, and delete `b64url` if nothing else uses it. Add `"github.com/piperbox/piper/internal/ghjwt"` to both files' imports; drop now-unused crypto imports.

Delete the `appJWT` test from `internal/source/github/github_test.go` — `internal/ghjwt` covers it now.

- [ ] **Step 5: Implement the relay App client**

Create `internal/relay/githubapp.go`:

```go
package relay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/ghjwt"
)

const defaultGitHubAPIBase = "https://api.github.com"

// GitHubAppConfig is the relay's App credentials. An empty AppID means the
// relay runs BYO-only: no ingress endpoint, no token brokering.
type GitHubAppConfig struct {
	AppID         string
	PrivateKeyPEM string
	WebhookSecret string
	APIBase       string // defaults to https://api.github.com
}

// GitHubApp is the relay's view of one GitHub App: webhook signature
// verification, repo-scoped installation tokens, and repository listing. The
// private key never leaves this type.
type GitHubApp struct {
	appID   string
	key     *rsa.PrivateKey
	secret  string
	apiBase string
	http    *http.Client
}

func NewGitHubApp(cfg GitHubAppConfig) (*GitHubApp, error) {
	key, err := ghjwt.ParseKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse app private key: %w", err)
	}
	base := cfg.APIBase
	if base == "" {
		base = defaultGitHubAPIBase
	}
	return &GitHubApp{
		appID:   cfg.AppID,
		key:     key,
		secret:  cfg.WebhookSecret,
		apiBase: strings.TrimRight(base, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// VerifySignature checks GitHub's X-Hub-Signature-256 header against the App
// webhook secret in constant time.
func (g *GitHubApp) VerifySignature(header string, body []byte) bool {
	m := hmac.New(sha256.New, []byte(g.secret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(header), []byte(want))
}

// installationToken mints an unscoped installation token. Only Repos uses it;
// everything on the deploy path goes through RepoToken.
func (g *GitHubApp) installationToken(ctx context.Context, installationID string, body any) (string, time.Time, error) {
	jwt, err := ghjwt.Sign(g.appID, g.key, time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", time.Time{}, err
		}
		rdr = bytes.NewReader(b)
	}
	url := g.apiBase + "/app/installations/" + installationID + "/access_tokens"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", time.Time{}, fmt.Errorf("installation token: %s: %s", resp.Status, b)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, err
	}
	return out.Token, out.ExpiresAt, nil
}

// RepoToken mints an installation token scoped to a single repository with the
// minimum permissions a deploy needs. Scoping is what bounds the blast radius
// of a compromised box to the one repo it already deploys.
func (g *GitHubApp) RepoToken(ctx context.Context, installationID, repo string) (string, time.Time, error) {
	name := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		name = repo[i+1:] // GitHub's "repositories" field takes bare names
	}
	return g.installationToken(ctx, installationID, map[string]any{
		"repositories": []string{name},
		"permissions": map[string]string{
			"contents":    "read",
			"deployments": "write",
		},
	})
}

// Repos lists the repositories an installation can reach, as "owner/name".
// This is what a dashboard's repo picker renders; no list is ever cached.
func (g *GitHubApp) Repos(ctx context.Context, installationID string) ([]string, error) {
	tok, _, err := g.installationToken(ctx, installationID, nil)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.apiBase+"/installation/repositories", nil)
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("list repositories: %s: %s", resp.Status, b)
	}
	var out struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Repositories))
	for _, r := range out.Repositories {
		names = append(names, r.FullName)
	}
	return names, nil
}
```

- [ ] **Step 6: Read the config in the relay binary**

In `cmd/piper-relay/main.go`, next to the existing `PIPER_RELAY_GITHUB_CLIENT_ID` / `_SECRET` reads (around line 162-184), add:

```go
	var ghApp *relay.GitHubApp
	appID := os.Getenv("PIPER_RELAY_GITHUB_APP_ID")
	keyPath := os.Getenv("PIPER_RELAY_GITHUB_APP_KEY")
	if appID != "" && keyPath != "" {
		info, err := os.Stat(keyPath)
		if err != nil {
			log.Fatalf("github app key: %v", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			log.Fatalf("github app key %s is group/world readable (mode %o); chmod 600 it", keyPath, info.Mode().Perm())
		}
		pemBytes, err := os.ReadFile(keyPath)
		if err != nil {
			log.Fatalf("github app key: %v", err)
		}
		ghApp, err = relay.NewGitHubApp(relay.GitHubAppConfig{
			AppID:         appID,
			PrivateKeyPEM: string(pemBytes),
			WebhookSecret: os.Getenv("PIPER_RELAY_GITHUB_WEBHOOK_SECRET"),
		})
		if err != nil {
			log.Fatalf("github app: %v", err)
		}
		log.Printf("relay: GitHub App %s configured (brokered git deploys enabled)", appID)
	}
```

Leave `ghApp` unused for now — Task 6 wires it into the ingress handler. If the compiler rejects the unused variable, add `_ = ghApp` with a `// wired in Task 6` comment and delete it there.

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./internal/ghjwt/ ./internal/relay/ ./internal/source/github/ -v`
Expected: PASS

- [ ] **Step 8: Verify and commit**

```bash
make verify
git add internal/ghjwt/ internal/relay/githubapp.go internal/relay/githubapp_test.go \
        internal/source/github/ cmd/piper-relay/main.go
git commit -m "$(cat <<'EOF'
feat(relay): GitHub App client with repo-scoped installation tokens

Adds signature verification, per-repo token minting with contents:read +
deployments:write, and installation repo listing. JWT signing moves to
internal/ghjwt now that agent and relay both need it; the relay never imports
the agent's provider package.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Control ops for bindings and token brokering

The box needs to tell the relay about its bindings and ask for tokens. Both ride the existing authenticated `KindControl` channel.

**Files:**
- Modify: `internal/tunnel/tunnel.go:138-155` (`ControlRequest`, `ControlResponse`)
- Modify: `internal/relay/server.go:225` (the op switch)
- Modify: `internal/agent/tunnelclient.go`
- Create: `internal/relay/ghtoken.go`
- Modify: `internal/relay/server_test.go` (new tests)
- Modify: `internal/agent/tunnelclient_test.go` (new tests)

**Interfaces:**
- Consumes: `Store.BindRepo`, `Store.UnbindRepo`, `Store.AgentBoundToRepo` (Task 3); `Store.InstallationForAccount` (Task 2); `GitHubApp.RepoToken` (Task 4); `Store.AgentAccount` (existing, used at `internal/relay/proxy.go:183`).
- Produces:
  - `tunnel.ControlRequest` gains `Repo string` and `Branch string`
  - `tunnel.ControlResponse` gains `Token string` and `Expires string` (RFC3339)
  - `func (c *TunnelClient) BindRepo(app, repo, branch string) error`
  - `func (c *TunnelClient) UnbindRepo(app string) error`
  - `func (c *TunnelClient) GitHubToken(repo string) (string, error)`
  - `func (s *Store) GitHubTokenFor(ctx context.Context, app *GitHubApp, agentName, repo string) (string, time.Time, error)`

The receiver type is confirmed: `TunnelClient` (`internal/agent/tunnelclient.go:33`).

- [ ] **Step 1: Write the failing relay test**

Append to `internal/relay/server_test.go`:

```go
func TestBindRepoControlOp(t *testing.T) {
	sess, _, _, base, st := startTestRelay(t, nil, nil)

	control := func(req tunnel.ControlRequest) tunnel.ControlResponse {
		t.Helper()
		cs, err := sess.OpenKind(tunnel.KindControl)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		if err := tunnel.WriteMsg(cs, req); err != nil {
			t.Fatal(err)
		}
		var resp tunnel.ControlResponse
		if err := tunnel.ReadMsg(cs, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if resp := control(tunnel.ControlRequest{
		Op: "bind-repo", App: "blog", Repo: "alice/blog", Branch: "main",
	}); resp.Error != "" {
		t.Fatalf("bind-repo error: %s", resp.Error)
	}
	ok, err := st.AgentBoundToRepo(base, "alice/blog")
	if err != nil || !ok {
		t.Fatalf("binding not stored: ok=%v err=%v", ok, err)
	}

	if resp := control(tunnel.ControlRequest{Op: "bind-repo", App: "blog"}); resp.Error == "" {
		t.Fatal("bind-repo without repo/branch was accepted")
	}

	if resp := control(tunnel.ControlRequest{Op: "unbind-repo", App: "blog"}); resp.Error != "" {
		t.Fatalf("unbind-repo error: %s", resp.Error)
	}
	if ok, _ := st.AgentBoundToRepo(base, "alice/blog"); ok {
		t.Fatal("binding survived unbind-repo")
	}
}

func TestGHTokenControlOpRejectsUnboundRepo(t *testing.T) {
	sess, _, _, _, _ := startTestRelay(t, nil, nil)
	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "gh-token", Repo: "someone/else"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == "" || resp.Token != "" {
		t.Fatalf("unbound repo minted a token: %+v", resp)
	}
}
```

`startTestRelay` is confirmed to return `(sess *tunnel.Session, tlsAddr, httpAddr, baseDomain string, st *Store)` (`internal/relay/server_test.go:21`), so the five-value destructuring above is right as written. (Task 7 widens the helper to also return the `*Router`; these call sites gain a trailing `_` then.) The test relay carries no `*GitHubApp`, which is fine: Step 4 deliberately orders the binding check before the nil-App guard, so `TestGHTokenControlOpRejectsUnboundRepo` genuinely exercises the authz path rather than passing on the missing App.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestBindRepoControlOp|TestGHTokenControlOp' -v`
Expected: FAIL — `unknown field Repo in struct literal`

- [ ] **Step 3: Widen the control protocol**

In `internal/tunnel/tunnel.go`, extend the two structs:

```go
// ControlRequest is an agent→relay control message on a KindControl stream.
type ControlRequest struct {
	Op       string `json:"op"` // "register" | "deregister" | "provision" | "add-domain" | "remove-domain" | "domain-active" | "bind-repo" | "unbind-repo" | "gh-token"
	App      string `json:"app,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Token    string `json:"token,omitempty"`  // "provision": the box's control-API bearer for the relay to inject
	Domain   string `json:"domain,omitempty"` // custom domain for add/remove/active operations
	Repo     string `json:"repo,omitempty"`   // "owner/name" for bind-repo and gh-token
	Branch   string `json:"branch,omitempty"` // tracked branch for bind-repo
}

// ControlResponse is the relay's reply. Error is non-empty on failure.
type ControlResponse struct {
	Hostname string `json:"hostname,omitempty"`
	Error    string `json:"error,omitempty"`
	Token    string `json:"token,omitempty"`   // "gh-token": repo-scoped installation token
	Expires  string `json:"expires,omitempty"` // "gh-token": RFC3339 expiry
}
```

- [ ] **Step 4: Implement the token-brokering authz chain**

Create `internal/relay/ghtoken.go`:

```go
package relay

import (
	"context"
	"errors"
	"time"
)

// ErrNoGitHubApp is returned when a relay without App credentials is asked to
// broker a token.
var ErrNoGitHubApp = errors.New("relay has no github app configured")

// ErrRepoNotBound is returned when an agent asks for a token for a repository
// it does not deploy.
var ErrRepoNotBound = errors.New("repo not bound to this agent")

// GitHubTokenFor mints a repo-scoped installation token for agentName, after
// checking the full chain: the agent must have a binding for repo, and the
// account owning that agent must hold the installation the token comes from.
// Without the binding check a single compromised box could read every
// repository its account granted the App.
func (s *Store) GitHubTokenFor(ctx context.Context, app *GitHubApp, agentName, repo string) (string, time.Time, error) {
	// Binding check first — it is the authz boundary, and keeping it ahead of
	// the nil-App guard means a relay without App credentials still exercises
	// it (the control-op test would otherwise pass with the check deleted).
	bound, err := s.AgentBoundToRepo(agentName, repo)
	if err != nil {
		return "", time.Time{}, err
	}
	if !bound {
		return "", time.Time{}, ErrRepoNotBound
	}
	if app == nil {
		return "", time.Time{}, ErrNoGitHubApp
	}
	accountID, _, err := s.AgentAccount(agentName)
	if err != nil {
		return "", time.Time{}, err
	}
	installationID, err := s.InstallationForAccount(accountID)
	if err != nil {
		return "", time.Time{}, err
	}
	return app.RepoToken(ctx, installationID, repo)
}
```

`AgentAccount` is confirmed: `(accountID, username string, err error)` (`internal/relay/hostnames.go:30`). It returns `ErrBadCredential` when the owning account is disabled, so the operator kill-switch blocks token minting here for free.

- [ ] **Step 5: Dispatch the new ops**

The op switch lives in `handleControl` (`internal/relay/server.go:219`), a plain function reached via `Serve` → `serveTunnel` → `serveControl`. Add three cases before the `default`; `sess.BaseDomain` is the agent's `agents.name`. There is no server struct — thread a `ghApp *GitHubApp` parameter through `Serve` → `serveTunnel` → `serveControl` → `handleControl`, updating the `relay.Serve` call in `cmd/piper-relay/main.go:207` (pass Task 4's `ghApp`) and the inline accept loop in `startTestRelay` (`internal/relay/server_test.go`, pass `nil`).

```go
	case "bind-repo":
		if req.App == "" || req.Repo == "" || req.Branch == "" {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "bind-repo: app, repo and branch required"})
			return
		}
		if err := st.BindRepo(sess.BaseDomain, req.App, req.Repo, req.Branch); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	case "unbind-repo":
		if req.App == "" {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "unbind-repo: app required"})
			return
		}
		if err := st.UnbindRepo(sess.BaseDomain, req.App); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	case "gh-token":
		if req.Repo == "" {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "gh-token: repo required"})
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tok, exp, err := st.GitHubTokenFor(ctx, ghApp, sess.BaseDomain, req.Repo)
		cancel()
		if err != nil {
			// The detail stays server-side: a box must not learn whether a repo
			// exists, only that it is not authorized for it here.
			log.Printf("relay: gh-token for %s repo %s: %v", sess.BaseDomain, req.Repo, err)
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "github token unavailable"})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{
			Token: tok, Expires: exp.UTC().Format(time.RFC3339),
		})
```

Add `"context"` and `"time"` to the file's imports if absent.

- [ ] **Step 6: Add the agent-side calls**

Append to `internal/agent/tunnelclient.go`, matching the shape of the existing helpers at lines 87-121:

```go
// BindRepo tells the relay that app deploys from repo@branch on this box, so
// the relay can route that repository's webhooks here.
func (c *TunnelClient) BindRepo(app, repo, branch string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "bind-repo", App: app, Repo: repo, Branch: branch})
	return err
}

// UnbindRepo drops an app's repo binding on the relay.
func (c *TunnelClient) UnbindRepo(app string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "unbind-repo", App: app})
	return err
}

// GitHubToken asks the relay for an installation token scoped to repo. Brokered
// boxes hold no GitHub App key, so this is their only way to reach the repo.
func (c *TunnelClient) GitHubToken(repo string) (string, error) {
	resp, err := c.control(tunnel.ControlRequest{Op: "gh-token", Repo: repo})
	if err != nil {
		return "", err
	}
	return resp.Token, nil
}
```

`control` is confirmed to return `(string, error)` (`internal/agent/tunnelclient.go:125`) — widen it to `(tunnel.ControlResponse, error)` so `GitHubToken` can read `Token`, and update the six existing helper methods at lines 86-123 (`Register` keeps returning `resp.Hostname`; the others discard the response).

- [ ] **Step 7: Add the agent-side test**

Append to `internal/agent/tunnelclient_test.go`, cribbing the fake-relay shape of `TestTunnelClientDomainOps` (line 300):

```go
func TestTunnelClientRepoOps(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	relaySess := <-sessCh

	got := make(chan tunnel.ControlRequest, 3)
	go func() {
		for {
			kind, stream, err := relaySess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindControl {
				stream.Close()
				continue
			}
			var req tunnel.ControlRequest
			_ = tunnel.ReadMsg(stream, &req)
			got <- req
			resp := tunnel.ControlResponse{}
			if req.Op == "gh-token" {
				resp.Token = "ghs_x"
			}
			_ = tunnel.WriteMsg(stream, resp)
			stream.Close()
		}
	}()

	// Retry until the session is up, exactly as TestTunnelClientDomainOps does.
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err = c.BindRepo("blog", "alice/blog", "main"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("BindRepo: %v", err)
	}
	if req := <-got; req.Op != "bind-repo" || req.App != "blog" || req.Repo != "alice/blog" || req.Branch != "main" {
		t.Fatalf("BindRepo sent %+v", req)
	}

	tok, err := c.GitHubToken("alice/blog")
	if err != nil {
		t.Fatalf("GitHubToken: %v", err)
	}
	if tok != "ghs_x" {
		t.Fatalf("token = %q, want ghs_x", tok)
	}
	if req := <-got; req.Op != "gh-token" || req.Repo != "alice/blog" {
		t.Fatalf("GitHubToken sent %+v", req)
	}

	if err := c.UnbindRepo("blog"); err != nil {
		t.Fatalf("UnbindRepo: %v", err)
	}
	if req := <-got; req.Op != "unbind-repo" || req.App != "blog" {
		t.Fatalf("UnbindRepo sent %+v", req)
	}
}
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./internal/tunnel/ ./internal/relay/ ./internal/agent/ -v`
Expected: PASS

- [ ] **Step 9: Verify and commit**

```bash
make verify
git add internal/tunnel/tunnel.go internal/relay/server.go internal/relay/ghtoken.go \
        internal/relay/server_test.go internal/agent/tunnelclient.go internal/agent/tunnelclient_test.go
git commit -m "$(cat <<'EOF'
feat(relay): bind-repo, unbind-repo and gh-token control ops

Agent->relay calls ride the existing authenticated KindControl channel, so the
box needs no second credential. gh-token mints only for a repo the asking agent
actually deploys, and only through its own account's installation.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Webhook ingress on the relay

**Files:**
- Create: `internal/relay/ingress.go`
- Create: `internal/relay/ingress_test.go`
- Modify: `cmd/piper-relay/main.go` (mount `POST /gh`)

**Interfaces:**
- Consumes: `GitHubApp.VerifySignature` (Task 4), `Store.LinkInstallation`/`UnlinkInstallation`/`AccountForInstallation` (Task 2), `Store.BindingsForRepo` (Task 3).
- Produces:
  - `type Deliverer interface { Deliver(ctx context.Context, b Binding, eventType string, payload []byte) error }`
  - `func NewGitHubIngress(st *Store, app *GitHubApp, d Deliverer) http.Handler`

- [ ] **Step 1: Write the failing test**

Create `internal/relay/ingress_test.go`:

```go
package relay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type capturingDeliverer struct {
	mu    sync.Mutex
	calls []Binding
}

func (c *capturingDeliverer) Deliver(_ context.Context, b Binding, _ string, _ []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, b)
	return nil
}

func (c *capturingDeliverer) seen() []Binding {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Binding(nil), c.calls...)
}

func signed(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func postEvent(t *testing.T, h http.Handler, event, sig, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/gh", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newTestIngress(t *testing.T, st *Store, d Deliverer) http.Handler {
	t.Helper()
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewGitHubIngress(st, app, d)
}

func TestIngressRejectsBadSignature(t *testing.T) {
	st := openTestStore(t)
	d := &capturingDeliverer{}
	h := newTestIngress(t, st, d)

	rec := postEvent(t, h, "push", "sha256=deadbeef", `{}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(d.seen()) != 0 {
		t.Fatal("delivered despite bad signature")
	}
}

func TestIngressRoutesPushToBinding(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}

	d := &capturingDeliverer{}
	h := newTestIngress(t, st, d)

	body := `{"ref":"refs/heads/main","after":"abc",` +
		`"repository":{"full_name":"alice/blog"},"installation":{"id":55}}`
	rec := postEvent(t, h, "push", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	seen := d.seen()
	if len(seen) != 1 || seen[0].AgentName != agent || seen[0].App != "blog" {
		t.Fatalf("delivered = %+v", seen)
	}
}

func TestIngressDropsUnlinkedInstallation(t *testing.T) {
	st := openTestStore(t)
	d := &capturingDeliverer{}
	h := newTestIngress(t, st, d)

	body := `{"ref":"refs/heads/main","repository":{"full_name":"alice/blog"},"installation":{"id":999}}`
	rec := postEvent(t, h, "push", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if len(d.seen()) != 0 {
		t.Fatal("routed an event for an unlinked installation")
	}
}

func TestIngressLinksAndUnlinksInstallation(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.UpsertAccount("1001", "alice"); err != nil {
		t.Fatal(err)
	}
	h := newTestIngress(t, st, &capturingDeliverer{})

	created := `{"action":"created","installation":{"id":55,` +
		`"account":{"type":"User","login":"alice"}},"sender":{"id":1001,"login":"alice"}}`
	if rec := postEvent(t, h, "installation", signed("s3cret", []byte(created)), created); rec.Code != http.StatusAccepted {
		t.Fatalf("created status = %d", rec.Code)
	}
	if _, err := st.AccountForInstallation("55"); err != nil {
		t.Fatalf("installation not linked: %v", err)
	}

	deleted := fmt.Sprintf(`{"action":"deleted","installation":{"id":55,`+
		`"account":{"type":"User","login":"alice"}},"sender":{"id":%d,"login":"alice"}}`, 1001)
	if rec := postEvent(t, h, "installation", signed("s3cret", []byte(deleted)), deleted); rec.Code != http.StatusAccepted {
		t.Fatalf("deleted status = %d", rec.Code)
	}
	if _, err := st.AccountForInstallation("55"); err == nil {
		t.Fatal("installation survived deletion")
	}
}

func TestIngressPongsPing(t *testing.T) {
	st := openTestStore(t)
	h := newTestIngress(t, st, &capturingDeliverer{})
	body := `{"zen":"hi"}`
	rec := postEvent(t, h, "ping", signed("s3cret", []byte(body)), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestIngress -v`
Expected: FAIL — `undefined: NewGitHubIngress`

- [ ] **Step 3: Implement the ingress handler**

Create `internal/relay/ingress.go`:

```go
package relay

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
)

// maxWebhookBody mirrors the agent-side cap in internal/webhook.
const maxWebhookBody = 5 << 20

// Deliverer hands a verified, routed webhook to one bound agent.
type Deliverer interface {
	Deliver(ctx context.Context, b Binding, eventType string, payload []byte) error
}

// ghEnvelope is the slice of a GitHub webhook the relay needs to route. Payload
// interpretation stays on the box: the relay reads only the routing keys.
type ghEnvelope struct {
	Action       string `json:"action"`
	Repository   struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Type  string `json:"type"`
			Login string `json:"login"`
		} `json:"account"`
	} `json:"installation"`
	Sender struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"sender"`
}

// NewGitHubIngress serves the App's single webhook URL. It verifies the App
// signature, keeps installation linkage current, and routes everything else to
// the bound agents of the installation's account. It never routes an event
// whose installation is not linked to an account.
func NewGitHubIngress(st *Store, app *GitHubApp, d Deliverer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if !app.VerifySignature(r.Header.Get("X-Hub-Signature-256"), body) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}

		event := r.Header.Get("X-GitHub-Event")
		if event == "ping" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "pong")
			return
		}

		var env ghEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		installationID := strconv.FormatInt(env.Installation.ID, 10)

		if event == "installation" {
			handleInstallationEvent(st, env, installationID)
			w.WriteHeader(http.StatusAccepted)
			return
		}

		accountID, err := st.AccountForInstallation(installationID)
		if err != nil {
			// Unlinked installation: acknowledge so GitHub stops retrying, but
			// never route. This is the tenancy boundary.
			log.Printf("relay: %s event for unlinked installation %s", event, installationID)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		bindings, err := st.BindingsForRepo(accountID, env.Repository.FullName)
		if err != nil {
			http.Error(w, "routing error", http.StatusInternalServerError)
			return
		}

		// Routing is by repository only. Whether the branch matches is the
		// agent's decision, exactly as in BYO mode; two components filtering the
		// same condition is how pushes end up deploying nowhere.
		w.WriteHeader(http.StatusAccepted)
		for _, b := range bindings {
			go func(b Binding) {
				if err := d.Deliver(context.Background(), b, event, body); err != nil {
					log.Printf("relay: deliver %s to %s/%s: %v", event, b.AgentName, b.App, err)
				}
			}(b)
		}
	})
}

// handleInstallationEvent keeps github_installations in step with GitHub. It is
// written to be order-independent: the OAuth redirect and this webhook race.
func handleInstallationEvent(st *Store, env ghEnvelope, installationID string) {
	switch env.Action {
	case "created", "new_permissions_accepted", "unsuspend":
		senderID := strconv.FormatInt(env.Sender.ID, 10)
		typ := "user"
		if env.Installation.Account.Type == "Organization" {
			typ = "org"
		}
		if err := st.LinkInstallation(installationID, senderID, typ, env.Installation.Account.Login); err != nil {
			log.Printf("relay: link installation %s: %v", installationID, err)
		}
	case "deleted", "suspend":
		if err := st.UnlinkInstallation(installationID); err != nil {
			log.Printf("relay: unlink installation %s: %v", installationID, err)
		}
	}
}
```

The `go func` per binding means the test's `capturingDeliverer` may race the assertion. Make the handler deliver synchronously when there is exactly one binding? No — instead, in `ingress_test.go`, have `capturingDeliverer.Deliver` signal a channel and have `TestIngressRoutesPushToBinding` wait on it before asserting:

```go
type capturingDeliverer struct {
	mu    sync.Mutex
	calls []Binding
	done  chan struct{}
}
```
with `Deliver` doing a non-blocking `select { case c.done <- struct{}{}: default: }` after appending, constructed as `&capturingDeliverer{done: make(chan struct{}, 8)}`, and the routing test waiting:

```go
	select {
	case <-d.done:
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery within 2s")
	}
```

Apply that to the test file before running it; leave the drop/ping tests asserting only that nothing arrived after a short `time.Sleep(100 * time.Millisecond)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -run TestIngress -v -race`
Expected: PASS — six tests, no race reports.

- [ ] **Step 5: Mount it in the relay binary**

There is no mux in `cmd/piper-relay/main.go` — the account API mux lives inside `NewAPIWithTunnel` (`internal/relay/api.go:24`), and main passes the finished `apiHandler` straight to `relay.Serve` (`main.go:189,207`). Mount the ingress by wrapping in main, when `ghApp != nil` (this consumes the variable Task 4 left dangling):

```go
	ctrl := apiHandler
	if ghApp != nil {
		outer := http.NewServeMux()
		outer.Handle("POST /gh", relay.NewGitHubIngress(st, ghApp, delivery))
		outer.Handle("/", apiHandler)
		ctrl = outer
	}
```

and pass `ctrl` (not `apiHandler`) to `relay.Serve`. The account API is served under the host `api.<apex>` on the TLS listener, so the App's webhook URL is `https://api.<apex>/gh` — that is what gets registered on the GitHub App (see the checklist at the bottom).

`delivery` does not exist yet — Task 7 creates it. Until then pass a placeholder that logs and returns nil, and delete it in Task 7:

```go
	// Replaced by the tunnel deliverer in Task 7.
	delivery := relay.DeliverFunc(func(ctx context.Context, b relay.Binding, event string, payload []byte) error {
		log.Printf("relay: would deliver %s to %s/%s", event, b.AgentName, b.App)
		return nil
	})
```

Add to `internal/relay/ingress.go`:

```go
// DeliverFunc adapts a function to Deliverer.
type DeliverFunc func(ctx context.Context, b Binding, eventType string, payload []byte) error

func (f DeliverFunc) Deliver(ctx context.Context, b Binding, eventType string, payload []byte) error {
	return f(ctx, b, eventType, payload)
}
```

- [ ] **Step 6: Verify and commit**

```bash
make verify
git add internal/relay/ingress.go internal/relay/ingress_test.go cmd/piper-relay/main.go
git commit -m "$(cat <<'EOF'
feat(relay): GitHub webhook ingress with installation-scoped routing

Verifies the App signature, keeps installation<->account linkage current, and
routes by repository to the bound agents of that installation's account only.
An event whose installation is not linked is acknowledged and dropped.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Deliver over the tunnel, re-signed

**Files:**
- Modify: `internal/relay/schema.sql` (add `agents.webhook_secret`)
- Modify: `internal/relay/accounts.go` (mint the secret in `EnrollForAccount`)
- Modify: `internal/relay/store.go` (`AgentWebhookSecret`)
- Create: `internal/relay/delivery.go`
- Create: `internal/relay/delivery_test.go`
- Modify: `cmd/piper-relay/main.go` (replace the placeholder deliverer)

**Interfaces:**
- Consumes: `Binding` (Task 3), `Deliverer` (Task 6), `Router.Lookup` (`internal/relay/router.go:110`), `tunnel.KindHTTP`.
- Produces:
  - `func (s *Store) AgentWebhookSecret(agentName string) (string, error)`
  - `type TunnelDelivery struct { ... }`
  - `func NewTunnelDelivery(st *Store, router *Router) *TunnelDelivery`
  - `var ErrAgentOffline = errors.New("agent not connected")`

- [ ] **Step 1: Write the failing test**

Create `internal/relay/delivery_test.go`:

```go
package relay

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

func TestDeliverySignsWithAgentSecretAndDropsGitHubs(t *testing.T) {
	sess, _, _, base, st, router := startTestRelay(t, nil, nil)

	secret, err := st.AgentWebhookSecret(base)
	if err != nil {
		t.Fatalf("AgentWebhookSecret: %v", err)
	}
	if secret == "" {
		t.Fatal("enrollment minted no webhook secret")
	}

	// Stand in for the box: accept the KindHTTP stream and answer 202.
	type got struct {
		host, sig, ghSig, event string
		body                    []byte
	}
	ch := make(chan got, 1)
	go func() {
		kind, conn, err := sess.AcceptKind()
		if err != nil || kind != tunnel.KindHTTP {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		body, _ := io.ReadAll(req.Body)
		ch <- got{
			host:  req.Host,
			sig:   req.Header.Get("X-Hub-Signature-256"),
			ghSig: req.Header.Get("X-Hub-Signature"),
			event: req.Header.Get("X-GitHub-Event"),
			body:  body,
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
	}()

	d := NewTunnelDelivery(st, router)
	payload := []byte(`{"ref":"refs/heads/main"}`)
	b := Binding{AgentName: base, App: "blog", Repo: "alice/blog", Branch: "main"}
	if err := d.Deliver(context.Background(), b, "push", payload); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	select {
	case g := <-ch:
		if g.host != "hooks."+base {
			t.Fatalf("Host = %q, want hooks.%s", g.host, base)
		}
		if g.event != "push" {
			t.Fatalf("event = %q", g.event)
		}
		if string(g.body) != string(payload) {
			t.Fatalf("body = %q", g.body)
		}
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(payload)
		want := "sha256=" + hex.EncodeToString(m.Sum(nil))
		if g.sig != want {
			t.Fatalf("signature = %q, want %q", g.sig, want)
		}
		if g.ghSig != "" {
			t.Fatal("GitHub's original signature was forwarded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no request arrived on the tunnel")
	}
}

func TestDeliveryOfflineAgent(t *testing.T) {
	st := openTestStore(t)
	_, base := enrolledAgent(t, st, "1001", "alice")
	d := NewTunnelDelivery(st, NewRouter())

	err := d.Deliver(context.Background(), Binding{AgentName: base, App: "blog"}, "push", []byte(`{}`))
	if !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("err = %v, want ErrAgentOffline", err)
	}
}
```

Confirmed: `startTestRelay` currently returns five values — `(sess, tlsAddr, httpAddr, baseDomain, st)` (`internal/relay/server_test.go:21`) — and does *not* return the router. Widen it to also return the `router` it builds at `server_test.go:35` as a sixth value, and add a trailing `_` to every existing call site (including the tests added in Task 5).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestDelivery -v`
Expected: FAIL — `undefined: st.AgentWebhookSecret`

- [ ] **Step 3: Add the per-agent secret**

In `internal/relay/schema.sql`, add the column to the existing `agents` table definition (edit in place — no migration):

```sql
CREATE TABLE IF NOT EXISTS agents (
    name           TEXT PRIMARY KEY,
    token_hash     TEXT NOT NULL UNIQUE,
    base_domain    TEXT NOT NULL,
    account_id     TEXT,
    control_token  TEXT,
    webhook_secret TEXT,
    created_at     TEXT NOT NULL
);
```

In `internal/relay/accounts.go`, inside `EnrollForAccount`'s retry loop, mint a secret alongside the token and store it. Replace the `tx.Exec` insert with:

```go
			rawSecret := make([]byte, 32)
			if _, err := rand.Read(rawSecret); err != nil {
				return Enrollment{}, err
			}
			secret := hex.EncodeToString(rawSecret)

			_, err := tx.Exec(
				`INSERT INTO agents(name, token_hash, base_domain, account_id, webhook_secret, created_at)
				 VALUES(?,?,?,?,?,?)`,
				base, hashToken(tok), base, accountID, secret, now)
			if err == nil {
				if err := tx.Commit(); err != nil {
					return Enrollment{}, err
				}
				return Enrollment{Token: tok, BaseDomain: base, WebhookSecret: secret}, nil
			}
```

and extend the `Enrollment` struct in the same file:

```go
// Enrollment is the result of a self-service claim: an enrollment token, the
// single-label base domain the relay assigned the agent under the apex, and the
// secret the relay signs brokered webhook deliveries to this box with.
type Enrollment struct {
	Token         string
	BaseDomain    string
	WebhookSecret string
}
```

In `internal/relay/store.go`, add the reader:

```go
// AgentWebhookSecret returns the secret the relay signs brokered webhook
// deliveries to agentName with. Unknown agents are ErrBadToken.
func (s *Store) AgentWebhookSecret(agentName string) (string, error) {
	var sec sql.NullString
	err := s.db.QueryRow(`SELECT webhook_secret FROM agents WHERE name=?`, agentName).Scan(&sec)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadToken
	}
	if err != nil {
		return "", err
	}
	return sec.String, nil
}
```

- [ ] **Step 4: Implement delivery**

Create `internal/relay/delivery.go`:

```go
package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// ErrAgentOffline is returned when a bound agent has no live tunnel session.
var ErrAgentOffline = errors.New("agent not connected")

// deliveryTimeout bounds one delivery attempt end to end.
const deliveryTimeout = 30 * time.Second

// TunnelDelivery hands a verified webhook to a box over its tunnel. It opens a
// KindHTTP stream — which the agent already pipes to Caddy on :80 — and speaks
// plain HTTP with Host hooks.<base>, so the box's existing webhook listener and
// Caddy route serve it exactly as a public request. Nothing new is exposed to
// the internet.
type TunnelDelivery struct {
	st     *Store
	router *Router
}

func NewTunnelDelivery(st *Store, router *Router) *TunnelDelivery {
	return &TunnelDelivery{st: st, router: router}
}

func (t *TunnelDelivery) Deliver(ctx context.Context, b Binding, eventType string, payload []byte) error {
	sess, ok := t.router.Lookup(b.AgentName)
	if !ok {
		return ErrAgentOffline
	}
	secret, err := t.st.AgentWebhookSecret(b.AgentName)
	if err != nil {
		return err
	}

	stream, err := sess.OpenKind(tunnel.KindHTTP)
	if err != nil {
		return fmt.Errorf("open delivery stream: %w", err)
	}
	defer stream.Close()
	deadline := time.Now().Add(deliveryTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = stream.SetDeadline(deadline)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://hooks."+b.AgentName+"/", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", eventType)
	// GitHub's own signature is never forwarded: the box shares no secret with
	// GitHub in brokered mode. Re-sign with the per-agent secret so the agent's
	// existing verification path is unchanged and the tunnel is not treated as
	// authenticating on its own.
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, payload))
	req.ContentLength = int64(len(payload))

	if err := req.Write(stream); err != nil {
		return fmt.Errorf("write delivery: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(stream), req)
	if err != nil {
		return fmt.Errorf("read delivery response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("box rejected delivery: %s", resp.Status)
	}
	return nil
}

func signPayload(secret string, payload []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(payload)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}
```

- [ ] **Step 5: Replace the placeholder in the relay binary**

In `cmd/piper-relay/main.go`, delete the `DeliverFunc` placeholder from Task 6 and build the real one where the router is already in scope:

```go
	if ghApp != nil {
		mux.Handle("POST /gh", relay.NewGitHubIngress(st, ghApp, relay.NewTunnelDelivery(st, router)))
	}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/relay/ -run 'TestDelivery|TestEnroll|TestIngress' -v -race`
Expected: PASS

- [ ] **Step 7: Verify and commit**

```bash
make verify
git add internal/relay/schema.sql internal/relay/accounts.go internal/relay/store.go \
        internal/relay/delivery.go internal/relay/delivery_test.go cmd/piper-relay/main.go
git commit -m "$(cat <<'EOF'
feat(relay): deliver brokered webhooks over the tunnel, re-signed

Opens a KindHTTP stream to the box's Caddy with Host hooks.<base>, so the
existing webhook listener serves it unchanged. GitHub's signature is dropped and
replaced with an HMAC over a per-agent secret minted at enroll: the tunnel is
not treated as authenticating on its own.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: Park and drain events for offline boxes

**Files:**
- Modify: `internal/relay/schema.sql`
- Modify: `internal/relay/delivery.go`
- Create: `internal/relay/pending_test.go`
- Modify: `internal/relay/server.go` (drain on session register)

**Interfaces:**
- Consumes: `TunnelDelivery`, `ErrAgentOffline` (Task 7).
- Produces:
  - `func (s *Store) ParkEvent(agentName, app, ref, event string, payload []byte) error`
  - `func (s *Store) DrainEvents(agentName string) ([]PendingEvent, error)`
  - `type PendingEvent struct { AgentName, App, Ref, Event string; Payload []byte }`
  - `func (t *TunnelDelivery) DrainFor(ctx context.Context, agentName string)`

- [ ] **Step 1: Write the failing test**

Create `internal/relay/pending_test.go`:

```go
package relay

import "testing"

func TestParkEventCoalescesByRef(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{"after":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{"after":"new"}`)); err != nil {
		t.Fatal(err)
	}

	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatalf("DrainEvents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d parked events, want 1 (coalesced)", len(got))
	}
	if string(got[0].Payload) != `{"after":"new"}` {
		t.Fatalf("payload = %s, want the newer one", got[0].Payload)
	}
	if got[0].App != "blog" || got[0].Ref != "main" || got[0].Event != "push" {
		t.Fatalf("event = %+v", got[0])
	}
}

func TestParkEventKeepsDistinctRefs(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(agent, "blog", "pr-7", "pull_request", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
}

func TestDrainEventsEmptiesTheSlot(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DrainEvents(agent); err != nil {
		t.Fatal(err)
	}
	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("drain was not destructive: %+v", got)
	}
}

func TestParkEventCapsPerAgent(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	for i := 0; i < maxPendingPerAgent+10; i++ {
		ref := "pr-" + itoa(i)
		if err := st.ParkEvent(agent, "blog", ref, "pull_request", []byte(`{}`)); err != nil {
			t.Fatalf("ParkEvent %d: %v", i, err)
		}
	}
	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > maxPendingPerAgent {
		t.Fatalf("parked %d events, cap is %d", len(got), maxPendingPerAgent)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestParkEvent|TestDrainEvents' -v`
Expected: FAIL — `undefined: st.ParkEvent`

- [ ] **Step 3: Add the table**

Append to `internal/relay/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS pending_events (
    agent_name TEXT NOT NULL REFERENCES agents(name),
    app        TEXT NOT NULL,
    ref        TEXT NOT NULL,
    event      TEXT NOT NULL,
    payload    BLOB NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (agent_name, app, ref)
);
```

- [ ] **Step 4: Implement park and drain**

Append to `internal/relay/delivery.go`:

```go
// maxPendingPerAgent bounds the parked-event table for one box. A PR-heavy repo
// creates one slot per open PR, so the cap is what stops an offline box from
// growing the table without limit. Oldest slots are evicted first.
const maxPendingPerAgent = 50

// PendingEvent is a webhook parked for a box that was offline when it arrived.
type PendingEvent struct {
	AgentName string
	App       string
	Ref       string
	Event     string
	Payload   []byte
}

// ParkEvent stores an undelivered event, coalescing by (agent, app, ref): a
// newer event for the same ref replaces the older one. Deploys are
// last-write-wins, so replaying intermediate commits on reconnect would be
// actively wrong — a box that was off overnight should deploy the tip, once.
// pendingTimeLayout is fixed-width, unlike RFC3339Nano (which trims trailing
// fractional zeros, so "…:05Z" sorts lexicographically *after* "…:05.4Z").
// The eviction and drain ordering below compare created_at as strings and
// depend on lexicographic == chronological.
const pendingTimeLayout = "2006-01-02T15:04:05.000000000Z"

func (s *Store) ParkEvent(agentName, app, ref, event string, payload []byte) error {
	now := time.Now().UTC().Format(pendingTimeLayout)
	if _, err := s.db.Exec(
		`INSERT INTO pending_events(agent_name, app, ref, event, payload, created_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(agent_name, app, ref) DO UPDATE SET
		     event = excluded.event, payload = excluded.payload, created_at = excluded.created_at`,
		agentName, app, ref, event, payload, now); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`DELETE FROM pending_events
		  WHERE agent_name = ?
		    AND rowid NOT IN (
		        SELECT rowid FROM pending_events WHERE agent_name = ?
		         ORDER BY created_at DESC LIMIT ?)`,
		agentName, agentName, maxPendingPerAgent)
	return err
}

// DrainEvents returns and removes every parked event for agentName. Read and
// delete share one immediate transaction (see Open's _txlock): a concurrent
// ParkEvent either commits before it and is returned, or blocks until after
// the delete and survives for the next drain — the blanket DELETE can never
// destroy a row this call did not return.
func (s *Store) DrainEvents(agentName string) ([]PendingEvent, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`SELECT app, ref, event, payload FROM pending_events
		  WHERE agent_name=? ORDER BY created_at`, agentName)
	if err != nil {
		return nil, err
	}
	var out []PendingEvent
	for rows.Next() {
		ev := PendingEvent{AgentName: agentName}
		if err := rows.Scan(&ev.App, &ev.Ref, &ev.Event, &ev.Payload); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM pending_events WHERE agent_name=?`, agentName); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// DrainFor replays every parked event for agentName. Called on reconnect, and
// after a park to close the race where the box came back between a delivery
// failing and the park landing. It bails while the agent is offline — the
// destructive drain must not run when nothing can be delivered — and a replay
// that fails is re-parked, never dropped: GitHub already got its 202, so a
// lost event here would never be retried by anyone.
func (t *TunnelDelivery) DrainFor(ctx context.Context, agentName string) {
	if _, ok := t.router.Lookup(agentName); !ok {
		return // events stay parked for the reconnect drain
	}
	events, err := t.st.DrainEvents(agentName)
	if err != nil {
		log.Printf("relay: drain pending for %s: %v", agentName, err)
		return
	}
	for _, ev := range events {
		b := Binding{AgentName: ev.AgentName, App: ev.App}
		if err := t.Deliver(ctx, b, ev.Event, ev.Payload); err != nil {
			log.Printf("relay: replay %s to %s/%s: %v (re-parking)", ev.Event, ev.AgentName, ev.App, err)
			if perr := t.st.ParkEvent(ev.AgentName, ev.App, ev.Ref, ev.Event, ev.Payload); perr != nil {
				log.Printf("relay: re-park %s for %s/%s: %v", ev.Event, ev.AgentName, ev.App, perr)
			}
		}
	}
}
```

Add `"log"` to the file's imports.

- [ ] **Step 5: Park on delivery failure**

In `internal/relay/ingress.go`, replace the delivery goroutine body so an offline box parks rather than losing the event. The ref key is the branch for pushes and `pr-<N>` otherwise; extend `ghEnvelope` with the two fields needed to compute it:

```go
type ghEnvelope struct {
	Action     string `json:"action"`
	Ref        string `json:"ref"`
	Number     int    `json:"number"`
	// ... existing fields
}
```

and:

```go
		for _, b := range bindings {
			go func(b Binding) {
				err := d.Deliver(context.Background(), b, event, body)
				if err == nil {
					return
				}
				// Park on ANY failure, not just offline: GitHub already got its
				// 202, so an event dropped here would never be retried by
				// anyone. Parking is coalescing and idempotent, so this is
				// safe for transient box-side errors too.
				if !errors.Is(err, ErrAgentOffline) {
					log.Printf("relay: deliver %s to %s/%s: %v (parking)", event, b.AgentName, b.App, err)
				}
				ref := strings.TrimPrefix(env.Ref, "refs/heads/")
				if env.Number > 0 {
					ref = "pr-" + strconv.Itoa(env.Number)
				}
				if err := st.ParkEvent(b.AgentName, b.App, ref, event, body); err != nil {
					log.Printf("relay: park %s for %s/%s: %v", event, b.AgentName, b.App, err)
					return
				}
				// Close the park/drain race: the box may have reconnected while
				// the delivery was failing, in which case its reconnect drain
				// already ran and missed this event. DrainFor no-ops while the
				// agent is still offline.
				d.DrainFor(context.Background(), b.AgentName)
			}(b)
		}
```

Add `"errors"` and `"strings"` to that file's imports. For the re-drain to be reachable from here, widen `Deliverer` (Task 6) with `DrainFor(ctx context.Context, agentName string)`: add a no-op `DrainFor` to `capturingDeliverer` in `ingress_test.go`, and delete the `DeliverFunc` adapter if Task 7 left it behind — a bare func no longer satisfies the widened interface.

- [ ] **Step 6: Drain on reconnect**

In `internal/relay/server.go`, immediately after the accepted session is registered with the router (`router.Register(sess)`), add:

```go
		if delivery != nil {
			go delivery.DrainFor(context.Background(), sess.BaseDomain)
		}
```

The registration happens in `serveTunnel` (`internal/relay/server.go:145`). As with `ghApp` in Task 5 there is no server struct — thread a `delivery *TunnelDelivery` parameter through `Serve` → `serveTunnel`, passing the one `cmd/piper-relay/main.go` builds in Task 7 and `nil` from any caller without an App; the drain is skipped when nil. (`startTestRelay`'s inline accept loop needs no drain wiring — the delivery tests call `DrainFor` directly.)

- [ ] **Step 7: Prove the replay carries the newer commit**

The coalescing test asserts the store keeps one row; this asserts the box actually
receives exactly one delivery with the *newer* SHA. Append to `internal/relay/delivery_test.go`:

```go
func TestDrainForReplaysOnlyTheNewestPerRef(t *testing.T) {
	sess, _, _, base, st, router := startTestRelay(t, nil, nil)

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"new"}`)); err != nil {
		t.Fatal(err)
	}

	bodies := make(chan string, 4)
	go func() {
		for {
			kind, conn, err := sess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindHTTP {
				conn.Close()
				continue
			}
			req, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				conn.Close()
				return
			}
			body, _ := io.ReadAll(req.Body)
			bodies <- string(body)
			_, _ = io.WriteString(conn, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
			conn.Close()
		}
	}()

	NewTunnelDelivery(st, router).DrainFor(context.Background(), base)

	select {
	case got := <-bodies:
		if got != `{"after":"new"}` {
			t.Fatalf("replayed %s, want the newer commit", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no replay arrived")
	}
	select {
	case extra := <-bodies:
		t.Fatalf("a second replay arrived: %s", extra)
	case <-time.After(300 * time.Millisecond):
	}

	left, err := st.DrainEvents(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d events still parked after drain", len(left))
	}
}
```

Run: `go test ./internal/relay/ -run TestDrainForReplays -v -race`
Expected: PASS

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v -race`
Expected: PASS

- [ ] **Step 9: Verify and commit**

```bash
make verify
git add internal/relay/schema.sql internal/relay/delivery.go internal/relay/delivery_test.go internal/relay/ingress.go \
        internal/relay/pending_test.go internal/relay/server.go cmd/piper-relay/main.go
git commit -m "$(cat <<'EOF'
feat(relay): park webhooks for offline boxes and drain on reconnect

One coalescing slot per (agent, app, ref) rather than a queue: a box that was
off overnight deploys the tip commit once instead of replaying every push it
missed. Capped per agent so a PR-heavy repo cannot grow the table unbounded.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: Enroll advertises brokered mode

**Files:**
- Modify: `internal/relay/api.go:231-273` (the `enroll` handler)
- Modify: `internal/relay/api_test.go`
- Modify: `internal/relayclient/relayclient.go` (the `Enrollment` struct that decodes the enroll response lives here, lines 34-38 — not in `relayonboard.go`)
- Modify: `internal/config/config.go` (`RelayFile`, `Config`, `Load`)
- Modify: `cmd/piper/relayonboard.go` (persist the new fields — **both** install flavors)
- Modify: `cmd/piper/relayonboard_test.go`

**Interfaces:**
- Consumes: `Enrollment.WebhookSecret` (Task 7).
- Produces:
  - enroll response gains `"webhook_secret"` and `"github_app": true|false`
  - `config.RelayFile` gains `WebhookSecret string \`json:"webhook_secret,omitempty"\`` and `GitHubBrokered bool \`json:"github_brokered,omitempty"\``
  - `config.Config` gains the same two fields, populated env-first in `Load` (`PIPER_WEBHOOK_SECRET`, `PIPER_GITHUB_BROKERED=1`) with `relay.json` fallback — the shipped systemd install carries enrollment *only* via env (`relayonboard.go:135-144` never writes `relay.json` there), so without the env path brokered mode is dead on that flavor
  - `relayclient.Enrollment` gains `WebhookSecret string \`json:"webhook_secret"\`` and `GitHubApp bool \`json:"github_app"\``

- [ ] **Step 1: Write the failing test**

Add to `internal/relay/api_test.go`, cribbing `TestEnrollWithAccountCredential` (line 79):

```go
func TestEnrollReturnsWebhookSecretAndAppFlag(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay.getpiper.co:7000", nil, nil, app)

	acc, _ := st.UpsertAccount("sub-1", "judy")
	cred, _ := st.MintAccountCredential(acc.ID)

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		WebhookSecret string `json:"webhook_secret"`
		GitHubApp     bool   `json:"github_app"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.WebhookSecret == "" {
		t.Fatal("enroll returned no webhook_secret")
	}
	if !out.GitHubApp {
		t.Fatal("github_app flag not advertised despite a configured App")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestEnrollReturns -v`
Expected: FAIL — `enroll returned no webhook_secret`

- [ ] **Step 3: Advertise in the enroll response**

The `api` struct needs to know whether an App is configured. Add a field and a constructor parameter:

```go
type api struct {
	// ... existing fields
	ghApp *GitHubApp // nil ⇒ relay serves BYO users only
}
```

Add a `ghApp *GitHubApp` parameter to `NewAPIWithTunnel` (currently five parameters, `api.go:24`), pass Task 4's `ghApp` from `cmd/piper-relay/main.go:189`, `nil` from `NewAPI` (`api.go:18`), and a trailing `nil` at every existing `NewAPIWithTunnel` call in `api_test.go`.

Then extend the response at `internal/relay/api.go:268`:

```go
	writeJSON(w, http.StatusOK, map[string]any{
		"enrollment_token": en.Token,
		"base_domain":      en.BaseDomain,
		"tunnel_endpoint":  a.tunnelEndpoint,
		"webhook_secret":   en.WebhookSecret,
		"github_app":       a.ghApp != nil,
	})
```

- [ ] **Step 4: Persist on the box — both install flavors**

In `internal/config/config.go`, extend `RelayFile`:

```go
// RelayFile is the persisted relay enrollment written by `piper connect` and
// read by piperd at startup. Environment variables override these values.
type RelayFile struct {
	RelayAddr  string `json:"relay_addr"`
	RelayToken string `json:"relay_token"`
	BaseDomain string `json:"base_domain"`
	Terminated bool   `json:"terminated,omitempty"`
	// WebhookSecret is the HMAC key the relay signs brokered GitHub deliveries
	// with; GitHubBrokered records that the relay holds an App, so this box
	// needs no App credentials of its own.
	WebhookSecret  string `json:"webhook_secret,omitempty"`
	GitHubBrokered bool   `json:"github_brokered,omitempty"`
}
```

add the same two fields to `Config`, and populate them in `Load` env-first, matching the existing relay lines at `config.go:58-60`:

```go
		WebhookSecret:  firstNonEmpty(os.Getenv("PIPER_WEBHOOK_SECRET"), rf.WebhookSecret),
		GitHubBrokered: os.Getenv("PIPER_GITHUB_BROKERED") == "1" || rf.GitHubBrokered,
```

The env path is not optional: the shipped systemd install carries enrollment *only* in `/etc/piper/piperd.env` — `connect` writes no `relay.json` on that flavor.

In `internal/relayclient/relayclient.go`, extend `Enrollment` (lines 34-38) with `WebhookSecret string \`json:"webhook_secret"\`` and `GitHubApp bool \`json:"github_app"\``.

In `cmd/piper/relayonboard.go`, carry both through each `connect` branch:

- non-systemd: add `WebhookSecret: en.WebhookSecret, GitHubBrokered: en.GitHubApp` to the `config.RelayFile` literal at line 147;
- systemd: extend the printed env upsert (lines 139-142) — add `PIPER_WEBHOOK_SECRET|PIPER_GITHUB_BROKERED` to the `sed` delete pattern, and `echo PIPER_WEBHOOK_SECRET=%s; echo PIPER_GITHUB_BROKERED=%d;` (1 when `en.GitHubApp`) to the append list.

- [ ] **Step 5: Update the CLI test fixtures**

In `cmd/piper/relayonboard_test.go`, add `"webhook_secret": "whsec-1", "github_app": true` to each stubbed enroll response body, extend the `want := config.RelayFile{...}` literal in `TestConnectEnrollsAndWritesRelayFile` (line 125) with both values, and extend `TestConnectSystemManagedGuidesEnvInstall` (line 279) to assert the printed command contains `PIPER_WEBHOOK_SECRET=whsec-1` and `PIPER_GITHUB_BROKERED=1`.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/relay/ ./internal/relayclient/ ./internal/config/ ./cmd/piper/ -v`
Expected: PASS

- [ ] **Step 7: Verify and commit**

```bash
make verify
git add internal/relay/api.go internal/relay/api_test.go internal/relayclient/relayclient.go \
        internal/config/config.go cmd/piper/relayonboard.go cmd/piper/relayonboard_test.go cmd/piper-relay/main.go
git commit -m "$(cat <<'EOF'
feat(relay): enroll mints a per-agent webhook secret and advertises the App

A box learns at enroll whether its relay brokers GitHub, and gets the secret
brokered deliveries are signed with. Relays without an App configured advertise
false and their boxes stay on the BYO path.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: piperd runs a brokered provider

**Files:**
- Create: `internal/source/github/relaytokens.go`
- Create: `internal/source/github/relaytokens_test.go`
- Modify: `cmd/piperd/main.go:580-625` (`webhookStarter`)

**Interfaces:**
- Consumes: `TokenSource`, `NewWithTokens` (Task 1); `TunnelClient.GitHubToken` (Task 5); `config.Config.WebhookSecret`/`GitHubBrokered` (Task 9 — already merged env-first in `Load`).
- Produces:
  - `type RelayTokens struct { Ask func(repo string) (string, error) }`
  - `func (r RelayTokens) Token(ctx context.Context, ev source.Event) (string, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/source/github/relaytokens_test.go`:

```go
package github

import (
	"context"
	"errors"
	"testing"

	"github.com/piperbox/piper/internal/source"
)

func TestRelayTokensAsksForTheEventRepo(t *testing.T) {
	var asked string
	rt := RelayTokens{Ask: func(repo string) (string, error) {
		asked = repo
		return "ghs_from_relay", nil
	}}
	tok, err := rt.Token(context.Background(), source.Event{Repo: "alice/blog"})
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_from_relay" {
		t.Fatalf("token = %q", tok)
	}
	if asked != "alice/blog" {
		t.Fatalf("asked for %q, want alice/blog", asked)
	}
}

func TestRelayTokensSurfacesRelayError(t *testing.T) {
	want := errors.New("relay says no")
	rt := RelayTokens{Ask: func(string) (string, error) { return "", want }}
	if _, err := rt.Token(context.Background(), source.Event{Repo: "alice/blog"}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/github/ -run TestRelayTokens -v`
Expected: FAIL — `undefined: RelayTokens`

- [ ] **Step 3: Implement the brokered token source**

Create `internal/source/github/relaytokens.go`:

```go
package github

import (
	"context"

	"github.com/piperbox/piper/internal/source"
)

// RelayTokens is the brokered TokenSource: the box holds no GitHub App key, so
// every token comes from the relay, already scoped to the one repository.
// Ask is the tunnel client's gh-token control op.
type RelayTokens struct {
	Ask func(repo string) (string, error)
}

func (r RelayTokens) Token(_ context.Context, ev source.Event) (string, error) {
	return r.Ask(ev.Repo)
}
```

- [ ] **Step 4: Wire brokered mode into the webhook starter**

In `cmd/piperd/main.go`, `webhookStarter` currently refuses to start without a stored App row. Give it the brokered path. Add fields and change `run`:

```go
type webhookStarter struct {
	cfg     config.Config
	st      *store.Store
	rt      *runtime.DockerRuntime
	ghToken func(repo string) (string, error) // nil unless brokered
	once    sync.Once
	srv     *http.Server
	handler *webhook.Handler
}

func (w *webhookStarter) run() {
	var prov source.Provider

	// A locally stored App is an explicit BYO override and wins over the
	// relay's offer, so a box that ran `piper github setup` keeps its own
	// credentials and its own trust boundary.
	if gh, err := w.st.GetGitHubApp(); err == nil {
		p, err := github.New(github.Config{
			AppID: gh.AppID, PrivateKeyPEM: gh.PrivateKey, WebhookSecret: gh.WebhookSecret,
		})
		if err != nil {
			log.Printf("webhook: github provider: %v", err)
			return
		}
		prov = p
		log.Printf("webhook: using this box's own GitHub App %d", gh.AppID)
	} else if w.cfg.GitHubBrokered && w.cfg.WebhookSecret != "" && w.ghToken != nil {
		prov = github.NewWithTokens(
			github.Config{WebhookSecret: w.cfg.WebhookSecret},
			github.RelayTokens{Ask: w.ghToken},
		)
		log.Printf("webhook: using the relay's GitHub App (brokered)")
	} else {
		log.Printf("webhook: no GitHub App configured")
		return
	}

	wdep := deploy.New(w.st, w.rt, caddy.NewClient(w.cfg.CaddyAdmin), w.cfg.BaseDomain)
	w.handler = webhook.New(prov, w.st, wdep, w.cfg.BaseDomain)
	w.srv = &http.Server{Addr: w.cfg.WebhookAddr, Handler: w.handler}
	go func() {
		if err := w.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("webhook serve: %v", err)
		}
	}()
	_, portStr, _ := net.SplitHostPort(w.cfg.WebhookAddr)
	port, _ := strconv.Atoi(portStr)
	if err := caddy.NewClient(w.cfg.CaddyAdmin).UpsertRoute("hooks."+w.cfg.BaseDomain, port); err != nil {
		log.Printf("webhook route: %v", err)
	}
	log.Printf("webhook listening on %s", w.cfg.WebhookAddr)
}
```

Update `newWebhookStarter` to take just the token func — `cfg` already carries the brokered flag and secret after Task 9 — and rework the boot gate at `cmd/piperd/main.go:472-477` so brokered mode starts without a `github_app` row:

```go
		wh = newWebhookStarter(cfg, st, rt, tc.GitHubToken)
		if _, err := st.GetGitHubApp(); err == nil {
			wh.start()
		} else if cfg.GitHubBrokered {
			wh.start()
		} else {
			log.Printf("no GitHub App configured; run `piper github setup` to enable git deploys")
		}
```

Add `"github.com/piperbox/piper/internal/source"` to the file's imports.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/source/github/ ./cmd/piperd/ -v`
Expected: PASS

- [ ] **Step 6: Verify and commit**

```bash
make verify
git add internal/source/github/relaytokens.go internal/source/github/relaytokens_test.go cmd/piperd/main.go
git commit -m "$(cat <<'EOF'
feat(agent): run a brokered GitHub provider when the relay holds the App

A brokered box starts its webhook listener with no local credentials: the
webhook secret comes from enroll and every installation token from the relay's
gh-token op. A locally stored App remains an explicit override and wins.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: `piper app link` registers the binding

**Files:**
- Modify: `internal/api/api.go:208` (the link handler)
- Modify: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `TunnelClient.BindRepo`/`UnbindRepo` (Task 5).
- Produces: no new exported names; the link handler gains an optional `binder` dependency.

- [ ] **Step 1: Write the failing test**

Add to `internal/api/api_test.go`:

```go
type fakeBinder struct {
	app, repo, branch string
	calls             int
}

func (f *fakeBinder) BindRepo(app, repo, branch string) error {
	f.app, f.repo, f.branch = app, repo, branch
	f.calls++
	return nil
}

func TestLinkRegistersBindingWithRelay(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	fb := &fakeBinder{}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, fb)
	body := strings.NewReader(`{"repo":"alice/blog","branch":"main"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/link", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rec.Code)
	}
	if fb.calls != 1 || fb.app != "blog" || fb.repo != "alice/blog" || fb.branch != "main" {
		t.Fatalf("binder got %+v", fb)
	}
}

func TestLinkSucceedsWithoutABinder(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil)
	body := strings.NewReader(`{"repo":"alice/blog","branch":"main"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/link", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rec.Code)
	}
	got, _ := s.AppByRepo("alice/blog")
	if got.Name != "blog" || got.Branch != "main" {
		t.Fatalf("link not persisted: %+v", got)
	}
}
```

(Crib `TestLinkApp` at `internal/api/api_test.go:324`; these are that test with a seventh `New` argument.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestLinkRegisters -v`
Expected: FAIL — the API has no binder parameter

- [ ] **Step 3: Add the binder seam**

In `internal/api/api.go`, define the interface next to the other dependency interfaces:

```go
// RepoBinder tells the relay which repository an app deploys from, so brokered
// webhooks can be routed to this box. Nil on LAN-only boxes.
type RepoBinder interface {
	BindRepo(app, repo, branch string) error
}
```

The package injects dependencies through `New` (`api.go:64`, currently six parameters) — add `binder RepoBinder` as a seventh, passing `nil` at every existing call site. In the link handler (`api.go:208`), after the app row is updated successfully:

```go
	if s.binder != nil {
		if err := s.binder.BindRepo(name, req.Repo, req.Branch); err != nil {
			// The local binding is authoritative and already stored; a relay
			// that is briefly unreachable must not fail the link. The binding
			// is re-pushed when the tunnel reconnects.
			log.Printf("api: register binding for %s with relay: %v", name, err)
		}
	}
```

In `cmd/piperd/main.go`, the API handler is built (line 405) *before* the tunnel client exists (line 451) — hoist the client: declare `var binder api.RepoBinder`, and when `cfg.RelayAddr != ""` create `tc := &agent.TunnelClient{}` ahead of `api.New` and assign `binder = tc` (creating it early is harmless; `Run` still starts later in the relay block). Do not pass a possibly-nil `*agent.TunnelClient` directly — a typed-nil interface would defeat the `s.binder != nil` guard.

- [ ] **Step 4: Re-push bindings on reconnect**

There is no agent-side hostname re-registration on reconnect (the relay restores routing from its own store when a session registers), so the re-push needs its own home: `tc.OnConnect` (`cmd/piperd/main.go:455`), which already runs on every (re)connect. Append to that callback:

```go
	apps, err := st.ListApps()
	if err == nil {
		for _, a := range apps {
			if a.Repo == "" {
				continue
			}
			if err := tc.BindRepo(a.Name, a.Repo, a.Branch); err != nil {
				log.Printf("relay: re-bind %s: %v", a.Name, err)
			}
		}
	}
```

Confirmed: `ListApps` (`internal/store/store.go:150`) and the `App.Repo`/`App.Branch` fields (`store.go:27-28`) exist as named.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/api/ ./cmd/piperd/ -v`
Expected: PASS

- [ ] **Step 6: Verify and commit**

```bash
make verify
git add internal/api/api.go internal/api/api_test.go cmd/piperd/main.go
git commit -m "$(cat <<'EOF'
feat(api): push repo bindings to the relay on link and reconnect

The relay routes brokered webhooks by binding, so linking an app now tells it
which repo that app deploys. A briefly unreachable relay does not fail the
link: the local row is authoritative and bindings are re-pushed on reconnect.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 12: Login through the App, install in the same trip

**Files:**
- Modify: `internal/relay/verifier_github.go`
- Modify: `internal/relay/verifier_github_test.go`
- Modify: `internal/relay/api.go` (install redirect handling in `loginCallback`)
- Modify: `internal/relay/api_test.go`
- Modify: `cmd/piper-relay/main.go`

**Interfaces:**
- Consumes: `Store.LinkInstallation` (Task 2); `GitHubApp` (Task 4).
- Produces:
  - `GitHubVerifier` gains an `AppSlug string` field; `AuthCodeURL` returns the App's install-and-authorize URL when it is set.
  - `GitHubAppConfig` gains `Slug string`; `func (g *GitHubApp) InstallURL() string` returns `https://github.com/apps/<slug>/installations/new` (empty when no slug).
  - the login-poll success response gains `"install_url"` when an App with a slug is configured — Task 13's `waitForInstall` reads it there, because the poll response is the only relay reply the CLI has seen before enrolling.

- [ ] **Step 1: Write the failing test**

Add to `internal/relay/verifier_github_test.go`:

```go
func TestAuthCodeURLUsesAppInstallWhenSlugSet(t *testing.T) {
	v := NewGitHubVerifier("client-id", "client-secret") // match the real constructor
	v.AppSlug = "piper-dev"

	got := v.AuthCodeURL("st4te")
	if !strings.Contains(got, "/apps/piper-dev/installations/new") {
		t.Fatalf("AuthCodeURL = %q, want the App install URL", got)
	}
	if !strings.Contains(got, "state=st4te") {
		t.Fatalf("AuthCodeURL = %q, missing state", got)
	}
}

func TestAuthCodeURLFallsBackToPlainOAuth(t *testing.T) {
	v := NewGitHubVerifier("client-id", "client-secret")
	got := v.AuthCodeURL("st4te")
	if strings.Contains(got, "/installations/new") {
		t.Fatalf("AuthCodeURL = %q, want the plain OAuth authorize URL", got)
	}
}
```

Add to `internal/relay/api_test.go` a test that a callback carrying `installation_id` links the installation, cribbing `TestWebLoginCallbackHappyPath` (line 191) but keeping its own `st` in scope:

```go
func TestLoginCallbackLinksInstallation(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	api := NewAPIWithTunnel(st, fv, "", nil, []string{"https://dash.getpiper.co/"}, nil)

	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")
	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})
	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state)+
			"&installation_id=55&setup_action=install", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body = %s", rr.Code, rr.Body.String())
	}

	acc, err := st.UpsertAccount("583231", "ivan") // idempotent: fetches the row the callback created
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.InstallationForAccount(acc.ID)
	if err != nil {
		t.Fatalf("InstallationForAccount: %v", err)
	}
	if inst != "55" {
		t.Fatalf("installation = %q, want 55", inst)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestAuthCodeURL|TestLoginCallbackLinks' -v`
Expected: FAIL — `v.AppSlug undefined`

- [ ] **Step 3: Point the browser at install-and-authorize**

In `internal/relay/verifier_github.go`, add the field and branch in `AuthCodeURL`:

```go
	// AppSlug is the GitHub App's URL slug. When set, the browser flow uses the
	// App's install page instead of the plain OAuth authorize endpoint: with
	// "Request user authorization during installation" enabled, one screen both
	// authorizes the user and selects which repositories the App may reach.
	AppSlug string
```

```go
func (v *GitHubVerifier) AuthCodeURL(state string) string {
	if v.AppSlug != "" {
		return "https://github.com/apps/" + url.PathEscape(v.AppSlug) +
			"/installations/new?state=" + url.QueryEscape(state)
	}
	// ... existing plain-OAuth URL construction, unchanged
}
```

- [ ] **Step 4: Link the installation in the callback**

In `internal/relay/api.go`'s `loginCallback`, after `UpsertAccount` succeeds (line 154) and before minting the credential, add:

```go
	// GitHub's install-and-authorize redirect carries the installation id
	// alongside the code, so one browser trip yields both identity and
	// installation. The installation webhook may also arrive first or later;
	// LinkInstallation is idempotent either way.
	if instID := r.URL.Query().Get("installation_id"); instID != "" {
		if err := a.st.LinkInstallation(instID, id.Subject, "user", id.Login); err != nil {
			log.Printf("relay: link installation %s for %s: %v", instID, acc.Username, err)
		}
	}
```

- [ ] **Step 5: Configure the slug**

In `cmd/piper-relay/main.go`, read `PIPER_RELAY_GITHUB_APP_SLUG` and set it on the verifier (the variable is `v`, `main.go:165-167`) and on the App config in the Task 4 block:

```go
	if gv, ok := v.(*relay.GitHubVerifier); ok {
		gv.AppSlug = os.Getenv("PIPER_RELAY_GITHUB_APP_SLUG")
	}
```

Add `Slug` to `GitHubAppConfig`, store it on `GitHubApp`, and expose the install page:

```go
// InstallURL is the App's install-and-authorize page. Empty when the operator
// configured no slug; the CLI then prints no install link.
func (g *GitHubApp) InstallURL() string {
	if g.slug == "" {
		return ""
	}
	return "https://github.com/apps/" + url.PathEscape(g.slug) + "/installations/new"
}
```

Then extend the login-poll success response (`internal/relay/api.go:225`) with `"install_url": a.ghApp.InstallURL()` when `a.ghApp != nil` — this is where Task 13's `waitForInstall` gets the URL, since the poll response is the only relay reply the CLI sees before enrolling.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/relay/ -v`
Expected: PASS

- [ ] **Step 7: Verify and commit**

```bash
make verify
git add internal/relay/verifier_github.go internal/relay/verifier_github_test.go \
        internal/relay/api.go internal/relay/api_test.go cmd/piper-relay/main.go
git commit -m "$(cat <<'EOF'
feat(relay): log in through the App's install-and-authorize screen

One browser trip now covers identity and repository selection: the callback
carries installation_id next to the code, so the installation is linked without
waiting for the webhook. Relays without an App slug keep the plain OAuth flow.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 13: Repo listing, docs, and the e2e runbook

**Files:**
- Modify: `internal/relay/api.go` (`GET /v1/github/repos`)
- Modify: `internal/relay/api_test.go`
- Modify: `internal/relayclient/relayclient.go` (client call — the CLI's relay-API calls live here, *not* in `internal/client`, which is the piperd loopback client)
- Modify: `cmd/piper/main.go` and `cmd/piper/relayonboard.go` (`piper github repos`, install polling)
- Modify: `docs/runbooks/git-deploy-e2e.md`
- Modify: `docs/getting-started.md`
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: `GitHubApp.Repos` (Task 4), `Store.InstallationForAccount` (Task 2), `api.authAccount` (`internal/relay/api.go:277`).
- Produces:
  - `GET /v1/github/repos` → `{"repos":["owner/name", ...]}`
  - `var relayclient.ErrNoInstallation = errors.New("github app not installed for this account")`
  - `func (c *relayclient.Client) GitHubRepos(ctx context.Context, accountCredential string) ([]string, error)` — the relay's 404 maps to `ErrNoInstallation`

- [ ] **Step 1: Write the failing test**

Add to `internal/relay/api_test.go`:

```go
// ghAPIStub serves the two GitHub endpoints repo listing touches.
func ghAPIStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/55/access_tokens":
			_, _ = w.Write([]byte(`{"token":"t","expires_at":"2026-07-20T12:00:00Z"}`))
		case "/installation/repositories":
			_, _ = w.Write([]byte(`{"repositories":[{"full_name":"alice/blog"},{"full_name":"alice/api"}]}`))
		default:
			t.Errorf("unexpected GitHub path %q", r.URL.Path)
		}
	}))
}

// reposAPI builds the account API with a GitHub App pointed at gh.
func reposAPI(t *testing.T, st *Store, gh *httptest.Server) http.Handler {
	t.Helper()
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewAPIWithTunnel(st, NewFakeVerifier(), "", nil, nil, app)
}

func getRepos(t *testing.T, h http.Handler, cred string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/github/repos", nil)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGitHubReposListsInstallationRepos(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()

	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}

	rec := getRepos(t, reposAPI(t, st, gh), cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body struct {
		Repos []string `json:"repos"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Repos) != 2 || body.Repos[0] != "alice/blog" || body.Repos[1] != "alice/api" {
		t.Fatalf("repos = %v", body.Repos)
	}
}

func TestGitHubReposRequiresCredential(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()
	rec := getRepos(t, reposAPI(t, openTestStore(t), gh), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGitHubReposWithoutInstallation(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()

	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}

	rec := getRepos(t, reposAPI(t, st, gh), cred)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
```

`NewAPIWithTunnel`'s last parameter is the `*GitHubApp` added in Task 9. Add
`"encoding/json"`, `"net/http"` and `"net/http/httptest"` to the file's imports if absent.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run TestGitHubRepos -v`
Expected: FAIL — 404 from an unregistered route

- [ ] **Step 3: Add the endpoint**

Register in `NewAPIWithTunnel` next to the other routes:

```go
	mux.HandleFunc("GET /v1/github/repos", a.githubRepos)
```

and implement it in `internal/relay/api.go`:

```go
// githubRepos lists the repositories the caller's installation can reach. No
// list is cached: it is read live through a fresh installation token, so a
// repository revoked in GitHub disappears here immediately.
func (a *api) githubRepos(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	if a.ghApp == nil {
		http.Error(w, "relay has no github app configured", http.StatusServiceUnavailable)
		return
	}
	instID, err := a.st.InstallationForAccount(acc.ID)
	if errors.Is(err, ErrNoInstallation) {
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

- [ ] **Step 4: Add the CLI command**

The CLI's relay-API calls live in `internal/relayclient` — not `internal/client`, whose `Manifest` (`client.go:340`) is a *piperd* call. Add `GitHubRepos(ctx, accountCredential)` to `relayclient` following the shape of `Enroll` (`relayclient.go:121`), sending the credential as `Bearer` and mapping the relay's 404 to a new `ErrNoInstallation` sentinel, so the poll loop below retries only on "not installed yet".

In `cmd/piper/main.go`, register `piper github repos` alongside `cmdGithub` at line 459, printing one `owner/name` per line, and a one-line hint when the relay answers 404:

```
No repositories yet — run `piper login` to install the Piper GitHub App on the repos you want to deploy.
```

- [ ] **Step 5: Close the headless install loop**

Device flow can authorize but cannot install, so nothing links the installation until
the user opens the install page (another tab on desktop, another device for a headless
box). Give `piper login` that ending. In `cmd/piper/relayonboard.go`, print the
install URL after the credential is saved and poll until the installation appears:

```go
// waitForInstall polls the relay until the account's GitHub App installation is
// on record. Device flow cannot install, so this is how `piper login` learns
// the user finished the install page — in another tab, or on another device
// for a headless box. A one-trip browser login for the CLI is follow-up #291.
func waitForInstall(rc *relayclient.Client, cred, installURL string) error {
	fmt.Printf("Install the Piper GitHub App on the repos you want to deploy:\n  %s\n\nWaiting…", installURL)
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		repos, err := rc.GitHubRepos(context.Background(), cred)
		if err == nil {
			fmt.Printf("\rInstalled — %d repo(s) available.\n", len(repos))
			return nil
		}
		if !errors.Is(err, relayclient.ErrNoInstallation) {
			return err
		}
		fmt.Print(".")
		pollSleep(3 * time.Second)
	}
	return errors.New("timed out waiting for the GitHub App install")
}
```

The install URL comes from the login-poll response's `install_url` (Task 12) — the
poll response is the only relay reply the CLI has seen at that point (enroll happens
later, on the box). Extend `relayclient.Account` with
`InstallURL string \`json:"install_url"\``; in `relayLogin`
(`cmd/piper/relayonboard.go:26`), after the credential is saved, call
`waitForInstall(rc, acc.AccountCredential, acc.InstallURL)` when
`acc.InstallURL != ""`. A relay without an App (or without a slug) sends no URL and
login ends exactly as today.

Test it in `cmd/piper/relayonboard_test.go` with an `httptest` stub relay (crib
`TestRelayLoginStoresCredential`, line 28) whose `/v1/github/repos` answers 404 twice
and then 200 with two repos, asserting `waitForInstall` returns nil after three polls
— the file's `pollSleep` seam (`relayonboard.go:21`) keeps the test from really
sleeping.

- [ ] **Step 6: Update the docs**

`docs/runbooks/git-deploy-e2e.md` — add a brokered-mode section. Its prerequisites differ from BYO in exactly three ways, and say so explicitly:

- no `hooks.<base>` DNS record and no publicly trusted certificate are required;
- no `piper github setup` step;
- the relay must run with `PIPER_RELAY_GITHUB_APP_ID`, `_APP_KEY`, `_WEBHOOK_SECRET`, `_APP_SLUG`, `_CLIENT_ID`, `_CLIENT_SECRET` set;
- the App's webhook URL is the relay's account-API host: `https://api.<apex>/gh` (Task 6 mounts `/gh` there).

Document the loopback variant using `NewAutoApproveVerifier` (`internal/relay/verifier.go:67`) exactly as the existing runbook does for login.

`docs/getting-started.md` — replace the BYO steps in the public-relay walkthrough with `piper login` → `piper create` → `piper app link` → `git push`, and keep the `piper github setup` instructions in a clearly marked "self-hosted relay / bring your own App" section.

`PROGRESS.md` — add one line under the git-deploy entries:

```markdown
- Relay-held GitHub App: one-trip login + install, brokered webhooks and tokens, BYO unchanged [#289]
```

- [ ] **Step 7: Run the full suite**

Run: `make verify`
Expected: gofmt clean, vet clean, all tests pass, arm64 cross-compile succeeds.

- [ ] **Step 8: Commit**

```bash
git add internal/relay/api.go internal/relay/api_test.go internal/relayclient/relayclient.go \
        cmd/piper/main.go cmd/piper/relayonboard.go cmd/piper/relayonboard_test.go docs/ PROGRESS.md
git commit -m "$(cat <<'EOF'
feat(cli): piper github repos, plus brokered-mode docs

Lists the repositories the account's installation can reach, read live through
a fresh installation token. Runbook and getting-started now separate the
brokered path from BYO, including the prerequisites brokered mode drops.

Part of #289

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

## Before opening the PR

- [x] Tracking issue opened: **[#289](https://github.com/piperbox/piper/issues/289)** — every task's commit trail already references it.
- [ ] `make verify` passes on the branch tip.
- [ ] Register the GitHub App itself under the `getpiper` org — no task does this, and tasks 4, 6, 7 and 12 cannot be exercised against real GitHub without it. Permissions `contents:read`, `deployments:write`, `pull_requests:read`; events `push`, `pull_request`, `installation`; webhook URL `https://api.<relay-apex>/gh` (the account-API host — Task 6 mounts `/gh` there); **"Request user authorization (OAuth) during installation" ON**; **"Enable Device Flow" ON** — device flow is the CLI's only login path, and GitHub Apps reject it unless this box is checked.
- [ ] Production rollout note: the `agents` schema changes in place, so deploying this relay to `public.getpiper.dev` means resetting `relay.db` (pre-1.x policy — no migrations). Every account re-runs `piper login`, every box re-runs `piper connect` (the pi4 included). The swap to the App's client id is otherwise invisible — accounts key on the stable `github_id`.
- [x] Org-install follow-up opened: **[#290](https://github.com/piperbox/piper/issues/290)** — stays open after this plan lands.
- [x] CLI one-trip browser-login follow-up opened: **[#291](https://github.com/piperbox/piper/issues/291)** — this plan ships device flow + install-page polling for `piper login`.
- [ ] Tick this plan's task checkboxes on #289 as each task merges.
- [ ] PR body carries `Closes #289` and links the spec.
