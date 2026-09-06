# BugHan REST API (v0.1)

The Issues API lets scripts, bots, and the web UI read and triage error
issues. It lives under `/api/0/` and speaks JSON.

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
events, transactions, sessions, feedback, rollups, and releases — a fresh
start. (The daily retention sweep, by contrast, drops old events but lets
issues survive.) Ingest keys and saved views are kept.
