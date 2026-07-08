# Manual E2E Runbook — `git push → live HTTPS URL`

This walks a human operator through the **entire** Piper stack end-to-end against
real infrastructure: a public relay, on-box TLS, a per-user GitHub App, and a real
`git push` that builds and publishes an app. It exercises what the automated tests
can't — real GitHub credentials, a real domain, a real tunnel, and a browser-trusted
cert.

If you only want to smoke-test the tunnel/TLS plumbing without GitHub or a public
box, jump to [Appendix A — local loopback smoke test](#appendix-a--local-loopback-smoke-test).

---

## What this proves

```
 git push
    │
    ▼
 GitHub ──webhook──▶ hooks.<base> ─┐
    ▲                              │  (public :443, SNI passthrough — relay never decrypts)
    │  Deployment status           ▼
    │                          ┌────────┐        ┌──────────────── your box ───────────────┐
    └──────────────────────────│ relay  │◀──tunnel(:7000)── piperd ─▶ Caddy :443 (TLS here) │
                               └────────┘                     │        │                     │
   browser ─▶ myapp.<base> :443 ─(SNI)─▶ relay ─▶ tunnel ─────┘        └▶ Docker container   │
                                                                       └──────────────────────┘
```

A push to a linked repo's tracked branch → GitHub webhook rides the tunnel to
`hooks.<base>` → piperd fetches the repo tarball, builds the Dockerfile, runs +
health-checks the container, routes it → the live URL `https://<app>.<base>` is
posted back to GitHub as a Deployment status.

---

## Prerequisites

**Roles** (can be three machines, or your laptop + one VPS):

| Role | What it needs |
| --- | --- |
| **Relay** | A host with a **public IP**, inbound `:443` and `:7000` open. A cheap VPS is fine. |
| **Box** (`piperd`) | Docker running. A Pi, a laptop, anything. Does **not** need a public IP. (Caddy is embedded in `piperd` — nothing to install.) |
| **Operator** | The `piper` CLI + a browser (for the GitHub App approval redirect). Usually the box itself. |

**Accounts / assets:**

- A **domain you control**, used as `<base>` (e.g. `alice.dev`). All apps live at
  `*.<base>`.
- **DNS you can point** `*.<base>` and `<base>` at the relay's public IP.
- For the wildcard cert: **DNS-01 API credentials** for that domain (this runbook
  uses Cloudflare — the only wired provider), **or** a browser-trusted wildcard cert
  you already own (BYO).
- A **GitHub repo** with a `Dockerfile` at its root, and the app inside it listening
  on a known container port (default `8080`).

> **Cert must be publicly trusted.** GitHub will refuse to deliver webhooks to a host
> with an untrusted cert, so this full path requires **Let's Encrypt production**
> (or a real CA) — not staging, not self-signed. Mind LE rate limits while iterating.

**Build the binaries** (on both relay and box, or cross-compile for the Pi):

```bash
make build          # → bin/piperd, bin/piper, bin/piper-relay
# Pi target: CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/piperd ./cmd/piperd  (+ piper-relay)
```

---

## Part A — DNS

Point both the wildcard and the apex at the **relay's public IP**. The wildcard
covers every app host *and* `hooks.<base>`; the apex is where the cert's SAN sits.

```
*.<base>   A   <relay-public-ip>
<base>     A   <relay-public-ip>
```

Verify before continuing:

```bash
dig +short myapp.<base>      # → <relay-public-ip>
dig +short hooks.<base>      # → <relay-public-ip>
```

> DNS-01 issuance validates via a `_acme-challenge.<base>` TXT record that lego
> writes and deletes through your provider API — that's separate from the A records
> above and needs the API token from Part C.

---

## Part B — Relay

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

---

## Part C — Box (`piperd` in relay mode)

`piperd` enters relay mode the moment `PIPER_RELAY_ADDR` is set: it obtains the
wildcard cert, loads it into Caddy on `:443`, dials the relay tunnel, and (once a
GitHub App exists) serves webhooks at `hooks.<base>`.

Pick **one** TLS path. The env vars below can be exported for a foreground run
(handy while walking this runbook, since logs stream to your terminal) or, for a
box that should survive reboots, dropped into `/etc/piper/piperd.env` and started
via the systemd unit (see [Run as a service](#run-piperd-as-a-service) below).

### Option 1 — ACME DNS-01 (Cloudflare), the real path

```bash
export PIPER_BASE_DOMAIN=<base>              # MUST equal the enrolled domain
export PIPER_RELAY_ADDR=<relay-public-ip>:7000
export PIPER_RELAY_TOKEN=rlyt_XXXXXXXXXXXXXXXX
export PIPER_ACME_EMAIL=you@example.com
export PIPER_DNS_PROVIDER=cloudflare
export CLOUDFLARE_DNS_API_TOKEN=<token-with-dns-edit-on-the-zone>
# export PIPER_ACME_CA=https://acme-staging-v02.api.letsencrypt.org/directory  # plumbing-only; GitHub rejects staging certs
export PIPER_DATA_DIR=$HOME/.piper

./bin/piperd
#   piperd listening on 127.0.0.1:8088 (apps at *.<base>)
#   no GitHub App configured; run `piper github setup` to enable git deploys
```

### Option 2 — BYO cert (you already hold a trusted wildcard)

```bash
export PIPER_BASE_DOMAIN=<base>
export PIPER_RELAY_ADDR=<relay-public-ip>:7000
export PIPER_RELAY_TOKEN=rlyt_XXXXXXXXXXXXXXXX
export PIPER_TLS_CERT_FILE=/path/fullchain.pem   # must cover *.<base> AND <base>
export PIPER_TLS_KEY_FILE=/path/privkey.pem
export PIPER_DATA_DIR=$HOME/.piper

./bin/piperd
```

### Run piperd as a service

For anything past a one-off test, run `piperd` under systemd so it comes back on
boot and restarts on failure. Put the TLS/relay env from the option above into
`/etc/piper/piperd.env` (mode `0600`) instead of exporting it, then install the
unit. State lives at `PIPER_DATA_DIR=/var/lib/piper` (set by the unit, not `$HOME`):

```bash
sudo install -m 0755 bin/piperd /usr/local/bin/piperd
sudo install -m 0644 packaging/systemd/piperd.service /etc/systemd/system/piperd.service
sudo install -d -m 0700 /etc/piper
sudo install -m 0600 packaging/systemd/piperd.env.example /etc/piper/piperd.env
# edit /etc/piper/piperd.env — add PIPER_RELAY_ADDR, PIPER_ACME_EMAIL, etc.
sudo systemctl daemon-reload
sudo systemctl enable --now piperd
sudo systemctl status piperd
sudo journalctl -u piperd -n 50 --no-pager
```

The unit runs as a `DynamicUser` in the `docker` group and binds `:80`/`:443` via
`CAP_NET_BIND_SERVICE` — no root. Apply later env edits with
`sudo systemctl restart piperd`.

To join the public relay instead of setting `PIPER_RELAY_ADDR`/`PIPER_RELAY_TOKEN`/`PIPER_BASE_DOMAIN`
by hand, run `piper login && piper connect` — on this systemd install `connect`
prints a ready `sudo sh -c … /etc/piper/piperd.env` command that upserts those
three keys for you (see the README's "Join the public relay").

**Health checks before moving on:**

- piperd logs `piperd listening …` and does **not** exit on `relay tls:` — a cert
  error here is the #1 blocker (see Troubleshooting).
- Relay logs show a tunnel client connecting.
- Docker is reachable (`docker ps` works as the piperd user) and `caddy version`
  resolves on `PATH`. If you run your own Caddy, set `PIPER_SKIP_CADDY=1` and route
  `:80`/`:443` yourself.

---

## Part D — Sanity: deploy one app by hand (proves Plan 1 + 2)

Before involving GitHub, prove the tunnel + TLS + deploy path with a manual deploy.
The repo ships a trivial `:8080` sample at `test/e2e/sampleapp` — use it, or any
local directory with a root `Dockerfile` whose app listens on the port you pass.

```bash
export PIPER_ADDR=http://127.0.0.1:8088          # only if piper runs off-box

./bin/piper create myapp --port 8080
./bin/piper deploy myapp --path ./test/e2e/sampleapp
#   deployed myapp: http://myapp.piper.localhost (running)   ← LAN URL in output

./bin/piper list
```

Now hit it **publicly through the relay** — this is the real proof:

```bash
curl -sS https://myapp.<base>/         # 200 from your container, TLS from your box
```

If that returns your app over HTTPS, the entire non-GitHub spine works. If it
doesn't, fix it here before adding GitHub — the git path rides these same rails.

---

## Part E — Create & install the GitHub App (one-time)

Run this **as the operator, with a browser available** (typically on the box, or
tunnel `127.0.0.1:8088` to your laptop). It asks piperd for a GitHub App manifest,
opens a browser to create the App under *your* account (or under an organization if
`--org <name>` is passed), catches the redirect, and
stores the App ID + private key + webhook secret **on the box** (they never leave it).

```bash
./bin/piper github setup [--org <name>]
#   Opening http://127.0.0.1:xxxxx — approve the App in your browser...
#   (browser: GitHub "Create App" screen → Create; it redirects back)
#   GitHub App configured. Install it on your repo, then run: piper app link <name> --repo owner/name
```

The App is named `piper-<base>`, subscribes to **push + pull_request**, points its
webhook at **`https://hooks.<base>`**, and requests `contents:read`,
`deployments:write`, `pull_requests:read`.

Then, in the GitHub UI: **Install the App** on the target repo (App settings →
Install App → pick the repo). piperd starts serving `hooks.<base>` as soon as
`github setup` completes — no restart needed.

**Confirm webhook delivery is reachable:** in GitHub → the App → *Advanced* →
*Recent Deliveries*, the initial `ping` should show a `2xx`. A red delivery here
means DNS/cert/tunnel for `hooks.<base>` isn't right — fix before pushing.

---

## Part F — Link a repo and push (proves Plan 3)

```bash
./bin/piper app link myapp --repo owner/name --branch main
#   linked myapp -> owner/name (main)
```

Now the payoff:

```bash
# in a clone of owner/name, on the tracked branch:
git commit --allow-empty -m "trigger piper deploy"
git push origin main
```

**Watch it happen:**

- **piperd logs:** webhook received → build → run → health → route.
- **GitHub → the repo → Deployments** (or the commit's status): a `production`
  deployment goes **pending → success**, and `success` carries the
  **Environment URL** `https://myapp.<base>`.
- **The live app:**

  ```bash
  curl -sS https://myapp.<base>/        # serves the just-pushed commit
  ```

That's the full loop: `git push` → live HTTPS URL, status reported to GitHub. ✅

To confirm it's really redeploying, change something visible in the app, push again,
and re-`curl` — the response should reflect the new commit, and a second Deployment
should appear.

---

## Teardown

```bash
# Box: stop piperd. Foreground run: Ctrl-C. Managed service:
sudo systemctl disable --now piperd
sudo systemctl clean --what=state piperd    # drops /var/lib/piper
# Piper images are tagged piper/<app>:<ts>; containers get auto-generated names,
# so clean up by image ancestor, per app:
docker rm -f $(docker ps -aq --filter ancestor=piper/myapp) 2>/dev/null
docker images --filter=reference='piper/*' -q | xargs -r docker rmi -f
rm -rf "$PIPER_DATA_DIR"          # foreground run only; drops apps, links, and stored GitHub App creds

# Relay: stop and disable the service; remove its persistent enrollment state.
sudo systemctl disable --now piper-relay
sudo systemctl clean --what=state piper-relay
# GitHub: uninstall / delete the piper-<base> App from your account settings.
# DNS: remove the A records if this was a throwaway.
```

---

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| piperd exits with `relay tls:` | DNS-01 failed (bad/missing `CLOUDFLARE_DNS_API_TOKEN`, token lacks DNS-edit on the zone), or LE rate limit | Verify the token edits the zone; watch for `_acme-challenge` TXT appearing; back off if rate-limited |
| `curl https://myapp.<base>` hangs / conn refused | Relay `:443`/`:7000` not publicly open, or tunnel not connected | Check both firewalls; run `systemctl status piper-relay`, inspect `journalctl -u piper-relay`, and confirm listeners with `ss -lnt`; confirm `PIPER_RELAY_ADDR` uses host:7000 |
| `curl` returns cert error | Wrong/missing wildcard, or staging/self-signed cert | Cert must cover `*.<base>` **and** `<base>` from a trusted CA |
| `create`/`deploy` fails with "name reserved" | You used `hooks` as an app name | `hooks` is reserved for the webhook host; pick another |
| GitHub `ping` delivery is red | `hooks.<base>` unreachable or untrusted cert | Same as the curl cert/tunnel checks — `hooks.<base>` rides the identical path |
| Push does nothing, no piperd log | App not installed on the repo, or repo not linked, or pushed a non-tracked branch | Install the App on the repo; `piper app link … --branch <pushed-branch>` |
| Deploy starts but health-check fails | App doesn't listen on the `--port` you set | Match `piper create --port N` to the container's listen port |
| Webhook 401 in piperd logs | Signature mismatch — stale App creds | Re-run `piper github setup`; ensure only one `piper-<base>` App is installed |

---

## Appendix A — local loopback smoke test

No domain, no VPS, no GitHub — just proves the relay→tunnel→TLS→container plumbing
on one machine. This mirrors `test/e2e/relay_test.go`. It **cannot** test the git
path (GitHub can't reach a private box, nor accept a self-signed webhook cert).

Use unprivileged ports and a self-signed wildcard, and add hosts entries so the
names resolve to loopback:

```bash
base=alice.localhost
# self-signed *.$base cert → cert.pem / key.pem (openssl or the helper in relay_test.go)

# terminal 1 — relay on unprivileged ports
PIPER_RELAY_TLS_ADDR=:8443 PIPER_RELAY_TUNNEL_ADDR=:7000 \
  ./bin/piper-relay enroll alice --domain $base           # copy token
PIPER_RELAY_TLS_ADDR=:8443 PIPER_RELAY_TUNNEL_ADDR=:7000 ./bin/piper-relay

# terminal 2 — box
PIPER_BASE_DOMAIN=$base PIPER_RELAY_ADDR=127.0.0.1:7000 \
  PIPER_RELAY_TOKEN=<token> \
  PIPER_TLS_CERT_FILE=cert.pem PIPER_TLS_KEY_FILE=key.pem \
  PIPER_DATA_DIR=$(mktemp -d) ./bin/piperd

# terminal 3 — deploy + hit it through the relay (SNI must match the cert)
./bin/piper create myapp --port 8080
./bin/piper deploy myapp --path ./test/e2e/sampleapp
curl -k --resolve myapp.$base:8443:127.0.0.1 https://myapp.$base:8443/
```

A `200` here means the tunnel + SNI + on-box TLS + container path is sound. Graduate
to the full runbook above to add the public relay and the GitHub half.
