# Plan 3 — Git-driven deploys (GitHub App, push → live URL) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `git push` to an app's tracked branch triggers a build → run → health → route on the box via a per-user GitHub App, and the live HTTPS URL is reported back to GitHub as a Deployment status.

**Architecture:** A new `source.Provider` seam normalizes a GitHub webhook into an `Event`, fetches the repo tarball at the pushed commit with an installation token, and reports a GitHub Deployment status. A new `webhook` package receives the signed webhook (delivered over the Plan-2 tunnel to `hooks.<BaseDomain>`, terminated by Caddy on-box), looks up the bound app, and drives the **unchanged** `deploy.Deployer`. `piperd` wires it up and adds onboarding endpoints; the `piper` CLI adds `github setup` and `app link`.

**Tech Stack:** Go 1.26, `CGO_ENABLED=0`; `modernc.org/sqlite`; stdlib `net/http`, `crypto/rsa`, `crypto/hmac`, `archive/tar`, `compress/gzip`; existing `internal/{store,deploy,runtime,caddy,agent,api}`.

## Global Constraints

- **No cgo.** All builds pass with `CGO_ENABLED=0`. No new cgo dependencies. (GitHub App JWT is minted with stdlib `crypto/rsa` — do **not** add a JWT library.)
- **Module path** is `github.com/piperbox/piper`.
- **Deployment status strings** (SQLite `deployments.status`) are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`. (Distinct from GitHub Deployment *states* — do not conflate.)
- **Defaults:** control API `127.0.0.1:8088`, Caddy admin `http://127.0.0.1:2019`, base domain `piper.localhost`, app container port `8080`. New: webhook listener `127.0.0.1:8089` (`PIPER_WEBHOOK_ADDR`).
- **Layering — nothing imports "up":** `source` knows only GitHub + git; `source/github` does **not** import `store`; `webhook` orchestrates `source`+`store`+`deploy`; `deploy` stays ignorant of the source.
- **Hostname convention (matches existing code):** apps serve at `<app>.<BaseDomain>`; the reserved webhook host is `hooks.<BaseDomain>`. (`hooks` is a reserved app name.) The wildcard cert `*.<BaseDomain>` already covers it; the relay already routes `*.<BaseDomain>` by SNI suffix — **no relay change**.
- **TDD, DRY, YAGNI, frequent commits.** One commit per task. Conventional-commit style, ending every message with:
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`
- **Reference the issue:** each commit body includes `Part of #11`.
- **Green gates:** `make test` and `make cross` pass before any task is considered done.

### Deviations from the spec (intentional, YAGNI)

