# BugHan — Product Design

**BugHan** is a public, OSS-shaped, self-hosted error + performance tracking platform in the Sentry/GlitchTip class. Frontend-focused: official Sentry browser SDKs work unmodified against it — no custom protocol, no custom SDK in v1.

This document is the locked product design produced by the [product design map](https://github.com/Atash03/BugHan/issues/1). Every decision links to its ticket, which holds the detail and rationale. The canonical glossary lives in [`CONTEXT.md`](./CONTEXT.md).

## 1. Scope & pillars

| Pillar | Depth |
|---|---|
| Errors | **Full** — ingest, grouping, triage, source maps |
| Performance (traces/transactions) | **Full** — ingest, waterfall, aggregates, web vitals |
| Releases + session health | **Thin** — crash-free rates, adoption; no deploy hooks/commit linking/diffs |
| Alerting | **Thin-in** — rules → email/webhook; no digests/channel apps/notification center |
| User feedback | Accepted (`feedback` + legacy `user_report`), read-only UI |

Out of scope: session replay, profiling, uptime monitoring, cron check-ins, native/non-browser SDKs, SSO/SAML, 2FA, UI i18n, multi-instance/HA.

Posture: single Docker-Compose instance, tens of projects, operating point tens-of-thousands events/day, ceiling ≤1M events/day.

## 2. Architecture

**Go, single static binary** (`bughan`) + **Postgres 16**. One process serves UI, REST API, and envelope ingest, and runs an embedded worker (a `jobs` table drained in-process; Postgres LISTEN/NOTIFY for wake-ups). No Redis, no ClickHouse, no external queue. ([Event pipeline](https://github.com/Atash03/BugHan/issues/32), [Deployment](https://github.com/Atash03/BugHan/issues/30))

```
┌────────────────────────── bughan (single binary) ──────────────────────────┐
│  net/http                                                                  │
│  ├─ /api/{project}/envelope/   ingest (decompress→auth→parse→persist)      │
│  ├─ /api/0/...                 REST API (Bearer API tokens) + release files│
│  ├─ /{org}/...                 server-rendered UI (html/template + htmx)   │
│  └─ embedded worker            alerts, retention sweep, rollup maintenance │
└────────────┬───────────────────────────────────────────────────────────────┘
             │ pgx
      ┌──────┴──────┐
      │  PostgreSQL │  everything: tenancy, events, rollups, files, jobs
      └─────────────┘
```

**Storage**: Postgres-only for v0.1 behind a thin storage layer. Hybrid ClickHouse cut-over trigger (documented, not built): sustained >200k events/day or raw event storage >100 GB. ([Storage](https://github.com/Atash03/BugHan/issues/18), [Pipeline](https://github.com/Atash03/BugHan/issues/32))

**Web UI**: server-rendered Go templates + htmx; no SPA build chain. Design polish per the [IA decision](https://github.com/Atash03/BugHan/issues/26).

## 3. Domain model

Flat tenancy per the [Data model ticket](https://github.com/Atash03/BugHan/issues/21):

- **Organization** → **Projects** (flat; no Teams in v0.1; permission seam documented for a future Teams layer).
- **Membership** joins a **User** to an Organization with one role: `owner` (everything + destroy/transfer), `admin` (projects, members, invites, ingest keys, project settings, creates projects), `member` (read all; triage: assign/resolve/ignore, comment).
- **Project** owns DSNs, issues, environments, releases, settings. Never moves organizations.
- **Ingest Key**: project-scoped UUID public key embedded in the DSN; multiple per project (rotation), revocable. No secret key.
- **API Token**: User-owned secret (`bghan_`-prefixed, stored hashed) for the REST API; power = union of the owner's memberships. Scopes v0.1: `read`, `write`.
- **Environment** and **Release** are per-project, first-class, event-declared. **Tags** stay event attributes (≤200 chars). **Affected User** (id/email/ip on events) is never a login-capable User.
- Open signup; org join by invite only.

## 4. Auth

([Auth model](https://github.com/Atash03/BugHan/issues/22))

- **Users**: email + password (argon2id). Server-side cookie sessions (HttpOnly, Secure, SameSite=Lax), 14-day sliding expiry, `auth_sessions` table. Email verification + password reset via emailed single-use tokens when SMTP is configured; without SMTP, verification is skipped and `bughan reset-password` CLI covers recovery. First-run setup creates the first User as owner of the first Organization.
- **Invites**: single-use tokenized link (7-day expiry), emailed or copyable; acceptance creates the Membership at the invited role.
- **Ingest auth**: `sentry_key` via query string (browser SDK's CORS-safe form), `X-Sentry-Auth` header, or envelope `dsn` header; all must agree; none → 403. Resolution (project + key validity) cached ~30s.
- **Rate limiting**: fixed-window per-project counter → 429 + `Retry-After` + `X-Sentry-Rate-Limits` per the Sentry protocol contract.

## 5. Ingest protocol (Sentry compatibility)

([Protocol research](https://github.com/Atash03/BugHan/tree/research/sentry-protocol) → ticket [#17](https://github.com/Atash03/BugHan/issues/17); [SDK payloads](https://github.com/Atash03/BugHan/tree/research/browser-sdk) → ticket [#20](https://github.com/Atash03/BugHan/issues/20))

- Single endpoint: `POST /api/{project_id}/envelope/` (query-string auth; `application/x-sentry-envelope` implied, `text/plain` accepted).
- Envelope framing per spec: JSON header line + items (JSON header + payload); **unknown item types are tolerated, never rejected**; gzip/deflate/br/zstd `content-encoding` handled with a hard decompressed-size cap (default 20 MB).
- Parsed item types: `event` (errors), `transaction`, `session` + `sessions` (aggregates), `feedback` + `user_report`, `client_report` (counted, discarded). Required fields: `event_id`, `timestamp`, `platform` (errors/feedback); transactions require `start_timestamp ≤ timestamp`.
- Server→SDK contract: 2xx accept; 429 rate-limit headers honored (SDK drops, never retries); 413 oversize; any 4xx/5xx = SDK drop + client report (never used as retry signal). `X-Sentry-Rate-Limits: retry_after:categories:scope:reason` emitted on throttles.
- **Support matrix**: sentry-js v8/v9/v10 validated (scripted smoke tests: Vite + webpack builds asserting error, transaction+web-vitals, and session flows appear via the API); v7 accepted-unsupported; v11 added when stable. Compat evolution = release-note monitoring + the smoke suite as tripwire. `@bughan/browser` wrapper deferred, not excluded. ([SDK strategy](https://github.com/Atash03/BugHan/issues/31))
- `sentry-trace`/`baggage`/DSC: the envelope `trace` header (DSC) is stored with transactions/events; error events persist `trace_id`/`span_id` from `contexts.trace` (SDKs attach them even without tracing) — this links errors ↔ traces.

## 6. Event pipeline

([Event pipeline and storage schema](https://github.com/Atash03/BugHan/issues/32))

```
decompress → auth (key lookup, 30s cache) → frame envelope → validate item
→ INLINE:  normalize (title/culprit/tags/level/env/release)
           group → issue upsert (counters, rollups)   [one tx per item]
           INSERT event (PK-dedupe on event_id) / transaction / session / feedback
           release upsert (implicit first-sighting) + session rollups
→ ASYNC (jobs table, bounded in-process worker): alert-rule evaluation;
   retention sweep (interval); rollup maintenance
```

Idempotency: event/transaction rows dedupe on `event_id` (PK conflict → 200 no-op); sessions upsert by `sid` with immutable terminal statuses and a 5-day update window; `sessions` aggregates additively increment rollups (double-delivery tolerated).

## 7. Storage schema

Postgres 16; monthly RANGE partitions on `*_part` tables. Full definitions in ticket #32.

- **Tenancy/auth**: `organizations`, `users`, `memberships`, `invitations`, `projects`, `ingest_keys`, `api_tokens`, `auth_sessions`.
- **Telemetry**: `events_part` (payload jsonb + generated query columns; `(issue_id, ts)`, `(project_id, trace_id)` indexes), `transactions_part` (spans stay **inside the payload** — the trace view reads one row), `sessions_part`, `session_rollups` (hourly by project×release×env, HLL distinct-did), `event_rollups` (hourly per issue + per project).
- **Issues**: `issues` (fingerprint UNIQUE per project, `grouping_version`, status/substatus, counters, assignee).
- **Other**: `releases`, `release_files` (bytea artifacts, SHA-256 dedupe), `feedbacks`, `alert_rules`, `alert_dedupe`, `jobs`, `ingest_counters`.

**Retention defaults** (env-overridable): error events 90d · transactions 30d (rollups persist) · raw sessions 30d (rollups 90d) · feedback 90d · release files tied to release (365d after last event). Issues survive raw-event deletion. Sweep = daily job dropping old partitions + small-table DELETE. Project/org delete cascades; per-project "delete all event data" endpoint.

## 8. Issue grouping & lifecycle

([Issue grouping depth](https://github.com/Atash03/BugHan/issues/24))

- **Hybrid**: SDK `fingerprint` array wins (with `{{ default }}` expansion). Otherwise server computes:
  `SHA-256( grouping_v1, type, title, culprit )` where
  - **title** = newest exception's `type` + message; parameterized `%s/%d` and numbers/UUIDs/hex ≥8 chars normalized to placeholders; truncated 256 chars,
  - **culprit** = top **in-app** frame (fallback last frame) with query/fragment stripped and bundle-hash path segments replaced (`main.9ab34f.js` → `main.{hash}.js`) + `function`; anonymous/webpack frames fall through to the next in-app frame; no in-app frame → title-only grouping.
- Computed server-side from stable event fields → stable across SDK upgrades. `grouping_version` column allows versioned algorithm changes without reshuffling old groups.
- **Lifecycle**: statuses `unresolved` / `resolved` / `ignored` (+ substatus). New event on resolved → `unresolved` + `substatus: regressed` (marked in UI; alertable). `ignored` absorbs events silently.
- **Issue metadata**: first/last seen, count, distinct affected-user count, latest level, assignee, hourly count rollup (sparklines).

## 9. Releases & session health

([Releases and session health](https://github.com/Atash03/BugHan/issues/25))

- Release = `(project, version)`; `environment`/`dist` are dimensions on events/sessions/files. Created implicitly on first event/session or explicitly via API (needed before source-map upload). "New release" marker = first event within 24h.
- Sessions stored individually (sid-keyed, most-recent-wins) + hourly `(project, release, environment)` rollups fed by both `session` and `sessions` items.
- **Crash** = session status `crashed`. Crash-free session rate = 1 − crashed/total; crash-free user rate via distinct `did`. Release page: adoption, rates, issues first-seen, source-map files.

## 10. Source maps & symbolication

([Source maps](https://github.com/Atash03/BugHan/issues/27))

- **v0.1 upload surface**: classic release-files API — `POST /api/0/organizations/{org}/releases/` (+ project-scoped), `POST /api/0/projects/{org}/{project}/releases/{version}/files/` (multipart `file`/`name`/`dist`/`header`), GET/DELETE list. Bearer API-token auth. This is the `sentry-cli sourcemaps upload` path. Chunk-upload + artifact-bundle assemble deferred to v0.2.
- Matching: `(release, dist)` + frame `abs_path`/`filename` (query/fragment stripped), **plus** `debugId` matching when maps carry one (events' `debug_meta.images` consulted first).
- Storage: Postgres `bytea` (caps 50 MB/file, 500 MB/release, SHA-256 dedupe). Object store = v0.2 seam.
- **Symbolication at view time with write-back**: first render symbolicates (sourcemap v3 + VLQ), result persisted on the event row (`symbolicated` column). Missing maps → minified frames + "no source map found" hint naming the release/abs_path. `sourcesContent` → inline code snippets; symbolicated issues badge.

## 11. Performance UX

([Performance UX and trace view](https://github.com/Atash03/BugHan/issues/28))

- **Trace view**: waterfall (root + spans, SVG/CSS, server-computed layout), error events sharing `trace_id` as red ticks on matching spans, web-vitals chips (LCP/CLS/INP/FCP/FP/TTFB with good/NI/poor thresholds), span list with op/text filters, top-5 self-time breakdown, span detail drawer.
- **Aggregates**: hourly per-transaction-name rollups (count, p50/p95/p99, failure rate) power the Performance page; "recently slow" = last-hour p95 > 2× 7-day baseline. Transaction detail: histogram + event list → trace view.
- **Linkage**: issue detail renders "View trace" when the event has trace context; trace view lists its error events.

## 12. Alerting

([Thin alerting spec](https://github.com/Atash03/BugHan/issues/29))

- Project-scoped rules: triggers `new_issue`, `regression`, `event_count` (≥N in M minutes); filters env/release/level. Actions: email list and/or generic webhook (multiple actions per rule).
- **Immediate delivery** (no digests): queued in the jobs table, 3 retries w/ backoff; webhook = 5s timeout, `X-BugHan-Event: alert`, `X-BugHan-Signature: sha256=<HMAC>` with a per-rule secret. Versioned JSON payload (rule/project/org/issue/event + URLs).
- Per-issue quiet period (default 30 min) in `alert_dedupe` — no timers. CRUD + enable/disable + "Send test" + delivery log in project settings.

## 13. Web app IA

([Web app IA](https://github.com/Atash03/BugHan/issues/26)) — top bar: org switcher · project switcher · environment filter · search · user menu. Sidebar: **Issues**, **Performance**, **Releases**, **Projects**, Settings. Pages: org dashboard (totals strip, events-over-time, latest issues, release health) · issues list (filters, saved views, bulk triage) · issue detail (stack trace, breadcrumbs, tags/context, trace link, feedback panel, activity) · performance (transaction table, vitals strip) · trace view · releases + detail · projects (cards, DSN onboarding) · feedback · alerts · settings (org/project/user) · auth pages (login/signup/reset/invite/setup). Issues-first hierarchy.

## 14. Deployment & ops

([Deployment topology](https://github.com/Atash03/BugHan/issues/30))

- Compose: `db` (postgres:16-alpine + pgdata volume) + `web` (single binary; migrations on boot; embedded worker; `BUGHAN_WORKER_EMBEDDED=false` splits worker-only if desired). No reverse proxy in the box.
- `.env`: `BUGHAN_SECRET_KEY`, `DATABASE_URL`, `BUGHAN_URL`, retention knobs, `BUGHAN_MAX_EVENT_MB`, optional `BUGHAN_SMTP_*`, `BUGHAN_SINGLE_ORG`.
- Sizing: ≤50k events/day → 1 vCPU/1 GB/10 GB · ≤200k/day → 2/2/50 · approaching 1M/day → 4/4/200+ (ClickHouse threshold).
- Ops: `GET /api/health/` (liveness + DB-ping readiness); stdout logs (JSON optional); forward-only versioned SQL migrations applied under lock on boot; upgrade = pull tag + `docker compose up -d`. Backup = nightly `pg_dump` runbook (Postgres holds all state). Unsupported: multi-instance/HA/K8s.

## 15. License & governance

([License and governance](https://github.com/Atash03/BugHan/issues/23)) — **MIT** for everything. Clean-room from the public protocol spec; no Sentry name/logo/branding; no FSL/BSL code; NOTICE file for any vendored BSD/MIT code. `CONTRIBUTING.md`, Contributor Covenant CoC, issue/PR templates. This file is the single design doc; material reversals append an ADR-style section.

---

## Decision index

| Decision | Ticket |
|---|---|
| Sentry protocol compat scope | [#17](https://github.com/Atash03/BugHan/issues/17) |
| Storage engine choice | [#18](https://github.com/Atash03/BugHan/issues/18) |
| GlitchTip architecture study | [#19](https://github.com/Atash03/BugHan/issues/19) |
| What the browser SDK sends | [#20](https://github.com/Atash03/BugHan/issues/20) |
| Data model and multi-tenancy | [#21](https://github.com/Atash03/BugHan/issues/21) |
| Auth model | [#22](https://github.com/Atash03/BugHan/issues/22) |
| License and governance | [#23](https://github.com/Atash03/BugHan/issues/23) |
| Issue grouping depth | [#24](https://github.com/Atash03/BugHan/issues/24) |
| Releases and session health | [#25](https://github.com/Atash03/BugHan/issues/25) |
| Web app information architecture | [#26](https://github.com/Atash03/BugHan/issues/26) |
| Source maps and symbolication | [#27](https://github.com/Atash03/BugHan/issues/27) |
| Performance UX and trace view | [#28](https://github.com/Atash03/BugHan/issues/28) |
| Thin alerting spec | [#29](https://github.com/Atash03/BugHan/issues/29) |
| Deployment topology | [#30](https://github.com/Atash03/BugHan/issues/30) |
| SDK strategy and compat test matrix | [#31](https://github.com/Atash03/BugHan/issues/31) |
| Event pipeline and storage schema | [#32](https://github.com/Atash03/BugHan/issues/32) |
| User feedback capture | [#33](https://github.com/Atash03/BugHan/issues/33) |

Research branches: [`research/sentry-protocol`](https://github.com/Atash03/BugHan/tree/research/sentry-protocol) · [`research/glitchtip-architecture`](https://github.com/Atash03/BugHan/tree/research/glitchtip-architecture) · [`research/browser-sdk`](https://github.com/Atash03/BugHan/tree/research/browser-sdk) · [`research/storage-engine-alt-hybrid`](https://github.com/Atash03/BugHan/tree/research/storage-engine-alt-hybrid)
