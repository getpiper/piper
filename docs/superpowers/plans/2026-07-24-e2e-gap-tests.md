# e2e Gap Tests Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Two new e2e tests closing the suite's biggest gaps: a failed deploy never takes down the running version, and the Plan-3 `webhook → deploy → PR preview` loop works binary-to-binary through the relay.

**Architecture:** Test 1 extends the LAN harness in `test/e2e/deploy_test.go`. Test 2 is a new passthrough-relay test (`test/e2e/webhook_test.go`) that delivers HMAC-signed synthetic GitHub events by TLS-dialing the relay with SNI `hooks.<base>`, with GitHub's API stubbed by an in-test server reached through one new env seam (`PIPER_GITHUB_API_BASE`).

**Tech Stack:** Go stdlib tests, real Docker + embedded Caddy (existing e2e harness), `modernc.org/sqlite` via `internal/store` for seeding.

**Spec:** `docs/superpowers/specs/2026-07-24-e2e-gap-tests-design.md`

## Global Constraints

- Every e2e test gates on `RUN_E2E=1` and must skip cleanly without it (`make test` stays green with no Docker).
- No cgo anywhere; `make verify` (gofmt → vet → test → cross) must pass after every task.
- Deployment status strings are exactly `"building"`, `"running"`, `"failed"`, `"stopped"`.
- Defaults in play: control API `127.0.0.1:8088`, app container port `8080`, webhook listener `127.0.0.1:8089`.
- Commits reference the tracking issue (`Part of #N`); conventional-commit style; co-author trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- Branch: `faruk/e2e-gap-tests` (already created, spec committed).

---

### Task 1: Tracking issue + LAN harness helpers + `TestDeployFailureAndRedeploy`

**Files:**
- Modify: `test/e2e/deploy_test.go`

**Interfaces:**
- Consumes: `client.New(addr, token)`, `c.CreateApp/Deploy/FollowDeploy/ListApps/Deployments` (all exist in `internal/client`), `waitPort(t, addr, d)` (exists in this file).
- Produces: `startLANPiperd(t *testing.T)` — builds and boots a LAN-only piperd on `:8088`, cleanup via `t.Cleanup`; `dockerfileFor(body string) string` — a single-file netcat app image serving `body`; Task 3 reuses `dockerfileFor`.

- [ ] **Step 1: Create the tracking issue**

```bash
gh issue create \
  --title "[deploy] e2e: failed deploy keeps the old version serving; webhook push → PR-preview loop" \
  --label enhancement,P2,size/M,agent,relay \
  --body "$(cat <<'EOF'
Two e2e gaps, per docs/superpowers/specs/2026-07-24-e2e-gap-tests-design.md:

1. **TestDeployFailureAndRedeploy** (LAN harness): deploy v1 → broken deploy ends \`failed\` while v1 keeps serving → v2 deploy swaps the served body; history shows stopped/failed/running.
2. **TestWebhookPushAndPreview** (passthrough-relay harness): HMAC-signed synthetic push delivered via relay SNI \`hooks.<base>\` → box builds from a stub GitHub tarball → app serves; PR opened → \`pr-7-<app>.<base>\` serves; PR closed → preview gone. Needs one seam: \`PIPER_GITHUB_API_BASE\`.
EOF
)"
```

Note the issue number `#N`; every commit below says `Part of #N` and the PR body will say `Closes #N`.

- [ ] **Step 2: Extract the boot helper and Dockerfile generator**

In `test/e2e/deploy_test.go`, replace the build/boot block of `TestEndToEndDeploy` (the lines from `dataDir := t.TempDir()` through `waitPort(t, "127.0.0.1:8088", 15*time.Second)`, keeping `repoRoot, _ := filepath.Abs("../..")` in the test) with a call to `startLANPiperd(t)`, and add these two helpers at the bottom of the file (add `"strings"` to imports):

