# Drop `set-domain` Shim Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Remove the v0.1.0 relay `set-domain` compatibility path now that no v0.1.0 boxes remain, while preserving disabled-account enforcement for the replacement `add-domain` path.

**Architecture:** The supported relay domain lifecycle remains `add-domain` → `domain-active` → `remove-domain`. `AddCustomDomain` becomes the single domain-claim entry point and performs the account-disabled check before beginning its write transaction. The legacy control handler, store method, protocol documentation, compatibility tests, and progress follow-up are deleted.

**Tech Stack:** Go, SQLite, yamux relay control streams, standard `testing` package.

## Global Constraints

- Keep changes surgical and limited to issue #268.
- Preserve the pre-1.x policy: no compatibility tombstone or legacy reader.
- Disabled accounts must receive `ErrBadCredential` from `AddCustomDomain`.
- Before completion run `gofmt -l .`, `go vet ./...`, `make test`, and `make cross`.

---

### Task 1: Preserve disabled-account enforcement on `add-domain`

**Files:**
- Modify: `internal/relay/domains_test.go`
- Modify: `internal/relay/domains.go`

**Interfaces:**
- Consumes: `(*Store).AgentDisabled(baseDomain string) (bool, error)` and `ErrUnknownAccount`.
- Produces: `(*Store).AddCustomDomain(baseDomain, domain string) error` returning `ErrBadCredential` for a disabled account.

- [x] **Step 1: Write the failing regression test**

Add `TestAddCustomDomainRejectsDisabledAccount`. Create an account-bound agent, prove a healthy claim succeeds, disable the account, then assert a second claim returns `ErrBadCredential` and is absent from `CustomDomains`.

- [x] **Step 2: Run the regression test and verify RED**

Run: `go test ./internal/relay -run '^TestAddCustomDomainRejectsDisabledAccount$' -count=1 -v`

Expected: FAIL because `AddCustomDomain` currently accepts the disabled account.

- [x] **Step 3: Add the minimal guard**

In `AddCustomDomain`, after domain validation and namespace checks but before `db.Begin`, call `AgentDisabled`. Propagate store errors except `ErrUnknownAccount`; return `ErrBadCredential` when disabled. Leave the existing transaction check to translate an unknown base domain to `ErrBadToken`.

- [x] **Step 4: Run focused tests and verify GREEN**

Run: `go test ./internal/relay -run '^(TestAddCustomDomainRejectsDisabledAccount|TestAddCustomDomainClaimAndList)$' -count=1 -v`

Expected: PASS.

### Task 2: Remove the v0.1.0 compatibility surface

**Files:**
- Modify: `internal/relay/server.go`
- Modify: `internal/relay/domains.go`
- Modify: `internal/relay/server_test.go`
- Modify: `internal/relay/store_test.go`
- Modify: `internal/relay/accepttunnels_test.go`
- Modify: `internal/tunnel/tunnel.go`
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: the supported `add-domain`, `domain-active`, and `remove-domain` operations.
- Produces: no `set-domain` control operation or `SetCustomDomain` relay store API.

- [x] **Step 1: Change the control-op test to require rejection**

Replace the success and hijack-specific `set-domain` tests with one `TestSetDomainControlOpRemoved` assertion that sends `set-domain`, expects `unknown op`, and verifies no custom-domain row was created.

- [x] **Step 2: Run the removal test and verify RED**

Run: `go test ./internal/relay -run '^TestSetDomainControlOpRemoved$' -count=1 -v`

Expected: FAIL because the relay still accepts `set-domain`.

- [x] **Step 3: Delete the legacy implementation and compatibility-only tests**

Remove the `set-domain` switch case, `SetCustomDomain`, its store tests, and the reconnect test's compatibility helper. Update that reconnect test to claim and confirm through `add-domain`/`domain-active`. Remove `set-domain` from `ControlRequest` comments and make the `Domain` comment operation-neutral.

- [x] **Step 4: Update the progress map**

Remove “+ `set-domain` compat shim” from #227 and remove the #268 follow-up sentence from the completed per-app-domain entry.

- [x] **Step 5: Run focused relay tests**

Run: `go test ./internal/relay/... -count=1`

Expected: PASS.

### Task 3: Repository verification

**Files:**
- Verify all changed Go and Markdown files.

**Interfaces:**
- Consumes: Tasks 1–2.
- Produces: a CI-clean branch ready for review.

- [x] **Step 1: Run formatting check**

Run: `gofmt -l .`

Expected: no output.

- [x] **Step 2: Run vet**

Run: `go vet ./...`

Expected: exit 0.

- [x] **Step 3: Run all tests**

Run: `make test`

Expected: exit 0.

- [x] **Step 4: Run the cross-build**

Run: `make cross`

Expected: exit 0.

- [x] **Step 5: Review the final diff and commit**

Confirm every changed line traces to #268, then commit with `fix(relay): drop set-domain compatibility shim` and reference `Closes #268` in the eventual PR body.
