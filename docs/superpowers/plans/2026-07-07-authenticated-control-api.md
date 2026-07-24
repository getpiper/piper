# Authenticated Control API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Require a bearer token on every `piperd` control-API request, minted on-box via `piperd token create` and supplied by the CLI after `piper login`.

**Architecture:** A new `tokens` table in the existing SQLite store (hash-at-rest, mirroring `internal/relay/store.go`). A composable `api.RequireToken` middleware wraps the control mux and is applied once in `cmd/piperd`. The CLI reads its token from `~/.piper/piper/config.json` (or `PIPER_TOKEN`) and sets `Authorization: Bearer`. Bootstrap is an on-box `piperd token` subcommand that writes the DB directly — no auth needed, because running it *is* proof of box ownership.

**Tech Stack:** Go (std `net/http`, `crypto/sha256`, `crypto/rand`), `modernc.org/sqlite` (pure Go), `github.com/google/uuid`.

## Global Constraints

- **No cgo.** Everything must build with `CGO_ENABLED=0`; only `modernc.org/sqlite` for SQLite. Verify with `make cross` (`CGO_ENABLED=0 GOOS=linux GOARCH=arm64`).
- **Module path** `github.com/piperbox/piper`.
- **Layering:** `store` knows only persistence; `api` is transport over `store`; `client` is the CLI's view of `api`. Nothing imports "up". `store` must **not** import `relay` (hence the `hashToken` helper is duplicated, not shared).
- **Defaults:** control API `127.0.0.1:8088`.
- **TDD:** every task is failing-test-first. Run `make test` and `make cross` before the final commit of any task that changes buildable code.
- **Commits:** conventional-commit style, one per task, ending with:
  ```
  Co-Authored-By: Claude {current model} <noreply@anthropic.com>
  ```
- **Branch:** all work on `ozykhan/auth-control-api` (already created); reference `Part of #49` / `Closes #72` in the PR body, not per-commit.

---

### Task 1: Store — API token model

**Files:**
- Modify: `internal/store/schema.sql` (add `tokens` table)
- Modify: `internal/store/store.go` (helper, `Token`, `ErrBadToken`, CRUD methods)
- Test: `internal/store/store_test.go` (append)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `store.ErrBadToken error`
  - `store.Token struct { ID, Label, Scope string; CreatedAt time.Time; RevokedAt *time.Time }`
  - `(*store.Store).CreateToken(label, scope string) (plaintext string, err error)`
  - `(*store.Store).AuthenticateToken(plaintext string) (store.Token, error)`
  - `(*store.Store).ListTokens() ([]store.Token, error)`
  - `(*store.Store).RevokeToken(label string) error`

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go`:

```go
func TestTokenCreateAuthenticateRevoke(t *testing.T) {
	s := openTemp(t)
	tok, err := s.CreateToken("laptop", "admin")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	got, err := s.AuthenticateToken(tok)
	if err != nil {
		t.Fatalf("AuthenticateToken: %v", err)
	}
	if got.Label != "laptop" || got.Scope != "admin" {
		t.Errorf("got %+v", got)
	}
	if _, err := s.AuthenticateToken("nope"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("unknown token: want ErrBadToken, got %v", err)
	}
	if err := s.RevokeToken("laptop"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := s.AuthenticateToken(tok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("after revoke: want ErrBadToken, got %v", err)
	}
	if err := s.RevokeToken("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke unknown: want ErrNotFound, got %v", err)
	}
}

