# Manual setup (building from source)

The one-line installer in the [README](../README.md#install) already does everything
below for you. Use this instead if you're building `piperd`/`piper-relay` from source,
or wiring your own automation.

## Run the agent as a service (manual / from source)

On the box that runs your apps (a Pi, a VPS, a laptop), install the static
`piperd` binary and the shipped systemd unit so the agent runs headless and comes back
on boot (`install` is the coreutils command — copy-into-place with a mode, needing root
to write these system paths):

```bash
sudo install -m 0755 bin/piperd /usr/local/bin/piperd
sudo install -m 0644 packaging/systemd/piperd.service \
  /etc/systemd/system/piperd.service
sudo install -d -m 0700 /etc/piper
sudo install -m 0600 packaging/systemd/piperd.env.example /etc/piper/piperd.env
sudo systemctl daemon-reload
sudo systemctl enable --now piperd
```

The unit runs as a `DynamicUser` in the `docker` group (so piperd can drive the host
Docker daemon), keeps state under `PIPER_DATA_DIR=/var/lib/piper`, and binds `:80`/`:443`
via `CAP_NET_BIND_SERVICE` — no root. Edit `/etc/piper/piperd.env` to override defaults
or switch on relay mode. See the
[end-to-end runbook](runbooks/git-deploy-e2e.md) for verification, logs, and teardown.

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
[end-to-end runbook](runbooks/git-deploy-e2e.md#part-b--relay) for verification,
address overrides, logs, and teardown.
