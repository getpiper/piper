# TUI: per-app domains in the app drilldown — design

**Issue:** #285 (deferred from #232). **Date:** 2026-07-20.

The TUI has no domain surface; `piper domains` and the #231 API are the only ways
to manage per-app custom domains. This adds list + add/remove to the app
drilldown, following the epic #224 UX vocabulary already used by the CLI.

## Layout: inline section, unified cursor

`appDetailView` gains a `DOMAINS` table under the deployments table, loaded in
the same `refresh` poll (`appDetailLoadedMsg` grows a
`domains []domain.AppDomainStatus` field), so status updates ride the existing
2s tick. One cursor traverses deployments first, then domains; keys are
context-sensitive by section:

- `a` — anywhere in the view: push the add-domain form.
- `x` — domain row: remove-domain y/n confirm; deployment row (or empty):
  delete-app type-name confirm, unchanged.
- `enter` — domain row: push the domain detail view; deployment row: logs,
  unchanged.
- Footer legend swaps with the cursor's section.

Rendering matches `piper domains list`: status `pending → issuing → active` /
`failed` with a one-glyph icon, `CERT EXPIRES` as `2006-01-02` or `-`, `DNS`
as `ok`/`no`.

```
  blog   https://blog.example.dev   :8080   repo=-

  DEPLOYMENT     STATUS       CREATED
  a1b2c3d4       ● running    2h ago

  DOMAIN            STATUS      CERT EXPIRES  DNS
▸ blog.example.com  ◌ pending   -             no
  www.example.com   ● active    2026-10-01    ok
```

## Add flow

`domainFormView`: a one-field form mirroring `linkFormView`. Submit emits
`addDomainMsg{app, domain}`; the root runs `AddAppDomain` off the UI thread
(the standard mutating-action pattern). On success the root **replaces** the
form with the domain detail view (the `deployStartedMsg` → logs replacement
trick), so the CNAME and live status appear immediately. On error, banner in
the form. Validation beyond non-empty stays server-side (`ErrInvalidDomain`).

## Domain detail view

`domainDetailView{app, domain}`: status, dns_ok, cert expiry, the exact CNAME
record(s) from `DNSRecords`, the issuance-waits-on-DNS note, and the error text
when failed. Its `refresh` polls `AppDomains(app)` and picks out its domain, so
it tracks `pending → issuing → active` live without leaving the TUI. Reached
from both the add flow and `enter` on a domain row.

## Remove

`newRemoveDomainConfirm` reuses `confirmView` in y/n mode (removal is
re-addable, so no type-name guard) → `removeDomainMsg{app, domain}` → root
calls `RemoveAppDomain` → `actionResultMsg{popLevels: 1}` back to the refreshed
detail view.

## Plumbing

- TUI `API` interface gains the three methods `*client.Client` already has:
  `AppDomains(app)`, `AddAppDomain(app, dom)`, `RemoveAppDomain(app, dom)`;
  `fakeAPI` grows matching stubs.
- New messages in `tui.go`: `addDomainMsg`, `removeDomainMsg`,
  `domainAddedMsg`, `domainDetailLoadedMsg` (a `pollResult`).
- Help overlay and app-detail footer gain the new keys.

## Testing

Same style as `appdetail_test.go` / `confirm_test.go`: fake API with canned
`AppDomainStatus` rows, drive `Update` with key messages, assert `View()`
output and emitted intent messages. TDD per task.

## Acceptance criteria (from #285)

- [ ] Domains visible in the app drilldown with live status.
- [ ] Add shows the exact CNAME and tracks pending → active without leaving the TUI.
- [ ] Remove confirms and cleans up.
- [ ] Keys discoverable via the footer legend + `?` help overlay.
