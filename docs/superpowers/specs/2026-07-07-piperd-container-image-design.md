# piperd Container Image + Compose Design

**Issue:** [#45](https://github.com/piperbox/piper/issues/45) (part of epic [#43](https://github.com/piperbox/piper/issues/43))

## Goal

Package `piperd` itself as a container image + Compose file so it can run under
Docker instead of as a native systemd service. The containerized agent drives the
**host** Docker daemon via a mounted socket — sibling containers, not
Docker-in-Docker. This does not change how `piperd` builds and runs *app*
containers (`internal/runtime`, `internal/deploy` are untouched); it only adds a
second way to run `piperd` itself.

## Image

Add a multi-stage `Dockerfile` at the repo root:

1. **Build stage** — `golang:1.26`. Runs
   `CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /out/piperd ./cmd/piperd`.
   `TARGETOS`/`TARGETARCH` are buildx's automatic cross-build args — no manual
   platform matrix needed. Builds from source (not from a release archive): the
   goreleaser pipeline (#58) doesn't yet publish a `dockers:` image, so this is
   the only artifact source available today.
2. **Runtime stage** — `gcr.io/distroless/static-debian12` (the default, root
   variant — not `:nonroot`). It ships `ca-certificates`, which `piperd` needs for
   outbound TLS (GitHub API, ACME, relay dial), has no shell, and stays root so it
   can read `/var/run/docker.sock` without GID-matching gymnastics. Running the
   container as root does not meaningfully change the trust boundary: mounting
   the host's `docker.sock` is already root-equivalent access to the host
   regardless of the container's own UID (see Trust caveat below).

`ENTRYPOINT ["/usr/local/bin/piperd"]`. No `HEALTHCHECK`: `piperd` has no health
endpoint of its own (only *app* containers get health-checked, by `piperd`, over
the host network); adding one would be a new feature outside this issue's scope.

Multi-arch: `linux/amd64` + `linux/arm64`, matching the issue's acceptance
criteria (`armv7` is not included — the systemd path already covers it, and
scope here tracks the issue as written).

## Compose

Add `deploy/compose/docker-compose.yml`:

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

- **`network_mode: host` is a requirement, not a preference.**
  `internal/runtime/docker.go`'s `Run` binds every app container's port with
  `HostIP: 127.0.0.1`, and `WaitHealthy` dials `127.0.0.1:<hostPort>` — behavior
  the parent epic (#43) explicitly says must stay unchanged. For those dials to
  reach anything, `piperd` itself must share the host's network namespace;
  otherwise `127.0.0.1` inside `piperd`'s own container resolves to its own empty
  loopback, not the host's. Host networking also lets the embedded Caddy manager
  bind `:80`/`:443` directly, with no port-mapping section needed. Trade-off:
  Linux-only — acceptable, since the native install path already requires
  Linux + systemd (`install.sh` refuses a full agent install on non-Linux).
- `PIPER_DATA_DIR` is set via `environment:` (mirrors how the systemd unit sets
  it via `Environment=`, not through the env file) so the named volume always
  lands at a fixed, predictable path.
- `env_file` points at the existing `packaging/systemd/piperd.env.example`
  rather than a duplicate copy — one tracked file documents every optional
  override (`PIPER_BASE_DOMAIN`, `PIPER_RELAY_ADDR`, ACME/DNS vars, etc.) for
  both install paths. Users who want local overrides copy it
  (`cp packaging/systemd/piperd.env.example deploy/compose/piperd.env`) and
  point `env_file` at their copy; `deploy/compose/piperd.env` is added to
  `.gitignore` for that purpose (the tracked `.example` file is the default).

No `image:` reference is added — Compose builds locally from the Dockerfile.
Publishing a tagged image to a registry is out of scope (see below).

## App↔piperd reachability model

`piperd`, running as a sibling container with `network_mode: host`, sees the
Docker host's network namespace exactly as the native systemd-installed binary
does. App containers still get regular (non-host) networking with their ports
published to `127.0.0.1:<random-port>` on the host; `piperd` (loopback-shared
with the host) reaches them the same way it always has, health-checks them the
same way, and Caddy proxies to that same `127.0.0.1:<port>`. From the app
container's point of view, nothing changes — it doesn't know or care whether
`piperd` is itself containerized.

## Trust caveat

Mounting `/var/run/docker.sock` into a container grants that container control
over the entire host Docker daemon: it can create privileged containers, mount
arbitrary host paths, and effectively act as root on the host. This is not a new
risk specific to the container packaging — the systemd unit already grants
equivalent control-plane access via `SupplementaryGroups=docker` — but it's worth
stating plainly next to the compose file: **only run this on a box you already
trust with root**, which matches Piper's whole operating model (your own
hardware, not a shared host).

## CI

Extend `.github/workflows/ci.yml` with a step that runs
`docker buildx build --platform linux/amd64,linux/arm64 -f Dockerfile .`
(no push, no `--load`) whenever `Dockerfile`, `deploy/compose/**`, or the
workflow file itself changes — the same "changed paths gate an expensive step"
pattern already used for the Go `code` filter. This keeps "multi-arch buildable"
a real enforced gate, mirroring how `make cross` gates the Go build today,
without requiring registry credentials.

## Tests

Following the `packaging/systemd/piperd_test.go` convention (parse the shipped
file as text, assert required directives), add a test — e.g.
`deploy/compose/compose_test.go` — asserting:

- `docker-compose.yml` contains `network_mode: host`, the `docker.sock` bind
  mount, the `piper_data` volume, and the `env_file` reference.
- `Dockerfile` builds `./cmd/piperd` and uses the `distroless/static` base
  image.

## Documentation

Add a "Run piperd in Docker" section to `README.md` alongside the existing
systemd install instructions, covering: `docker compose -f
deploy/compose/docker-compose.yml up -d --build`, where data persists (the
`piper_data` volume), and a pointer to the reachability model and trust caveat
above. Update `PROGRESS.md`'s epic #43 line item for #45 from ⬜ to ✅ once
merged.

## Out of scope

- Publishing a tagged image to a registry (GHCR or otherwise) via goreleaser's
  `dockers:` section. The issue's acceptance criteria stop at "authored and
  tested locally"; even though the release pipeline (#58) is done, wiring actual
  publishing is a distinct concern (registry auth, tagging scheme, `latest`
  policy) best scoped as its own follow-up rather than folded in here.
- Any change to `internal/runtime`, `internal/deploy`, or how app containers are
  built/run/networked.
- launchd or non-Linux container support.
- A `HEALTHCHECK` for the `piperd` container itself.