```go
// startLANPiperd builds piperd and boots it LAN-only against a fresh temp data
// dir, killing it on test cleanup. Returns once the control API accepts.
func startLANPiperd(t *testing.T) {
	t.Helper()
	repoRoot, _ := filepath.Abs("../..")
	bin := filepath.Join(t.TempDir(), "piperd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/piperd")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build piperd: %v\n%s", err, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+t.TempDir(),
		"PIPER_API_ADDR=127.0.0.1:8088",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	waitPort(t, "127.0.0.1:8088", 15*time.Second)
}

// dockerfileFor returns a one-file app image serving body on :8080, the same
// netcat single-liner as sampleapp. body must end in "\n" and contain no
// quotes or backslashes — it is spliced into a shell printf.
func dockerfileFor(body string) string {
	return "FROM alpine:3.20\nRUN apk add --no-cache netcat-openbsd\nEXPOSE 8080\n" +
		fmt.Sprintf("CMD while true; do printf 'HTTP/1.1 200 OK\\r\\nContent-Length: %d\\r\\n\\r\\n%s\\n' | nc -l -p 8080; done\n",
			len(body), strings.TrimSuffix(body, "\n"))
}
```

- [ ] **Step 3: Verify the refactor didn't break the existing test**

Run: `RUN_E2E=1 go test ./test/e2e/ -run TestEndToEndDeploy -count=1 -v` (needs Docker and free :80/:8088/:2019; if Docker is unavailable locally, run `go vet ./test/e2e/` and `go test ./test/e2e/` — the skip path — and rely on CI's e2e job).
Expected: PASS (or clean skip without Docker).

- [ ] **Step 4: Write `TestDeployFailureAndRedeploy`**

Append to `test/e2e/deploy_test.go`:

```go
// TestDeployFailureAndRedeploy proves the deploy orchestrator's core promise
// end-to-end: a failed build ends "failed" and never touches the running
// version, and the next good deploy swaps the served body.
func TestDeployFailureAndRedeploy(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker + free :80/:8088/:2019)")
	}
	repoRoot, _ := filepath.Abs("../..")
	startLANPiperd(t)

	c := client.New("http://127.0.0.1:8088", "")
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	deployAndFollow := func(srcDir, wantStatus string) {
		t.Helper()
		dep, err := c.Deploy("blog", srcDir)
		if err != nil {
			t.Fatalf("Deploy(%s): %v", srcDir, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		final, err := c.FollowDeploy(ctx, "blog", dep.ID, io.Discard)
		if err != nil {
			t.Fatalf("FollowDeploy(%s): %v", srcDir, err)
		}
		if final.Status != wantStatus {
			t.Fatalf("deploy from %s: status = %q, want %q", srcDir, final.Status, wantStatus)
		}
	}
	writeApp := func(dockerfile string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	assertServes := func(want string) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://127.0.0.1:80/", nil)
		req.Host = "blog.piper.localhost"
		deadline := time.Now().Add(20 * time.Second)
		var last string
		for time.Now().Before(deadline) {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				last = err.Error()
			} else {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK && string(b) == want {
					return
				}
				last = fmt.Sprintf("status %d body %q", resp.StatusCode, b)
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("never served %q: last %s", want, last)
	}

	// v1: the checked-in sample app.
	deployAndFollow(filepath.Join(repoRoot, "test/e2e/sampleapp"), "running")
	assertServes("hello piper\n")

	// Broken build → "failed", and v1 keeps serving; the app row stays running.
	deployAndFollow(writeApp("FROM alpine:3.20\nRUN false\n"), "failed")
	assertServes("hello piper\n")
	apps, err := c.ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Status != "running" {
		t.Fatalf("apps after failed deploy = %+v, want blog running", apps)
	}

	// v2: a good redeploy swaps the served body.
	deployAndFollow(writeApp(dockerfileFor("hello piper v2\n")), "running")
	assertServes("hello piper v2\n")

	// History: v1 retired to "stopped", the broken row "failed", v2 "running".
	deps, err := c.Deployments("blog")
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	got := map[string]int{}
	for _, d := range deps {
		got[d.Status]++
	}
	want := map[string]int{"stopped": 1, "failed": 1, "running": 1}
	if len(deps) != 3 || got["stopped"] != want["stopped"] || got["failed"] != want["failed"] || got["running"] != want["running"] {
		t.Fatalf("deployment history statuses = %v (%d rows), want one each of stopped/failed/running", got, len(deps))
	}
}
```

- [ ] **Step 5: Run the new test**

Run: `RUN_E2E=1 go test ./test/e2e/ -run TestDeployFailureAndRedeploy -count=1 -v`
Expected: PASS. If the failed-deploy assertion trips (v1 stops serving), that is a real product bug — stop and use superpowers:systematic-debugging; do not adjust the test to pass.

- [ ] **Step 6: Verify and commit**

Run: `make verify`
Expected: all four gates pass.

```bash
git add test/e2e/deploy_test.go
git commit -m "test(e2e): failed deploy keeps the old version serving; redeploy swaps it

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: `PIPER_GITHUB_API_BASE` seam

**Files:**
- Modify: `internal/config/config.go` (Config struct + `Load`)
- Modify: `cmd/piperd/main.go` (three sites)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `github.Config.APIBase` (exists; empty ⇒ `https://api.github.com`).
- Produces: `config.Config.GitHubAPIBase string`, loaded from env `PIPER_GITHUB_API_BASE`; Task 3's test sets that env var on the piperd process.

- [ ] **Step 1: Write the failing config test**

Append to `internal/config/config_test.go` (match the file's existing `t.Setenv` style):

```go
func TestGitHubAPIBaseFromEnv(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
	t.Setenv("PIPER_GITHUB_API_BASE", "http://127.0.0.1:9999")
	cfg := Load()
	if cfg.GitHubAPIBase != "http://127.0.0.1:9999" {
		t.Fatalf("GitHubAPIBase = %q, want the env override", cfg.GitHubAPIBase)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/config/ -run TestGitHubAPIBaseFromEnv -v`
Expected: FAIL to compile — `cfg.GitHubAPIBase undefined`.

- [ ] **Step 3: Add the field and env read**

In `internal/config/config.go`, add to the `Config` struct (after `GitHubBrokered bool`):

```go
	GitHubAPIBase  string // GitHub API base URL override (tests); empty ⇒ https://api.github.com
```

and in `Load()`'s returned literal (after the `GitHubBrokered:` line):

```go
		GitHubAPIBase:  env("PIPER_GITHUB_API_BASE", ""),
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS (all config tests).

- [ ] **Step 5: Thread it through `cmd/piperd/main.go`**

Three edits:

1. In `newRepoFetcher`, the BYO branch's `github.New` call gains the field:

```go
			p, err := github.New(github.Config{
				AppID: gh.AppID, PrivateKeyPEM: gh.PrivateKey, WebhookSecret: gh.WebhookSecret,
				APIBase: cfg.GitHubAPIBase,
			})
```

2. In `webhookStarter.run`, the BYO branch's `github.New` call likewise:

```go
		p, err := github.New(github.Config{
			AppID: gh.AppID, PrivateKeyPEM: gh.PrivateKey, WebhookSecret: gh.WebhookSecret,
			APIBase: w.cfg.GitHubAPIBase,
		})
```

3. The `api.New(st, dep, cfg.BaseDomain, "", …)` call: replace the hardcoded `""` (the `githubAPIBase` parameter used by the manifest `ExchangeCode`) with `cfg.GitHubAPIBase`.

- [ ] **Step 6: Verify and commit**

Run: `make verify`
Expected: all gates pass (no behavior change unless the env var is set).

```bash
git add internal/config/config.go internal/config/config_test.go cmd/piperd/main.go
git commit -m "feat(agent): PIPER_GITHUB_API_BASE seam for pointing the GitHub provider at a stub

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Webhook e2e — harness + push → deploy → serve

**Files:**
- Create: `test/e2e/webhook_test.go`

**Interfaces:**
- Consumes: `writeSelfSigned(t, base)` and `parseToken(t, out)` (exist in `relay_test.go`, same package), `waitPort` (deploy_test.go), `dockerfileFor(body)` (Task 1), `store.Open(path)` + `(*store.Store).SaveGitHubApp(store.GitHubApp{AppID, Slug, PrivateKey, WebhookSecret})`, `client.LinkApp(name, repo, branch, rootDir)`, env seam `PIPER_GITHUB_API_BASE` (Task 2).
- Produces: `TestWebhookPushAndPreview` plus helpers `appTarball(t, body)`, `sniClient()`, `deliver(...)`, `fetchVia(...)` that Task 4 extends with the PR-preview steps.

- [ ] **Step 1: Write the test file (push flow only)**

Create `test/e2e/webhook_test.go`:

```go
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/client"
	"github.com/getpiper/piper/internal/store"
)

// TestWebhookPushAndPreview proves Plan 3 end-to-end on the passthrough relay:
// an HMAC-signed synthetic GitHub delivery to hooks.<base> — TLS-dialed against
// the relay's public port by SNI, exactly as GitHub's delivery arrives — rides
// the tunnel to the box's webhook listener, which fetches the tarball from a
// stub GitHub API (via PIPER_GITHUB_API_BASE), builds, and serves the app.
// A pull_request opened event brings up pr-7-blog.<base>; closed tears it down.
func TestWebhookPushAndPreview(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker; Caddy is embedded)")
	}
	repoRoot, _ := filepath.Abs("../..")
	base := "alice.localhost"
	const (
		repo     = "alice/blog"
		secret   = "whsec-e2e"
		pushSHA  = "1111111111111111111111111111111111111111"
		prSHA    = "2222222222222222222222222222222222222222"
		pushBody = "push v1\n"
		prBody   = "pr preview\n"
	)

	certFile, keyF := writeSelfSigned(t, base)

	// Stub GitHub API: installation tokens, tarballs keyed by SHA, Deployments.
	var statusPosts atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			io.WriteString(w, `{"token":"ghs_e2e"}`)
		case strings.Contains(r.URL.Path, "/tarball/"+pushSHA):
			w.Write(appTarball(t, pushBody))
		case strings.Contains(r.URL.Path, "/tarball/"+prSHA):
			w.Write(appTarball(t, prBody))
		case strings.HasSuffix(r.URL.Path, "/deployments") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		case strings.HasSuffix(r.URL.Path, "/deployments") && r.Method == http.MethodGet:
			io.WriteString(w, `[{"id":1}]`)
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			statusPosts.Add(1)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("stub github: unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	// Build both binaries.
	binDir := t.TempDir()
	for _, c := range []string{"piperd", "piper-relay"} {
		b := exec.Command("go", "build", "-o", filepath.Join(binDir, c), "./cmd/"+c)
		b.Dir = repoRoot
		if out, err := b.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", c, err, out)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Enroll an agent, capture the token, start the relay (as TestRelayLoopback).
	relayData := t.TempDir()
	enroll := exec.Command(filepath.Join(binDir, "piper-relay"), "enroll", "alice", "--domain", base)
	enroll.Env = append(os.Environ(), "PIPER_RELAY_DATA_DIR="+relayData)
	out, err := enroll.CombinedOutput()
	if err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}
	token := parseToken(t, string(out))

	relay := exec.CommandContext(ctx, filepath.Join(binDir, "piper-relay"))
	relay.Env = append(os.Environ(),
		"PIPER_RELAY_DATA_DIR="+relayData,
		"PIPER_RELAY_TLS_ADDR=127.0.0.1:8443",
		"PIPER_RELAY_HTTP_ADDR=127.0.0.1:8880",
		"PIPER_RELAY_TUNNEL_ADDR=127.0.0.1:7000",
	)
	relay.Stdout, relay.Stderr = os.Stdout, os.Stderr
	if err := relay.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer relay.Process.Kill()
	waitPort(t, "127.0.0.1:7000", 10*time.Second)

	// Seed the BYO GitHub App row before piperd starts (one writer at a time).
	// store.Open runs the schema, so this also initializes a fresh piper.db.
	piperdData := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	st, err := store.Open(filepath.Join(piperdData, "piper.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SaveGitHubApp(store.GitHubApp{
		AppID: 1, Slug: "e2e", PrivateKey: keyPEM, WebhookSecret: secret,
	}); err != nil {
		t.Fatalf("seed github app: %v", err)
	}
	st.Close()

	// Start piperd in relay mode with the static cert and the GitHub stub.
	pd := exec.CommandContext(ctx, filepath.Join(binDir, "piperd"))
	pd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+piperdData,
		"PIPER_API_ADDR=127.0.0.1:8088",
		"PIPER_BASE_DOMAIN="+base,
		"PIPER_RELAY_ADDR=127.0.0.1:7000",
		"PIPER_RELAY_TOKEN="+token,
		"PIPER_TLS_CERT_FILE="+certFile,
		"PIPER_TLS_KEY_FILE="+keyF,
		"PIPER_GITHUB_API_BASE="+gh.URL,
	)
	pd.Stdout, pd.Stderr = os.Stdout, os.Stderr
	if err := pd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	defer pd.Process.Kill()
	waitPort(t, "127.0.0.1:8088", 15*time.Second)

	// Create the app and link the repo (tokenless on loopback).
	c := client.New("http://127.0.0.1:8088", "")
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := c.LinkApp("blog", repo, "main", ""); err != nil {
		t.Fatalf("LinkApp: %v", err)
	}

	hc := sniClient()

	// Push: signed delivery to hooks.<base> through the relay; retried until the
	// tunnel and webhook route are up. Then the app must serve the push body.
	pushPayload := `{"ref":"refs/heads/main","after":"` + pushSHA + `","repository":{"full_name":"` + repo + `"},"installation":{"id":99}}`
	deliver(t, hc, "https://hooks."+base+"/", "push", secret, pushPayload, 60*time.Second)
	fetchVia(t, hc, "https://blog."+base+"/", pushBody, 3*time.Minute)
	if statusPosts.Load() == 0 {
		t.Fatal("no deployment status was ever reported to the stub GitHub")
	}
}

// appTarball is a gzipped tar in GitHub codeload shape — a single top-level
// dir wrapping a Dockerfile — serving body (see dockerfileFor).
func appTarball(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := dockerfileFor(body)
	if err := tw.WriteHeader(&tar.Header{
		Name: "alice-blog-abc123/Dockerfile", Mode: 0o644,
		Size: int64(len(content)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// sniClient dials every request to the relay's public TLS port; the URL's
// hostname carries the SNI, exactly as a visitor (or GitHub) arrives once DNS
// points at the relay.
func sniClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:8443")
			},
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives: true,
		},
	}
}

// deliver POSTs an HMAC-signed webhook, retrying until the box accepts it with
// 202 (the tunnel, Caddy route, and listener all have to be up first).
func deliver(t *testing.T, hc *http.Client, url, event, secret, payload string, within time.Duration) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sig)
		resp, err := hc.Do(req)
		if err != nil {
			last = err.Error()
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusAccepted {
				return
			}
			last = fmt.Sprintf("status %d body %q", resp.StatusCode, b)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("webhook %s to %s never accepted: last %s", event, url, last)
}

// fetchVia polls url through the relay until it serves exactly want.
func fetchVia(t *testing.T, hc *http.Client, url, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		resp, err := hc.Get(url)
		if err != nil {
			last = err.Error()
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && string(b) == want {
				return
			}
			last = fmt.Sprintf("status %d body %q", resp.StatusCode, b)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never served %q: last %s", url, want, last)
}
```

Note: `prSHA` and `prBody` are declared now (the stub serves both tarballs) and used by Task 4; Go tolerates unused constants, so this compiles as-is.

- [ ] **Step 2: Run the test**

Run: `RUN_E2E=1 go test ./test/e2e/ -run TestWebhookPushAndPreview -count=1 -v`
Expected: PASS — 202 on the delivery, then `push v1` served at `blog.alice.localhost` through the relay. If the delivery never gets 202, read piperd's stderr in the test output first (`webhook: …` log lines say which stage rejected it); use superpowers:systematic-debugging rather than loosening assertions.

- [ ] **Step 3: Verify and commit**

Run: `make verify`
Expected: all gates pass.

```bash
git add test/e2e/webhook_test.go
git commit -m "test(e2e): synthetic GitHub push through the relay deploys and serves the app

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: Webhook e2e — PR preview open/close

**Files:**
- Modify: `test/e2e/webhook_test.go` (extend `TestWebhookPushAndPreview`)

**Interfaces:**
- Consumes: everything Task 3 produced (`hc`, `deliver`, `fetchVia`, `prSHA`, `prBody`, `base`, `repo`, `secret`, `pushBody`).
- Produces: the finished test; nothing downstream.

- [ ] **Step 1: Append the preview lifecycle to the test**

Add at the end of `TestWebhookPushAndPreview` (after the `statusPosts` check):

```go
	// PR opened → preview at pr-7-blog.<base> (flattened single label under the
	// wildcard); the production app keeps serving the push body.
	prOpened := `{"action":"opened","number":7,"pull_request":{"head":{"ref":"feature","sha":"` + prSHA + `"}},"repository":{"full_name":"` + repo + `"},"installation":{"id":99}}`
	deliver(t, hc, "https://hooks."+base+"/", "pull_request", secret, prOpened, 30*time.Second)
	fetchVia(t, hc, "https://pr-7-blog."+base+"/", prBody, 3*time.Minute)
	fetchVia(t, hc, "https://blog."+base+"/", pushBody, 20*time.Second)

	// PR closed → the preview route is torn down. The box's Caddy then answers
	// an empty 200 for the host (the route is gone, the TLS splice still
	// completes under the wildcard), so "gone" = anything but the preview body.
	prClosed := `{"action":"closed","number":7,"pull_request":{"head":{"ref":"feature","sha":"` + prSHA + `"}},"repository":{"full_name":"` + repo + `"},"installation":{"id":99}}`
	deliver(t, hc, "https://hooks."+base+"/", "pull_request", secret, prClosed, 30*time.Second)
	deadline := time.Now().Add(60 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		resp, err := hc.Get("https://pr-7-blog." + base + "/")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != prBody {
				gone = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !gone {
		t.Fatal("preview still serves its body after PR close")
	}
	fetchVia(t, hc, "https://blog."+base+"/", pushBody, 20*time.Second)
```

- [ ] **Step 2: Run the full test**

Run: `RUN_E2E=1 go test ./test/e2e/ -run TestWebhookPushAndPreview -count=1 -v`
Expected: PASS — preview up on opened, gone on closed, production app untouched throughout.

- [ ] **Step 3: Verify and commit**

Run: `make verify`
Expected: all gates pass.

```bash
git add test/e2e/webhook_test.go
git commit -m "test(e2e): PR-preview URL comes up on opened and is torn down on closed

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: Full suite run, PROGRESS.md, PR

**Files:**
- Modify: `PROGRESS.md` (one line in the "Always-green gates" section area of Plan 3)

**Interfaces:**
- Consumes: the finished tests and seam from Tasks 1–4.
- Produces: the merged-ready PR.

- [ ] **Step 1: Run the entire e2e suite**

Run: `make e2e`
Expected: all six tests pass (four existing + two new). This catches inter-test interference (shared ports :80/:443/:8088/:8443) that per-test runs miss.

- [ ] **Step 2: Add the PROGRESS.md line**

In `PROGRESS.md`, under the Plan 3 section, append after the last `✅` line (replace `#N` with the tracking issue):

```markdown
- ✅ e2e — deploy-failure resilience (failed build keeps the old version serving) + synthetic webhook push → deploy → PR-preview lifecycle through the relay — [#N](https://github.com/getpiper/piper/issues/N)
```

- [ ] **Step 3: Verify, commit, push, open the PR**

Run: `make verify`
Expected: all gates pass.

```bash
git add PROGRESS.md
git commit -m "docs(progress): e2e gap tests landed

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push -u origin faruk/e2e-gap-tests
gh pr create --base main \
  --title "[deploy] e2e: deploy-failure resilience + webhook/PR-preview loop" \
  --body "$(cat <<'EOF'
Two new e2e tests per docs/superpowers/specs/2026-07-24-e2e-gap-tests-design.md:

- **TestDeployFailureAndRedeploy** (LAN harness): a broken build ends `failed` while the running version keeps serving; the next good deploy swaps the body; history shows one each of stopped/failed/running.
- **TestWebhookPushAndPreview** (passthrough-relay harness): an HMAC-signed synthetic push TLS-dialed at the relay with SNI `hooks.<base>` rides the tunnel to the box, which builds from a stub GitHub tarball and serves the app; PR opened brings up `pr-7-blog.<base>`, PR closed tears it down.

One production seam: `PIPER_GITHUB_API_BASE` (mirrors `PIPER_TEST_ISSUER`) so piperd's GitHub provider can target the in-test stub.

Closes #N

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Expected: PR opens against `main`; the `verify` and `e2e` CI checks go green.
