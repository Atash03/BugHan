---
title: "BugHan — product design map"
labels: [wayfinder:map]
mirror_of: https://github.com/Atash03/BugHan/issues/1
---

## Destination

A locked product design for **BugHan**: a public, OSS-shaped, self-hosted error + performance tracking platform in the Sentry/GlitchTip class — multi-tenant data model (organizations → projects → members), frontend-focused via **Sentry-compatible ingestion** (official browser SDKs work as-is; no custom protocol, no custom SDK in v1). The design (deliverable: `DESIGN.md` in this repo) must cover: domain/data model, ingest protocol compat scope, event pipeline & storage, issue grouping & lifecycle, web UI, source maps, thin releases & session health, thin alerting, auth, and a modest-self-host deployment topology. Posture: single Docker-Compose instance, ~10s of projects, ≤1M events/day. The map is done when every decision below is locked and the design can be handed to implementation. **Planning-only effort — implementation is a future effort, not part of this map.**

> **Destination update (session goal, 2026-09-06):** the user redrew the destination from *planning-only* to **implement the product**. All design tickets are resolved (see Decisions so far) and `DESIGN.md` locks the design; implementation proceeds under a new implementation map, tracked in this repo.

## Notes

- Domain: error tracking, APM/performance traces, OSS self-hosted observability. Solo maintainer at crypto. OSS-shaped: public repo, contributors welcome.
- Skills per session: load `grill-me` + `domain-model` before every grilling ticket; `prototype` + `impeccable`/`design-taste-frontend` for UI-shape tickets; research tickets go to a subagent doing web/GitHub research.
- Scope decisions already fixed at charting (they live in the tickets that detail them, not here): pillars = **Errors + Performance (full)**, Releases + Sessions (**thin**), Alerting (**thin-in**: rules → email/webhook, no digests/channel apps); **Sentry-compatible ingestion**; focus = frontend/browser apps; repo public, named BugHan; modest self-host scale.
- Tracking conventions (GitHub has no native issue blocking → body convention): tickets carry `wayfinder:<type>` + `wayfinder:ticket` labels and list dependencies as `Blocked by: #id (Name)`. Frontier = open, unblocked, unassigned tickets in this repo. **Claiming = assign the issue to yourself (human: Atash03) before working it.**
- Research resolutions: findings live on `research/<slug>` branches of this repo, linked from the ticket. Each ticket records its answer on closure; this map only indexes gists.
- **Autonomy note (2026-09-06)**: the design tail (#22–#33) was resolved in one autonomous session per the redrawn destination; each resolution comment marks itself overridable by reopening the ticket.

## Decisions so far
- [Sentry protocol compat scope](https://github.com/Atash03/BugHan/issues/17) — ingest facts: `/envelope/` single endpoint, envelope parsing (unknown item types retained), required event fields, 429/413 rate-limit contract, separate `/api/0` upload API, target sentry-js v8–v10 (research)[`research/sentry-protocol`](https://github.com/Atash03/BugHan/tree/research/sentry-protocol).
- [Storage engine choice](https://github.com/Atash03/BugHan/issues/18) — Postgres-first verdict (JSONB + generated-column indexes + rollup tables), reversible hybrid-ClickHouse path; **open threshold** vs alternative view (see [`research/storage-engine-alt-hybrid`](https://github.com/Atash03/BugHan/tree/research/storage-engine-alt-hybrid)) — threshold resolved in Event pipeline ticket [`#32`](https://github.com/Atash03/BugHan/issues/32).
- [GlitchTip architecture study](https://github.com/Atash03/BugHan/issues/19) — Django/ninja + Angular, Postgres partitioned as sole required store (+ optional Valkey), Granian all-in-one process, MD5(title+culprit+type) grouping (our clearest "beat it" target), per-type retention w/ DuckDB cold storage, MIT (research)[`research/glitchtip-architecture`](https://github.com/Atash03/BugHan/tree/research/glitchtip-architecture).
- [What the browser SDK sends](https://github.com/Atash03/BugHan/issues/20) — envelope shapes for error/transaction/session, web-vital measurements (lcp/cls/fp/fcp/ttfb/inp), span-op conventions, sentry-trace/baggage propagation, SDK drop-vs-retry semantics (research)[`research/browser-sdk`](https://github.com/Atash03/BugHan/tree/research/browser-sdk).
- [Data model and multi-tenancy](https://github.com/Atash03/BugHan/issues/21) — flat Organization → Project (**no Teams in v0.1**, permission seam documented); org-wide roles owner/admin/member (no per-project ACLs); multiple Ingest Keys per project (no secret key) + User-owned API Tokens; open signup, invite-only org join; Environment & Release are per-project entities, Tags stay attributes, Affected User ≠ User; no notification-preference entities (alert rules carry recipients) — glossary seeded in [`CONTEXT.md`](https://github.com/Atash03/BugHan/blob/main/CONTEXT.md).
- [Auth model](https://github.com/Atash03/BugHan/issues/22) — email+password (argon2id), server-side cookie sessions (14d sliding), SMTP-gated verification/reset + CLI fallback; invites = single-use token links (7d); ingest auth via sentry_key (query/header/envelope-dsn, cached); user-owned API tokens `bghan_` (hashed, Bearer, read/write scopes); SSO/2FA/org-keys out.
- [License and governance](https://github.com/Atash03/BugHan/issues/23) — MIT everywhere; clean-room from public protocol spec, no Sentry name/branding/FSL code, NOTICE for any vendored BSD/MIT code; CONTRIBUTING + CoC + templates; single root `DESIGN.md`, ADR-append policy for reversals.
- [Issue grouping depth](https://github.com/Atash03/BugHan/issues/24) — hybrid: SDK `fingerprint` array wins (with `{{default}}` expansion); default = SHA-256(type, normalized title [param/number/uuid normalization, 256c], culprit from top in-app frame [hash-stripped filename + function]); server-side from stable fields → SDK-upgrade-stable; versioned grouping algorithm; resolved+event → regressed; issue carries first/last-seen, count, user_count, level, assignee.
- [Releases and session health](https://github.com/Atash03/BugHan/issues/25) — Release = (project, version); env/dist are dimensions; implicit creation on first sighting or explicit via API; "new release" = first-event ≤24h; sessions stored individually + hourly (project, release, env) rollups; crash = status `crashed`; crash-free session/user rates; deploy hooks/commit linking/diffs dropped.
- [Web app information architecture](https://github.com/Atash03/BugHan/issues/26) — top-bar shell (org/project switcher, env filter, search) + sidebar Issues/Performance/Releases/Projects/Settings; 11-page set: org dashboard, issues list (filters + saved views), issue detail (stacktrace/breadcrumbs/tags/feedback/activity), performance, trace view, releases, projects, feedback, alerts, settings (org/project/user), auth pages; issues-first hierarchy.
- [Source maps and symbolication](https://github.com/Atash03/BugHan/issues/27) — v0.1 = classic release-files API (`/api/0/.../releases/{version}/files/` multipart, Bearer token) + release-create; chunk-upload/assemble deferred v0.2; match by (release, dist, abs_path) + debug_id index; artifacts in Postgres bytea (50MB/file, 500MB/release caps, SHA-256 dedupe); symbolication at-view-time with write-back cache; missing-map hints; sourcesContent snippets.
- [Performance UX and trace view](https://github.com/Atash03/BugHan/issues/28) — waterfall (root + spans, error ticks via trace_id), web-vitals chips w/ thresholds, span list + filters, self-time breakdown; hourly per-transaction-name rollups (p50/p95/p99, failure rate); "recently slow" = p95 > 2× 7-day baseline; errors carry trace_id/span_id → bidirectional issue↔trace links.
- [Thin alerting spec](https://github.com/Atash03/BugHan/issues/29) — project rules: triggers new_issue/regression/event_count(N, M) with env/release/level filters; actions = email list + generic webhook (HMAC-signed, versioned JSON payload); immediate delivery, per-issue quiet period (30m default), quiet-state table; CRUD + test button + delivery log; digests/channel apps/notification center out.
- [Event pipeline and storage schema](https://github.com/Atash03/BugHan/issues/32) — **Postgres-only v0.1** (threshold: >200k events/day or >100 GB → hybrid ClickHouse); **Go single binary** (net/http, pgx, server-rendered + htmx, embedded worker, `jobs` table + LISTEN/NOTIFY); pipeline: decompress→auth→frame→validate→inline normalize/group/persist (+rollups)→async alerts; monthly partitions; full table list incl. events/transactions (spans in payload)/sessions+session_rollups/issues/event_rollups/feedbacks/release_files/alerts; retention 90/30/30/365 per type; event_id PK dedupe, session upsert rules, per-project rate limits.
- [Deployment topology](https://github.com/Atash03/BugHan/issues/30) — compose = db (postgres:16) + web (single binary, embedded worker, migrations on boot; optional worker split via env); `.env` surface (secret, db url, url, retention, SMTP); sizing table (1GB→4GB); `/api/health/`; forward-only migrations, stdout logs; backup = nightly pg_dump runbook; unsupported: multi-instance/HA/K8s.

<!-- one line per closed ticket: <title> (link) — gist -->

## Not yet specified

- Trace-context propagation into own backend services (Go/Node) — hangs off the SDK strategy; backend SDKs are a future effort.
- Notification channel breadth beyond email/webhook (e.g. Telegram) — likely a future map.
- UI design language / design-system conventions (settles during implementation).
- Query/data API surface for external tooling (sharpens after UI + D8).
- Release deploy automation (sentry-cli/CI commit linking) — thin scope questions (sharpens after D9).
- Chunk-upload + artifact-bundle assemble endpoints for bundler-plugin debug-ID source-map uploads (deferred v0.2 by the source-maps decision).

## Out of scope

- Session replay/recordings, profiling, uptime monitoring — excluded pillars.
- Native mobile & non-browser SDKs as designed/tested artifacts (Sentry-compat ingestion may still accept their envelopes; no support commitment).
- Full alerting parity: digests, channel integrations, notification center.
- Horizontal scaling / service-scale architecture; SSO/SAML/2FA; UI i18n; BugHan's own observability.

<!-- Out of scope additions from resolutions -->
- `@bughan/browser` SDK wrapper — deferred, not excluded (condition in [SDK strategy](https://github.com/Atash03/BugHan/issues/31)).
