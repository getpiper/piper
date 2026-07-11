# Relay organizations — design

Design for [#104](https://github.com/getpiper/piper/issues/104): org model,
membership, and org-scoped agent authz on the relay. Phase-3 dependency of the
dashboard roadmap (#76). Builds directly on the trust model
(`2026-07-07-relay-control-trust-model-design.md`) and GitHub identities (#99):
relay accounts are GitHub identities today, boxes belong to one account, and the
control proxy authorizes `owner == caller`. Organizations make a team share
boxes/apps — a free feature.

## Decision: an org is an account

An org is a row in `accounts` with `type='org'`, no GitHub identity, and no
credentials — it can never log in or call anything; it exists only to be an
owner. This is the GitHub model (orgs are accounts), and it is the shape the
existing code is already implicitly built for: everything that means "owner"
in the relay — `agents.account_id`, `hostnames.account_id`, `appHostname`,
the per-account agent/app quotas, the operator kill-switch — keys off
`accounts.id` and works for orgs unchanged.

Rejected alternatives:

- **Separate `orgs` table with polymorphic `owner_type`/`owner_id`** on agents
  and hostnames: strongest typing, but every ownership query, the hostname
  derivation, and the quota logic grow a branch, and org-slug vs. username
  uniqueness needs a cross-table constraint enforced in code. Roughly double
  the surface for the same behavior.
- **Nullable `org_id` alongside `account_id` on agents**: smallest diff, but
  ownership goes ambiguous (the enroller keeps `account_id` while the org is a
  second authz path — who owns the box when the enroller leaves the org?).
  That ambiguity is what #104 exists to remove.

The username/slug namespace being shared is a requirement, not a side effect:
both user and org names appear as DNS-label components in relay-assigned
hostnames (`<hash>-<name>.<apex>`), so they must be unique in one namespace —
which the `accounts.username` unique column already provides.

## Data model

Schema changes (`internal/relay/schema.sql` + migrations in `store.go`):

```sql
-- accounts: two new columns, one relaxed constraint
type         TEXT NOT NULL DEFAULT 'user'   -- 'user' | 'org'
github_id    TEXT UNIQUE                    -- now nullable: NULL for orgs
github_login TEXT                           -- raw GitHub login, refreshed at every login

CREATE TABLE org_members (
    org_id     TEXT NOT NULL REFERENCES accounts(id),
    account_id TEXT NOT NULL REFERENCES accounts(id),
    role       TEXT NOT NULL,               -- 'owner' | 'member'
    created_at TEXT NOT NULL,
    PRIMARY KEY (org_id, account_id)
);

CREATE TABLE org_invites (
    org_id       TEXT NOT NULL REFERENCES accounts(id),
    github_login TEXT NOT NULL,             -- stored lowercased
    invited_by   TEXT NOT NULL REFERENCES accounts(id),
    created_at   TEXT NOT NULL,
    PRIMARY KEY (org_id, github_login)
);
```

- `github_login` is needed because `accounts.username` is a *derived, munged*
  slug (lowercased, truncated, possibly `-2`-suffixed) — it cannot match an
  invite typed as a GitHub username. The raw login is stored on the account and
  refreshed on every login (GitHub logins can be renamed).
- SQLite cannot drop `NOT NULL` in place, so existing DBs migrate `accounts`
  via table rebuild (create-copy-drop-rename); fresh installs get the new
  `schema.sql`. The existing `ALTER TABLE ADD COLUMN` migration loop covers the
  purely additive columns.

## Org lifecycle

- **Create:** any user account may create an org. The org's slug is derived
  from the requested name with the existing `deriveUsername`, made unique in
  `accounts.username` exactly as user slugs are. The creator becomes the sole
  `owner`.
- **Invite:** owners invite by GitHub username. The invite is a pending row
  matched against user accounts by lowercased `github_login` — so inviting
  someone who has never logged into the relay works; the invite is waiting when
  they first sign in. Invites are always as `member`; owners promote afterwards.
  The invitee explicitly accepts or declines — nobody lands in an org without
  consent.
- **Roles:** owners manage, members drive. Members see and drive the org's
  boxes (full control plane). Owners additionally invite/remove members,
  promote/demote, enroll boxes into the org, and delete the org. No finer
  roles (YAGNI).
- **Delete:** owner-only, refused (`409`) while the org still owns agents — no
  orphaned boxes. No org rename in v1.

## Authz

The control proxy's owner check (`internal/relay/proxy.go`) becomes one store
query, `CanControl(callerID, agentBase)`:

> allowed iff the agent's owning account **is** the caller, or the caller has
> any `org_members` row for that owning account.

- Role does not matter here — owners and members both drive boxes; role only
  gates org management.
- Failure stays `404`, indistinguishable from "no such agent", so existence
  never leaks across tenants (the #104 acceptance criterion). Org management
  endpoints behave the same: a non-member hitting `/v1/orgs/<slug>/…` gets
  `404`.
- The disabled-owner rejection already in `AgentAccount` keeps working:
  disabling an org (operator kill-switch, by slug) severs every member's
  control access at once.
- **Nothing changes at the box.** The relay still injects the agent's stored
  control bearer (Token B), and `piperd` still validates it on every request.
  Token B stays **per-agent**, not per-member: membership is relay-side authz,
  consistent with the trust model's "the relay is the tenancy authority".

`GET /agents` grows from "my agents" to "my agents + my orgs' agents"; each
row gains an `owner` field (the owning slug) so callers can tell them apart.

## Enrollment into an org

`POST /v1/enroll` gains an optional `org` (slug). The relay checks the caller
is an **owner** of that org, then runs the existing `EnrollForAccount` with the
org's account id: the box gets `<hash>-<orgslug>.<apex>` and counts against the
org's own agent quota. Omitting `org` is personal enrollment, unchanged.
Transfer of existing personal boxes into an org is out of scope for v1
(enroll-into-org only) — it drags the hostname-rename question in for a flow
nobody has asked for yet.

## API surface

All under the authenticated account API (bearer = account credential). CLI
stays out of scope (#104: "CLI can follow later"); the dashboard consumes this
surface.

| Endpoint | Who |
| --- | --- |
| `POST /v1/orgs` `{name}` | any user account (creator becomes owner) |
| `GET /v1/orgs` | caller's orgs, with their role |
| `GET /v1/orgs/{slug}/members` | any member |
| `PUT /v1/orgs/{slug}/members/{username}` `{role}` | owner (promote/demote) |
| `DELETE /v1/orgs/{slug}/members/{username}` | owner, or the member themselves (leave) |
| `POST /v1/orgs/{slug}/invites` `{github_username}` | owner |
| `GET /v1/orgs/{slug}/invites` | owner |
| `DELETE /v1/orgs/{slug}/invites/{login}` | owner (revoke) |
| `GET /v1/invites` | caller's pending invites |
| `POST /v1/invites/{slug}/accept` · `…/decline` | the invitee |
| `DELETE /v1/orgs/{slug}` | owner, only when the org owns no agents |

## Edge cases

**The org stays inert as a principal:**

- No credential is ever minted for an org (`account_creds` never references
  one) and login upserts match on `github_id`, which an org lacks — an org
  structurally cannot authenticate. Belt-and-braces: `MintAccountCredential`
  and the login paths refuse `type='org'` rows outright.
- An org cannot be invited into an org (invites match `github_login`, which
  orgs don't have), and org creation is refused if the caller is somehow an
  org.
- Only `type='org'` accounts resolve under `/v1/orgs/…`; a user slug there is
  `404`.

**Membership:**

- **Last owner** cannot leave or demote themselves — `409` until another owner
  exists or the org is deleted. An org can never become ownerless.
- **Duplicate invite** is idempotent success; inviting an existing member is
  `409`. Invites to GitHub usernames the relay can't verify (it never calls
  GitHub's API for this) sit pending — harmless and revocable.
- **Removed/leaving members** lose box access on their next request — authz is
  evaluated per request; nothing to invalidate. Their personal boxes are
  untouched.
- **GitHub login renames:** `github_login` refreshes at every login, so a
  pending invite matches whoever *currently* holds that GitHub username — the
  same trust call GitHub's own org invites make.

**Abuse note:** orgs are free and self-service, so one user could create many
orgs to multiply their agent quota. v1 accepts this — the operator kill-switch
covers abuse, and a per-account org cap mirroring the existing agent/app caps
can be added if ever needed.

## Testing

TDD, at the three existing seams:

- **Store** (`accounts_test.go` pattern): org create + slug collision;
  membership CRUD + role changes; last-owner refusals; invite lifecycle
  including invite-before-first-login and login-rename matching; `CanControl`
  truth table; org agent quota independent of members' personal quotas;
  delete refused while agents exist; migration test against a pre-org DB
  fixture.
- **Control proxy** (`proxy_test.go` pattern): a member drives an org box
  end-to-end through the proxy; a non-member gets `404` (leak check, for both
  org boxes and org endpoints); a disabled org severs member access;
  `/agents` merges personal + org rows with `owner`.
- **API**: the role-enforcement matrix (member vs. owner across every
  management endpoint); enroll-with-org happy path and non-owner refusal.

## Non-goals

- Finer roles beyond owner/member.
- Cross-org box sharing / explicit grants.
- Transfer of existing personal boxes into an org (revisit on demand).
- Org rename.
- CLI `--org` support (follows later; the model lives in the control surface).
- Per-account org caps.
- Validating invited usernames against GitHub.
