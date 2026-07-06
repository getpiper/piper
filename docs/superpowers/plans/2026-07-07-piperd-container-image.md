# piperd Container Image + Compose Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Package `piperd` as a multi-arch container image plus a Docker Compose file, so it can run under Docker as a sibling container driving the host Docker daemon, instead of only as a native systemd service.

**Architecture:** A multi-stage root `Dockerfile` cross-compiles the static `piperd` binary (`golang:1.26` build stage) into a minimal `gcr.io/distroless/static-debian12` runtime stage. `deploy/compose/docker-compose.yml` runs that image with `network_mode: host` (required, not optional — see spec), a bind-mounted `/var/run/docker.sock`, and a named volume for state. A single Go test file (`deploy/compose/compose_test.go`) asserts required directives are present in the Dockerfile, compose file, and docs, following the existing `packaging/systemd/*_test.go` convention of parsing shipped artifacts as text.

**Tech Stack:** Docker (multi-stage builds, Buildx multi-platform), Docker Compose, Go 1.26 (`CGO_ENABLED=0`), GitHub Actions.

**Spec:** [`docs/superpowers/specs/2026-07-07-piperd-container-image-design.md`](../specs/2026-07-07-piperd-container-image-design.md)

## Global Constraints

- No cgo: every build must work with `CGO_ENABLED=0` (repo-wide hard constraint).
- Module path is `github.com/getpiper/piper`; Go toolchain is `1.26` (`go.mod`).
- `PIPER_DATA_DIR` default in code is `./data`; the systemd unit and this container path both override it to `/var/lib/piper` — keep that value consistent.
- `make test` and `make cross` must stay green (repo-wide CI gate).
- Follow the existing `packaging/systemd/*_test.go` pattern: a Go test in the same directory as the shipped artifact, reading it as plain text and asserting required substrings.

---

### Task 1: Write failing contract tests for the Dockerfile, compose file, and docs

**Files:**
- Create: `deploy/compose/compose_test.go`

**Interfaces:**
- Produces: `repositoryFile(t *testing.T, parts ...string) string` — test helper reading a file relative to the repo root (two directories up from `deploy/compose/`), used by Task 4's doc assertions and mirroring `packaging/systemd/piper-relay_test.go`'s helper of the same name.
- Consumes (in later tasks): the exact literal strings asserted below must appear verbatim in `Dockerfile`, `deploy/compose/docker-compose.yml`, `docs/manual-setup.md`, and `README.md`.

- [ ] **Step 1: Write the test file**

```go
package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repositoryFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDockerfileContract(t *testing.T) {
	dockerfile := repositoryFile(t, "Dockerfile")

	required := []string{
		"FROM golang:1.26 AS build",
		"ARG TARGETOS",
		"ARG TARGETARCH",
		"GOOS=$TARGETOS GOARCH=$TARGETARCH",
		"CGO_ENABLED=0",
		"./cmd/piperd",
		"FROM gcr.io/distroless/static-debian12",
		"ENV PIPER_DATA_DIR=/var/lib/piper",
		"VOLUME /var/lib/piper",
		`ENTRYPOINT ["/usr/local/bin/piperd"]`,
	}
	for _, directive := range required {
		if !strings.Contains(dockerfile, directive) {
			t.Errorf("Dockerfile missing %q", directive)
		}
	}
}

func TestComposeContract(t *testing.T) {
	b, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(b)

	required := []string{
		"network_mode: host",
		"/var/run/docker.sock:/var/run/docker.sock",
		"piper_data:/var/lib/piper",
		"PIPER_DATA_DIR=/var/lib/piper",
		"../../packaging/systemd/piperd.env.example",
	}
	for _, directive := range required {
		if !strings.Contains(compose, directive) {
			t.Errorf("docker-compose.yml missing %q", directive)
		}
	}
}

func TestDockerDocumentation(t *testing.T) {
	manual := repositoryFile(t, "docs", "manual-setup.md")
	for _, text := range []string{
		"docker compose -f deploy/compose/docker-compose.yml up -d --build",
		"network_mode: host",
		"root-equivalent",
	} {
		if !strings.Contains(manual, text) {
			t.Errorf("docs/manual-setup.md missing %q", text)
		}
	}

	readme := repositoryFile(t, "README.md")
	if !strings.Contains(readme, "run piperd in Docker via Compose") {
		t.Errorf("README missing pointer phrase %q", "run piperd in Docker via Compose")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./deploy/compose/... -v`
