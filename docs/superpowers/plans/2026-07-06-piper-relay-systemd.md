# Piper Relay systemd Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship and document a hardened systemd service that keeps `piper-relay` running across failures and reboots while preserving enrollment state.

**Architecture:** A single unit runs the existing static binary as a systemd dynamic user, persists `relay.db` through `StateDirectory=piper-relay`, and grants only the capability needed to bind port 443. Enrollment remains a separate one-shot command executed through `systemd-run` with the same state-directory semantics; repository tests validate the unit contract and the operator documentation.

**Tech Stack:** systemd unit files, Go standard-library tests, Markdown documentation.

## Global Constraints

- Systemd is the only packaging target; do not add Docker or Compose files.
- Install the binary at `/usr/local/bin/piper-relay` and the unit at `/etc/systemd/system/piper-relay.service`.
- Persist relay state at `/var/lib/piper-relay` and expose it as `PIPER_RELAY_DATA_DIR`.
- Run the service without a permanent user by using `DynamicUser=yes` and `StateDirectory=piper-relay`.
- Grant `CAP_NET_BIND_SERVICE` only; the relay must not run as root.
- Keep enrollment separate from service startup and execute it with matching dynamic-user and state-directory properties.
- Default listeners remain TCP `:443` and `:7000`; `/etc/piper-relay.env` may override their environment variables.
- Production Go packages remain unchanged.
- Before completion, run `gofmt -l .`, `go vet ./...`, `make test`, and `make cross`, in that order.

---

### Task 1: Ship the hardened systemd unit

**Files:**
- Create: `packaging/systemd/piper-relay_test.go`
- Create: `packaging/systemd/piper-relay.service`

**Interfaces:**
- Consumes: the existing `/usr/local/bin/piper-relay` executable and its `PIPER_RELAY_DATA_DIR`, `PIPER_RELAY_TLS_ADDR`, and `PIPER_RELAY_TUNNEL_ADDR` environment variables.
- Produces: `packaging/systemd/piper-relay.service`, installed by operators as the `piper-relay.service` system unit.

- [ ] **Step 1: Write the failing unit-contract test**

Create `packaging/systemd/piper-relay_test.go`:

```go
package systemd

import (
	"os"
	"strings"
	"testing"
)

func TestPiperRelayServiceContract(t *testing.T) {
	b, err := os.ReadFile("piper-relay.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)

	required := []string{
		"After=network-online.target",
		"Wants=network-online.target",
		"ExecStart=/usr/local/bin/piper-relay",
		"Environment=PIPER_RELAY_DATA_DIR=/var/lib/piper-relay",
		"EnvironmentFile=-/etc/piper-relay.env",
		"DynamicUser=yes",
		"StateDirectory=piper-relay",
		"AmbientCapabilities=CAP_NET_BIND_SERVICE",
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"Restart=on-failure",
		"RestartSec=2s",
		"WantedBy=multi-user.target",
	}
	for _, directive := range required {
		if !strings.Contains(unit, directive) {
			t.Errorf("unit missing %q", directive)
		}
	}
}
```

- [ ] **Step 2: Run the test and verify the missing unit fails it**

Run: `go test ./packaging/systemd -run TestPiperRelayServiceContract -v`

Expected: FAIL because `piper-relay.service` does not exist.

- [ ] **Step 3: Add the minimal service unit**

Create `packaging/systemd/piper-relay.service`:

