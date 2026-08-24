# Sentry protocol compatibility scope

Research for BugHan (ticket #17). Establishes the **facts** about Sentry's ingest protocol so a later
ticket can decide exactly which subset BugHan must implement for official browser SDKs
(`@sentry/browser`, current major versions) to work unmodified. Findings are drawn from the official
developer docs at [`develop.sentry.dev`](https://develop.sentry.dev/), the API reference at
[`docs.sentry.dev`](https://docs.sentry.dev/), the [`getsentry/sentry-javascript`](https://github.com/getsentry/sentry-javascript)
source, [`getsentry/relay`](https://github.com/getsentry/relay), and the
[`glitchtip/glitchtip-backend`](https://gitlab.com/glitchtip/glitchtip-backend) source (Sentry's
proven-compatible reimplementation). No design decisions are made here.

> Note: the SDK specs are versioned and moving fast (several changelogs in this doc reference dates up
> to 2026). Facts below reflect the spec as of the research date. The canonical, citable source for each
> claim is linked inline.

---

## 1. Envelope spec

### 1.1 What an envelope is

Envelopes are the modern ingestion format. They look like HTTP multipart form data: a single JSON
**envelope header** line, followed by zero or more **items**, each of which is its own JSON **item
header** line followed by a raw payload. See
[Envelopes](https://develop.sentry.dev/sdk/foundations/envelopes/).

```
Envelope = Headers { "\n" Item } [ "\n" ] ;
Item     = Headers "\n" Payload ;
Payload  = { * } ;
```

Key serialization rules (all from the [Envelopes](https://develop.sentry.dev/sdk/foundations/envelopes/)
spec):

- Headers are **JSON objects encoded on a single line**, UTF-8, terminated by `\n` or EOF. No leading/trailing whitespace.
- An envelope **header is required but may be empty** (`{}`).
- Each item has a **`type`** header (string, required) and a **`length`** header (int, recommended — byte length of the payload). If `length` is omitted the payload runs to the next newline.
- Newlines are UNIX `\n` (ASCII 10). A preceding `\r` is treated as part of the payload, not a newline.
- Envelopes should end with a trailing newline (optional). EOF does not implicitly terminate a payload when more bytes are expected.
- Implementations **must gracefully skip and retain items of unknown type** and forward unknown attributes.

Envelope headers valid in all situations:

| Header | Type | Required? | Notes |
|---|---|---|---|
| `event_id` | UUID string | required when the envelope carries an event/transaction/feedback | 32-hex (dashes discouraged), lowercase |
| `dsn` | string | optional | full DSN for self-authentication (Relay ≥ 21.6.0) |
| `sdk` | object | recommended | same shape as the `sdk` event interface |
| `sent_at` | RFC 3339 UTC string | recommended | clock-drift correction; write once, at send time |
| `trace` | object | see DSC | dynamic sampling context, copied from `baggage` |

Full example (from the spec):

```
{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","dsn":"https://e12d836b15bb49d7bbf99e64295d995b:@sentry.io/42"}\n
{"type":"attachment","length":10,"content_type":"text/plain","filename":"hello.txt"}\n
\xef\xbb\xbfHello\r\n\n
{"type":"event","length":41,"content_type":"application/json","filename":"application.log"}\n
{"message":"hello world","level":"error"}\n
```

### 1.2 Store endpoint path

Envelopes are POSTed to:

```
POST /api/<project_id>/envelope/
```

(`<project_id>` is the numeric project ID — the last segment of the DSN.) See
[Envelopes → Ingestion](https://develop.sentry.dev/sdk/foundations/envelopes/#ingestion).

- Accepted `content-type` is **`application/x-sentry-envelope`**, which is **implied if the header is
  missing**. `text/plain`, `multipart/form-data`, and `application/x-www-form-urlencoded` are also
  accepted (to reduce CORS preflights) and behave identically.
- The legacy JSON store endpoint `POST /api/<project_id>/store/` is **deprecated**; all data should go
  to the envelope endpoint ([Store Endpoint](https://develop.sentry.dev/sdk/store/)).
- Other ingest endpoints (not needed for browser focus, listed for completeness):
  `/api/<project_id>/minidump/`, `/api/<project_id>/unreal/`, `/api/<project_id>/playstation/`, and
  `/api/<project_id>/security/` (CSP reports)
  ([Authentication](https://develop.sentry.dev/sdk/foundations/transport/authentication/)).

### 1.3 Authentication / DSN key format

A DSN is:

```
'{PROTOCOL}://{PUBLIC_KEY}:{SECRET_KEY}@{HOST}{PATH}/{PROJECT_ID}'
```

The ingest base URI is `{PROTOCOL}://{HOST}{PATH}`, and endpoints are `{BASE_URI}/api/{PROJECT_ID}/{ENDPOINT}/`
([Authentication](https://develop.sentry.dev/sdk/foundations/transport/authentication/)).

Authentication is **one of** three mechanisms (any combination must agree, or the request is rejected;
if all are missing → `403 Forbidden`):

1. **`X-Sentry-Auth` header**:
   `X-Sentry-Auth: Sentry sentry_version=7, sentry_client=<name>/<version>, sentry_key=<public key>[, sentry_secret=<secret>]`
2. **Query string**: `?sentry_version=7&sentry_key=<public key>&sentry_client=...`
3. **Envelope `dsn` header**: full DSN string (requires Relay ≥ 21.6.0).

Required/optional auth fields: `sentry_key` (required), `sentry_version` (required, current value
**`7`**), `sentry_client` (recommended), `sentry_timestamp` (deprecated — use `sent_at` envelope
header), `sentry_secret` (deprecated; pass through if set but never require it).

> **Browser-specific fact**: `sentry-js` deliberately authenticates via the **query string**, not the
> `X-Sentry-Auth` header, to avoid CORS preflight requests. It sends `sentry_version`, `sentry_key`,
> and `sentry_client` as query params on the envelope URL and does **not** send the secret. Source:
> [`packages/core/src/api.ts`](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/api.ts)
> (`SENTRY_API_VERSION = '7'`, `getEnvelopeEndpointWithUrlEncodedAuth`).

### 1.4 `sentry-trace` and `baggage` headers

These are **propagation** headers between services, not ingest headers. Facts relevant to a compatible
server (which mostly means *not breaking* them and understanding what arrives in the envelope):

- **`sentry-trace`** format: `traceid-spanid[-sampled]` where `traceid` is 32 hex chars (128-bit),
  `spanid` is 16 hex chars (64-bit), and `sampled` is an optional single char `0`/`1` (absent = defer).
  See [Trace Propagation](https://develop.sentry.dev/sdk/foundations/trace-propagation/#sentry-trace-header).
- **`baggage`** (W3C baggage) carries the **Dynamic Sampling Context (DSC)**. Sentry keys are prefixed
  `sentry-`: `sentry-trace_id`, `sentry-public_key`, `sentry-sample_rate`, `sentry-sampled`,
  `sentry-release`, `sentry-environment`, `sentry-transaction`, `sentry-org_id`, `sentry-sample_rand`.
  See [DSC spec](https://develop.sentry.dev/sdk/foundations/trace-propagation/dynamic-sampling-context/).
- The DSC is also sent to Sentry as the envelope header **`trace`** (the baggage `sentry-*` pairs with
  the prefix stripped). A transaction envelope must carry a `trace` DSC header.
- The DSC is **frozen** once propagated; downstream services must copy it verbatim.

### 1.5 Gzip / compression

- The **envelope format itself has no compression**; compression happens at the HTTP layer
  ([Envelopes](https://develop.sentry.dev/sdk/foundations/envelopes/)).
- Relay/Sentry accept these `content-encoding`s on envelope requests: **`gzip`**, **`deflate`**,
  **`br`** (Brotli), **`zstd`** ([Compression](https://develop.sentry.dev/sdk/foundations/transport/compression/)).
- `transfer-encoding: chunked` is supported for very large requests (allows omitting `content-length`).
- **Browser fact**: `sentry-js` does **not** gzip envelopes by default; it POSTs the serialized
  envelope as the raw body (fetch string body). So a compatible server must accept uncompressed
  envelopes, and should accept the encodings above for SDKs/Relay that do compress
  ([`packages/browser/src/transports/fetch.ts`](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser/src/transports/fetch.ts)).

### 1.6 Envelope size limits

From [Envelopes → Size Limits](https://develop.sentry.dev/sdk/foundations/envelopes/#size-limits)
(current, subject to change):

- 200 MiB per envelope after decompression (all items).
- 1 MiB per event/transaction/span/log/metric item.
- 100 KiB monitor check-in, 4 KiB client report, 50 MiB profile, 10 MiB compressed replay / 100 MiB decompressed.
- 100 session items per envelope; 100 pre-aggregated buckets per `sessions` item.

### 1.7 Envelope item constraints

Envelope items of different telemetry types must **not** be mixed in one envelope (Relay rate-limits per
data category). Exceptions: session updates may accompany their crash event, attachments accompany their
event, and `user_report`/feedback accompany their event. `event`, `transaction`, and `feedback` items are
mutually exclusive (at most one per envelope). See
[Envelope Items](https://develop.sentry.dev/sdk/foundations/envelopes/envelope-items/).

---

## 2. Event types & minimal payloads

### 2.1 Error events (`"event"` item)

A Sentry error event is a JSON object packed into an `event` envelope item. Required attributes
([Event Payloads](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/)):

| Field | Required | Notes |
|---|---|---|
| `event_id` | required | 32-char lowercase hex UUID v4 |
| `timestamp` | required | RFC 3339 string or Unix seconds (int/float) |
| `platform` | required | e.g. `javascript`; from Sentry's platform list |

Highly-recommended optional attributes a server should at least store/echo: `level` (defaults to
`error`; values `fatal|error|warning|info|debug`), `logger`, `transaction`, `server_name`, `release`,
`dist`, `tags` (string→string, each <200 chars), `environment` (default `production`), `modules`,
`extra`, `fingerprint` (grouping), `errors`.

Core interfaces a browser server must parse or at least preserve: `exception` (values[] with
`type`/`value`), `message`, `stacktrace`, `breadcrumbs`, `user`, `request`, `contexts` (including
`contexts.trace`), `threads`, `sdk`. The server understands legacy/non-canonical shapes for backward
compat. Field size limits (variable-size fields) are listed in
[Event Payloads → Size Limits](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/#size-limits)
(messages ≤ 8192 chars, context objects ≤ 8 kB, extra items ≤ 16 kB, stack traces ≤ 50 frames, etc.).

The envelope header `event_id` is required for `event` items and takes precedence over the payload's
`event_id` if they differ.

### 2.2 Transactions / spans (`"transaction"` item)

A transaction is a span + event. It is sent as a `transaction` envelope item and **must** include a
`contexts.trace` and (recommended) a `spans` array. Required
([Transaction Payloads](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/transaction/)):

- `type` = `"transaction"`
- `start_timestamp` (RFC 3339 or Unix seconds) — must be ≤ `timestamp`
- `timestamp`

Recommended/optional: `contexts.trace` (with `trace_id`, `span_id`, `op`, optional `parent_span_id`,
`status`), `spans` (array), `measurements` (standard web-vitals keys `fp/fcp/lcp/fid/cls/ttfb` etc.),
`transaction_info.source` (required for dynamic sampling: `custom|url|route|view|component|task|unknown`).

Span attributes ([Span Interface](https://develop.sentry.dev/sdk/foundations/transport/event-payloads/span/)):
required `span_id` (16 hex), `trace_id` (32 hex), `start_timestamp`, `timestamp`; optional
`parent_span_id`, `op`, `description`, `status`, `tags`, `data`, `origin`. Spans in the `spans` array
need not be ordered (server sorts by start/end).

Envelope header `event_id` is required for `transaction` items; the `trace` envelope header (DSC) is
optional but recommended.

### 2.3 Sessions (`"session"` item) and session aggregates (`"sessions"` item)

Sessions are client-driven health tracking, sent as `session` items
([Sessions](https://develop.sentry.dev/sdk/sessions/)). Single-session payload:

```json
{"sid":"7c7b6585-f901-4351-bf8d-02711b721929","init":true,
 "started":"2020-02-07T14:16:00Z","duration":60,"status":"exited",
 "attrs":{"release":"my-project-name@1.0.0","environment":"production"}}
```

- `started` (ISO string) and `attrs` are **required**; `attrs.release` is the only required `attrs` key.
- Optional: `sid`, `did` (distinct id, hashed on server), `seq`, `timestamp` (received), `init`
  (default false — first update **must** set `init:true`), `duration` (float seconds), `status`
  (`ok|exited|crashed|abnormal`, default `ok`), `errors` (default 0), `abnormal_mechanism`,
  `attrs.environment`, `attrs.ip_address`, `attrs.user_agent`.
- `crashed`/`exited`/`abnormal` are terminal; no further updates allowed. Session attributes are
  immutable after first transmission (only status/duration/errors may change).
- Aggregates use a `sessions` item: `{"aggregates":[{...}],"attrs":{...}}` where each aggregate has
  `started` (rounded to the minute), optional `did`, and counts `exited|abnormal|crashed|errored`.

### 2.4 User feedback

Two mechanisms exist:

- **`feedback` item** (current, stable) — a full event payload with a `contexts.feedback` object.
  Required event fields: `event_id`, `timestamp`, `platform`. `contexts.feedback.message` is required
  (≤ 4096 chars); optional `contact_email`, `name`, `url`, `associated_event_id`, `replay_id`.
  Envelope header `event_id` required. Mutually exclusive with `transaction`. See
  [Feedback](https://develop.sentry.dev/sdk/telemetry/feedbacks/).
- **`user_report` item** (deprecated, still accepted) — `{"event_id","email","name","comments"}`.
  `event_id` required (associates with an error ≤ 30 min old; discarded if the event doesn't exist).
  See [User Report](https://develop.sentry.dev/sdk/telemetry/user-reports/).

For a v1 browser-focused server, the minimal parsing burden is: `event` (errors), `transaction` (if
performance is in scope), `session`/`sessions`, and `feedback` (or at least `user_report`). Everything
else can be skipped-but-retained per the "unknown item types must be retained" rule.

---

## 3. Source-map / artifact ingest

This is a **separate authenticated API surface** (`/api/0/...`), not the SDK ingest endpoint. It uses
**organization auth tokens** (Bearer) with CI/release scopes, not the DSN key.

### 3.1 Release files (classic `release` + `dist` association)

- **Endpoint** (project-scoped): `POST /api/0/projects/{organization_id_or_slug}/{project_id_or_slug}/releases/{version}/files/`
  ([Upload a New Project Release File](https://docs.sentry.dev/api/releases/upload-a-new-project-release-file/))
- **Endpoint** (organization-scoped): `POST /api/0/organizations/{organization_id_or_slug}/releases/{version}/files/`
  ([Upload a New Organization Release File](https://docs.sentry.dev/api/releases/upload-a-new-organization-release-file/))
- **Request**: `multipart/form-data` with fields:
  - `file` (required) — the source map (or other artifact) contents.
  - `name` — full path the file is referenced as (e.g. the JS file URI); defaults to the uploaded filename.
  - `dist` — the distribution name to associate the file with (matches the `dist` in the event payload).
  - `header` — repeatable `"key:value"` strings (e.g. `Content-Type: application/json`).
- Releases are created first via `POST /api/0/organizations/{org}/releases/` or
  `POST /api/0/projects/{org}/{project}/releases/`, which take `version` (required) and optional
  `ref`, `url`, `projects`, `dateReleased`. Sourcemaps are matched to events by `release` + `dist`
  ([CLI releases](https://docs.sentry.io/cli/releases/)).
- File format: a **JSON source map** (v3) with `version`, `sources`, `sourcesContent`, `mappings`,
  `names`; Sentry does not require a specific structure beyond JSON — it stores the raw file.

### 3.2 Debug-ID / artifact bundles (modern bundler plugins)

Newer bundler plugins (`@sentry/vite-plugin`, `@sentry/webpack-plugin`, etc.) inject a deterministic
`debugId` into each JS file and its source map, then upload **artifact bundles** keyed by debug id via
a chunk-upload + assemble flow instead of the release-files endpoint. The endpoints (implemented by
both Sentry and GlitchTip):

- `POST /api/0/organizations/{org}/chunk-upload/` — upload compressed chunks (`gzip`/`zlib`), sha1
  checksum, per-category accept types: `debug_files`, `release_files`, `sources`, `artifact_bundles`,
  `proguard`, `pdbs`.
- `POST /api/0/organizations/{org}/artifactbundle/assemble/` (and `/releases/{version}/assemble/`) —
  assemble uploaded chunks into a bundle by checksum; returns `state: created|not_found` +
  `missingChunks`.

GlitchTip reference: [`apps/files/api.py`](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/master/apps/files/api.py)
(chunk-upload) and
[`apps/sourcecode/api.py`](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/master/apps/sourcecode/api.py)
(artifact bundle assemble).

> Implication for BugHan: source-map ingest is **optional for "SDK works unmodified"** — it is only
> required for symbolication/stack-unminification in the UI, not for events to be accepted. `sentry-cli`
> and plugins require a much larger API surface (releases, files, chunk-upload, assemble, difs) and
> token auth, so it is a separate scope decision.

---

## 4. Server → SDK contract

### 4.1 Success and error semantics

- `HTTP 2xx` = success; the SDK considers the envelope sent
  ([Offline Caching](https://develop.sentry.dev/sdk/foundations/transport/offline-caching/)).
- On `4xx`/`5xx` the SDK **discards the envelope** and records a **client report** (`send_error`
  outcome).
- **`413 Content Too Large`**: discard, record client report, **never retry**.
- **`429 Too Many Requests`**: discard, respect rate limits, **never retry**, and **do not** record a
  client report (upstream already counts it).
- Network errors (timeout, DNS failure, reset) **may** be retried; SDK-internal processing errors must
  discard + record `internal_sdk_error` (to avoid endless retries).

### 4.2 Rate limiting

Rate limits are communicated via status `429` + `Retry-After`, plus the special header
`X-Sentry-Rate-Limits` ([Rate Limiting](https://develop.sentry.dev/sdk/foundations/transport/rate-limiting/)).

```
X-Sentry-Rate-Limits: <quota_limit>, <quota_limit>, ...
quota_limit = retry_after:categories:scope:reason_code:namespaces:...
```

- `retry_after` = seconds (int or float) until the limit expires.
- `categories` = semicolon-separated data categories; **empty = all categories**. Categories include
  `default`, `error`, `transaction`, `session`, `attachment`, `security`, `feedback`, `replay`,
  `span`, `metric_bucket`, `profile`, `monitor`, `log_item`, etc.
  ([Relay data categories](https://github.com/getsentry/relay/blob/master/relay-base-schema/src/data_category.rs)).
- `scope` (`organization|project|key`) and `reason_code` may be ignored by SDKs.
- The header may appear on **any** response (including `200 OK`), proactively disabling categories.
- SDK parsing rules: on every response read `X-Sentry-Rate-Limits`; on `429` read `Retry-After`
  (treat as all categories); on `429` with neither, assume 60 s for all categories. Keep the max limit
  per category; ignore unknown categories/scopes; track per-DSN.

### 4.3 What the SDK does when rejected / offline

From the `sentry-js` source
([`packages/core/src/transports/base.ts`](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/transports/base.ts)):

- Before sending, items whose data category is currently rate-limited are **dropped from the envelope**
  (`ratelimit_backoff` outcome); if the envelope becomes empty it is not sent.
- A bounded promise buffer (size 64 core / 40 browser) queues sends; overflow drops with
  `queue_overflow` and records a client report.
- On response: `413` → `send_error` client report; otherwise rate limits are updated from
  `X-Sentry-Rate-Limits`/`Retry-After`. Non-2xx is logged, not thrown.
- On network error → `network_error` client report and the promise rejects (the envelope is not retried
  automatically in the browser SDK; disk caching is a mobile/desktop feature — see
  [Offline Caching](https://develop.sentry.dev/sdk/foundations/transport/offline-caching/)).
- The browser fetch transport reads exactly `X-Sentry-Rate-Limits` and `Retry-After` from responses
  ([`packages/browser/src/transports/fetch.ts`](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser/src/transports/fetch.ts)).
- Browser requests use `keepalive` (below 60 KB and < 15 concurrent) so navigation doesn't cancel them.

Client reports themselves are sent as a `client_report` envelope item (discarded events counted by
category + reason), so a compatible server should tolerate that item type.

---

## 5. Compatibility matrix

### 5.1 `sentry-js` majors currently supported

From the npm registry `@sentry/browser` dist-tags (as of research date) and
[`getsentry/sentry-javascript`](https://github.com/getsentry/sentry-javascript) releases:

| Major | npm tag | Status |
|---|---|---|
| v10 | `latest` (10.70.0) | current stable |
| v9 | `v9` (9.47.1) | maintained |
| v8 | `v8` (8.55.2) | maintained |
| v7 | `v7` (7.120.4) | legacy / effectively EOL (security-only) |
| v11 | `next` (11.0.0-alpha.1) | pre-release |

**Recommendation for research**: target the wire format shared by **v8/v9/v10** (all use envelopes,
query-string DSN auth, the same `event`/`transaction`/`session`/`feedback` item types). v7 uses the same
envelope wire format but an older feature surface; v11 is not yet stable. The envelope protocol itself
has been stable since Sentry v20.6.0 / Relay, so a v8-v10-compatible server also accepts v7 envelopes.

### 5.2 Server-relevant SDK features

| Feature | Server impact | Source |
|---|---|---|
| **`tunnel`** | If set, the SDK POSTs the serialized envelope to a user-supplied URL instead of the DSN-derived `/api/<project>/envelope/`; DSN is still required for metadata. No `X-Sentry-Auth`/query auth is added to the tunnel request (same-origin). Server-side tunnels must forward to the envelope endpoint. | [JS options](https://docs.sentry.io/platforms/javascript/configuration/options/), [`api.ts`](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/api.ts) |
| **compression** | SDK may `content-encode` envelopes (`gzip`/`deflate`/`br`/`zstd`); browser SDK sends uncompressed by default. Server must handle `content-encoding`. | [Compression](https://develop.sentry.dev/sdk/foundations/transport/compression/) |
| **`beforeSend`** | Runs entirely client-side (mutate/drop event before send). No server contract, but it means the server must accept the *resulting* event shape and tolerate events that were filtered client-side. | [JS options](https://docs.sentry.io/platforms/javascript/configuration/options/) |
| **`sampleRate`** (errors) | Client-side drop (default 1.0). Server just sees fewer events; a `sessions`-based release health needs the session update even when the error is sampled out. | [JS options](https://docs.sentry.io/platforms/javascript/configuration/options/) |
| **`tracesSampleRate` / `tracesSampler`** | Client-side transaction sampling (head-based; requires `tracesSampleRate` or `tracesSampler` to enable tracing). Server sees sampled transactions only; must honor DSC on `trace` header + `baggage` for trace-wide sampling. | [JS options](https://docs.sentry.io/platforms/javascript/configuration/options/), [DSC](https://develop.sentry.dev/sdk/foundations/trace-propagation/dynamic-sampling-context/) |
| **`dsn` / query-string auth** | Browser SDK authenticates via `sentry_version=7&sentry_key=…&sentry_client=…` in the URL query (CORS-safe), not the `X-Sentry-Auth` header. Server must accept both. | [`api.ts`](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/api.ts) |

### 5.3 GlitchTip reference (proven compatible subset)

GlitchTip implements exactly the ingest surface a compatible server needs, and it is the closest
reference for BugHan. Its root URLconf
([`glitchtip/urls.py`](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/master/glitchtip/urls.py))
exposes:

- `api/<int:project_id>/envelope/` — envelope ingest (errors, transactions, sessions, feedback, attachments, minidumps).
- `api/<int:project_id>/minidump/` — native minidumps.
- `api/<int:project_id>/store/` — legacy JSON store (still accepted; [`apps/event_ingest/api.py`](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/master/apps/event_ingest/api.py)).
- `api/<int:project_id>/security/` — CSP reports.
- `v1/logs` — OTLP/HTTP logs.
- A full `/api/0/...` auth-token API (releases, release files, chunk-upload, artifact bundles, difs,
  projects, environments, stats) for `sentry-cli` and the UI.

GlitchTip also demonstrates the server→SDK contract: `429` + `Retry-After` via a throttle exception and
`413` via a request-too-big handler
([`glitchtip/api/api.py`](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/master/glitchtip/api/api.py)).

---

## 6. Fact summary (for the design ticket)

1. **One ingest endpoint** — `POST /api/<project_id>/envelope/` — accepts all SDK data. Auth via
   query string (`sentry_version=7&sentry_key=…`) or `X-Sentry-Auth` header.
2. **Envelope** = JSON header line + item (JSON header line + payload) pairs; unknown item types must
   be retained, not rejected.
3. **Must parse minimally**: `event`, `transaction` (if perf), `session`/`sessions`, `feedback`
   (or `user_report`). Required event fields: `event_id`, `timestamp`, `platform`.
4. **Return codes**: `2xx` = accept; `429` + `X-Sentry-Rate-Limits`/`Retry-After` = backoff (SDK drops,
   never retries); `413` = too large (SDK drops); any `4xx`/`5xx` = drop + client report.
5. **Source-map upload is a separate token-authenticated `/api/0/...` API** (release files
   `release`+`dist` multipart, or debug-id chunk-upload+assemble) — optional for SDK compatibility,
   required only for unminification.
6. **Target `sentry-js` v8–v10** wire format (v11 alpha, v7 legacy). Server-relevant client features:
   `tunnel`, `content-encoding` compression, `beforeSend` (client-side), `sampleRate`/
   `tracesSampleRate` (client-side), query-string DSN auth.