Expected: `FAIL` — `Dockerfile` and `deploy/compose/docker-compose.yml` don't exist yet (`os.ReadFile` errors), so all three tests fail.

- [ ] **Step 3: Commit**

```bash
git add deploy/compose/compose_test.go
git commit -m "test: add contract tests for piperd container image + compose"
```

---

### Task 2: Add the Dockerfile and .dockerignore

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`
- Test: `deploy/compose/compose_test.go` (`TestDockerfileContract`, from Task 1)

**Interfaces:**
- Consumes: none beyond the repo's own `go.mod`/`go.sum`/`cmd/piperd`.
- Produces: an image built by `docker build .` (or Buildx with `--platform linux/amd64,linux/arm64`) that Task 3's compose file references via `build: {context: ../.., dockerfile: Dockerfile}`.

- [ ] **Step 1: Write `Dockerfile`**

```dockerfile
# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w" -o /out/piperd ./cmd/piperd

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/piperd /usr/local/bin/piperd
ENV PIPER_DATA_DIR=/var/lib/piper
VOLUME /var/lib/piper
EXPOSE 80 443
ENTRYPOINT ["/usr/local/bin/piperd"]
```

- [ ] **Step 2: Write `.dockerignore`**

```
.git
bin
data
*.sqlite
*.sqlite-*
*.db
*.log
.DS_Store
```

- [ ] **Step 3: Run the Dockerfile contract test to verify it passes**

Run: `go test ./deploy/compose/... -run TestDockerfileContract -v`
Expected: `PASS`

- [ ] **Step 4: Build the image locally to verify it actually builds**

Run: `docker build -t piperd:local .`
Expected: build succeeds; `docker run --rm piperd:local` prints usage/starts and exits cleanly on Ctrl-C (no flags needed to start — it reads env with defaults).

- [ ] **Step 5: Commit**

```bash
git add Dockerfile .dockerignore
git commit -m "feat(docker): add piperd Dockerfile (distroless, multi-arch)"
```

---

### Task 3: Add the Compose file and gitignore entry

**Files:**
- Create: `deploy/compose/docker-compose.yml`
- Modify: `.gitignore`
- Test: `deploy/compose/compose_test.go` (`TestComposeContract`, from Task 1)

**Interfaces:**
- Consumes: `Dockerfile` (Task 2), `packaging/systemd/piperd.env.example` (existing file, unmodified).
- Produces: `deploy/compose/docker-compose.yml`, referenced by Task 4's docs.

- [ ] **Step 1: Write `deploy/compose/docker-compose.yml`**

```yaml
services:
  piperd:
    build:
      context: ../..
      dockerfile: Dockerfile
    network_mode: host
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - piper_data:/var/lib/piper
    environment:
      - PIPER_DATA_DIR=/var/lib/piper
    env_file:
      - ../../packaging/systemd/piperd.env.example

volumes:
  piper_data:
```

- [ ] **Step 2: Add a gitignore entry for local env overrides**

In `.gitignore`, under the `# secrets / env` section (which currently reads):

```
# secrets / env
.env
.env.*
!.env.example
```

add a line so the section reads:

```
# secrets / env
.env
.env.*
!.env.example
deploy/compose/piperd.env
```

- [ ] **Step 3: Run the compose contract test to verify it passes**