```ini
[Unit]
Description=Piper relay (SNI passthrough and tunnel server)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/piper-relay
Environment=PIPER_RELAY_DATA_DIR=/var/lib/piper-relay
EnvironmentFile=-/etc/piper-relay.env
DynamicUser=yes
StateDirectory=piper-relay
StateDirectoryMode=0700
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 4: Run the focused test and package tests**

Run: `go test ./packaging/systemd -run TestPiperRelayServiceContract -v`

Expected: PASS.

Run: `go test ./packaging/systemd`

Expected: PASS.

- [ ] **Step 5: Commit the unit and its contract test**

```bash
git add packaging/systemd/piper-relay.service packaging/systemd/piper-relay_test.go
git commit -m "feat(relay): add managed systemd service"
```

---

### Task 2: Document installation, enrollment, operation, and teardown

**Files:**
- Modify: `packaging/systemd/piper-relay_test.go`
- Modify: `README.md`
- Modify: `docs/runbooks/git-deploy-e2e.md`
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: `packaging/systemd/piper-relay.service` from Task 1 and the existing `piper-relay enroll <name> --domain <base>` CLI.
- Produces: a concise README installation path, an end-to-end runbook procedure, and a progress-map link to issue 38.

- [ ] **Step 1: Add failing documentation-contract tests**

Append these helpers and tests to `packaging/systemd/piper-relay_test.go`, and add `"path/filepath"` to its import block:

```go
func repositoryFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestServiceDocumentation(t *testing.T) {
	readme := repositoryFile(t, "README.md")
	for _, text := range []string{
		"packaging/systemd/piper-relay.service",
		"systemctl enable --now piper-relay",
	} {
		if !strings.Contains(readme, text) {
			t.Errorf("README missing %q", text)
		}
	}

	runbook := repositoryFile(t, "docs", "runbooks", "git-deploy-e2e.md")
	for _, text := range []string{
		"systemd-run",
		"PIPER_RELAY_DATA_DIR=/var/lib/piper-relay",
		"systemctl enable --now piper-relay",
		"journalctl -u piper-relay",
		"ss -lnt",
	} {
		if !strings.Contains(runbook, text) {
			t.Errorf("runbook missing %q", text)
		}
	}
}
```

- [ ] **Step 2: Run the documentation test and verify it fails for missing instructions**

Run: `go test ./packaging/systemd -run TestServiceDocumentation -v`

Expected: FAIL, reporting the absent systemd installation and lifecycle strings.

- [ ] **Step 3: Add the concise README service section**

Insert this section after `## Components` and before `## Git deploys` in `README.md`:

````markdown
## Run the relay as a service

On a Linux relay host, build or download the static `piper-relay` binary, then install
the binary and the shipped systemd unit:

```bash
sudo install -m 0755 bin/piper-relay /usr/local/bin/piper-relay
sudo install -m 0644 packaging/systemd/piper-relay.service \
  /etc/systemd/system/piper-relay.service
sudo systemctl daemon-reload
```

Enroll the box before starting the service, then enable it at boot:

```bash
sudo systemd-run --pipe --wait --collect \
  --property=DynamicUser=yes \
  --property=StateDirectory=piper-relay \
  --setenv=PIPER_RELAY_DATA_DIR=/var/lib/piper-relay \
  /usr/local/bin/piper-relay enroll <name> --domain <base-domain>
sudo systemctl enable --now piper-relay
```