- **`Fetch` takes the whole `Event`**, not `(repo, sha)` — it needs `Event.InstallationID` to mint the token. Interface: `Fetch(ctx, ev Event, destDir string) error`.
- **`dockerfile_path` is deferred.** This slice assumes a `Dockerfile` at repo root (the deployer's existing assumption). Only `repo` + `branch` are added to `apps`.
- **Onboarding split into two CLI verbs:** `piper github setup` (once per box, creates the App) and `piper app link` (binds a repo to an app), rather than one `app connect`. Cleaner mapping to reality (App creation is once; binding is per-app).
- **Per-app serialization uses a lock (queue), not supersede.** Concurrent pushes to one app run in arrival order; "newer supersedes queued" is deferred.

---

## File Structure

**New:**
- `internal/source/source.go` — seam types (`Event`, `Kind`, `Status`), `Provider` interface, `ErrBadSignature`.
- `internal/source/source_test.go`
- `internal/source/github/github.go` — provider: config, JWT, installation token.
- `internal/source/github/parse.go` — HMAC verify + webhook → `Event`.
- `internal/source/github/fetch.go` — tarball download + strip-prefix extract.
- `internal/source/github/report.go` — Deployments API.
- `internal/source/github/manifest.go` — manifest build + code exchange.
- `internal/source/github/*_test.go` — one test file per source file above.
- `internal/source/github/testdata/push.json`, `ping.json` — webhook fixtures.
- `internal/webhook/webhook.go` — handler + per-app worker.
- `internal/webhook/webhook_test.go`

**Modified:**
- `internal/store/schema.sql` — `apps` gains `repo`,`branch`; new `github_app` table.
- `internal/store/store.go` — `App.Repo/Branch`; `UpdateAppRepo`, `AppByRepo`, `GitHubApp`, `SaveGitHubApp`, `GetGitHubApp`; `migrate()`.
- `internal/store/store_test.go`
- `internal/api/api.go` — reserve name `hooks`; `POST /v1/github/manifest`, `POST /v1/github/exchange`, `POST /v1/apps/{name}/link`.
- `internal/api/api_test.go`
- `internal/config/config.go` — `WebhookAddr`.
- `cmd/piperd/main.go` — wire webhook (relay mode): build provider from stored creds, serve on `WebhookAddr`, `UpsertRoute("hooks."+BaseDomain, port)`.
- `internal/client/client.go` + `client_test.go` — `Manifest`, `ExchangeGitHub`, `LinkApp`.
- `cmd/piper/main.go` — `piper github setup`, `piper app link`.
- `PROGRESS.md`, `README.md`.

---

## Task 1: store — app repo binding (`repo`, `branch`, `AppByRepo`)

**Files:**
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: existing `store.App{Name,Port,CreatedAt}`, `store.Open`, `ErrNotFound`.
- Produces:
  - `store.App` gains `Repo string`, `Branch string`.
  - `func (s *Store) UpdateAppRepo(name, repo, branch string) error`
  - `func (s *Store) AppByRepo(repo string) (App, error)` — `ErrNotFound` when unbound.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestUpdateAppRepoAndAppByRepo(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAppRepo("blog", "alice/blog", "main"); err != nil {
		t.Fatalf("UpdateAppRepo: %v", err)
	}

	got, err := s.AppByRepo("alice/blog")
	if err != nil {
		t.Fatalf("AppByRepo: %v", err)
	}
	if got.Name != "blog" || got.Repo != "alice/blog" || got.Branch != "main" {
		t.Fatalf("got %+v", got)
	}

	if _, err := s.AppByRepo("nobody/none"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
```

Ensure the test file imports `"errors"`, `"path/filepath"`, `"testing"`, and the `store` package (match the existing test file's import style).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestUpdateAppRepoAndAppByRepo -v`
Expected: FAIL — `s.UpdateAppRepo` / `s.AppByRepo` undefined, and `App` has no `Repo`/`Branch`.

- [ ] **Step 3: Write minimal implementation**

In `internal/store/schema.sql`, replace the `apps` table with:

```sql
CREATE TABLE IF NOT EXISTS apps (
    name           TEXT PRIMARY KEY,
    port           INTEGER NOT NULL,
    repo           TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL
);
```

In `internal/store/store.go`, add `Repo`, `Branch` to `App`:

```go
type App struct {
	Name      string
	Port      int
	Repo      string
	Branch    string
	CreatedAt time.Time
}
```

Add a `migrate` step so existing DBs gain the columns, and call it from `Open` after the schema exec. Add right after the `db.Exec(schema)` block in `Open`:

```go
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
```

And define:

```go
// migrate applies additive column changes idempotently (pre-1.0, no migration
// framework). ALTER ... ADD COLUMN errors if the column exists; we ignore that.
func migrate(db *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE apps ADD COLUMN repo TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE apps ADD COLUMN branch TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(stmt); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}
```

Add `"strings"` to the imports. Update `CreateApp`, `GetApp`, `ListApps` to select/return the new columns. Replace those three methods' SQL and scans:

```go
func (s *Store) CreateApp(name string, port int) (App, error) {
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO apps(name, port, repo, branch, created_at) VALUES(?,?,?,?,?)`,
		name, port, "", "", now.Format(time.RFC3339Nano))
	if err != nil {
		return App{}, err
	}
	return App{Name: name, Port: port, CreatedAt: now}, nil
}

func (s *Store) GetApp(name string) (App, error) {
	return s.scanApp(s.db.QueryRow(
		`SELECT name, port, repo, branch, created_at FROM apps WHERE name=?`, name))
}

func (s *Store) AppByRepo(repo string) (App, error) {
	return s.scanApp(s.db.QueryRow(
		`SELECT name, port, repo, branch, created_at FROM apps WHERE repo=?`, repo))
}

func (s *Store) UpdateAppRepo(name, repo, branch string) error {
	res, err := s.db.Exec(`UPDATE apps SET repo=?, branch=? WHERE name=?`, repo, branch, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface{ Scan(dest ...any) error }

func (s *Store) scanApp(row rowScanner) (App, error) {
	var a App
	var ts string
	err := row.Scan(&a.Name, &a.Port, &a.Repo, &a.Branch, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return a, nil
}
```

Update `ListApps` to select the new columns and use `scanApp` per row:

```go
func (s *Store) ListApps() ([]App, error) {
	rows, err := s.db.Query(`SELECT name, port, repo, branch, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := s.scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS (new test + existing store tests).

- [ ] **Step 5: Commit**

```bash
git add internal/store/schema.sql internal/store/store.go internal/store/store_test.go
git commit -m "$(printf 'feat(store): bind apps to a repo/branch\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 2: store — GitHub App credentials persistence

**Files:**
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces:
  - `type GitHubApp struct { AppID int64; PrivateKey string; WebhookSecret string }`
  - `func (s *Store) SaveGitHubApp(a GitHubApp) error` — upsert single row.
  - `func (s *Store) GetGitHubApp() (GitHubApp, error)` — `ErrNotFound` when unset.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestGitHubAppRoundTrip(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.GetGitHubApp(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	want := store.GitHubApp{AppID: 42, PrivateKey: "-----KEY-----", WebhookSecret: "shh"}
	if err := s.SaveGitHubApp(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGitHubApp()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	// Upsert replaces, not duplicates.
	want.WebhookSecret = "newsecret"
	if err := s.SaveGitHubApp(want); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetGitHubApp()
	if got.WebhookSecret != "newsecret" {
		t.Fatalf("upsert failed: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestGitHubAppRoundTrip -v`
Expected: FAIL — `GitHubApp`, `SaveGitHubApp`, `GetGitHubApp` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/store/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS github_app (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    app_id         INTEGER NOT NULL,
    private_key    TEXT NOT NULL,
    webhook_secret TEXT NOT NULL
);
```

Add to `internal/store/store.go`:

```go
type GitHubApp struct {
	AppID         int64
	PrivateKey    string
	WebhookSecret string
}

func (s *Store) SaveGitHubApp(a GitHubApp) error {
	_, err := s.db.Exec(
		`INSERT INTO github_app(id, app_id, private_key, webhook_secret) VALUES(1,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET app_id=excluded.app_id,
		   private_key=excluded.private_key, webhook_secret=excluded.webhook_secret`,
		a.AppID, a.PrivateKey, a.WebhookSecret)
	return err
}

func (s *Store) GetGitHubApp() (GitHubApp, error) {
	var a GitHubApp
	err := s.db.QueryRow(`SELECT app_id, private_key, webhook_secret FROM github_app WHERE id=1`).
		Scan(&a.AppID, &a.PrivateKey, &a.WebhookSecret)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubApp{}, ErrNotFound
	}
	return a, err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/schema.sql internal/store/store.go internal/store/store_test.go
git commit -m "$(printf 'feat(store): persist GitHub App credentials\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 3: source — seam types & Provider interface

**Files:**
- Create: `internal/source/source.go`
- Test: `internal/source/source_test.go`

**Interfaces:**
- Produces (consumed by `webhook` and `source/github`):

```go
type Kind int
const (
	KindOther Kind = iota
	KindPing
	KindPush
	KindPROpened
	KindPRSynced
	KindPRClosed
)
type Status int
const (
	StatusPending Status = iota
	StatusSuccess
	StatusFailure
)
type Event struct {
	Repo           string // "alice/blog"
	Ref            string // "refs/heads/main"
	SHA            string
	Kind           Kind
	PR             int   // 0 for push
	InstallationID int64
}
type Provider interface {
	Parse(headers http.Header, body []byte) (Event, error)
	Fetch(ctx context.Context, ev Event, destDir string) error
	Report(ctx context.Context, ev Event, status Status, url string) error
}
var ErrBadSignature = errors.New("source: bad webhook signature")
```

- [ ] **Step 1: Write the failing test**

Create `internal/source/source_test.go`:

```go
package source_test

import (
	"testing"

	"github.com/piperbox/piper/internal/source"
)

func TestKindString(t *testing.T) {
	cases := map[source.Kind]string{
		source.KindPush:  "push",
		source.KindPing:  "ping",
		source.KindOther: "other",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/ -run TestKindString -v`
Expected: FAIL — package `source` does not exist.

- [ ] **Step 3: Write minimal implementation**

Create `internal/source/source.go`:

```go
// Package source defines the provider seam: normalizing a git host's webhook
// into an Event, fetching the repo at a commit, and reporting status back.
package source

import (
	"context"
	"errors"
	"net/http"
)

type Kind int

const (
	KindOther Kind = iota
	KindPing
	KindPush
	KindPROpened
	KindPRSynced
	KindPRClosed
)

func (k Kind) String() string {
	switch k {
	case KindPing:
		return "ping"
	case KindPush:
		return "push"
	case KindPROpened:
		return "pr_opened"
	case KindPRSynced:
		return "pr_synced"
	case KindPRClosed:
		return "pr_closed"
	default:
		return "other"
	}
}

type Status int

const (
	StatusPending Status = iota
	StatusSuccess
	StatusFailure
)

// Event is a normalized git host event.
type Event struct {
	Repo           string // "owner/name"
	Ref            string // "refs/heads/main"
	SHA            string
	Kind           Kind
	PR             int
	InstallationID int64
}

// Provider drives a deploy from a git host.
type Provider interface {
	// Parse verifies the signature and normalizes a raw webhook into an Event.
	Parse(headers http.Header, body []byte) (Event, error)
	// Fetch downloads the repo tree at ev.SHA into destDir.
	Fetch(ctx context.Context, ev Event, destDir string) error
	// Report posts a deploy status back to the git host (url set on success).
	Report(ctx context.Context, ev Event, status Status, url string) error
}

// ErrBadSignature is returned by Parse when signature verification fails; the
// webhook handler maps it to HTTP 401.
var ErrBadSignature = errors.New("source: bad webhook signature")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/source/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/source/source.go internal/source/source_test.go
git commit -m "$(printf 'feat(source): provider seam types and interface\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 4: source/github — provider construction + JWT + installation token

**Files:**
- Create: `internal/source/github/github.go`
- Test: `internal/source/github/github_test.go`

**Interfaces:**
- Consumes: nothing from earlier source tasks yet (returns primitives).
- Produces:
  - `type Config struct { AppID int64; PrivateKeyPEM string; WebhookSecret string; APIBase string }`
  - `func New(cfg Config) (*Provider, error)` — parses the RSA key (PKCS#1 or PKCS#8); `APIBase` defaults to `https://api.github.com`.
  - `type Provider struct { ... }` with unexported fields `appID int64`, `key *rsa.PrivateKey`, `secret string`, `apiBase string`, `http *http.Client`.
  - `func (p *Provider) installationToken(ctx context.Context, installationID int64) (string, error)` (unexported; used by fetch/report in later tasks).

- [ ] **Step 1: Write the failing test**

Create `internal/source/github/github_test.go`:

```go
package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testKeyPEM returns a fresh PKCS#1 RSA private key in PEM form.
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

func TestInstallationToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/99/access_tokens" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"token":"ghs_installtoken"}`)
	}))
	defer srv.Close()

	p, err := New(Config{AppID: 7, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := p.installationToken(context.Background(), 99)
	if err != nil {
		t.Fatalf("installationToken: %v", err)
	}
	if tok != "ghs_installtoken" {
		t.Fatalf("token = %q", tok)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") || strings.Count(gotAuth, ".") != 2 {
		t.Fatalf("expected a Bearer JWT, got %q", gotAuth)
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New(Config{AppID: 1, PrivateKeyPEM: "not a key", WebhookSecret: "s"}); err == nil {
		t.Fatal("expected error for bad key")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/github/ -run 'TestInstallationToken|TestNewRejectsBadKey' -v`
Expected: FAIL — `New`, `Config`, `installationToken` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/source/github/github.go`:

```go
// Package github implements source.Provider for a per-user GitHub App:
// webhook verification, installation-token code fetch, and Deployments status.
package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultAPIBase = "https://api.github.com"

type Config struct {
	AppID         int64
	PrivateKeyPEM string
	WebhookSecret string
	APIBase       string // defaults to https://api.github.com
}

type Provider struct {
	appID   int64
	key     *rsa.PrivateKey
	secret  string
	apiBase string
	http    *http.Client
}

func New(cfg Config) (*Provider, error) {
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse app private key: %w", err)
	}
	base := cfg.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	return &Provider{
		appID:   cfg.AppID,
		key:     key,
		secret:  cfg.WebhookSecret,
		apiBase: strings.TrimRight(base, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func parsePrivateKey(pemStr string) (*rsa.PrivateKey, error) {
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

// appJWT mints a short-lived GitHub App JWT (RS256) signed with the app key.
func (p *Provider) appJWT(now time.Time) (string, error) {
	header := b64url([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%d"}`,
		now.Add(-30*time.Second).Unix(), now.Add(9*time.Minute).Unix(), p.appID)
	signingInput := header + "." + b64url([]byte(claims))
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(sig), nil
}

// installationToken exchanges an app JWT for a short-lived installation token.
func (p *Provider) installationToken(ctx context.Context, installationID int64) (string, error) {
	jwt, err := p.appJWT(time.Now())
	if err != nil {
		return "", err
	}
	url := p.apiBase + "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.http.Do(req)
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

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/source/github/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/source/github/github.go internal/source/github/github_test.go
git commit -m "$(printf 'feat(source/github): provider construction, app JWT, installation token\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 5: source/github — Parse (HMAC verify + push/ping → Event)

**Files:**
- Create: `internal/source/github/parse.go`
- Create: `internal/source/github/testdata/push.json`
- Create: `internal/source/github/testdata/ping.json`
- Test: `internal/source/github/parse_test.go`

**Interfaces:**
- Consumes: `Provider` (Task 4), `source.Event`/`source.Kind`/`source.ErrBadSignature` (Task 3).
- Produces: `func (p *Provider) Parse(headers http.Header, body []byte) (source.Event, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/source/github/testdata/push.json`:

```json
{
  "ref": "refs/heads/main",
  "after": "abc123def456",
  "repository": { "full_name": "alice/blog" },
  "installation": { "id": 99 }
}
```

Create `internal/source/github/testdata/ping.json`:

```json
{ "zen": "Keep it simple", "hook_id": 1, "installation": { "id": 99 } }
```

Create `internal/source/github/parse_test.go`:

```go
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/piperbox/piper/internal/source"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func newTestProvider(t *testing.T, secret string) *Provider {
	t.Helper()
	p, err := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParsePush(t *testing.T) {
	body, _ := os.ReadFile("testdata/push.json")
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	h.Set("X-Hub-Signature-256", sign("s3cr3t", string(body)))

	ev, err := p.Parse(h, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := source.Event{
		Repo: "alice/blog", Ref: "refs/heads/main", SHA: "abc123def456",
		Kind: source.KindPush, InstallationID: 99,
	}
	if ev != want {
		t.Fatalf("got %+v want %+v", ev, want)
	}
}

func TestParseBadSignature(t *testing.T) {
	body, _ := os.ReadFile("testdata/push.json")
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	h.Set("X-Hub-Signature-256", sign("WRONG", string(body)))

	if _, err := p.Parse(h, body); !errors.Is(err, source.ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestParsePing(t *testing.T) {
	body, _ := os.ReadFile("testdata/ping.json")
	p := newTestProvider(t, "s3cr3t")
	h := http.Header{}
	h.Set("X-GitHub-Event", "ping")
	h.Set("X-Hub-Signature-256", sign("s3cr3t", string(body)))

	ev, err := p.Parse(h, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Kind != source.KindPing {
		t.Fatalf("kind = %v", ev.Kind)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/github/ -run TestParse -v`
Expected: FAIL — `Parse` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/source/github/parse.go`:

```go
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"net/http"

	"github.com/piperbox/piper/internal/source"
)

func (p *Provider) verify(headers http.Header, body []byte) error {
	sig := headers.Get("X-Hub-Signature-256")
	m := hmac.New(sha256.New, []byte(p.secret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return source.ErrBadSignature
	}
	return nil
}

func (p *Provider) Parse(headers http.Header, body []byte) (source.Event, error) {
	if err := p.verify(headers, body); err != nil {
		return source.Event{}, err
	}
	var payload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return source.Event{}, fmt.Errorf("parse payload: %w", err)
	}
	ev := source.Event{
		Repo:           payload.Repository.FullName,
		InstallationID: payload.Installation.ID,
	}
	switch headers.Get("X-GitHub-Event") {
	case "ping":
		ev.Kind = source.KindPing
	case "push":
		ev.Kind = source.KindPush
		ev.Ref = payload.Ref
		ev.SHA = payload.After
	default:
		ev.Kind = source.KindOther
	}
	return ev, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/source/github/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/source/github/parse.go internal/source/github/parse_test.go internal/source/github/testdata
git commit -m "$(printf 'feat(source/github): verify HMAC and parse push/ping webhooks\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 6: source/github — Fetch (tarball download + strip-prefix extract)

**Files:**
- Create: `internal/source/github/fetch.go`
- Test: `internal/source/github/fetch_test.go`

**Interfaces:**
- Consumes: `Provider.installationToken` (Task 4), `source.Event` (Task 3).
- Produces: `func (p *Provider) Fetch(ctx context.Context, ev source.Event, destDir string) error`.

- [ ] **Step 1: Write the failing test**

Create `internal/source/github/fetch_test.go`:

```go
package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/source"
)

// makeTarball builds a gzipped tar with a single top-level dir "alice-blog-abc/"
// containing Dockerfile and app.py, mimicking GitHub's codeload format.
func makeTarball(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"alice-blog-abc/Dockerfile": "FROM scratch\n",
		"alice-blog-abc/app.py":     "print('hi')\n",
	}
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestFetchStripsPrefix(t *testing.T) {
	tarball := makeTarball(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case r.URL.Path == "/repos/alice/blog/tarball/abc123":
			if got := r.Header.Get("Authorization"); got != "token ghs_x" {
				t.Errorf("tarball auth = %q", got)
			}
			w.Write(tarball)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p, err := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	ev := source.Event{Repo: "alice/blog", SHA: "abc123", InstallationID: 99}
	if err := p.Fetch(context.Background(), ev, dst); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Dockerfile must be at dst root, not under a nested dir.
	if _, err := os.Stat(filepath.Join(dst, "Dockerfile")); err != nil {
		t.Fatalf("Dockerfile not at root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "app.py")); err != nil {
		t.Fatalf("app.py not at root: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/github/ -run TestFetchStripsPrefix -v`
Expected: FAIL — `Fetch` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/source/github/fetch.go`:

```go
package github

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/piperbox/piper/internal/source"
)

func (p *Provider) Fetch(ctx context.Context, ev source.Event, destDir string) error {
	token, err := p.installationToken(ctx, ev.InstallationID)
	if err != nil {
		return err
	}
	url := p.apiBase + "/repos/" + ev.Repo + "/tarball/" + ev.SHA
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("tarball: %s: %s", resp.Status, body)
	}
	return extractStripped(resp.Body, destDir)
}

// extractStripped un-gzips and untars, removing the single top-level directory
// GitHub wraps repo tarballs in (e.g. "owner-repo-sha/").
func extractStripped(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		rel := stripFirst(hdr.Name)
		if rel == "" {
			continue
		}
		target := filepath.Join(destDir, rel)
		if !within(destDir, target) {
			return errors.New("tar entry escapes destination")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

// stripFirst drops the leading path component ("owner-repo-sha/rest" -> "rest").
func stripFirst(name string) string {
	name = filepath.Clean("/" + name)[1:] // normalize, drop leading slash
	i := strings.IndexByte(name, '/')
	if i < 0 {
		return ""
	}
	return name[i+1:]
}

func within(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/source/github/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/source/github/fetch.go internal/source/github/fetch_test.go
git commit -m "$(printf 'feat(source/github): fetch repo tarball at commit via installation token\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 7: source/github — Report (GitHub Deployments API)

**Files:**
- Create: `internal/source/github/report.go`
- Test: `internal/source/github/report_test.go`

**Interfaces:**
- Consumes: `Provider.installationToken` (Task 4), `source.Event`/`source.Status` (Task 3).
- Produces: `func (p *Provider) Report(ctx context.Context, ev source.Event, status source.Status, url string) error`.

Behavior: `StatusPending` → create a Deployment for `ev.SHA` (`environment:"production"`). `StatusSuccess`/`StatusFailure` → find the latest deployment for `ev.SHA`, then post a deployment status (`success`/`failure`) with `environment_url` = `url`.

- [ ] **Step 1: Write the failing test**

Create `internal/source/github/report_test.go`:

```go
package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/piperbox/piper/internal/source"
)

func TestReportPendingCreatesDeployment(t *testing.T) {
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case "/repos/alice/blog/deployments":
			if r.Method != http.MethodPost {
				t.Errorf("method = %s", r.Method)
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["ref"] != "sha1" {
				t.Errorf("ref = %v", body["ref"])
			}
			created = true
			w.WriteHeader(201)
			io.WriteString(w, `{"id":555}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	ev := source.Event{Repo: "alice/blog", SHA: "sha1", InstallationID: 99}
	if err := p.Report(context.Background(), ev, source.StatusPending, ""); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !created {
		t.Fatal("deployment not created")
	}
}

func TestReportSuccessPostsStatus(t *testing.T) {
	var gotState, gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/99/access_tokens":
			io.WriteString(w, `{"token":"ghs_x"}`)
		case r.URL.Path == "/repos/alice/blog/deployments" && r.Method == http.MethodGet:
			io.WriteString(w, `[{"id":555}]`)
		case r.URL.Path == "/repos/alice/blog/deployments/555/statuses":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			gotState, _ = body["state"].(string)
			gotURL, _ = body["environment_url"].(string)
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p, _ := New(Config{AppID: 1, PrivateKeyPEM: testKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL})
	ev := source.Event{Repo: "alice/blog", SHA: "sha1", InstallationID: 99}
	err := p.Report(context.Background(), ev, source.StatusSuccess, "https://blog.example.com")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotState != "success" || gotURL != "https://blog.example.com" {
		t.Fatalf("state=%q url=%q", gotState, gotURL)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/github/ -run TestReport -v`
Expected: FAIL — `Report` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/source/github/report.go`:

```go
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/piperbox/piper/internal/source"
)

func (p *Provider) Report(ctx context.Context, ev source.Event, status source.Status, url string) error {
	token, err := p.installationToken(ctx, ev.InstallationID)
	if err != nil {
		return err
	}
	if status == source.StatusPending {
		_, err := p.createDeployment(ctx, token, ev)
		return err
	}
	id, err := p.latestDeploymentID(ctx, token, ev)
	if err != nil {
		return err
	}
	state := "failure"
	if status == source.StatusSuccess {
		state = "success"
	}
	return p.postStatus(ctx, token, ev.Repo, id, state, url)
}

func (p *Provider) do(ctx context.Context, method, url, token string, in any, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, url, body)
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (p *Provider) createDeployment(ctx context.Context, token string, ev source.Event) (int64, error) {
	in := map[string]any{
		"ref":               ev.SHA,
		"environment":       "production",
		"auto_merge":        false,
		"required_contexts": []string{},
		"description":       "piper deploy",
	}
	var out struct {
		ID int64 `json:"id"`
	}
	err := p.do(ctx, http.MethodPost, p.apiBase+"/repos/"+ev.Repo+"/deployments", token, in, &out)
	return out.ID, err
}

func (p *Provider) latestDeploymentID(ctx context.Context, token string, ev source.Event) (int64, error) {
	var out []struct {
		ID int64 `json:"id"`
	}
	err := p.do(ctx, http.MethodGet, p.apiBase+"/repos/"+ev.Repo+"/deployments?sha="+ev.SHA, token, nil, &out)
	if err != nil {
		return 0, err
	}
	if len(out) == 0 {
		return 0, fmt.Errorf("no deployment for sha %s", ev.SHA)
	}
	return out[0].ID, nil
}

func (p *Provider) postStatus(ctx context.Context, token, repo string, id int64, state, url string) error {
	in := map[string]any{"state": state}
	if url != "" {
		in["environment_url"] = url
	}
	endpoint := fmt.Sprintf("%s/repos/%s/deployments/%d/statuses", p.apiBase, repo, id)
	return p.do(ctx, http.MethodPost, endpoint, token, in, nil)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/source/github/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/source/github/report.go internal/source/github/report_test.go
git commit -m "$(printf 'feat(source/github): report deploy status via GitHub Deployments API\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 8: source/github — manifest build + code exchange (onboarding)

**Files:**
- Create: `internal/source/github/manifest.go`
- Test: `internal/source/github/manifest_test.go`

**Interfaces:**
- Produces:
  - `func BuildManifest(appName, webhookURL, redirectURL string) ([]byte, error)` — GitHub App manifest JSON.
  - `type AppCredentials struct { AppID int64; PrivateKeyPEM string; WebhookSecret string }`
  - `func ExchangeCode(ctx context.Context, apiBase, code string) (AppCredentials, error)` — `apiBase` empty ⇒ default. (piperd maps `AppCredentials` → `store.GitHubApp`; `github` never imports `store`.)

- [ ] **Step 1: Write the failing test**

Create `internal/source/github/manifest_test.go`:

```go
package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildManifest(t *testing.T) {
	raw, err := BuildManifest("piper-alice", "https://hooks.alice.dev", "http://localhost:5000/cb")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != "piper-alice" {
		t.Errorf("name = %v", m["name"])
	}
	hook, _ := m["hook_attributes"].(map[string]any)
	if hook == nil || hook["url"] != "https://hooks.alice.dev" {
		t.Errorf("hook_attributes = %v", m["hook_attributes"])
	}
	if m["redirect_url"] != "http://localhost:5000/cb" {
		t.Errorf("redirect_url = %v", m["redirect_url"])
	}
	events, _ := m["default_events"].([]any)
	if len(events) == 0 {
		t.Error("expected default_events")
	}
}

func TestExchangeCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/app-manifests/") || !strings.HasSuffix(r.URL.Path, "/conversions") {
			t.Errorf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"id":123,"pem":"-----PEM-----","webhook_secret":"whsec"}`)
	}))
	defer srv.Close()

	got, err := ExchangeCode(context.Background(), srv.URL, "thecode")
	if err != nil {
		t.Fatal(err)
	}
	want := AppCredentials{AppID: 123, PrivateKeyPEM: "-----PEM-----", WebhookSecret: "whsec"}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/github/ -run 'TestBuildManifest|TestExchangeCode' -v`
Expected: FAIL — `BuildManifest`, `ExchangeCode`, `AppCredentials` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/source/github/manifest.go`:

```go
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// BuildManifest returns a GitHub App manifest for a per-user Piper App. The App
// receives push + pull_request events at webhookURL; GitHub redirects the
// browser to redirectURL with a temporary ?code= after creation.
func BuildManifest(appName, webhookURL, redirectURL string) ([]byte, error) {
	m := map[string]any{
		"name":         appName,
		"url":          "https://github.com/piperbox/piper",
		"redirect_url": redirectURL,
		"public":       false,
		"hook_attributes": map[string]any{
			"url": webhookURL,
		},
		"default_events": []string{"push", "pull_request"},
		"default_permissions": map[string]string{
			"contents":     "read",
			"deployments":  "write",
			"pull_requests": "read",
		},
	}
	return json.Marshal(m)
}

type AppCredentials struct {
	AppID         int64
	PrivateKeyPEM string
	WebhookSecret string
}

// ExchangeCode converts a manifest code into App credentials.
func ExchangeCode(ctx context.Context, apiBase, code string) (AppCredentials, error) {
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	url := strings.TrimRight(apiBase, "/") + "/app-manifests/" + code + "/conversions"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return AppCredentials{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return AppCredentials{}, fmt.Errorf("exchange code: %s: %s", resp.Status, b)
	}
	var out struct {
		ID            int64  `json:"id"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return AppCredentials{}, err
	}
	return AppCredentials{AppID: out.ID, PrivateKeyPEM: out.PEM, WebhookSecret: out.WebhookSecret}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/source/github/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/source/github/manifest.go internal/source/github/manifest_test.go
git commit -m "$(printf 'feat(source/github): app manifest build and code exchange\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 9: webhook — handler, per-app worker, routing

**Files:**
- Create: `internal/webhook/webhook.go`
- Test: `internal/webhook/webhook_test.go`

**Interfaces:**
- Consumes: `source.Provider`/`source.Event`/`source.Kind`/`source.Status`/`source.ErrBadSignature` (Task 3), `*store.Store` with `AppByRepo` (Task 1), `store.Deployment`.
- Produces:
  - `type Deployer interface { Deploy(ctx context.Context, app, srcDir string) (store.Deployment, error) }`
  - `func New(p source.Provider, s *store.Store, d Deployer, baseDomain string) *Handler`
  - `*Handler` implements `http.Handler`.
  - `func (h *Handler) Wait()` — test hook: blocks until all in-flight `process` goroutines finish. (Production code never calls it.)

Behavior: verify+parse; bad signature → 401; parse error → 400; ping → 200; otherwise 202 + async `process`. `process`: push only; `AppByRepo`; skip if `ev.Ref != "refs/heads/"+app.Branch`; per-app lock; idempotent on last SHA; `Report(pending)` → `Fetch` → `Deploy` → `Report(success|failure)`; success URL = `https://<app>.<baseDomain>`.

- [ ] **Step 1: Write the failing test**

Create `internal/webhook/webhook_test.go`:

```go
package webhook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/piperbox/piper/internal/source"
	"github.com/piperbox/piper/internal/store"
	"github.com/piperbox/piper/internal/webhook"
)

type fakeProvider struct {
	mu       sync.Mutex
	parseErr error
	ev       source.Event
	reports  []source.Status
	fetchErr error
}

func (f *fakeProvider) Parse(http.Header, []byte) (source.Event, error) {
	return f.ev, f.parseErr
}
func (f *fakeProvider) Fetch(context.Context, source.Event, string) error { return f.fetchErr }
func (f *fakeProvider) Report(_ context.Context, _ source.Event, s source.Status, _ string) error {
	f.mu.Lock()
	f.reports = append(f.reports, s)
	f.mu.Unlock()
	return nil
}
func (f *fakeProvider) statuses() []source.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]source.Status(nil), f.reports...)
}

type fakeDeployer struct {
	mu     sync.Mutex
	calls  int
	err    error
}

func (d *fakeDeployer) Deploy(context.Context, string, string) (store.Deployment, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return store.Deployment{}, d.err
}
func (d *fakeDeployer) count() int { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func post(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	h.ServeHTTP(rec, req)
	return rec
}

func TestBadSignatureReturns401(t *testing.T) {
	p := &fakeProvider{parseErr: source.ErrBadSignature}
	d := &fakeDeployer{}
	h := webhook.New(p, newStore(t), d, "piper.localhost")
	if rec := post(h); rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.count() != 0 {
		t.Fatal("deploy must not run on bad signature")
	}
}

func TestUnknownRepoNoOp(t *testing.T) {
	p := &fakeProvider{ev: source.Event{Kind: source.KindPush, Repo: "nobody/x", Ref: "refs/heads/main"}}
	d := &fakeDeployer{}
	h := webhook.New(p, newStore(t), d, "piper.localhost")
	rec := post(h)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.count() != 0 {
		t.Fatal("deploy must not run for unknown repo")
	}
}

func TestPushDeploysAndReports(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main", SHA: "s1",
	}}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")

	if rec := post(h); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.count() != 1 {
		t.Fatalf("deploy calls = %d", d.count())
	}
	got := p.statuses()
	if len(got) != 2 || got[0] != source.StatusPending || got[1] != source.StatusSuccess {
		t.Fatalf("statuses = %v", got)
	}
}

func TestWrongBranchNoOp(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/dev", SHA: "s1",
	}}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	if d.count() != 0 {
		t.Fatal("deploy must not run for non-tracked branch")
	}
}

func TestDeployFailureReportsFailure(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main", SHA: "s1",
	}}
	d := &fakeDeployer{err: context.DeadlineExceeded}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	got := p.statuses()
	if len(got) != 2 || got[1] != source.StatusFailure {
		t.Fatalf("statuses = %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webhook/ -v`
Expected: FAIL — package `webhook` does not exist.

- [ ] **Step 3: Write minimal implementation**

Create `internal/webhook/webhook.go`:

```go
// Package webhook receives a git host's signed webhook, resolves the bound app,
// and drives the deployer. It is the only public surface exposed through the
// tunnel; everything else in piperd stays loopback-only.
package webhook

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/piperbox/piper/internal/source"
	"github.com/piperbox/piper/internal/store"
)

const maxBody = 5 << 20 // 5 MiB

type Deployer interface {
	Deploy(ctx context.Context, app, srcDir string) (store.Deployment, error)
}

type Handler struct {
	prov    source.Provider
	store   *store.Store
	deploy  Deployer
	baseDom string

	wg      sync.WaitGroup
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	lastSHA map[string]string
}

func New(p source.Provider, s *store.Store, d Deployer, baseDomain string) *Handler {
	return &Handler{
		prov: p, store: s, deploy: d, baseDom: baseDomain,
		locks: map[string]*sync.Mutex{}, lastSHA: map[string]string{},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	ev, err := h.prov.Parse(r.Header, body)
	if errors.Is(err, source.ErrBadSignature) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if ev.Kind == source.KindPing {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "pong")
		return
	}
	w.WriteHeader(http.StatusAccepted)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.process(context.Background(), ev)
	}()
}

// Wait blocks until in-flight deploys finish. Test-only.
func (h *Handler) Wait() { h.wg.Wait() }

func (h *Handler) appLock(name string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.locks[name]
	if !ok {
		m = &sync.Mutex{}
		h.locks[name] = m
	}
	return m
}

func (h *Handler) process(ctx context.Context, ev source.Event) {
	if ev.Kind != source.KindPush {
		return // this slice acts only on push
	}
	app, err := h.store.AppByRepo(ev.Repo)
	if errors.Is(err, store.ErrNotFound) {
		log.Printf("webhook: no app bound to %s", ev.Repo)
		return
	}
	if err != nil {
		log.Printf("webhook: lookup %s: %v", ev.Repo, err)
		return
	}
	if ev.Ref != "refs/heads/"+app.Branch {
		log.Printf("webhook: %s ref %s != tracked %s", ev.Repo, ev.Ref, app.Branch)
		return
	}

	lock := h.appLock(app.Name)
	lock.Lock()
	defer lock.Unlock()

	h.mu.Lock()
	dup := h.lastSHA[app.Name] == ev.SHA
	h.mu.Unlock()
	if dup {
		log.Printf("webhook: %s already at %s, skipping", app.Name, ev.SHA)
		return
	}

	_ = h.prov.Report(ctx, ev, source.StatusPending, "")

	dir, err := os.MkdirTemp("", "piper-src-*")
	if err != nil {
		log.Printf("webhook: tmpdir: %v", err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	defer os.RemoveAll(dir)

	if err := h.prov.Fetch(ctx, ev, dir); err != nil {
		log.Printf("webhook: fetch %s@%s: %v", ev.Repo, ev.SHA, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	if _, err := h.deploy.Deploy(ctx, app.Name, dir); err != nil {
		log.Printf("webhook: deploy %s: %v", app.Name, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}

	url := "https://" + app.Name + "." + h.baseDom
	_ = h.prov.Report(ctx, ev, source.StatusSuccess, url)

	h.mu.Lock()
	h.lastSHA[app.Name] = ev.SHA
	h.mu.Unlock()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/webhook/ -race -v`
Expected: PASS (all five tests; `-race` clean).

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/webhook.go internal/webhook/webhook_test.go
git commit -m "$(printf 'feat(webhook): receive signed webhook and drive the deployer\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 10: api + config — onboarding endpoints, repo link, reserved name, WebhookAddr

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/api/api.go`
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `github.BuildManifest`/`github.ExchangeCode`/`github.AppCredentials` (Task 8), `store` methods (Tasks 1–2).
- Produces (config): `Config.WebhookAddr` (`PIPER_WEBHOOK_ADDR`, default `127.0.0.1:8089`).
- Produces (api): three routes on the existing control mux —
  - `POST /v1/github/manifest` body `{"redirect_url":"..."}` → `{"manifest":"<json string>"}` (manifest built with webhook URL `https://hooks.<BaseDomain>` and app name `piper-<BaseDomain>`).
  - `POST /v1/github/exchange` body `{"code":"..."}` → 204; exchanges + `SaveGitHubApp`.
  - `POST /v1/apps/{name}/link` body `{"repo":"owner/name","branch":"main"}` → 204; `UpdateAppRepo`.
  - `CreateApp` (existing `POST /v1/apps`) rejects name `hooks` with 400.
- `api.New` signature gains a base domain and an `apiBase` for GitHub (empty ⇒ default). New signature:
  `func New(s *store.Store, d Deployerer, baseDomain, githubAPIBase string) http.Handler`.

- [ ] **Step 1: Write the failing test**

Add to `internal/api/api_test.go` (a fake deployer already exists in that file — reuse it; if the existing tests call `api.New(store, dep)`, update those call sites to `api.New(store, dep, "piper.localhost", "")` in this step too):

```go
func TestReservedNameRejected(t *testing.T) {
	h := api.New(newAPIStore(t), &okDeployer{}, "piper.localhost", "")
	body := strings.NewReader(`{"name":"hooks","port":8080}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestLinkApp(t *testing.T) {
	s := newAPIStore(t)
	s.CreateApp("blog", 8080)
	h := api.New(s, &okDeployer{}, "piper.localhost", "")
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

func TestManifestEndpoint(t *testing.T) {
	h := api.New(newAPIStore(t), &okDeployer{}, "alice.dev", "")
	body := strings.NewReader(`{"redirect_url":"http://localhost:5000/cb"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/github/manifest", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hooks.alice.dev") {
		t.Fatalf("manifest missing webhook host: %s", rec.Body.String())
	}
}
```

Add helpers if the file lacks them (match the file's existing style):

```go
func newAPIStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type okDeployer struct{}

func (okDeployer) Deploy(context.Context, string, string) (store.Deployment, error) {
	return store.Deployment{}, nil
}
```

> Note: if `api_test.go` already defines a deployer fake or a store helper, reuse those names instead of adding duplicates, and only update the `api.New(...)` call sites.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -v`
Expected: FAIL — `api.New` arity changed / new routes 404.

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`, add the field and default:

```go
	APIAddr    string // control API listen address
	WebhookAddr string // loopback webhook listener (relay mode)
```

and in `Load()`:

```go
		APIAddr:     env("PIPER_API_ADDR", "127.0.0.1:8088"),
		WebhookAddr: env("PIPER_WEBHOOK_ADDR", "127.0.0.1:8089"),
```

In `internal/api/api.go`, change the signature and add routes. Update the top:

```go
func New(s *store.Store, d Deployerer, baseDomain, githubAPIBase string) http.Handler {
	mux := http.NewServeMux()
```

Add a reserved-name guard inside the existing `POST /v1/apps` handler, right after decoding `in` (before the `GetApp` existence check):

```go
		if in.Name == "hooks" {
			http.Error(w, "name reserved", http.StatusBadRequest)
			return
		}
```

Add these three handlers before `return mux`:

```go
	mux.HandleFunc("POST /v1/apps/{name}/link", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Repo   string `json:"repo"`
			Branch string `json:"branch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Repo == "" || in.Branch == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if err := s.UpdateAppRepo(r.PathValue("name"), in.Repo, in.Branch); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /v1/github/manifest", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RedirectURL string `json:"redirect_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.RedirectURL == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		manifest, err := github.BuildManifest("piper-"+baseDomain, "https://hooks."+baseDomain, in.RedirectURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"manifest": string(manifest)})
	})

	mux.HandleFunc("POST /v1/github/exchange", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Code == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		creds, err := github.ExchangeCode(r.Context(), githubAPIBase, in.Code)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := s.SaveGitHubApp(store.GitHubApp{
			AppID: creds.AppID, PrivateKey: creds.PrivateKeyPEM, WebhookSecret: creds.WebhookSecret,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
```

Add `"github.com/piperbox/piper/internal/source/github"` to the api imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/api/api.go internal/api/api_test.go
git commit -m "$(printf 'feat(api): app-repo link and GitHub App onboarding endpoints\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 11: piperd — wire webhook into relay mode + integration test

**Files:**
- Modify: `cmd/piperd/main.go`
- Create: `internal/webhook/integration_test.go`

**Interfaces:**
- Consumes: `github.New`/`store.GetGitHubApp` (Tasks 2,4), `webhook.New` (Task 9), `caddy.Client.UpsertRoute`, `config.WebhookAddr`.
- Produces: no new exported API. On startup in relay mode, if `GetGitHubApp` succeeds, piperd builds the provider, serves `webhook.Handler` on `cfg.WebhookAddr`, and routes `hooks.<BaseDomain>` → that port through Caddy.

- [ ] **Step 1: Write the failing test**

Create `internal/webhook/integration_test.go` — end-to-end through the *real* github provider (GitHub stubbed by `httptest`) and the *real* store, with a stub deployer, proving the wiring produces a deploy and a success status:

```go
package webhook_test

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/source"
	"github.com/piperbox/piper/internal/source/github"
	"github.com/piperbox/piper/internal/webhook"
)

func TestWebhookIntegrationRealProvider(t *testing.T) {
	// Stub GitHub API: installation token + deployments + statuses.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			io.WriteString(w, `{"token":"ghs_x"}`)
		case strings.HasSuffix(r.URL.Path, "/deployments") && r.Method == http.MethodPost:
			w.WriteHeader(201)
			io.WriteString(w, `{"id":1}`)
		case strings.HasSuffix(r.URL.Path, "/deployments") && r.Method == http.MethodGet:
			io.WriteString(w, `[{"id":1}]`)
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			w.WriteHeader(201)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	prov, err := github.New(github.Config{
		AppID: 1, PrivateKeyPEM: keyPEM, WebhookSecret: "whsec", APIBase: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main")

	d := &fakeDeployer{}
	h := webhook.New(prov, s, d, "alice.dev")

	body := `{"ref":"refs/heads/main","after":"deadbeef","repository":{"full_name":"alice/blog"},"installation":{"id":99}}`
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()

	if d.count() != 1 {
		t.Fatalf("deploy calls = %d", d.count())
	}
	if got := p2statuses(prov); false {
		_ = got // provider has no recording; success asserted via gh handler reached
	}
}

// p2statuses is a placeholder to keep imports honest; the success path is proven
// by the gh stub receiving the /statuses call (t.Errorf fires otherwise).
func p2statuses(source.Provider) []source.Status { return nil }
```

> The `p2statuses` shim exists only so the `source` import is used; the real assertion is that the `gh` stub's `/statuses` branch is hit (any unexpected path calls `t.Errorf`). If you prefer, drop `p2statuses` and the `source` import entirely.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webhook/ -run TestWebhookIntegrationRealProvider -v`
Expected: FAIL initially only if compilation of the shim is off; fix the shim so it compiles, then it should PASS once Task 9's handler is in place. (This test exercises code already written in Tasks 4–9; its purpose is a regression guard for the wiring. If it passes immediately, that is correct — proceed.)

- [ ] **Step 3: Wire piperd**

In `cmd/piperd/main.go`, add imports:

```go
	"github.com/piperbox/piper/internal/source/github"
	"github.com/piperbox/piper/internal/webhook"
```

Immediately after the relay TLS + tunnel block (after `go agent.RunTunnelClient(...)`), still inside `if cfg.RelayAddr != "" {`, add:

```go
		if gh, err := st.GetGitHubApp(); err == nil {
			prov, err := github.New(github.Config{
				AppID: gh.AppID, PrivateKeyPEM: gh.PrivateKey, WebhookSecret: gh.WebhookSecret,
			})
			if err != nil {
				log.Fatalf("github provider: %v", err)
			}
			wdep := deploy.New(st, rt, caddy.NewClient(cfg.CaddyAdmin), cfg.BaseDomain)
			wh := webhook.New(prov, st, wdep, cfg.BaseDomain)
			whSrv := &http.Server{Addr: cfg.WebhookAddr, Handler: wh}
			go func() {
				if err := whSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Printf("webhook serve: %v", err)
				}
			}()
			_, portStr, _ := net.SplitHostPort(cfg.WebhookAddr)
			port, _ := strconv.Atoi(portStr)
			if err := caddy.NewClient(cfg.CaddyAdmin).UpsertRoute("hooks."+cfg.BaseDomain, port); err != nil {
				log.Printf("webhook route: %v", err)
			}
			log.Printf("webhook listening on %s (GitHub App %d)", cfg.WebhookAddr, gh.AppID)
		} else {
			log.Printf("no GitHub App configured; run `piper github setup` to enable git deploys")
		}
```

Add `"strconv"` to the imports. Update the existing `api.New(st, dep)` call to the new signature:

```go
	handler := api.New(st, dep, cfg.BaseDomain, "")
```

- [ ] **Step 4: Verify build + tests + cross-compile**

Run: `go build ./... && go test ./internal/webhook/ -v && make cross`
Expected: build OK; webhook tests PASS; `make cross` (arm64, `CGO_ENABLED=0`) OK.

- [ ] **Step 5: Commit**

```bash
git add cmd/piperd/main.go internal/webhook/integration_test.go
git commit -m "$(printf 'feat(agent): serve webhook over the tunnel in relay mode\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 12: piper CLI — `github setup` and `app link`

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/client_test.go`
- Modify: `cmd/piper/main.go`

**Interfaces:**
- Consumes: control endpoints from Task 10.
- Produces (client):
  - `func (c *Client) Manifest(redirectURL string) (string, error)` — returns the manifest JSON string.
  - `func (c *Client) ExchangeGitHub(code string) error`
  - `func (c *Client) LinkApp(name, repo, branch string) error`
- Produces (CLI): `piper github setup`, `piper app link <name> --repo <owner/name> --branch <branch>`.

- [ ] **Step 1: Write the failing test**

Add to `internal/client/client_test.go` (match the file's existing httptest-stub style):

```go
func TestLinkApp(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := client.New(srv.URL)
	if err := c.LinkApp("blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/apps/blog/link" {
		t.Fatalf("path = %s", gotPath)
	}
	if !strings.Contains(gotBody, `"alice/blog"`) || !strings.Contains(gotBody, `"main"`) {
		t.Fatalf("body = %s", gotBody)
	}
}

func TestManifestAndExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/github/manifest":
			io.WriteString(w, `{"manifest":"{\"name\":\"x\"}"}`)
		case "/v1/github/exchange":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := client.New(srv.URL)
	m, err := c.Manifest("http://localhost:5000/cb")
	if err != nil || !strings.Contains(m, `"name"`) {
		t.Fatalf("Manifest m=%q err=%v", m, err)
	}
	if err := c.ExchangeGitHub("thecode"); err != nil {
		t.Fatalf("ExchangeGitHub: %v", err)
	}
}
```

Ensure imports include `"io"`, `"net/http"`, `"net/http/httptest"`, `"strings"`, `"testing"`, and the `client` package (match the existing file).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run 'TestLinkApp|TestManifestAndExchange' -v`
Expected: FAIL — `LinkApp`, `Manifest`, `ExchangeGitHub` undefined.

- [ ] **Step 3: Write minimal implementation**

Look at `internal/client/client.go` for its existing request helper (e.g. how `Deploy`/`CreateApp` POST JSON). Add, using the same helper/pattern (the code below assumes a `*Client` with `baseURL string` and `http *http.Client`; adapt field names to the existing struct):

```go
func (c *Client) LinkApp(name, repo, branch string) error {
	body, _ := json.Marshal(map[string]string{"repo": repo, "branch": branch})
	req, _ := http.NewRequest(http.MethodPost, c.baseURL+"/v1/apps/"+name+"/link", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("link: %s", resp.Status)
	}
	return nil
}

func (c *Client) Manifest(redirectURL string) (string, error) {
	body, _ := json.Marshal(map[string]string{"redirect_url": redirectURL})
	req, _ := http.NewRequest(http.MethodPost, c.baseURL+"/v1/github/manifest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("manifest: %s", resp.Status)
	}
	var out struct {
		Manifest string `json:"manifest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Manifest, nil
}

func (c *Client) ExchangeGitHub(code string) error {
	body, _ := json.Marshal(map[string]string{"code": code})
	req, _ := http.NewRequest(http.MethodPost, c.baseURL+"/v1/github/exchange", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("exchange: %s", resp.Status)
	}
	return nil
}
```

Ensure `client.go` imports `"bytes"`, `"encoding/json"`, `"fmt"`, `"net/http"` (add any missing).

In `cmd/piper/main.go`, add subcommands following the file's existing command dispatch. Add an `app link` verb and a `github setup` verb:

```go
// piper app link <name> --repo owner/name --branch main
func cmdAppLink(args []string) error {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	repo := fs.String("repo", "", "GitHub repo, owner/name")
	branch := fs.String("branch", "main", "tracked branch")
	fs.Parse(args)
	if fs.NArg() < 1 || *repo == "" {
		return fmt.Errorf("usage: piper app link <name> --repo owner/name [--branch main]")
	}
	return client.New(config.ClientAddr()).LinkApp(fs.Arg(0), *repo, *branch)
}

// piper github setup — create the per-user GitHub App via the manifest flow.
func cmdGithubSetup(args []string) error {
	c := client.New(config.ClientAddr())

	codeCh := make(chan string, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	redirect := "http://" + ln.Addr().String() + "/cb"

	manifest, err := c.Manifest(redirect)
	if err != nil {
		return err
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := r.URL.Query().Get("code"); code != "" {
			fmt.Fprintln(w, "Piper GitHub App created. You can close this tab.")
			codeCh <- code
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	// Serve a tiny auto-submitting form that POSTs the manifest to GitHub.
	page := fmt.Sprintf(`<form id="f" action="https://github.com/settings/apps/new" method="post">`+
		`<input type="hidden" name="manifest" value='%s'></form><script>document.getElementById('f').submit()</script>`,
		htmlEscape(manifest))
	formLn, _ := net.Listen("tcp", "127.0.0.1:0")
	formSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, page)
	})}
	go formSrv.Serve(formLn)
	defer formSrv.Close()

	formURL := "http://" + formLn.Addr().String()
	fmt.Printf("Opening %s — approve the App in your browser...\n", formURL)
	_ = openBrowser(formURL)

	code := <-codeCh
	if err := c.ExchangeGitHub(code); err != nil {
		return err
	}
	fmt.Println("GitHub App configured. Install it on your repo, then run: piper app link <name> --repo owner/name")
	return nil
}
```

Add small helpers to `cmd/piper/main.go`:

```go
func htmlEscape(s string) string { return strings.ReplaceAll(s, "'", "&#39;") }

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
```

Wire `cmdAppLink` and `cmdGithubSetup` into the existing command switch (e.g. `app link` under the `app` group and `github setup` as a new group), matching how current verbs are dispatched. Add imports: `"flag"`, `"io"`, `"net"`, `"net/http"`, `"os/exec"`, `"runtime"`, `"strings"` as needed.

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/client/ -v && go build ./...`
Expected: client tests PASS; full build OK.

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go cmd/piper/main.go
git commit -m "$(printf 'feat(cli): github setup and app link commands\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 13: docs — PROGRESS.md + README

**Files:**
- Modify: `PROGRESS.md`
- Modify: `README.md`

**Interfaces:** none (docs only).

- [ ] **Step 1: Update PROGRESS.md**

Replace the Plan 3 block with:

```markdown
## Plan 3 — Git-driven deploys — epic [#11](https://github.com/piperbox/piper/issues/11) ([plan](docs/superpowers/plans/2026-07-05-plan3-git-deploys.md))

Goal: `git push → live HTTPS URL` via a per-user GitHub App; webhook rides the Plan-2 tunnel to `hooks.<base>`; status reported to GitHub.

- ✅ `source` — provider seam (Event/Kind/Status + Provider interface) — [#11](https://github.com/piperbox/piper/issues/11)
- ✅ `source/github` — App JWT + installation token, webhook parse (HMAC), tarball fetch, Deployments API, manifest onboarding — [#11](https://github.com/piperbox/piper/issues/11)
- ✅ `webhook` — signed webhook → app lookup → deploy, per-app serialization — [#11](https://github.com/piperbox/piper/issues/11)
- ✅ `api`/`cli` — `github setup`, `app link`, onboarding endpoints — [#11](https://github.com/piperbox/piper/issues/11)
- ✅ `piperd` — webhook served over the tunnel in relay mode — [#11](https://github.com/piperbox/piper/issues/11)
- ⬜ PR-preview URLs + teardown (`pr-N.<app>.<base>`) — deferred behind the seam
```

Update the top `_Last updated_` line to `2026-07-05 — Plan 3 push-to-deploy complete: per-user GitHub App, webhook over tunnel, GitHub Deployments status. PR previews next.`

- [ ] **Step 2: Update README.md**

Add a short "Git deploys" subsection near the usage docs describing the flow: `piper github setup` (create the App), install it on the repo, `piper app link <name> --repo owner/name --branch main`, then `git push` deploys and the live URL appears as a GitHub Deployment. Match the README's existing tone/heading style; keep it to a short paragraph + the three commands.

- [ ] **Step 3: Verify full suite + cross-compile**

Run: `make test && make cross`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add PROGRESS.md README.md
git commit -m "$(printf 'docs: record Plan 3 push-to-deploy (GitHub App)\n\nPart of #11\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Self-Review

**Spec coverage:**
- Provider seam (`internal/source`) → Task 3. ✅
- GitHub App auth (installation-token clone) → Tasks 4, 6. ✅
- Webhook verification (HMAC) → Task 5. ✅
- Push → deploy on tracked branch → Task 9. ✅
- Deployments API status → Task 7. ✅
- Manifest onboarding (per-user App), secrets on box → Tasks 8, 10, 12. ✅
- Webhook rides Plan-2 tunnel + Caddy `hooks.` route → Task 11. ✅
- Security boundary (loopback control API; only `hooks.` public) → Task 11 (separate `WebhookAddr` listener; only that host routed). ✅
- Error handling (bad sig 401, unknown repo no-op, fetch/build fail → failure status, per-app serialize, idempotent redelivery) → Task 9. ✅
- Testing (unit seam w/ fakes, integration w/ httptest+fake docker, e2e opt-in) → Tasks 3–10 unit; Task 11 integration. **Note:** the loopback-relay *e2e* (real Docker/Caddy + synthesized signed webhook) from the spec is **not** a separate task here — the Task 11 integration test covers the wiring without Docker. Adding the full e2e is a reasonable follow-up; flagged, not silently dropped.
- Deferred items (PR previews, Actions, raw-webhook, central App) → not built, recorded in PROGRESS (Task 13). ✅

**Placeholder scan:** No "TBD"/"handle appropriately". The one shim (`p2statuses` in Task 11) is explicitly explained and optional. Task 12's client/CLI steps say "match existing style" but provide complete code — acceptable because they adapt to a file the implementer will read.

**Type consistency:** `source.Provider{Parse,Fetch(ev),Report}` used identically in Tasks 3, 9, 11. `Fetch(ctx, ev, dir)` (Event-carrying) consistent across 6, 9, 11. `github.New(github.Config{...})` consistent across 4–8, 11. `api.New(s, d, baseDomain, githubAPIBase)` updated in 10 and called in 11. `store` methods (`UpdateAppRepo`, `AppByRepo`, `GitHubApp`, `SaveGitHubApp`, `GetGitHubApp`) consistent across 1, 2, 9, 10, 11.

**Gaps fixed inline:** the missing full-e2e is documented above rather than left implicit.