Run: `go test ./deploy/compose/... -run TestComposeContract -v`
Expected: `PASS`

- [ ] **Step 4: Validate the compose file with the real Docker Compose CLI**

Run: `docker compose -f deploy/compose/docker-compose.yml config`
Expected: exits 0 and prints the resolved config (confirms valid YAML/schema; does not require actually building).

- [ ] **Step 5: Commit**

```bash
git add deploy/compose/docker-compose.yml .gitignore
git commit -m "feat(docker): add docker-compose.yml for running piperd via Docker"
```

---

### Task 4: Document the Docker/Compose path

**Files:**
- Modify: `docs/manual-setup.md` (insert new section after line 29, before `## Run the relay as a service`)
- Modify: `README.md` (extend the existing pointer sentence in the `## Install` section)
- Test: `deploy/compose/compose_test.go` (`TestDockerDocumentation`, from Task 1)

**Interfaces:**
- Consumes: `docker-compose.yml` (Task 3), `Dockerfile` (Task 2).
- Produces: nothing consumed by later tasks — this is the last content task.

- [ ] **Step 1: Insert a new section into `docs/manual-setup.md`**

Insert immediately after the line `[end-to-end runbook](runbooks/git-deploy-e2e.md) for verification, logs, and teardown.` (end of the "Run the agent as a service (manual / from source)" section) and before the `## Run the relay as a service` heading:

```markdown

## Run piperd in Docker (Compose)

Prefer to run `piperd` itself as a container instead of a systemd service? Build and
start it with Compose from the repo root:

```bash
docker compose -f deploy/compose/docker-compose.yml up -d --build
```

This builds the image from the repo's `Dockerfile`, mounts the host's
`/var/run/docker.sock` (piperd drives the **host** Docker daemon as a sibling
container — not Docker-in-Docker), and persists state in the named `piper_data`
volume at `/var/lib/piper`. To override defaults, copy the env file and point
`env_file` at your copy instead of the tracked example:

```bash
cp packaging/systemd/piperd.env.example deploy/compose/piperd.env
```

Then edit `deploy/compose/piperd.env` and change the `env_file:` entry in
`docker-compose.yml` to `deploy/compose/piperd.env` (already gitignored).

**Networking:** the compose file sets `network_mode: host`, which is required, not
optional. `piperd` publishes every app container's port to `127.0.0.1` on the
Docker host and dials that same address for health checks, so `piperd` must share
the host's network namespace to see them — otherwise `127.0.0.1` inside piperd's
own container would be its own empty loopback, not the host's. Host networking
also lets the embedded Caddy manager bind `:80`/`:443` directly. This is Linux-only,
matching the rest of the service-install path.

**Trust:** mounting `docker.sock` grants the container root-equivalent control over
the host's Docker daemon — the same trust boundary the systemd unit already accepts
via its `docker` group membership (see the previous section). Only run this on a
box you already trust with root.
```

- [ ] **Step 2: Extend the README pointer sentence**

In `README.md`, in the `## Install` section, change:

```markdown
Prefer to build from source, run the relay as a service, or wire your own automation?
See [`docs/manual-setup.md`](docs/manual-setup.md).
```

to:

```markdown
Prefer to build from source, run piperd in Docker via Compose, run the relay as a
service, or wire your own automation? See [`docs/manual-setup.md`](docs/manual-setup.md).
```

- [ ] **Step 3: Run the documentation contract test to verify it passes**

Run: `go test ./deploy/compose/... -v`
Expected: `PASS` for all three tests in the package (`TestDockerfileContract`, `TestComposeContract`, `TestDockerDocumentation`).

- [ ] **Step 4: Commit**

```bash
git add docs/manual-setup.md README.md
git commit -m "docs: document running piperd via Docker Compose"
```

---

### Task 5: Gate multi-arch buildability in CI

**Files:**
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: `Dockerfile` (Task 2), path-filter step `id: changes` already defined in the `verify` job.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Add a `docker` path filter alongside the existing `code` filter**