Open inbound TCP ports `443` and `7000`. See the
[end-to-end runbook](docs/runbooks/git-deploy-e2e.md#part-b--relay) for verification,
address overrides, logs, and teardown.
````

- [ ] **Step 4: Replace the runbook's foreground Part B with the managed-service procedure**

Replace the contents of `docs/runbooks/git-deploy-e2e.md` from the paragraph below `## Part B — Relay` through `Keep the token from step 1 — it goes to the box next.` with:

````markdown
On the relay host, install the binary and service unit:

```bash
sudo install -m 0755 bin/piper-relay /usr/local/bin/piper-relay
sudo install -m 0644 packaging/systemd/piper-relay.service \
  /etc/systemd/system/piper-relay.service
sudo systemctl daemon-reload
```

Enrollment is a separate one-shot command, not the service. Run it through a
transient unit so it writes to the same systemd-managed state directory as the
service:

```bash
sudo systemd-run --pipe --wait --collect \
  --property=DynamicUser=yes \
  --property=StateDirectory=piper-relay \
  --setenv=PIPER_RELAY_DATA_DIR=/var/lib/piper-relay \
  /usr/local/bin/piper-relay enroll alice --domain <base>
#   enrolled alice for <base>
#   token: rlyt_XXXXXXXXXXXXXXXX      ← copy this
```

Do not run enrollment directly as root with
`PIPER_RELAY_DATA_DIR=/var/lib/piper-relay`; a root-owned `relay.db` may prevent the
dynamic service user from opening it.

Enable the relay at boot and start it now:

```bash
sudo systemctl enable --now piper-relay
sudo systemctl status piper-relay
sudo journalctl -u piper-relay -n 50 --no-pager
sudo ss -lnt '( sport = :443 or sport = :7000 )'
```

The final command must show listeners on `:443` and `:7000`. Open inbound TCP ports
`443` and `7000` in both the host firewall and the VPS provider firewall.

To override listener addresses, create `/etc/piper-relay.env` before starting the
service:

```bash
PIPER_RELAY_TLS_ADDR=:443
PIPER_RELAY_TUNNEL_ADDR=:7000
```

Then apply changes with `sudo systemctl restart piper-relay`. Keep the enrollment
token — it goes to the box next.
````

- [ ] **Step 5: Update teardown and troubleshooting for systemd**

In the runbook's teardown code block, replace the foreground relay comment with:

```bash
# Relay: stop and disable the service; remove its persistent enrollment state.
sudo systemctl disable --now piper-relay
sudo systemctl clean --what=state piper-relay
```

In the troubleshooting row for `curl https://myapp.<base>`, replace the Fix cell with:

```markdown
Check both firewalls; run `systemctl status piper-relay`, inspect `journalctl -u piper-relay`, and confirm listeners with `ss -lnt`; confirm `PIPER_RELAY_ADDR` uses host:7000
```

- [ ] **Step 6: Add issue 38 to the progress map**

After the existing `piper-relay` enrollment/SNI/tunnel-server line in `PROGRESS.md`, add:

```markdown
- ✅ `piper-relay` managed systemd service + operator docs — [#38](https://github.com/piperbox/piper/issues/38)
```

- [ ] **Step 7: Run the documentation contract and inspect the rendered Markdown source**

Run: `go test ./packaging/systemd -run TestServiceDocumentation -v`

Expected: PASS.

Run: `git diff --check`

Expected: no output and exit status 0.

Manually inspect the changed Markdown source to confirm fenced blocks are balanced and commands are copyable:

```bash
git diff -- README.md docs/runbooks/git-deploy-e2e.md PROGRESS.md
```

Expected: the README contains the short installation path; Part B contains enrollment, enablement, verification, firewall, and override instructions; teardown and troubleshooting no longer assume a foreground relay.

- [ ] **Step 8: Run all packaging tests**

Run: `go test ./packaging/systemd`

Expected: PASS.

- [ ] **Step 9: Commit the operator documentation**

```bash
git add packaging/systemd/piper-relay_test.go README.md docs/runbooks/git-deploy-e2e.md PROGRESS.md
git commit -m "docs(relay): document managed service setup"
```

---

### Task 3: Complete repository verification

**Files:**
- Modify only files from Tasks 1 and 2 if verification exposes an issue directly caused by this work.

**Interfaces:**
- Consumes: the systemd unit, tests, and documentation from Tasks 1 and 2.
- Produces: verification evidence that issue 38 satisfies the repository's formatting, vet, test, and cross-build gates.

- [ ] **Step 1: Check Go formatting**

Run: `gofmt -l .`

Expected: no output.

- [ ] **Step 2: Run static analysis**

Run: `go vet ./...`

Expected: exit status 0 with no diagnostics.

- [ ] **Step 3: Run the full test suite**

Run: `make test`

Expected: all packages pass; Docker-dependent tests may report a clean skip when Docker is unavailable.

- [ ] **Step 4: Run the cross-build gate**

Run: `make cross`

Expected: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...` exits 0.

- [ ] **Step 5: Confirm branch scope and history**

Run:

```bash
git status --short
git diff --stat main...HEAD
git log --oneline main..HEAD
```

Expected: the worktree is clean; the diff contains only the spec, plan, service unit, unit test, README, runbook, and progress map; history contains the design commit plus the focused implementation commits.