func TestTokenDuplicateLabelRejected(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateToken("laptop", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken("laptop", "admin"); err == nil {
		t.Fatal("want error on duplicate label")
	}
}

func TestListTokens(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateToken("a", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken("b", "readonly"); err != nil {
		t.Fatal(err)
	}
	toks, err := s.ListTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 {
		t.Fatalf("len = %d, want 2", len(toks))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'Token' -v`
Expected: FAIL — `s.CreateToken undefined` (compile error).

- [ ] **Step 3: Add the `tokens` table**

Append to `internal/store/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS tokens (
    id         TEXT PRIMARY KEY,
    label      TEXT NOT NULL UNIQUE,
    token_hash TEXT NOT NULL UNIQUE,
    scope      TEXT NOT NULL DEFAULT 'admin',
    created_at TEXT NOT NULL,
    revoked_at TEXT
);
```

- [ ] **Step 4: Implement the store methods**

In `internal/store/store.go`, extend the import block to add the three crypto/encoding imports:

```go
import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)
```

Append to `internal/store/store.go`:

```go
// ErrBadToken is returned when a token is unknown or has been revoked.
var ErrBadToken = errors.New("bad token")

type Token struct {
	ID        string
	Label     string
	Scope     string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// hashToken is the at-rest representation of an API token. Duplicated from
// internal/relay (store must not import relay); it is a three-line helper.
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// CreateToken mints a random token with the given label and scope, stores only
// its hash, and returns the plaintext once.
func (s *Store) CreateToken(label, scope string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	_, err := s.db.Exec(
		`INSERT INTO tokens(id, label, token_hash, scope, created_at) VALUES(?,?,?,?,?)`,
		uuid.NewString(), label, hashToken(tok), scope,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return tok, nil
}

// AuthenticateToken resolves a plaintext token to its Token, or ErrBadToken if
// the token is unknown or revoked.
func (s *Store) AuthenticateToken(tok string) (Token, error) {
	var t Token
	var created string
	var revoked sql.NullString
	err := s.db.QueryRow(
		`SELECT id, label, scope, created_at, revoked_at FROM tokens WHERE token_hash=?`,
		hashToken(tok)).Scan(&t.ID, &t.Label, &t.Scope, &created, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrBadToken
	}
	if err != nil {
		return Token{}, err
	}
	if revoked.Valid {
		return Token{}, ErrBadToken
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return t, nil
}

// ListTokens returns all tokens (metadata only; never the plaintext or hash).
func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.db.Query(
		`SELECT id, label, scope, created_at, revoked_at FROM tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var created string
		var revoked sql.NullString
		if err := rows.Scan(&t.ID, &t.Label, &t.Scope, &created, &revoked); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if revoked.Valid {
			rt, _ := time.Parse(time.RFC3339Nano, revoked.String)
			t.RevokedAt = &rt
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken marks the token with the given label revoked. ErrNotFound if no
// active token with that label exists.
func (s *Store) RevokeToken(label string) error {
	res, err := s.db.Exec(
		`UPDATE tokens SET revoked_at=? WHERE label=? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), label)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'Token' -v`
Expected: PASS (all three).

- [ ] **Step 6: Commit**

```bash
git add internal/store/schema.sql internal/store/store.go internal/store/store_test.go
git commit -m "$(cat <<'EOF'
feat(store): API tokens (create/authenticate/list/revoke)

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: API — RequireToken middleware

**Files:**
- Modify: `internal/api/api.go` (add `RequireToken`, `bearerToken`)
- Modify: `cmd/piperd/main.go:106` (wrap the handler with `api.RequireToken`)
- Test: `internal/api/auth_test.go` (new)

**Interfaces:**
- Consumes: `(*store.Store).AuthenticateToken` (Task 1).
- Produces: `api.RequireToken(s *store.Store, next http.Handler) http.Handler` — returns 401 unless the request carries a valid `Authorization: Bearer <token>`.

Note: middleware is a **separate composable function**, not folded into `New`, so the existing route tests stay unchanged and the auth logic is tested in isolation. `cmd/piperd` is the only production caller and always wraps, so there is no unauthenticated path in the running daemon.

- [ ] **Step 1: Write the failing test**

Create `internal/api/auth_test.go`:

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireTokenRejectsAndAccepts(t *testing.T) {
	s := newTestStore(t)
	tok, err := s.CreateToken("cli", "admin")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
	h := RequireToken(s, next)

	cases := []struct {
		name       string
		header     string
		wantCode   int
		wantReach  bool
	}{
		{"no header", "", http.StatusUnauthorized, false},
		{"bad token", "Bearer nope", http.StatusUnauthorized, false},
		{"not bearer", "Basic xyz", http.StatusUnauthorized, false},
		{"valid", "Bearer " + tok, http.StatusOK, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantCode || reached != c.wantReach {
				t.Fatalf("code=%d reached=%v, want code=%d reached=%v",
					rec.Code, reached, c.wantCode, c.wantReach)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestRequireToken -v`
Expected: FAIL — `undefined: RequireToken`.

- [ ] **Step 3: Implement the middleware**

Append to `internal/api/api.go`:

```go
// RequireToken wraps next so every request must carry a valid
// `Authorization: Bearer <token>`. Unknown, malformed, or revoked tokens get 401.
func RequireToken(s *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		if _, err := s.AuthenticateToken(tok); err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return tok, tok != ""
}
```

(`net/http`, `strings`, and `store` are already imported in `api.go`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api/ -run TestRequireToken -v`
Expected: PASS (all four subtests).

- [ ] **Step 5: Wire it into the daemon**

In `cmd/piperd/main.go`, replace the handler construction (currently around line 106):

```go
	handler := api.New(st, dep, cfg.BaseDomain, "", func() {
		if wh != nil {
			wh.start()
		}
	})
```

with:

```go
	handler := api.RequireToken(st, api.New(st, dep, cfg.BaseDomain, "", func() {
		if wh != nil {
			wh.start()
		}
	}))
```

- [ ] **Step 6: Verify the whole build and suite**

Run: `make test && make cross`
Expected: PASS; `piperd` compiles.

- [ ] **Step 7: Commit**

```bash
git add internal/api/api.go internal/api/auth_test.go cmd/piperd/main.go
git commit -m "$(cat <<'EOF'
feat(api): require bearer token on the control plane

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: piperd — `token` subcommand (bootstrap)

**Files:**
- Modify: `cmd/piperd/main.go` (arg dispatch + `runTokenCmd`)
- Test: `cmd/piperd/token_test.go` (new)

**Interfaces:**
- Consumes: `CreateToken`, `ListTokens`, `RevokeToken` (Task 1).
- Produces:
  - `tokenStore interface { CreateToken(label, scope string) (string, error); ListTokens() ([]store.Token, error); RevokeToken(label string) error }`
  - `runTokenCmd(st tokenStore, args []string, out io.Writer) error`

- [ ] **Step 1: Write the failing tests**

Create `cmd/piperd/token_test.go`:

```go
package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/store"
)

func tokenTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "piperd.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTokenCmdCreateListRevoke(t *testing.T) {
	s := tokenTestStore(t)
	var out bytes.Buffer

	if err := runTokenCmd(s, []string{"create", "--name", "laptop"}, &out); err != nil {
		t.Fatalf("create: %v", err)
	}
	tok := strings.TrimSpace(out.String())
	if tok == "" {
		t.Fatal("no token printed")
	}
	if _, err := s.AuthenticateToken(tok); err != nil {
		t.Fatalf("created token not valid: %v", err)
	}

	out.Reset()
	if err := runTokenCmd(s, []string{"list"}, &out); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "laptop") {
		t.Fatalf("list missing token: %q", out.String())
	}

	out.Reset()
	if err := runTokenCmd(s, []string{"revoke", "laptop"}, &out); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.AuthenticateToken(tok); err == nil {
		t.Fatal("token still valid after revoke")
	}
}

func TestTokenCmdCreateRequiresName(t *testing.T) {
	s := tokenTestStore(t)
	if err := runTokenCmd(s, []string{"create"}, &bytes.Buffer{}); err == nil {
		t.Fatal("want error when --name missing")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/piperd/ -run TokenCmd -v`
Expected: FAIL — `undefined: runTokenCmd`.

- [ ] **Step 3: Implement `runTokenCmd` and dispatch**

In `cmd/piperd/main.go`, add `"flag"` and `"io"` to the import block. Add the dispatch at the very top of `main()`, before `cfg := config.Load()`:

```go
	if len(os.Args) > 1 && os.Args[1] == "token" {
		st, err := store.Open(filepath.Join(config.Load().DataDir, "piper.db"))
		if err != nil {
			log.Fatalf("store: %v", err)
		}
		defer st.Close()
		if err := runTokenCmd(st, os.Args[2:], os.Stdout); err != nil {
			log.Fatalf("token: %v", err)
		}
		return
	}
```

Add these definitions (e.g. above `main`):

```go
type tokenStore interface {
	CreateToken(label, scope string) (string, error)
	ListTokens() ([]store.Token, error)
	RevokeToken(label string) error
}

// runTokenCmd implements `piperd token <create|list|revoke>`, writing directly
// to the on-box store. It needs no auth: running it is proof of box ownership.
func runTokenCmd(st tokenStore, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: piperd token <create|list|revoke>")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		name := fs.String("name", "", "label for the token")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("token create: --name is required")
		}
		tok, err := st.CreateToken(*name, "admin")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, tok)
		return nil
	case "list":
		toks, err := st.ListTokens()
		if err != nil {
			return err
		}
		for _, tk := range toks {
			status := "active"
			if tk.RevokedAt != nil {
				status = "revoked"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\n", tk.Label, tk.Scope, status)
		}
		return nil
	case "revoke":
		if len(args) < 2 {
			return fmt.Errorf("usage: piperd token revoke <name>")
		}
		return st.RevokeToken(args[1])
	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/piperd/ -run TokenCmd -v`
Expected: PASS (both).

- [ ] **Step 5: Verify the whole build and suite**

Run: `make test && make cross`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/piperd/main.go cmd/piperd/token_test.go
git commit -m "$(cat <<'EOF'
feat(agent): piperd token create/list/revoke (bootstrap)

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: config — CLI credentials file + data-dir default

**Files:**
- Modify: `internal/config/config.go` (data-dir default, `ClientConfig`, `LoadClient`, `SaveClient`)
- Test: `internal/config/config_test.go` (append)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `config.ClientConfig struct { Addr string ` + "`json:\"addr\"`" + `; Token string ` + "`json:\"token\"`" + ` }`
  - `config.LoadClient() (ClientConfig, error)` — reads `~/.piper/piper/config.json`, applies `PIPER_ADDR`/`PIPER_TOKEN` overrides, defaults `Addr` to `http://127.0.0.1:8088`.
  - `config.SaveClient(ClientConfig) error` — writes that file, mode `0600`.
  - `config.Load().DataDir` now defaults to `~/.piper/piperd`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go` (and add `"path/filepath"` to its imports):

```go
func TestDefaultDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PIPER_DATA_DIR", "")
	got := Load().DataDir
	want := filepath.Join(home, ".piper", "piperd")
	if got != want {
		t.Fatalf("DataDir = %q, want %q", got, want)
	}
}

func TestLoadClientDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Addr != "http://127.0.0.1:8088" || cc.Token != "" {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestSaveAndLoadClient(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClient(ClientConfig{Addr: "http://box:8088", Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Addr != "http://box:8088" || cc.Token != "secret" {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestLoadClientEnvOverridesFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClient(ClientConfig{Addr: "http://box:8088", Token: "filetok"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_TOKEN", "envtok")
	cc, _ := LoadClient()
	if cc.Token != "envtok" {
		t.Fatalf("token = %q, want envtok", cc.Token)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'DataDir|Client' -v`
Expected: FAIL — `undefined: LoadClient` and DataDir mismatch (`./data`).

- [ ] **Step 3: Implement**

Replace the import line in `internal/config/config.go`:

```go
import "os"
```

with:

```go
import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)
```

Change the `DataDir` line inside `Load()` from:

```go
		DataDir:     env("PIPER_DATA_DIR", "./data"),
```

to:

```go
		DataDir:     env("PIPER_DATA_DIR", defaultDataDir()),
```

Append to `internal/config/config.go`:

```go
// defaultDataDir is piperd's SQLite home when PIPER_DATA_DIR is unset:
// ~/.piper/piperd. Falls back to ./data if the home dir can't be resolved.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "./data"
	}
	return filepath.Join(home, ".piper", "piperd")
}

// ClientConfig is the piper CLI's saved credentials/target.
type ClientConfig struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

func clientConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".piper", "piper", "config.json"), nil
}

// LoadClient reads ~/.piper/piper/config.json, then applies PIPER_ADDR /
// PIPER_TOKEN env overrides and the localhost default for Addr. A missing file
// is not an error.
func LoadClient() (ClientConfig, error) {
	var cc ClientConfig
	path, err := clientConfigPath()
	if err != nil {
		return cc, err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &cc)
	} else if !errors.Is(err, os.ErrNotExist) {
		return cc, err
	}
	if v := os.Getenv("PIPER_ADDR"); v != "" {
		cc.Addr = v
	}
	if cc.Addr == "" {
		cc.Addr = "http://127.0.0.1:8088"
	}
	if v := os.Getenv("PIPER_TOKEN"); v != "" {
		cc.Token = v
	}
	return cc, nil
}

// SaveClient writes cc to ~/.piper/piper/config.json with 0600 perms, creating
// the directory if needed.
func SaveClient(cc ClientConfig) error {
	path, err := clientConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -run 'DataDir|Client' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
feat(config): ~/.piper/piper credentials + ~/.piper/piperd data dir

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: client — attach the bearer token

**Files:**
- Modify: `internal/client/client.go` (`New` signature + `Authorization` header)
- Modify: `internal/client/client_test.go` (two-arg `New`; header assertion)
- Modify: `cmd/piper/main.go` (build client from `config.LoadClient`)
- Test: covered by `internal/client/client_test.go`

**Interfaces:**
- Consumes: `config.LoadClient` (Task 4).
- Produces: `client.New(base, token string) *Client` — sets `Authorization: Bearer <token>` on every request when `token != ""`.

- [ ] **Step 1: Write the failing test**

Append to `internal/client/client_test.go`:

```go
func TestSetsAuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode([]store.App{})
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "s3cr3t").ListApps(); err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("Authorization = %q, want Bearer s3cr3t", gotAuth)
	}
}
```

- [ ] **Step 2: Update existing `New(...)` call sites in the test to two args**

In `internal/client/client_test.go`, change every `New(srv.URL)` to `New(srv.URL, "")` — there are 6: in `TestListApps`, `TestCreateApp`, `TestDeploy`, `TestLinkApp`, `TestManifestAndExchange`, `TestClientMethodsReportHTTPError`.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/client/ -run Authorization -v`
Expected: FAIL — `too many arguments in call to New` (compile error) until Step 4.

- [ ] **Step 4: Implement the token-carrying client**

Replace the `Client` struct and `New` in `internal/client/client.go`:

```go
type Client struct {
	base  string
	token string
	http  *http.Client
}

func New(base, token string) *Client {
	if base == "" {
		base = "http://127.0.0.1:8088"
	}
	return &Client{base: base, token: token, http: &http.Client{}}
}

// do builds a request to c.base+path, attaches the auth header (when set) and
// the content type (when non-empty), and sends it.
func (c *Client) do(method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.http.Do(req)
}
```

Then route the existing methods through `do`. Replace each `c.http.Get(...)` / `c.http.Post(...)` call:

- `CreateApp`: `resp, err := c.do(http.MethodPost, "/v1/apps", "application/json", bytes.NewReader(body))`
- `ListApps`: `resp, err := c.do(http.MethodGet, "/v1/apps", "", nil)`
- `Deploy`: `resp, err := c.do(http.MethodPost, "/v1/apps/"+name+"/deploy", "application/x-tar", &body)`
- `LinkApp`: `resp, err := c.do(http.MethodPost, "/v1/apps/"+name+"/link", "application/json", bytes.NewReader(body))`
- `Manifest`: `resp, err := c.do(http.MethodPost, "/v1/github/manifest", "application/json", bytes.NewReader(body))`
- `ExchangeGitHub`: `resp, err := c.do(http.MethodPost, "/v1/github/exchange", "application/json", bytes.NewReader(body))`

(`io` is already imported in `client.go`.)

- [ ] **Step 5: Update the CLI call sites**

In `cmd/piper/main.go`, add a helper (e.g. above `run`):

```go
func dialClient(stderr io.Writer) (*client.Client, bool) {
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, false
	}
	return client.New(cc.Addr, cc.Token), true
}
```

Replace the client constructions:

- In `run` `create`: replace
  `if err := client.New(config.ClientAddr()).CreateApp(name, *port); err != nil {`
  with:
  ```go
  c, ok := dialClient(stderr)
  if !ok {
  	return 1
  }
  if err := c.CreateApp(name, *port); err != nil {
  ```
- In `run` `deploy`: replace `dep, err := client.New(config.ClientAddr()).Deploy(name, *path)` with:
  ```go
  c, ok := dialClient(stderr)
  if !ok {
  	return 1
  }
  dep, err := c.Deploy(name, *path)
  ```
- In `run` `list`: replace `apps, err := client.New(config.ClientAddr()).ListApps()` with:
  ```go
  c, ok := dialClient(stderr)
  if !ok {
  	return 1
  }
  apps, err := c.ListApps()
  ```
- In `cmdApp`: replace `if err := client.New(config.ClientAddr()).LinkApp(name, *repo, *branch); err != nil {` with:
  ```go
  c, ok := dialClient(stderr)
  if !ok {
  	return 1
  }
  if err := c.LinkApp(name, *repo, *branch); err != nil {
  ```
- In `githubSetup`: replace `c := client.New(config.ClientAddr())` with:
  ```go
  c, ok := dialClient(stderr)
  if !ok {
  	return 1
  }
  ```

- [ ] **Step 6: Run tests and full build**

Run: `go test ./internal/client/ -v && make test && make cross`
Expected: PASS; `piper` compiles. (`config.ClientAddr` remains defined and still covered by its own test; it is simply no longer called by the CLI.)

- [ ] **Step 7: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go cmd/piper/main.go
git commit -m "$(cat <<'EOF'
feat(cli): send Authorization: Bearer from saved credentials

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: piper — `login` command

**Files:**
- Modify: `cmd/piper/main.go` (`login` case, `login` func, usage line)
- Test: `cmd/piper/login_test.go` (new)

**Interfaces:**
- Consumes: `config.LoadClient`, `config.SaveClient` (Task 4); `client.New` + `ListApps` (Task 5).
- Produces: `piper login --token <t> [--addr <url>]` — verifies the token against the target with `GET /v1/apps`, then writes `~/.piper/piper/config.json`. Non-zero exit and no write on failure.

- [ ] **Step 1: Write the failing tests**

Create `cmd/piper/login_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/store"
)

func TestLoginSavesConfigOnSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]store.App{})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--addr", srv.URL, "--token", "good"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Token != "good" || cc.Addr != srv.URL {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestLoginRejectsBadToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--addr", srv.URL, "--token", "bad"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	cc, _ := config.LoadClient()
	if cc.Token != "" {
		t.Fatalf("token should not be saved, got %q", cc.Token)
	}
}

func TestLoginRequiresToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := run([]string{"login"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/piper/ -run Login -v`
Expected: FAIL — `login` is an unknown command, so `run` returns 2 (usage) for the success case → assertion fails.

- [ ] **Step 3: Implement the `login` command**

In `cmd/piper/main.go`, add a case to the `switch args[0]` in `run`, before `default:`:

```go
	case "login":
		fs := flag.NewFlagSet("login", flag.ContinueOnError)
		fs.SetOutput(stderr)
		token := fs.String("token", "", "API token from `piperd token create`")
		addr := fs.String("addr", "", "piperd address (default http://127.0.0.1:8088)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		return login(*addr, *token, stdout, stderr)
```

Add the `login` function:

```go
// login verifies token against the target (GET /v1/apps) and, on success,
// saves it to ~/.piper/piper/config.json.
func login(addr, token string, stdout, stderr io.Writer) int {
	if token == "" {
		fmt.Fprintln(stderr, "usage: piper login --token <token>  (create one with `piperd token create`)")
		return 2
	}
	if addr == "" {
		cc, err := config.LoadClient()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		addr = cc.Addr
	}
	if _, err := client.New(addr, token).ListApps(); err != nil {
		fmt.Fprintln(stderr, "error: token rejected:", err)
		return 1
	}
	if err := config.SaveClient(config.ClientConfig{Addr: addr, Token: token}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "logged in to %s\n", addr)
	return 0
}
```

Update the `usage` line to include `login`:

```go
func usage(w io.Writer) int {
	fmt.Fprintln(w, "usage: piper <version|login|create|deploy|list|app|github> [args]")
	return 2
}
```

(`flag`, `fmt`, `io` are already imported in `cmd/piper/main.go`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/piper/ -run Login -v`
Expected: PASS (all three).

- [ ] **Step 5: Verify the whole build and suite**

Run: `make test && make cross`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/piper/main.go cmd/piper/login_test.go
git commit -m "$(cat <<'EOF'
feat(cli): piper login stores a verified token in ~/.piper/piper

Part of #49

Co-Authored-By: Claude {current model} <noreply@anthropic.com>
EOF
)"
```

---

## Post-plan: docs & PR

After Task 6, before opening the PR:

- Update `README.md` / any quickstart that shows `piper create …` to note the one-time `piperd token create` + `piper login` step (the API now rejects unauthenticated calls). Keep it terse.
- Update `PROGRESS.md`: mark #72 landed with a one-line entry + `[#72]`.
- Open the PR into `main` with body containing `Closes #72` and `Part of #49`; squash-merge.

## Self-Review notes (author)

- **Spec coverage:** tokens table + hash-at-rest (T1); always-on middleware, no loopback bypass (T2); `piperd token` bootstrap (T3); `~/.piper/piper` + `~/.piper/piperd` + scope-column-defaulted-`admin` (T1 schema, T4); `piper login` + verify + client header + `PIPER_TOKEN` override (T4–T6). Scope **enforcement** intentionally deferred (spec §Decisions). Webhook server untouched (not referenced). ✅
- **Deviation from spec wording:** spec §Components/2 said "wrap inside `api.New`"; plan uses a composable `api.RequireToken` wired in `main.go` instead — same guarantee (daemon always wraps), far less test churn, auth unit-tested in isolation. Called out for reviewer.
- **Type consistency:** `Token`, `ErrBadToken`, `CreateToken/AuthenticateToken/ListTokens/RevokeToken`, `RequireToken`, `ClientConfig`, `LoadClient/SaveClient`, `client.New(base, token)`, `runTokenCmd`/`tokenStore` used identically across tasks. ✅
- **Leftover:** `config.ClientAddr()` is no longer called by the CLI but remains defined and tested; left in place deliberately (harmless, still-tested exported helper) rather than touching its unrelated test.