In `.github/workflows/ci.yml`, the `Detect code changes` step currently has:

```yaml
      - name: Detect code changes
        id: changes
        uses: dorny/paths-filter@d1c1ffe0248fe513906c8e24db8ea791d46f8590 # v3
        with:
          filters: |
            code:
              - '**.go'
              - 'go.mod'
              - 'go.sum'
              - 'Makefile'
              - '.github/workflows/ci.yml'
              - '.goreleaser.yaml'
```

Change the `filters:` block to:

```yaml
          filters: |
            code:
              - '**.go'
              - 'go.mod'
              - 'go.sum'
              - 'Makefile'
              - '.github/workflows/ci.yml'
              - '.goreleaser.yaml'
            docker:
              - 'Dockerfile'
              - '.dockerignore'
              - 'deploy/compose/**'
              - '.github/workflows/ci.yml'
```

- [ ] **Step 2: Add multi-arch build steps at the end of the `verify` job**

After the existing `goreleaser check` step (the last step in the job), append:

```yaml

      - name: Set up QEMU
        if: steps.changes.outputs.docker == 'true'
        uses: docker/setup-qemu-action@v3

      - name: Set up Docker Buildx
        if: steps.changes.outputs.docker == 'true'
        uses: docker/setup-buildx-action@v3

      - name: Build piperd image (multi-arch, no push)
        if: steps.changes.outputs.docker == 'true'
        uses: docker/build-push-action@v6
        with:
          context: .
          file: Dockerfile
          platforms: linux/amd64,linux/arm64
          push: false
```

- [ ] **Step 3: Validate the workflow YAML parses**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))" && echo OK`
Expected: `OK`

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: build piperd image for linux/amd64+arm64 on Dockerfile/compose changes"
```

---

### Task 6: Update PROGRESS.md and run full verification

**Files:**
- Modify: `PROGRESS.md`

**Interfaces:**
- Consumes: all previous tasks (this is the final task).

- [ ] **Step 1: Flip the PROGRESS.md line item for #45**

In `PROGRESS.md`, under `## Install & run piperd as a service`, change:

```markdown
- ⬜ Container image + compose (host `docker.sock`; publish blocked on release pipeline) — [#45](https://github.com/getpiper/piper/issues/45)
```

to:

```markdown
- ✅ Container image + compose (host `docker.sock`; registry publish tracked separately) — [#45](https://github.com/getpiper/piper/issues/45)
```

- [ ] **Step 2: Update the "Last updated" line**

Change the `_Last updated: ..._` line at the top of `PROGRESS.md` to reflect this change, e.g.:

```markdown
_Last updated: 2026-07-07 — piperd ships a container image + Compose file (epic #43); registry publish remains a follow-up. Plan 3 complete: push-to-deploy plus PR-preview URLs + teardown (`pr-<N>-<app>.<base>`, flattened for the wildcard cert). Live tracker: [issues](https://github.com/getpiper/piper/issues)._
```

- [ ] **Step 3: Run the full verification sequence**

Run, in order:

```bash
gofmt -l .
go vet ./...
make test
make cross
docker build -t piperd:local .
docker compose -f deploy/compose/docker-compose.yml config
```

Expected: `gofmt -l .` prints nothing; `go vet` and `make test` exit 0; `make cross` exits 0; the `docker build` succeeds; `docker compose ... config` prints the resolved config and exits 0.

- [ ] **Step 4: Commit**

```bash
git add PROGRESS.md
git commit -m "docs(progress): mark piperd container image + compose as done (#45)"
```

---

## Out of scope (do not implement)

- Publishing a tagged image to a registry (GHCR or goreleaser `dockers:`).
- Any change to `internal/runtime`, `internal/deploy`, or how app containers are built/run/networked.
- A `HEALTHCHECK` directive for the `piperd` container.
- launchd or non-Linux container support.
