# GlitchTip architecture study

Research for BugHan (ticket #19). Establishes the **facts** about how GlitchTip implements a
Sentry-protocol-compatible self-hosted error + performance tracker, so later tickets can decide what
BugHan reuses or beats. Findings are drawn primarily from the GlitchTip source repositories
(cloned and inspected) and from Sentry's public SDK/self-hosted docs. No design decisions are made here.

> Note: GlitchTip is under active development. The facts below reflect the **current `master` of
> `glitchtip/glitchtip-backend`** (v6.2.x line, early 2026) and `glitchtip/glitchtip-frontend`
> (Angular 22). Several subsystems (Rust ingest, VTasks, Granian, Valkey, DuckDB cold storage) are
> recent replacements, so older blog posts/guides describe an earlier stack (Celery, uWSGI, Redis).

---

## 1. Repo layout

GlitchTip is split into three repositories under the `glitchtip` GitLab group:

| Repo | Purpose |
|---|---|
| [`glitchtip/glitchtip`](https://gitlab.com/glitchtip/glitchtip) | Meta repo: issue tracking, wiki, deployment info. README links to the two product repos. |
| [`glitchtip/glitchtip-backend`](https://gitlab.com/glitchtip/glitchtip-backend) | Django backend (API + ingest + worker). The bulk of the product. |
| [`glitchtip/glitchtip-frontend`](https://gitlab.com/glitchtip/glitchtip-frontend) | Angular frontend (SPA), built and served by the backend. |

The backend's own [`README.md`](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/master/README.md)
describes it as: *"a partial fork / mostly re-implementation of Sentry's open source codebase before it
went proprietary"*, with the explicit goals of simplicity, a "typical Django app" layout, low resource
usage ("runs with as little as 512MB of ram"), and an MIT license.

The backend is organized as a standard Django project:

- `glitchtip/` — settings, root URLconf, ASGI/WSGI entrypoints, and the ingest-specific machinery
  (`ingest_asgi.py`, `rust_ingest.py`, `cold_storage.py`, `partition_manager.py`).
- `apps/` — one Django app per domain concern. Notable apps (from `ls apps/`): `event_ingest`,
  `issue_events`, `performance`, `alerts`, `difs` (debug info files / symbolication), `sourcecode`
  (source maps), `releases`, `uptime`, `logs`, `projects`, `teams`, `environments`, `organizations_ext`,
  `users`, `api_tokens`, `oauth`, `files`, `importer` (Sentry import), `stats`, `observability`,
  `mcp` (Model Context Protocol), `stripe` (hosted billing), `wizard`.
- `sentry/` — a vendored subset of Sentry's **BSD-licensed** code (`culprit.py`, `eventtypes`,
  `interfaces/stacktrace.py`, `stacktraces/`, `utils/`) used for culprit/location derivation and
  stacktrace walking. Its presence is disclosed in [`NOTICE.md`](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/master/NOTICE.md).
- `events/` — test fixtures (sample events from many SDKs).
- `bin/` — process-entrypoint shell scripts (web/worker/all-in-one).
- `compose*.yml`, `Dockerfile`, `.gitlab-ci.yml` — deployment/build artifacts.

---

## 2. Stack

### Backend (Python/Django)

From [`pyproject.toml`](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/master/pyproject.toml):

- **Language/framework**: Python ≥3.12 (image is Python 3.14), **Django 6.0** (pinned `<6.1`), with
  `django-async-backend` for an **async ORM**. The app is async-first.
- **API layer**: **django-ninja** + **Pydantic** (not Django REST Framework). The README credits
  "django-ninja/Pydantic — brings typed and async-first api design". OpenAPI is exposed
  (`ENABLE_OPENAPI`) and the frontend generates TS types from it.
- **Auth**: `django-allauth` (email/social/MFA, plus per-organization OIDC SSO); `django-organizations`
  for the org → team → member multi-tenancy model.
- **Storage of error data**: **PostgreSQL** ("We use Postgres to store error data"). Partitioned:
  RANGE (date) + HASH (organization) sub-partitioning via `django-postgres-partition` and a custom
  `psql_partition` layer; optional advanced partitioning via `pg_partman` (`compose.part.yml`).
  The hot/queryable issue projection (`IssueIndex`) is a hash-partitioned table with a
  `tsvector` full-text-search column (Gin index).
- **Queue / cache / broker**: **VTasks** (`django-vtasks`) is GlitchTip's own async task queue — it
  **replaced Celery**. Tasks can run on **Valkey** (a Redis fork) or fall back to a **Postgres broker**
  (so Valkey is optional). Caching uses `django-vcache`. (The author's blog
  "[Making Django Fast: VTasks processes tasks 4x faster than Celery](https://glitchtip.com/blog/2026-04-13-django-vtasks/)"
  documents the motivation.)
- **Rust acceleration**: `glitchtip-rust` (a PyO3 extension, module `gt_rust`) provides: request-body
  decompression + envelope framing, the **Rust PostgreSQL driver** (`gt_rust.django_backend`, built on
  `django-vpg` — now the *only* DB engine; psycopg is library-only), and **symbolic**
  (native symbolication, JS sourcemap remapping, ProGuard deobfuscation, C++ demangling).
- **Web server**: **Granian** (a Rust ASGI server), which replaced uWSGI. Modes: `web`,
  `worker`, and `all_in_one` (web + embedded VTasks worker + scheduler in a single process, via
  `bin/run-all-in-one.sh`, `GLITCHTIP_EMBED_WORKER=true`).
- **Other**: `sentry-sdk` (GlitchTip reports its own errors to a DSN), `django-prometheus`
  (Prometheus metrics), `anonymizeip`/`django-ipware` (IP handling), `django-storages` (S3/Azure/GCS
  for artifacts/cold storage), **DuckDB** (cold-storage analytics), `minidump` (minidump parsing),
  `opentelemetry-proto` (OTLP log ingest), `user-agents`, `django-import-export`.

### Frontend (Angular)

From [`package.json`](https://gitlab.com/glitchtip/glitchtip-frontend/-/blob/master/package.json) and
[`angular.json`](https://gitlab.com/glitchtip/glitchtip-frontend/-/blob/master/angular.json):

- **Angular 22** (Angular CLI), **Angular Material**, SCSS, strict mode, `OnPush` change detection.
- **Cypress** for E2E, **Karma** for unit tests, `openapi-typescript` generates the API client from the
  backend's OpenAPI schema.
- **i18n** via Angular localize (source locale `en-US`, translations `fr`, `nb`).
- Built with `npm run build-prod` and copied into the backend's `static/` directory; the Django backend
  serves the SPA. (This is why the backend README says the repo is "backend API only" without the frontend.)

### Databases & queues summary

| Concern | Technology |
|---|---|
| Primary data (events, issues, projects, etc.) | PostgreSQL 14–18, RANGE+HASH partitioned |
| Queue / tasks | VTasks (own queue) — Celery removed |
| Cache / dedup / throttle blocks | Valkey (optional; Postgres fallback) |
| Cold storage (old events/spans/logs) | S3 / Azure / GCS / local dir, queried via DuckDB |
| Self-observability | Prometheus metrics + optional Sentry DSN |

---

## 3. Env config

Configuration is Django-environ based (`glitchtip/settings.py`). Core variables:

- **Connection**: `DATABASE_URL` (postgres), `VALKEY_URL` (empty string disables Valkey), `SECRET_KEY`,
  `GLITCHTIP_DOMAIN` / `GLITCHTIP_URL` / `APP_URL`.
- **Limits**: `GLITCHTIP_MAX_UNZIPPED_PAYLOAD_SIZE` (default 15 MB), `GLITCHTIP_MAX_UNZIPPED_ARCHIVE_SIZE`
  (512 MB), `GLITCHTIP_MAX_ARCHIVE_MEMBERS` (10,000), `DATA_UPLOAD_MAX_MEMORY_SIZE`.
- **Retention**: `GLITCHTIP_RETENTION_DAYS` (master default **90**; legacy alias
  `GLITCHTIP_MAX_EVENT_LIFE_DAYS`), plus per-type overrides `GLITCHTIP_EVENT_RETENTION_DAYS`,
  `GLITCHTIP_TRANSACTION_RETENTION_DAYS`, `GLITCHTIP_SPAN_RAW_RETENTION_DAYS` (30),
  `GLITCHTIP_UPTIME_RETENTION_DAYS`, `GLITCHTIP_FILE_RETENTION_DAYS`, `GLITCHTIP_LOG_RETENTION_DAYS`,
  `GLITCHTIP_RELEASE_RETENTION_DAYS` (365).
- **Hot/cold**: `GLITCHTIP_EVENT_HOT_DAYS` (30), `GLITCHTIP_LOG_HOT_DAYS` (7),
  `GLITCHTIP_COLD_STORAGE_BUCKET`/`_DIR`, `GLITCHTIP_ENABLE_DUCKDB`, `GLITCHTIP_COLD_STORAGE_CLEANUP_ENABLED`.
- **Feature flags**: `GLITCHTIP_ENABLE_LOGS`, `GLITCHTIP_ENABLE_UPTIME`, `GLITCHTIP_ENABLE_MCP`,
  `GLITCHTIP_ENABLE_DUCKDB`, `ENABLE_TEST_API`, `ENABLE_ADMIN`, `ENABLE_OPENAPI`.
- **Ingest/ops**: `GLITCHTIP_PII_SCRUB_DEFAULT` (JSON), `GLITCHTIP_THROTTLE_CHECK_INTERVAL` (5000),
  `MAINTENANCE_EVENT_FREEZE`, `PARTITION_HASH_BUCKETS` (2), `CORS_ORIGIN_ALLOW_ALL` (**true** by default
  — browser SDKs send from arbitrary origins), `SERVER_ROLE`, `GRANIAN_WORKERS`, `VTASKS_CONCURRENCY`,
  `VTASKS_INGEST_CONCURRENCY`, `VTASKS_INGEST_BATCH_COUNT`.
- **Billing** (hosted SaaS only): `BILLING_ENABLED` (default **False** in self-hosted), Stripe keys,
  `GLITCHTIP_FREE_TIER_EVENTS` (1000), overage metering.

---

## 4. Ingest pipeline

### Endpoints (from `glitchtip/urls.py` and `glitchtip/ingest_asgi.py`)

| Path | Purpose |
|---|---|
| `POST /api/<project_id>/envelope/` | Sentry **envelope** ingest (events, transactions, feedback, logs, minidump attachments) |
| `POST /api/<project_id>/store/` | Legacy Sentry **store** endpoint (single JSON event) |
| `POST /api/<project_id>/security/` | Browser **CSP** reports |
| `POST /api/<project_id>/minidump/` | Crashpad/Breakpad minidump uploads (multipart) |
| `POST /v1/logs` (also `/v1/traces`, `/v1/metrics`) | Native **OTLP/HTTP** ingest (project resolved from DSN key) |

The ingest paths are served through a **minimal ASGI middleware stack** — only `SecurityMiddleware` and
`CorsMiddleware` — bypassing session/auth/CSRF/locale/allauth on the hot path.

### Envelope parsing & normalization

- **Envelope framing + decompression runs in Rust** (`gt_rust`): gzip/deflate/br/zstd decompression with
  a hard cap on decompressed size (`GLITCHTIP_MAX_UNZIPPED_PAYLOAD_SIZE`), enforced while streaming, to
  bound memory against zip-bombs. The Rust layer returns the lifted envelope header + item header lines +
  verbatim payloads; **item schema validation stays in Python (Pydantic)**.
- The envelope header (`event_id`, `dsn`, `sdk`, `sent_at`) and each item header (`type`, `length`,
  `content_type`, `filename`) are validated by Pydantic schemas (`apps/event_ingest/schema.py`).
- **Supported item types** (`SupportedItemType`): `transaction`, `event`, `user_report`, `feedback`,
  `log`, `otel_log`.
- **Explicitly ignored item types** (`IgnoredItemType`): `session`, `sessions`, `client_report`,
  `attachment` (except the `event.minidump` attachment, which is extracted), `check_in`, `profile`,
  `replay_recording`, `replay_event`, `replay_video`, `span`, `profile_chunk`, `trace_metric`.
  Unknown/new types are skipped but reported to Sentry with a per-type fingerprint, so SDK-spec evolution
  surfaces as its own diagnostic issue.
- **Event normalization**: a Pydantic `WebIngestIssueEvent` model coerces/truncates fields (lenient
  ingest schema), computes the event title (exception type + value via `sentry/eventtypes/error.py`),
  culprit/location (`sentry/culprit.py` + `sentry/stacktraces/`), interpolates parameterized messages
  (`transform_parameterized_message`), and scrubs PII server-side.
- **PII scrubbing**: per-project `scrub_config` falling back to the fleet default
  `GLITCHTIP_PII_SCRUB_DEFAULT`; org/project flags scrub IP addresses.

### Grouping (fingerprinting)

Grouping is deliberately **much simpler than Sentry's**. From `apps/event_ingest/utils.py` and
`apps/event_ingest/process_event.py`:

```python
def generate_hash(title, culprit, type, extra=None):
    # "Generate insecure hash used for grouping issues"
    hash_input = title + culprit + str(type)   # plus {{ default }} fingerprint expansion
    return hashlib.md5(hash_input.encode()).hexdigest()
```

i.e. the issue key is `MD5(title + culprit + event_type)`, where `title` = exception type/message
(or `"<untitled>"`), `culprit` = transaction/location, and `type` = ERROR / DEFAULT / CSP. An event's
explicit `fingerprint` array overrides this (the literal `{{ default }}` token expands to the computed
default hash). There is **no** hierarchical stacktrace-normalization grouping config like Sentry's
`grouping` strategies — only the vendored `sentry/stacktraces/` code picks the crash frame for the culprit.

### Dedup, queueing, persistence

- **Event-id dedup**: `cache.aadd("uuid" + event_id)` before enqueue — a repeated event id is dropped.
- **Queueing**: the parsed/scrubbed payload is serialized and enqueued as a VTasks task
  (`ingest_event`, `ingest_transaction`, `ingest_user_report`, `ingest_logs`), with UUIDv7 primary ids
  (`UUID7Helper`) for time-ordered partitioning.
- **Persistence**: events are written to partitioned Postgres tables; the issue list is served by a
  hash-partitioned `IssueIndex` projection (with FTS), while event bodies age into cold storage
  (DuckDB over object storage) after `GLITCHTIP_EVENT_HOT_DAYS`.

### Authentication & rate limiting (DSN auth)

From `apps/event_ingest/authentication.py`:

- **DSN key resolution**: accepts (in order) a `sentry_key`/`glitchtip_key` query param, an
  `Authorization: Bearer <key>` header (OTLP), or the Sentry `X-Sentry-Auth` / `Authorization` header
  (parsed by the vendored `sentry/utils/auth.py`). The public key is a **UUID**, globally unique — which
  is what lets OTLP `/v1/logs` resolve the project without a project id in the URL.
- **Project lookup**: `get_project_auth_info(project_id, sentry_key)` is a **Postgres stored procedure**
  returning the project + org throttling/scrub/first-event state in one round trip (with a read-replica
  fallback). Invalid key → HTTP 403 and a 30s "v" cache block (per project+key).
- **Rate limiting / quota**: org- and project-level `event_throttle_rate` (0–100, a percentage).
  Rejection is **probabilistic** (`random.randint(0,100) > throttle_rate`). Throttle state is cached in
  Valkey under a project-scoped key (`t:org:project`) for 30 s, so repeat floods are bounced in one
  Valkey round trip without a DB hit. `Retry-After` = `ceil(0.02 * throttle^2.3)`. A 100% throttle or
  org-level "not accepting events" → 429 (Retry-After 600). Billing quota re-check runs out-of-band via
  `check_organization_throttle` (1 in `GLITCHTIP_THROTTLE_CHECK_INTERVAL`). `MAINTENANCE_EVENT_FREEZE` →
  HTTP 503. Over-size bodies → HTTP 413.

### Retention

Per-type retention (defaults to 90 days for events/transactions/logs/uptime/files; 365 for releases;
30 for raw spans), enforced by partition drops (`maintain_partitions`) and a cold-storage promotion +
cleanup path. Hot data stays in Postgres for a short window (`EVENT_HOT_DAYS`), older data moves to
object storage queried through DuckDB.

---

## 5. Feature coverage vs. omissions

### Supported (present in code)

- **Issues / events** (errors) — full issue lifecycle: grouping, tags, breadcrumbs, contexts, search (FTS).
- **Performance / transactions** (`apps/performance`) — transaction events, spans, span grouping +
  SQL parameterization (`parameterize.py`), histograms, transaction groups, raw-span retention.
- **Source maps & symbolication** (`apps/difs` + `apps/sourcecode` + `gt_rust.symbolic`) — debug
  information files, JS sourcemap remapping, ProGuard deobfuscation, native symbolication, C++ demangling,
  chunked artifact upload with SHA-1 verification.
- **Releases** (`apps/releases`), **environments**, **tags**, **user reports / feedback** (user-facing
  crash reports — both legacy `user_report` and SDK v8 `feedback` envelope items).
- **Alerts** (`apps/alerts`) — email + webhooks (incl. Slack Block Kit); per-project alerts.
- **Uptime monitoring** (`apps/uptime`) — a GlitchTip *addition* Sentry doesn't have (HTTP checks,
  up/down state, email + webhook alerts).
- **Logs** (`apps/logs`, feature flag `GLITCHTIP_ENABLE_LOGS`) — Sentry-SDK log envelopes and native
  **OpenTelemetry** logs; log retention/cold storage.
- **CSP reports** (`/security/`), **minidump** (in-progress / not end-to-end supported yet).
- **Multi-tenancy**: orgs → teams → members (`django-organizations`), per-org OIDC SSO, API tokens.
- **Misc**: Sentry **import** (`apps/importer`), **MCP** server, **Prometheus** metrics, API key auth.

### Deliberately omitted / cut vs. Sentry

Confirmed by the `IgnoredItemType` list and the absence of apps/models:

- **Sessions / release health** — `session`/`sessions` items are ignored; there is no session model and
  therefore no release-adoption/crash-rate analytics.
- **Session Replay** — `replay_recording` / `replay_event` / `replay_video` ignored; no replay app.
- **Profiling** — `profile` / `profile_chunk` ignored.
- **Cron / check-ins** — `check_in` ignored.
- **Client reports** — `client_report` ignored.
- **Native crash pipelines beyond minidump** — no Unreal/PlayStation ingest.
- **No Relay** — GlitchTip does **not** run Sentry's Relay proxy; it implements the HTTP ingest contract
  directly in the app (Rust envelope handling in-process).

### Author's stated philosophy

The backend README is explicit: *"Simplicity over features"*, *"our code base is a fraction of the size
of Sentry"*, *"Lightweight… 512MB of ram"*. GlitchTip trades Sentry's feature surface for a small,
single-binary-ish footprint and a clean MIT license.

---

## 6. Web UI

- **Framework**: Angular 22 SPA + Angular Material, built to static assets and served by Django.
  The frontend is a from-scratch Angular project (not Sentry's React UI).
- **Page set** (from the backend's SPA catch-all route regex in `glitchtip/urls.py`): auth/login/register,
  **issues** (`/organizations/<slug>/issues/…`), **issue detail** (client-side route under `…/issues/<id>/`),
  **settings**, **performance**, **projects**, **releases**, **organizations**, **profile**,
  **uptime-monitors**, **logs**, **system-info**, accept-invite, reset-password.
- **Issue detail** is rendered entirely client-side from the REST/OpenAPI API; the backend serves data via
  django-ninja endpoints (issue list uses the partitioned `IssueIndex` + FTS). Settings are a set of
  Angular routes backed by the settings API.
- The API is typed end-to-end: `openapi-typescript` generates the TS client from the backend OpenAPI spec.

---

## 7. Licensing

- **Backend**: [MIT](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/master/LICENSE)
  (Copyright © 2019 GlitchTip).
- **Frontend**: [MIT](https://gitlab.com/glitchtip/glitchtip-frontend/-/blob/master/LICENSE)
  (Copyright © 2020 GlitchTip).
- **Meta repo**: [MIT](https://gitlab.com/glitchtip/glitchtip/-/blob/master/LICENSE)
  (Copyright © 2023 David Burke).
- **Vendored Sentry code**: the `sentry/` directory is **BSD-licensed** Sentry code, attributed in
  [`NOTICE.md`](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/master/NOTICE.md):
  *"This product includes BSD licensed software developed by Sentry"*.
- **Attribution notes**: the backend README acknowledges *"the Sentry team for their ongoing open source
  SDK work and formerly open source backend of which this project is based on"*.

**Contrast with Sentry** (relevant to BugHan's licensing posture): Sentry's server was relicensed from
BSD-3-Clause to the **Business Source License (BSL)** in late 2019, then to the **Functional Source
License (FSL)** in Nov 2023 — both source-available, neither OSI open source
([Sentry's FSL announcement](https://blog.sentry.io/introducing-the-functional-source-license-freedom-without-free-riding/),
[`sentry/LICENSE` (BSL 1.1)](https://github.com/getsentry/sentry/blob/master/LICENSE)). GlitchTip is a
"partial fork / mostly re-implementation" of the pre-relicense open-source Sentry, released under MIT.
The SDKs themselves remain MIT/BSD open source, which is the part an SDK-compatible server actually
consumes.

---

## 8. Sentry protocol compatibility

GlitchTip's stated goal is that an **unmodified Sentry SDK works** against it, and it implements the SDK
ingest HTTP contract directly (no Relay). Relevant Sentry spec facts (for BugHan's protocol ticket):

- **DSN / auth** — DSN format `{protocol}://{public_key}:{secret_key}@{host}/{project_id}`; requests go
  to `{base}/api/{project_id}/{endpoint}/`; auth via `X-Sentry-Auth: Sentry sentry_version=7,
  sentry_client=…, sentry_key=<public key>, …` (or `?sentry_version=7&sentry_key=…`). Protocol version is
  **7**; the secret key is deprecated. Source:
  [develop.sentry.dev — Authentication](https://develop.sentry.dev/sdk/foundations/transport/authentication/).
- **Endpoints** — `/envelope/`, `/store/`, `/minidump/`, `/security/` (CSP), `/unreal/`,
  `/playstation/`. GlitchTip implements the first four.
  ([Authentication doc](https://develop.sentry.dev/sdk/foundations/transport/authentication/),
  [Envelopes doc](https://develop.sentry.dev/sdk/foundations/envelopes/)).
- **Envelope format** — newline-delimited: one JSON header line (fields `event_id`, `dsn`, `sdk`,
  `sent_at`), then items, each a JSON item-header line (`type`, `length`, `content_type`, …) followed by a
  length-prefixed (or newline-terminated) payload. Implementations must **skip unknown item types** and
  retain them; `sent_at` (RFC 3339) replaced the deprecated `sentry_timestamp`.
  ([Envelopes spec](https://develop.sentry.dev/sdk/foundations/envelopes/),
  [Envelope Items](https://develop.sentry.dev/sdk/foundations/envelopes/envelope-items/)).
- **Rate-limit semantics** — Sentry signals API limits via `X-Sentry-Rate-Limit-Limit/Remaining/Reset`
  and concurrent-limit headers, and rejects over-limit requests (429). GlitchTip's ingest throttling is
  its own probabilistic mechanism (see §4), returning 429 + `Retry-After`; it does not appear to emit
  Sentry's full `X-Sentry-Rate-Limit-*` header set on ingest.
  ([docs.sentry.io — Rate Limits](https://docs.sentry.io/api/ratelimits/)).
- **Release health** — Sentry's release-adoption/crash-rate analytics depend on **session** envelope
  items ([Sentry Releases docs](https://docs.sentry.io/product/releases/)); GlitchTip ignores sessions, so
  this feature is absent (see §5).
- **How GlitchTip keeps compatibility over time**: it pins to the current envelope spec, validates items
  with Pydantic schemas that are lenient on unknown fields, and routes **unknown envelope item types** to
  a diagnostic path (fingerprinted per-type) instead of failing — so a new SDK item type degrades to a
  tracked warning rather than a hard error. Its compatibility surface is the SDK-facing HTTP API, **not**
  Relay's internal protocol, and **not** the full Sentry web API (it exposes its own django-ninja API with
  Sentry-inspired route shapes such as `/api/0/organizations/…/issues/…/events/…/json/`).

---

## 9. Facts most relevant to BugHan (reuse / beat at our scale)

Observations only — no decisions:

- **Reuse directly (open source)**: the vendored `sentry/` grouping/culprit code (BSD), the envelope
  item-type taxonomy, and Sentry's public envelope/DSN/auth/rate-limit specs are all reusable references
  for BugHan's ingestion contract.
- **Simplest thing that works**: GlitchTip proves a **single-process all-in-one** (Granian + embedded
  VTasks worker) with **just Postgres** (Valkey optional) can run Sentry-compatible ingest at
  "tens of thousands of events/day, a few projects" — the exact BugHan deployment target — on ~512 MB RAM.
- **Grouping is the obvious "beat"**: GlitchTip uses `MD5(title+culprit+type)` grouping with no
  stacktrace normalization. This is simpler and cheaper than Sentry's hierarchical grouping, but weaker
  (e.g. reordered/refactored stacktraces and anonymous minified frames group less accurately). BugHan's
  "beat at our scale" opportunity is a more faithful grouping without adopting all of Sentry's machinery.
- **Rate limiting**: GlitchTip uses percentage-based probabilistic throttling cached in Valkey — cheap
  and effective for abuse/quota, but coarser than Sentry's windowed counters. A BugHan design choice
  point.
- **Feature boundary is crisp**: sessions/replay/profiling/cron are *deliberately* out — confirmed by an
  explicit ignore-list in code. This is a ready-made "cut list" precedent for BugHan's v0.1 scope.
- **Licensing**: MIT across the board (with a BSD vendored shim) — no copyleft or BSL/FSL entanglement for
  BugHan to navigate if it reuses GlitchTip ideas/code.

---

## Sources

- GlitchTip meta repo: <https://gitlab.com/glitchtip/glitchtip>
- GlitchTip backend: <https://gitlab.com/glitchtip/glitchtip-backend> (README, `pyproject.toml`,
  `compose.yml`, `Dockerfile`, `glitchtip/urls.py`, `glitchtip/ingest_asgi.py`,
  `apps/event_ingest/*`, `apps/issue_events/models.py`, `sentry/*`, `NOTICE.md`, `CHANGELOG`, `bin/*`)
- GlitchTip frontend: <https://gitlab.com/glitchtip/glitchtip-frontend> (`package.json`, `angular.json`)
- GlitchTip docs: <https://glitchtip.com/documentation> · blog: <https://glitchtip.com/blog>
- GlitchTip author blogs: ["GlitchTip 6 released"](https://glitchtip.com/blog/2026-02-03-glitchtip-6-released/),
  ["Making Django Fast: VTasks processes tasks 4x faster than Celery"](https://glitchtip.com/blog/2026-04-13-django-vtasks/)
- Sentry SDK specs: [Envelopes](https://develop.sentry.dev/sdk/foundations/envelopes/),
  [Envelope Items](https://develop.sentry.dev/sdk/foundations/envelopes/envelope-items/),
  [Authentication / DSN](https://develop.sentry.dev/sdk/foundations/transport/authentication/)
- Sentry docs: [Rate Limits](https://docs.sentry.io/api/ratelimits/),
  [Releases](https://docs.sentry.io/product/releases/), [Relay](https://docs.sentry.io/product/relay/)
- Sentry licensing: [FSL announcement](https://blog.sentry.io/introducing-the-functional-source-license-freedom-without-free-riding/),
  [`sentry/LICENSE`](https://github.com/getsentry/sentry/blob/master/LICENSE)
