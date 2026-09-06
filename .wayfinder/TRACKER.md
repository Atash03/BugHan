# Wayfinding operations — GitHub tracker

**Canonical tracker: GitHub issues on `Atash03/BugHan`** (the earlier local-markdown plan was superseded mid-charting; this file documents how the map actually runs). Files under `.wayfinder/` are a read-only mirror — refresh them after each resolution, never treat them as the source of truth.

## Layout

- **Map**: [issue #1 — BugHan — product design map](https://github.com/Atash03/BugHan/issues/1), label `wayfinder:map`.
- **Tickets**: child issues labelled `wayfinder:<type>` (`research|prototype|grilling|task`) + `wayfinder:ticket`.
- **Research findings**: committed to `research/<slug>` branches; the closing ticket links its branch.

## Operations

- **Claim**: `gh issue edit <n> --add-assignee Atash03` **before any work**. Open + unassigned = unclaimed.
- **Block edge**: body convention — tickets list dependencies as `Blocked by: #id (Name)`. A ticket is unblocked when every listed id is `CLOSED`. (GitHub has no native blocking.)
- **Frontier query**: open + unassigned + every `Blocked by:` id closed. Example:
  ```sh
  gh issue list --state open --json number,title,assignees,labels \
    --jq '.[] | select((.assignees|length)==0) | "\(.number)\t\(.title)"'
  ```
  then check each candidate body's `Blocked by:` lines against closed issues.
- **Resolve**: post the answer as a resolution comment → close the issue → append one gist line to the map's *Decisions so far*. The decision lives only in its ticket; the map indexes.
- **Fog / out of scope**: live in the map body's *Not yet specified* / *Out of scope* sections. Graduating fog = create a ticket + delete the fog line.
- **Mirror sync**: after any map/ticket change, regenerate `.wayfinder/map.md` from issue #1's body.

## Session conventions

- Load `grill-me` + `domain-model` before grilling tickets; `prototype` (+ design skills) for UI-shape tickets.
- Never resolve more than one ticket per session (research tickets excepted).
