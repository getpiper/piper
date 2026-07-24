# Piper Relay systemd Service Design

**Issue:** [#38](https://github.com/piperbox/piper/issues/38)

## Goal

Provide a supported systemd installation for `piper-relay` that starts on boot,
restarts after failure, binds ports 443 and 7000 without running as root, and keeps
the enrollment database in persistent system-managed storage.

## Packaging

Add `packaging/systemd/piper-relay.service`. The service runs
`/usr/local/bin/piper-relay` with a systemd dynamic user and uses
`StateDirectory=piper-relay`, exposed to the process as
`PIPER_RELAY_DATA_DIR=/var/lib/piper-relay`. It reads optional address overrides
from `/etc/piper-relay.env`.

The unit waits for network availability, restarts on failure, and grants only
`CAP_NET_BIND_SERVICE` so the unprivileged process can bind port 443. It also uses
systemd's standard filesystem and privilege hardening directives without
restricting the network access required by the relay.

## Enrollment and service lifecycle

Enrollment remains a manual one-shot operation and is not part of service startup.
The documented enrollment command uses a transient `systemd-run` unit with the
same `DynamicUser` and `StateDirectory` settings as the long-running service. This
allows enrollment to create or update `relay.db` with ownership that systemd can
hand safely to later dynamic-user instances.

Operators must not run enrollment directly as root against
`/var/lib/piper-relay`: doing so could create a root-owned database that the service
cannot open.

The documented sequence is:

1. Install the static binary at `/usr/local/bin/piper-relay` and the unit at
   `/etc/systemd/system/piper-relay.service`.
2. Reload systemd and run enrollment through `systemd-run`, retaining its printed
   token.
3. Enable and start `piper-relay`.
4. Verify unit status, logs, and listeners on TCP ports 443 and 7000.
5. Open inbound TCP ports 443 and 7000 in both host and provider firewalls.

## Documentation

Add a concise service-installation section to `README.md`. Update Part B of
`docs/runbooks/git-deploy-e2e.md` so the primary VPS procedure installs, enrolls,
and enables the service instead of keeping a foreground process alive. Update its
teardown and troubleshooting guidance to use `systemctl` and `journalctl`.

## Verification

Add a repository test that parses the unit as text and checks the operationally
required directives: executable path, persistent state directory, dynamic user,
bind capability, restart policy, and install target. The test also checks that the
README and runbook reference the shipped unit and service lifecycle commands.

Run the complete repository verification sequence before completion:

1. `gofmt -l .`
2. `go vet ./...`
3. `make test`
4. `make cross`

## Out of scope

- Container or Compose packaging.
- Release archive integration or GoReleaser configuration.
- A managed `piperd` service.
- TLS, ACME, relay protocol, or application-code changes.
