# BugHan REST API (v0.1)

The `/api/0/` surface lets scripts, bots, CI (source-map upload), and the
web UI manage tenancy, triage error issues, and read performance, releases,
and alerting. It speaks JSON. SDK event ingest lives next to it at
`POST /api/{project}/envelope/` (see [Ingest](#ingest-sdk-envelope-endpoint)
below).

Terminology follows [CONTEXT.md](../CONTEXT.md): an **Issue** is a durable
grouping of related **Events**; **Affected Users** (user hashes) are not
login-capable Users.

## Authentication

Two credential kinds (see `POST /api/0/tokens/` to mint one):

| Credential | Header | Authority |
|---|---|---|
| API token | `Authorization: Bearer bghan_<48 hex>` | Union of the owner's memberships, limited by the token's `scope` |
| Session cookie | `bughan_session` cookie + `X-CSRF-Token` header on mutations | Full user authority |

**Scopes** (API tokens only): `write` implies `read`; `read` tokens get 403 on
any mutation. Sessions are never scope-limited.

**Roles** (org membership): `member` reads everything and triages (resolve /
ignore / assign / comment / manage saved views); `admin` additionally destroys
data (`delete-data`). Non-members get 404 — issue existence is never leaked
across organizations.

## Conventions

- **Errors**: non-2xx responses carry `{"error": "<message>"}`.
- **Pagination**: list endpoints accept `limit` (default 25, max 100) and
  `offset`, and answer with an envelope:

  ```json
  { "data": [ ... ], "meta": { "total": 42, "limit": 25, "offset": 0 } }
  ```

- **Timestamps**: PostgreSQL timestamp strings, e.g. `2026-09-06 10:00:01+00`.
- **Issue JSON**:

  ```json
  {
    "id": "0e9f39a1-…", "fingerprint": "2e437c7f…",
    "title": "TypeError: Cannot read properties of undefined",
    "culprit": "app.js in saveCart",
    "type": "error", "level": "error",
    "status": "unresolved", "substatus": "",
    "first_seen": "…", "last_seen": "…",
    "count": 17, "user_count": 4,
    "assignee": { "id": "…", "email": "neo@acme.dev", "name": "Neo" }
  }
  ```

  `assignee` is `null` when unassigned. `substatus` is `"regressed"` after a
  resolved issue receives a new event (status flips back to `unresolved`).
  Statuses: `unresolved` / `resolved` / `ignored` (ignored issues absorb new
  events silently).

- **Activity JSON**: `{ "id", "type", "body", "data", "author", "created_at" }`
  with `type` ∈ `note` (human comment), `status` (`data.from`/`data.to`),
  `assignment` (`data.assignee` user id or null). `author` is null for
  machine-originated entries.

---

## Issue list

```
GET /api/0/projects/{orgSlug}/{projectSlug}/issues/
```

Sorts descending; defaults to `last_seen`.

| Parameter | Values | Notes |
|---|---|---|
| `sort` | `last_seen` `first_seen` `count` `user_count` | unknown value → 400 |
| `limit`, `offset` | ints | clamped to 1–100 / ≥ 0 |
| `status` | repeatable | `unresolved` `resolved` `ignored` |
| `level` | repeatable | e.g. `error`, `warning` |
| `environment` | repeatable | matches any event of the issue |
| `release` | repeatable | matches any event of the issue |
| `tag` | repeatable, `key:value` | value matched exactly; first colon splits; value may contain colons |
| `query` | string | literal, case-insensitive substring over `title` and `culprit` (`%`/`_` are literal) |

**Example**: `GET /api/0/projects/acme/web-app/issues/?status=unresolved&tag=browser:Chrome&sort=count&limit=10`

## Tag facets

```
GET /api/0/projects/{orgSlug}/{projectSlug}/tags/
```

Accepts the same filter parameters as the issue list. Returns one entry per
tag key (top 10 keys by volume), each with its top 10 values by **count of
distinct issues affected**:

```json
[
  { "key": "browser",
    "values": [ { "value": "Chrome", "count": 12 },
                { "value": "Firefox", "count": 3 } ] }
]
```

## Issue detail, events

```
GET /api/0/issues/{issueID}/
GET /api/0/issues/{issueID}/events/?limit=25&offset=0
GET /api/0/issues/{issueID}/events/{eventID}/
```

- Event list rows: `{ "id", "issue_id", "timestamp", "received_at",
  "platform", "level", "environment", "release", "dist", "title", "culprit",
  "message", "type", "user_hash" }` — newest first, no payload.
- Event detail adds `"tags"` (object), `"payload"` (raw SDK event),
  `"breadcrumbs"` and `"contexts"` (extracted from the payload for
  convenience), `"trace_id"`, `"symbolicated"` (null until source maps are
  applied).

## Triage actions

```
PUT /api/0/issues/{issueID}/
```

Body fields are independent; an empty object → 400.

| Field | Values |
|---|---|
| `status` | `resolved` \| `unresolved` \| `ignored` (clears `substatus`) |
| `assignedTo` | `"me"` \| member email \| user id \| `null` (unassign); non-members → 400 |

Both changes land on the issue's activity feed. Returns the updated issue.

## Bulk triage

```
PUT /api/0/projects/{orgSlug}/{projectSlug}/issues/bulk/
```

```json
{ "ids": ["…", "…"], "status": "resolved", "assignedTo": "me" }
```

`status` and/or `assignedTo`, applied to every listed issue of this project
(unknown ids are skipped). Answers `{ "updated": <n> }` and records activity
per touched issue (bulk status entries carry no `from`).

## Comments & activity

```
POST /api/0/issues/{issueID}/comments/     {"body": "looks fixed to me"}   → 201
GET  /api/0/issues/{issueID}/activity/     ?limit=25&offset=0
```

Comments are activity entries of type `note` (trimmed, non-empty). The
activity feed returns newest first and mixes notes with status changes and
assignments.

## Saved views

Bookmarkable issue-list queries, scoped to a project:

```
GET    /api/0/projects/{orgSlug}/{projectSlug}/views/
POST   /api/0/projects/{orgSlug}/{projectSlug}/views/    {"name": "My unresolved", "query": "status=unresolved&query=timeout"}   → 201
DELETE /api/0/projects/{orgSlug}/{projectSlug}/views/{viewID}/                                              → 204
```

`query` is the raw issue-list query string (any filter parameters above). A
view URL in the UI is `/<org>/<project>/issues/?<query>`. Names are unique per
project (duplicate → 400). Any member can create/delete.

## Delete all event data

```
POST /api/0/projects/{orgSlug}/{projectSlug}/delete-data/   → 204
```

**Admin only. Irreversible.** Permanently deletes the project's issues,
events, transactions, sessions, feedback, rollups, releases, alert
deliveries, and alert dedupe rows — a fresh start. (The daily retention
sweep, by contrast, drops old events but lets issues survive.) Ingest keys,
saved views, and alert rules are kept (rules are settings, not event data).

## Alert rules

Thin alerting (DESIGN.md §12): project-scoped rules fire on `new_issue`,
`regression`, or `event_count` (≥N events on one issue within M minutes),
optionally scoped by environment / release / level. Each (rule, issue) pair
then stays quiet for the rule's `quiet_minutes` (default 30; 0 disables).
Actions are an email list and/or one generic webhook, delivered immediately
by the embedded worker (evaluation is enqueued at ingest; delivery attempts
ride on the jobs table's 3-attempt retries).

Reads need `member`; mutations need `admin`. Read-scope API tokens get 403
on mutations. The webhook secret is never serialized — rotate it with
`rotate_secret` (a secret is minted automatically when a webhook URL is
first set).

```
GET    /api/0/projects/{orgSlug}/{projectSlug}/alerts/
POST   /api/0/projects/{orgSlug}/{projectSlug}/alerts/
GET    /api/0/projects/{orgSlug}/{projectSlug}/alerts/{ruleID}/
PUT    /api/0/projects/{orgSlug}/{projectSlug}/alerts/{ruleID}/
DELETE /api/0/projects/{orgSlug}/{projectSlug}/alerts/{ruleID}/   → 204
```

Rule JSON:

```json
{
  "id": "…", "project_id": "…", "name": "Page the dev",
  "trigger": "new_issue", "enabled": true,
  "environments": ["production"], "releases": [], "levels": ["error"],
  "threshold_count": 10, "threshold_minutes": 60, "quiet_minutes": 30,
  "email_to": ["dev@example.com"], "webhook_url": "https://hooks.example.com/bughan"
}
```

`trigger` ∈ `new_issue` `regression` `event_count`. A rule needs at least
one action (`email_to` or `webhook_url`); `webhook_url` must be http(s).

```
POST /api/0/projects/{orgSlug}/{projectSlug}/alerts/{ruleID}/test/
```

Fires the rule's actions immediately with a `alert-test` payload (the
project's latest issue/event, or synthetic placeholders when empty) and
records the attempts with `action: "test"`. Answers
`{ "sent": 2, "failed": 0 }`.

## Alert deliveries

```
GET /api/0/projects/{orgSlug}/{projectSlug}/alerts-deliveries/?rule_id=…&issue_id=…&limit=25&offset=0
```

The delivery log, newest first (`member`). One row per attempted action:

```json
{ "data": [
  { "id": "7", "rule_id": "…", "rule_name": "Page the dev",
    "issue_id": "…", "event_id": "…", "trigger": "new_issue",
    "action": "webhook", "target": "https://hooks.example.com/bughan",
    "status": "sent", "error": "", "created_at": "…" } ],
  "meta": { "total": 1, "limit": 25, "offset": 0 } }
```

`action` ∈ `email` `webhook` `test`; `status` ∈ `sent` `failed`.

### Webhook contract

`POST <webhook_url>` with a 5-second timeout, `Content-Type: application/json`,
`X-BugHan-Event: alert` (or `alert-test`), and — when the rule carries a
secret — `X-BugHan-Signature: sha256=<HMAC-SHA256(body, secret)>`.
Non-2xx counts as failed (and is retried with the job). Versioned body:

```json
{ "version": 1, "event": "alert", "trigger": "new_issue",
  "rule": { "id": "…", "name": "Page the dev", "trigger": "new_issue" },
  "project": { "id": "…", "name": "Web App", "slug": "web-app",
               "organization": "acme" },
  "issue": { "id": "…", "title": "TypeError: boom", "culprit": "app.js",
             "level": "error", "status": "unresolved", "count": 3,
             "event_id": "…", "environment": "production", "release": "1.0" },
  "urls": { "issue": "https://bugs.example.com/acme/web-app/issues/…/",
            "event": "https://bugs.example.com/acme/web-app/issues/…/?event=…" } }
```

---

## Tenancy & credentials

```
GET  /api/0/                                              → whoami + server version
GET  /api/0/organizations/                                → my orgs (with my role)
POST /api/0/organizations/            {"name": "Acme"}    → 201 {id, name, slug}
GET  /api/0/organizations/{org}/                          → org detail (member)
GET  /api/0/organizations/{org}/members/                  → membership list (member)
DELETE /api/0/organizations/{org}/members/{memberID}/     → remove (admin) → 204
POST /api/0/organizations/{org}/invites/  {"email": "…", "role": "member"} → 201 invite (admin)
GET  /api/0/organizations/{org}/invites/                  → pending invites (admin)
DELETE /api/0/organizations/{org}/invites/{inviteID}/     → revoke (admin) → 204
```

Invites are single-use, 7-day tokenized links; acceptance is a browser flow
(`GET /accept/{token}`). Roles: `owner` > `admin` > `member` (see
Authentication above for what each may do).

```
GET  /api/0/organizations/{org}/projects/                 → project list (member)
POST /api/0/organizations/{org}/projects/  {"name": "Web App", "platform": "javascript"} → 201 (admin)
GET  /api/0/projects/{org}/{project}/                     → project detail incl. DSN (member)
```

Project creation mints the first ingest key and returns it inline
(`keys: [{id, name, dsn}]`); `platform` defaults to `javascript`.

```
GET    /api/0/projects/{org}/{project}/keys/              → ingest keys + DSNs (member)
POST   /api/0/projects/{org}/{project}/keys/  {"name": "ci"} → 201 {id, name, dsn, active} (admin)
DELETE /api/0/projects/{org}/{project}/keys/{keyID}/      → revoke; its traffic 403s from then on (admin) → 204
```

The DSN is `scheme://<ingest-key>@<host>/<project-id>` (built from
`BUGHAN_URL`), handed to the SDK as-is. Several keys may live side by side
for rotation; revoking one stops only its traffic.

```
GET    /api/0/tokens/                        → my tokens (id, name, prefix, scope, active)
POST   /api/0/tokens/  {"name": "ci", "scope": "write"} → 201, with the single plaintext `token` (bghan_…)
DELETE /api/0/tokens/{tokenID}/              → revoke → 204
```

`scope` is `read` or `write` (default `write`); only the full secret is
returned at creation — it is stored hashed and never readable again.

## Releases & source-map files

Classic release-files surface (DESIGN.md §10) — the path `sentry-cli
sourcemaps upload` drives with **sentry-cli 2.x** (3.x removed it; see
[docs/self-host.md](./self-host.md)). Release creation is idempotent:
re-creating an existing release answers `208` with an empty project list.

```
POST /api/0/organizations/{org}/releases/                        {"version": "my-app@1.0.0", "projects": ["web-app"]} → 201|208 (admin)
GET  /api/0/organizations/{org}/releases/                        → release list (member)
POST /api/0/projects/{org}/{project}/releases/                  {"version": "my-app@1.0.0"} → 201|208 (admin)
GET  /api/0/projects/{org}/{project}/releases/                  → release list (member)
```

Release rows: `{version, shortVersion, status, firstEvent, lastEvent,
dateCreated}`. Releases are also created implicitly the first time an
event/session declares the version.

```
POST   /api/0/projects/{org}/{project}/releases/{version}/files/    multipart: file + name (+ dist, repeatable header "Name: Value") → 201 (admin)
GET    /api/0/projects/{org}/{project}/releases/{version}/files/    → file list (member)
DELETE /api/0/projects/{org}/{project}/releases/{version}/files/{fileID}/ → 204 (admin)
GET    /api/0/organizations/{org}/releases/{version}/files/         → files across the org's projects (member)
DELETE /api/0/organizations/{org}/releases/{version}/files/{fileID}/ → 204 (admin)
```

Caps: 50 MB/file, 500 MB/release (both → `413`). Identical re-uploads
dedupe (SHA-256) and return the existing row; same name with different
content → `409`. File rows: `{id, name, dist, size, sha256, sha1, headers,
dateCreated}` (`sha1` exists because sentry-cli's parser requires it).

## Performance & traces

Project-scoped reads for any member. Windowed endpoints take
`?statsPeriod=` (`1h`/`24h`/`7d`/`30d` suffixes; default `24h`, capped at
`30d`).

```
GET /api/0/projects/{org}/{project}/performance/summary/?statsPeriod=24h
```

Per-transaction-name aggregates over the window: `{name, count, failures,
p50, p95, p99, recently_slow}` (`recently_slow` = last-hour p95 > 2× the
7-day baseline).

```
GET /api/0/projects/{org}/{project}/performance/transaction/?name=/users/:id&statsPeriod=24h
```

`name` is required: window aggregates + hourly rollup series + recent raw
events (each with `trace_id`, duration, and extracted web vitals) + a
duration histogram.

```
GET /api/0/projects/{org}/{project}/traces/{traceID}/
```

The waterfall payload: `{trace_id, transactions: [{id, name, status,
environment, release, duration_ms, vitals, spans: [{span_id, op,
description, duration_ms}]}], errors: [{id, issue_id, title, level, culprit,
timestamp}]}` — error events sharing the `trace_id` ride along as ticks.

```
GET /api/0/projects/{org}/{project}/releases-health/?statsPeriod=24h
```

Session health per release: `{release, total, crashed, abnormal, errored,
crash_free_sessions, crash_free_users, unique_users}` (crash-free user rate
unions the HLL distinct-`did` sketches).

## Ingest (SDK envelope endpoint)

```
POST /api/{projectID}/envelope/?sentry_key=<ingest-key>&sentry_version=7&sentry_client=sentry.javascript.browser/10.70.0
POST /api/{projectID}/store/?sentry_key=<ingest-key>   (legacy JSON store, older SDKs)
```

- Auth: `?sentry_key=` (the browser SDK's CORS-safe form), `X-Sentry-Auth`
  header, or the envelope `dsn` header (tunnel mode); all present values
  must agree; none/invalid → `403`. Key resolution is cached ~30s.
- Framing: JSON header line + items; `Content-Type:
  application/x-sentry-envelope` implied, `text/plain` accepted;
  `Content-Encoding: gzip`/`deflate`/`br`/`zstd` handled with a
  decompressed-size cap (`BUGHAN_MAX_EVENT_MB`, default 20 MB; over → `413`).
- Unknown item types are tolerated, never rejected. `event_id`/`sid`
  collisions dedupe to a `200` no-op (SDK retries are safe).
- Server→SDK contract: `2xx` accept; `429` + `Retry-After` +
  `X-Sentry-Rate-Limits` when `BUGHAN_RATE_LIMIT_PER_MIN` trips (the SDK
  drops, never retries); any other 4xx/5xx = drop + client report (never a
  retry signal).

## Liveness

```
GET /api/health/   (also /api/health)
```

No auth. `200 {"status":"ok","version":"…","time":"…"}` when live and the
DB pings; `503 {"status":"degraded","database":"unreachable",…}` when
Postgres is down. `Cache-Control: no-store`; excluded from access logs.

## Feedback

User feedback (`feedback` + legacy `user_report` envelopes) is accepted at
ingest, linked to its issue inside a 30-minute association window
(late-arriving errors backfill the link), retained 90 days, and surfaced
read-only: the issue-detail panel and the project's feedback page in the
web UI. There is no feedback REST API in v0.1.
