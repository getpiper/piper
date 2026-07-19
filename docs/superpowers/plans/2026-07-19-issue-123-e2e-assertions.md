# Issue #123 E2E Assertions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close issue #123's remaining e2e coverage gap by proving stopped and deleted apps are no longer served and deleted apps return API 404.

**Architecture:** Strengthen the existing `TestEndToEndDeploy` lifecycle without changing production code. A small e2e helper will fetch the public hostname and require Caddy's exact no-route response; the existing public client will verify the deleted resource returns a typed HTTP 404.

**Tech Stack:** Go 1.26, `testing`, `net/http`, the existing `internal/client`, real Docker, embedded Caddy.

## Global Constraints

- Modify tests only; production behavior is already present.
- Exercise public behavior through `client.Client` and the app hostname, not Caddy's admin API.
- Preserve the existing deploy → stop → delete lifecycle.
- Keep `CGO_ENABLED=0` compatibility and introduce no dependencies.
- Complete verification with `gofmt -l .`, `go vet ./...`, `make test`, and `make cross`.

---

### Task 1: Strengthen stop and delete e2e assertions

**Files:**
- Modify: `test/e2e/deploy_test.go:3-151`
- Verify: `test/e2e/deploy_test.go`

**Interfaces:**
- Consumes: `(*client.Client).App(name string) (api.App, error)` and `client.StatusError.Code`.
- Produces: `assertRouteGone(t *testing.T, req *http.Request)`, an e2e-only assertion helper.

- [ ] **Step 1: Add the route-gone helper and stop/delete assertions with deliberate mutations**

Add `errors` to the imports. Replace the weak post-stop response-body comparison with `assertRouteGone(t, req)`. After the existing empty-list assertion, require `c.App("blog")` to produce `*client.StatusError`, but deliberately compare against `http.StatusGone` for the red run. Call `assertRouteGone(t, req)` after the API assertion.

Add this helper below `TestEndToEndDeploy`, deliberately expecting 404 for the red run:

```go
func assertRouteGone(t *testing.T, req *http.Request) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch route after teardown: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read route response after teardown: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound || len(body) != 0 {
		t.Fatalf("route after teardown = status %d, body %q; want 404 with empty body", resp.StatusCode, body)
	}
}
```

Use this deleted-app assertion, deliberately expecting 410 for its red run:

```go
	_, err = c.App("blog")
	var statusErr *client.StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusGone {
		t.Fatalf("App after delete err = %v, want HTTP 410 StatusError", err)
	}
	assertRouteGone(t, req)
```

- [ ] **Step 2: Run the focused e2e test and verify the route mutation fails**

Run:

```bash
RUN_E2E=1 go test ./test/e2e/... -run '^TestEndToEndDeploy$' -count=1 -v
```

Expected: FAIL from `assertRouteGone`; Caddy returns status 200 with an empty body rather than the deliberately expected 404.

- [ ] **Step 3: Correct the route expectation but retain the API mutation**

Change the helper condition and failure message to the intended public no-route contract:

```go
	if resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("route after teardown = status %d, body %q; want 200 with empty body", resp.StatusCode, body)
	}
```

- [ ] **Step 4: Run the focused e2e test and verify the API mutation fails**

Run:

```bash
RUN_E2E=1 go test ./test/e2e/... -run '^TestEndToEndDeploy$' -count=1 -v
```

Expected: FAIL after deletion because `c.App("blog")` returns a `*client.StatusError` with code 404 rather than the deliberately expected 410.

- [ ] **Step 5: Correct the deleted-app expectation**

Replace the mutated assertion with:

```go
	_, err = c.App("blog")
	var statusErr *client.StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusNotFound {
		t.Fatalf("App after delete err = %v, want HTTP 404 StatusError", err)
	}
	assertRouteGone(t, req)
```

- [ ] **Step 6: Format and run the focused e2e test green**

Run:

```bash
gofmt -w test/e2e/deploy_test.go
RUN_E2E=1 go test ./test/e2e/... -run '^TestEndToEndDeploy$' -count=1 -v
```

Expected: PASS. The test deploys the sample app, proves stop removes its public response, proves delete returns API 404, and proves the hostname remains unrouted.

- [ ] **Step 7: Run the full repository verification sequence**

Run each command and require success:

```bash
gofmt -l .
go vet ./...
make test
make cross
```

Expected: `gofmt -l .` prints nothing; every remaining command exits 0.

- [ ] **Step 8: Commit the implementation**

```bash
git add test/e2e/deploy_test.go docs/superpowers/plans/2026-07-19-issue-123-e2e-assertions.md
git commit -m "test: close remaining app lifecycle e2e gap" -m "Closes #123." -m "Co-Authored-By: OpenAI GPT-5 <noreply@openai.com>"
```
