---
name: file-issue
description: File a GitHub issue on this repo with the required title prefix and labels. Use when opening an issue, or when a finding during other work is worth tracking rather than fixing inline.
---

# Filing an issue

Piper's tracker is public and is how contributors and future sessions see what's
done and what's open. Every issue needs a title prefix and a full label set — a
bare issue is an incomplete one.

## Title: `[area] lowercase description`

The prefix names the surface the work touches, so scanning the board (or
`gh issue list --search "in:title tunnel"`) finds everything in an area.

| Prefix | Surface |
| --- | --- |
| `[agent]` | `piperd` daemon: control-plane API, orchestration |
| `[cli]` | the `piper` CLI |
| `[deploy]` | build → run → health → route → retire flow |
| `[runtime]` | Docker build/run driver |
| `[proxy]` | Caddy routing, TLS |
| `[store]` | SQLite persistence |
| `[relay]` | `piper-relay` + outbound tunnel |
| `[repo]` | governance: CI, branch protection, tooling |
| `[docs]` | docs, design, README |

Reuse an existing prefix unless a genuinely new surface has appeared.

## Labels: type + priority + size + binary

**All four groups**, every time. Note the prefixes above are *not* labels — the
only binary labels are `agent`, `cli`, `relay`.

- **Type** — `bug`, `enhancement`, or `documentation`
- **Priority** — `P1` golden-path user pain · `P2` real bugs, hardening, roadmap
  gaps · `P3` cleanups, test gaps, docs, polish
- **Size** — `size/XS` a few lines · `size/S` up to ~half a day · `size/M` ~a day
  · `size/L` several days · `size/XL` epic-scale, so split it
- **Binary** — `agent`, `cli`, `relay`; more than one is fine

Size is orthogonal to priority: small-but-`P1` and large-but-`P3` are both
normal. Add `epic` for a multi-part tracker whose sub-tasks are checkboxes
linking child issues, and the usual open-source labels (`good first issue`,
`help wanted`) where they fit.

Confirm what exists rather than guessing: `gh label list`.

## Body

State the defect, not the fix hunt. What earns its place:

- **Symptom** — the observable failure, with the real error text where there is one
- **Root cause** — file and line, and the mechanism, when you have traced it
- **A concrete failure case** — the inputs or state that trigger it
- **Suggested fix** — only if you have one worth acting on
- **Honesty about confidence** — say plainly when something is derived from
  reading code rather than reproduced, so nobody treats an inference as observed

If a finding surfaced from other work, link that PR or issue. If it means an
earlier issue was closed incomplete, say so — that is useful signal, not an
accusation.

## Scale it

A typo needs no issue at all. A plan task or real defect gets the full treatment.
Do not open an issue for something you are about to fix in the same change.

## Afterwards

Reference the issue from commits and the PR body as `Part of #N`, and put
`Closes #N` in the PR body only when the merge genuinely finishes it. Do not
close issues by hand mid-session — let the merge do it, and leave the issue open
when a real remainder survives.
