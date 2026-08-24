# Browser SDK (sentry-javascript) — what the server receives

Research ticket #20. Scope: establish **facts** about the wire payloads an unmodified Sentry browser SDK sends to an ingestion server, so BugHan's ingestion contract can be made Sentry-compatible. No design decisions here.

Version note: findings verified against the `develop` branch of [`getsentry/sentry-javascript`](https://github.com/getsentry/sentry-javascript) and the authoritative SDK specs on [develop.sentry.dev](https://develop.sentry.dev/sdk/). The current released SDK is **v10.x** (latest tag `10.70.0` at the time of writing). Some specifics (mechanism `type` strings, span streaming, the offline transport) are version-dependent and are flagged where relevant. Broad payload *shapes* are stable across v7–v10.

---

## 0. Transport: how every payload arrives

The browser SDK wraps everything in an **envelope** and POSTs it to the project envelope endpoint. It does **not** use the legacy store endpoint.

- Endpoint: `POST /api/{project_id}/envelope/` — built from the DSN as `{scheme}://{host}[:port][/{path}]/api/{projectId}/envelope/`. ([api.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/api.ts), [Envelopes spec](https://develop.sentry.dev/sdk/foundations/envelopes/))
- Auth is sent as **query-string parameters** to avoid a CORS preflight: `sentry_version=7`, `sentry_key=<public key>`, `sentry_client=<sdkName>/<version>`. Header auth (`X-Sentry-Auth`) is also defined by the spec but the browser SDK uses the query form. ([api.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/api.ts))
- Content-Type: `application/x-sentry-envelope` is implied; the SDK may send `text/plain` to avoid preflight. ([Envelopes spec](https://develop.sentry.dev/sdk/foundations/envelopes/))
- The legacy store endpoint `POST /api/{project_id}/store/` is deprecated by the spec; a compatible server should still consider accepting it for older SDKs, but modern browser SDKs use `/envelope/`.

### Envelope serialization

An envelope is newline-delimited: one JSON header line, then for each item a JSON item-header line followed by a JSON/binary payload line.

```
{ "event_id":"9ec79c33ec9942ab8353589fcb2e04dc", "sent_at":"2023-01-01T00:00:00.000Z", "sdk":{"name":"sentry.javascript.browser","version":"10.70.0"} }
{ "type":"event" }
{ "event_id":"9ec79c33ec9942ab8353589fcb2e04dc", "level":"error", ... }
```

Envelope headers the browser SDK emits: `event_id`, `sent_at` (RFC 3339, UTC), `sdk:{name,version}`, and `trace:<DSC>` when a dynamic sampling context exists. `dsn` is added to the envelope header only when a `tunnel` is configured. ([createEventEnvelopeHeaders](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/utils/envelope.ts), [envelope.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/envelope.ts))

Item `type` values the browser SDK sends: `event`, `transaction`, `session` (and `sessions` for aggregates — not used by the browser SDK), `attachment`, `client_report`, plus replay/profile/etc. when those features are on. Envelope item → data category mapping (relevant for rate limits): `event`→`error`, `transaction`→`transaction`, `session`→`session`, `sessions`→`session`, `client_report`→`internal`. ([envelope.ts utils](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/utils/envelope.ts))

---

## 1. Error events

### Event envelope item

An error event is an envelope item `{ "type": "event" }` whose payload is the event JSON. Required top-level fields are `event_id`, `timestamp`, and `platform` (`javascript`). ([Event Payloads](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/))

Common optional fields the browser SDK sets: `level`, `environment`, `release`, `dist`, `tags`, `extra`, `user`, `request`, `contexts`, `breadcrumbs`, `sdk`, `transaction`, `fingerprint`, and the `exception` interface.

### Exception / stacktrace shape

`exception` is an object with a `values` array (ordered oldest→newest for chained exceptions). Each entry:

```json
{
  "exception": {
    "values": [
      {
        "type": "TypeError",
        "value": "Cannot read properties of undefined (reading 'foo')",
        "module": "webpack:///./src/app.js",
        "mechanism": {
          "type": "onerror",
          "handled": false
        },
        "stacktrace": {
          "frames": [
            {
              "filename": "https://example.com/static/js/main.abc123.js",
              "abs_path": "https://example.com/static/js/main.abc123.js",
              "function": "App.render",
              "lineno": 42,
              "colno": 7,
              "in_app": true,
              "context_line": "  return <button onClick={this.click}>x</button>;",
              "pre_context": ["...", "..."],
              "post_context": ["...", "..."],
              "vars": { "self": "{...}" }
            },
            { "function": "anonymous", "filename": "…", "in_app": false }
          ]
        }
      }
    ]
  }
}
```

Frames are ordered **oldest (caller) → newest (callee)**; the last frame is the one that threw. Frame fields: `filename`/`abs_path`, `function`, `lineno`, `colno`, `in_app`, `context_line`/`pre_context`/`post_context`, `vars`, and (Debug ID mode) `debug_id`. `module` on the exception is optional. ([Stack Trace Interface](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/stacktrace/), [Errors spec](https://develop.sentry.dev/sdk/telemetry/errors/))

### `level` and `mechanism` (handled/unhandled)

- `level` is one of `fatal | error | warning | info | debug`; the SDK sets `error` for captured exceptions. ([Event Payloads](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/))
- `mechanism.type` distinguishes how it was captured. v8 used `onerror`, `onunhandledrejection`, `promise`, `console`, `generic`; current v9/v10 use the "trace origin" scheme: `auto.browser.global_handlers.onerror`, `auto.browser.global_handlers.onunhandledrejection`. `mechanism.handled` is `false` for global handlers, `true` for `captureException` (default `generic`). `attachStacktrace: true` produces a `synthetic` mechanism for message captures. ([globalhandlers.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser/src/integrations/globalhandlers.ts), [Errors spec](https://develop.sentry.dev/sdk/telemetry/errors/))

### Tags, contexts, user, request

- `tags` are a map of string→string, `< 200` chars each. The browser SDK's default tags are minimal (`handled: yes/no` is one it sets); everything else is user-supplied via `tags` option or scope. ([Event Payloads](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/))
- `contexts` carries `browser` (`{name, version}`), `os` (`{name, version}`), `trace` (trace/span ids), and any user context. The SDK sends `browser` and `os` contexts automatically. ([Contexts Interface](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/contexts/))
- `user` interface: `id`, `username`, `email`, `ip_address` (well-known keys) + arbitrary extra keys. ([User Interface](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/user/))
- `request` interface for client SDKs describes the *page* the error happened on: `url`, `headers`, etc. ([Request Interface](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/request/))
- `sdk` interface: `{name:"sentry.javascript.browser", version, integrations:[...], packages:[...]}`. ([SDK Interface](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/sdk/))

### Breadcrumbs (default integrations)

The SDK records breadcrumbs by default: console (`category:"console"`), HTTP (`type:"http"`, `category:"xhr"` or `"fetch"`), navigation (`type:"navigation"`, `category:"navigation"`), and UI (`category:"ui.click"`, `"ui.input"`, …). Serialized as `breadcrumbs: { "values": [...] }` (or a bare array), oldest→newest:

```json
{
  "breadcrumbs": {
    "values": [
      {
        "type": "navigation",
        "category": "navigation",
        "timestamp": "2023-01-01T00:00:00.000Z",
        "data": { "from": "/login", "to": "/dashboard" }
      },
      {
        "type": "http",
        "category": "xhr",
        "level": "info",
        "timestamp": "2023-01-01T00:00:01.000Z",
        "data": { "url": "https://api.example.com/users", "method": "GET", "status_code": 200 }
      },
      {
        "type": "default",
        "category": "console",
        "level": "info",
        "message": "rendered",
        "timestamp": "2023-01-01T00:00:02.000Z"
      },
      {
        "type": "default",
        "category": "ui.click",
        "message": "button#save",
        "level": "info",
        "timestamp": "2023-01-01T00:00:03.000Z"
      }
    ]
  }
}
```

Breadcrumb fields: `type`, `category`, `message`, `level` (default `info`), `data`, `timestamp`. HTTP crumbs put `method`, `url`, `status_code`, `reason` under `data`. ([Breadcrumbs Interface](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/breadcrumbs/))

### Source maps: client- or server-side?

**Source maps are applied server-side.** The browser SDK does **not** download and apply `.map` files at runtime in production.

- **Release/dist mode (legacy):** the SDK sends `release` (and optional `dist`) on the event plus raw minified frames whose `abs_path`/`filename` is the deployed bundle URL. The server matches `abs_path + release + dist` against an uploaded artifact. The SDK does nothing with maps itself.
- **Debug ID mode (default since the bundler plugins inject IDs):** build plugins inject `//# debugId=<uuid>` and register a global map (`window._sentryDebugIds`, or Vercel-style `_debugIds`). At event serialization the SDK reads that global, maps each frame `filename`→`debug_id`, and emits a `debug_meta` interface:

```json
{
  "debug_meta": {
    "images": [
      { "type": "sourcemap", "code_file": "https://example.com/static/js/main.abc123.js", "debug_id": "395835f4-03e0-4436-80d3-136f0749a893" }
    ]
  }
}
```

The server looks up the source map **by `debug_id`** and symbolicates. Frames may also carry their own `debug_id`. ([Debug Meta Interface](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/debugmeta/), [debug-ids.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/utils/debug-ids.ts), [Source Maps doc](https://docs.sentry.io/platforms/javascript/sourcemaps/))

The "when does it fetch maps" question therefore only matters server-side: maps are fetched from the uploaded artifact store keyed by release/dist or debug ID — never by the browser at capture time.

---

## 2. Transactions and spans

### Transaction event shape

A transaction is an envelope item `{ "type": "transaction" }` (data category `transaction`). It is a span + event hybrid: ([Transaction Payloads](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/transaction/))

```json
{
  "type": "transaction",
  "event_id": "…",
  "transaction": "/users/:id",
  "transaction_info": { "source": "route" },
  "start_timestamp": 1700000000.000,
  "timestamp": 1700000001.234,
  "contexts": {
    "trace": {
      "trace_id": "743ad8bbfdd84e99bc38b4729e2864de",
      "span_id": "a0cfbde2bdff3adc",
      "parent_span_id": "99659d76b7cdae94",
      "op": "pageload",
      "status": "ok"
    }
  },
  "spans": [ /* child spans, see below */ ],
  "measurements": {
    "lcp": { "value": 2049, "unit": "millisecond" },
    "fcp": { "value": 1500, "unit": "millisecond" },
    "fp":  { "value": 1200, "unit": "millisecond" },
    "ttfb": { "value": 13, "unit": "millisecond" },
    "ttfb.requestTime": { "value": 11, "unit": "millisecond" },
    "cls": { "value": 0.21, "unit": "" }
  }
}
```

Required: `type`, `start_timestamp`, `timestamp` (start ≤ end or Relay drops it). `contexts.trace` is required for Relay to accept it. `measurements` map a name → `{value, unit}`. Standard web measurement names include `fp`, `fcp`, `lcp`, `fid` (legacy), `cls`, `ttfb`, `ttfb.requestTime`. ([Transaction Payloads](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/transaction/))

### Span shape

Child spans live in the transaction's `spans` array (unordered; server sorts). Fields: `span_id` (16 hex), `parent_span_id`, `trace_id` (32 hex), `op`, `description`, `start_timestamp`, `timestamp`, `status`, `tags`, `data` (arbitrary attributes), and `origin`. ([Span Interface](https://develop.sentry.dev/sdk/foundations/envelopes/event-payloads/span/))

```json
{
  "spans": [
    {
      "span_id": "b01b9f6349558cd1",
      "parent_span_id": "a0cfbde2bdff3adc",
      "trace_id": "743ad8bbfdd84e99bc38b4729e2864de",
      "op": "http.client",
      "description": "GET https://api.example.com/users",
      "start_timestamp": 1700000000.100,
      "timestamp": 1700000000.350,
      "status": "ok",
      "origin": "auto.http.browser",
      "data": {
        "http.request.method": "GET",
        "url.full": "https://api.example.com/users",
        "server.address": "api.example.com",
        "http.response.status_code": 200
      }
    }
  ]
}
```

(For HTTP client spans the SDK also historically used `data:{url, method, status_code, type:"xhr"|"fetch"}` plus a `tags:{"http.status_code":"200"}`; the current SDK prefers the OTel-style attribute keys `http.request.method`, `url.full`, `http.response.status_code`.)

### `op` / `description` conventions the browser SDK emits

| Span | `op` | description |
|---|---|---|
| Page load transaction | `pageload` | route name (e.g. `/users/:id`) |
| Navigation (history change) | `navigation` | route name |
| Redirect navigation | `navigation.redirect` | URL |
| Outgoing fetch/XHR | `http.client` | `GET /path` (`{method} {url-without-query}`) |
| Resource timing | `resource.<initiatorType>` (`resource.script`, `resource.css`, `resource.img`, `resource.link`, `resource.other`) | resource URL |
| Navigation sub-timings | `browser.dns`, `browser.connect`, `browser.tls_ssl`, `browser.cache`, `browser.request`, `browser.response`, `browser.redirect`, `browser.unload_event`, `browser.dom_content_loaded_event`, `browser.load_event` | page URL |
| Paint | `browser.paint` | `first-paint` / `first-contentful-paint` |
| Long task / long animation frame | `ui.long_task` / `ui.long_animation_frame` | `Main UI thread blocked` |
| Interaction (INP) | `ui.interaction.click` (also `.press`/`.hover`/`.drag`) | selector string |

([browserTracingIntegration.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser/src/tracing/browserTracingIntegration.ts), [entries.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser-utils/src/performance/entries.ts), [Web Vitals Module](https://develop.sentry.dev/sdk/telemetry/traces/modules/web-vitals/))

### Web vitals — names and where they land

| Metric | Measurement key / location | unit |
|---|---|---|
| LCP | `measurements.lcp` on the pageload transaction (or a standalone `web_vital` span when span streaming is on) | `millisecond` |
| CLS | `measurements.cls` on the pageload transaction (or standalone span) | `""` (unitless) |
| INP | standalone interaction span with `measurements.inp` (streamed, always) | `millisecond` |
| FP | `measurements.fp` | `millisecond` |
| FCP | `measurements.fcp` | `millisecond` |
| TTFB | `measurements.ttfb` | `millisecond` |
| TTFB request time | `measurements.ttfb.requestTime` | `millisecond` |
| FID | `measurements.fid` (**legacy**, removed in newer SDKs in favor of INP) | `millisecond` |

So the answer to "which arrive and in what names": `lcp`, `cls`, `fp`, `fcp`, `ttfb`, `ttfb.requestTime` as transaction `measurements`, and `inp` as a measurement on a dedicated `ui.interaction.*` span. `fid` was the pre-INP first-input metric. ([web-vitals tracking.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser-utils/src/web-vitals/tracking.ts), [webVitals.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser/src/integrations/webVitals.ts), [Web Vitals Module](https://develop.sentry.dev/sdk/telemetry/traces/modules/web-vitals/))

### How `performance` data reaches the server

- **Web vitals** are read via `PerformanceObserver` handlers (`LargestContentfulPaint`, `LayoutShift`, `paint`, `first-input`/INP, `navigation`) and written onto the pageload span as measurements/attributes when it ends.
- **Navigation + resource timing** come from buffered `performance.getEntries()` (`navigation` and `resource` entry types), converted into the child spans above. `fetch`/`xhr` initiators are skipped (already covered by `http.client` spans). Long tasks/long-animation-frames/interactions come from live `PerformanceObserver`s. ([entries.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser-utils/src/performance/entries.ts))

The browser SDK does not ship raw `PerformanceEntry` objects; it always converts them to Sentry spans/measurements before sending.

---

## 3. Sessions

### Envelope item

The browser SDK sends **individual** session updates as `{ "type": "session" }` items (the `sessions` aggregate item is for high-volume server-mode SDKs). Envelope header for sessions: `{ "sent_at": …, "sdk": {…} }`. ([envelope.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/envelope.ts), [Sessions spec](https://develop.sentry.dev/sdk/telemetry/sessions/))

### Session payload (init vs update)

```json
// init: true on the first envelope for a session
{ "sid": "7c7b6585-f901-4351-bf8d-02711b721929", "init": true,  "started": "2023-01-01T00:00:00.000Z", "timestamp": "2023-01-01T00:00:00.000Z", "status": "ok", "errors": 0, "attrs": { "release": "app@1.0.0", "environment": "production" } }

// subsequent update (init: false), e.g. after an unhandled error
{ "sid": "7c7b6585-f901-4351-bf8d-02711b721929", "init": false, "started": "2023-01-01T00:00:00.000Z", "timestamp": "2023-01-01T00:02:10.000Z", "status": "crashed", "errors": 1, "duration": 130, "attrs": { "release": "app@1.0.0", "environment": "production" } }
```

Fields: `sid` (uuid4), `init`, `started`/`timestamp` (ISO 8601), `status`, `errors`, `duration` (seconds), `did` (distinct user id), and `attrs` = `{release, environment, ip_address, user_agent}`. ([session.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/session.ts), [Sessions spec](https://develop.sentry.dev/sdk/telemetry/sessions/))

Status lifecycle: `init → ok → exited | crashed | abnormal | unhandled`. Terminal states are `exited`, `crashed`, `abnormal`, `unhandled`. The JS SDK sets `crashed` on an unhandled error by default. Only `status`, `duration`, and `errors` may change between updates; `sid`/`did`/`started`/`attrs` are immutable. The server treats sessions as **additive**: each update carries the full state and the most-recent update is authoritative.

### Cadence (browser specifics)

- A session starts at init and the **first (init) envelope is deferred** until the browser is idle or the page is hidden (`whenIdleOrHidden`), so it doesn't hurt LCP.
- A session update is sent on the **first error** (errors: 0→1), and again when an **unhandled** error transitions status to `crashed`. (See `_updateSessionFromEvent` in [client.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/client.ts).)
- `captureSession` flips `init` to `false` after the first send, so later envelopes are updates.
- Default lifecycle is **`page`**: one session per page load (`ignoreDuration: true` — browser session duration is discarded). `lifecycle: 'route'` starts a new session per navigation. ([browsersession.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser/src/integrations/browsersession.ts))
- Server-side TTL: sessions are only updatable for 5 days; a session with no second event is marked "good" after that. ([Sessions spec](https://develop.sentry.dev/sdk/telemetry/sessions/))

Fields that matter server-side: `sid` (dedupe key), `init` (skip dedup), `release`/`environment` (bucketing), `status`/`errors` (crash-free rate), `started`/`timestamp` (bucketing/duration), `did` (affected users).

---

## 4. Propagation (`sentry-trace`, `baggage`, `traceparent`)

The browser SDK instruments outgoing `fetch` and `XHR` and adds headers (only to URLs matching `tracePropagationTargets`; default excludes most third parties and the Sentry ingest host itself):

- **`sentry-trace`**: `<traceId>-<spanId>[-sampled]` — 32-hex trace id, 16-hex span id, optional `0`/`1`; a missing flag means "defer". (The SDK never sends a bare sampling decision without ids.)
- **`baggage`**: comma-separated `key=value` list; Sentry keys are DSC fields prefixed `sentry-`: `sentry-trace_id`, `sentry-public_key`, `sentry-sample_rate`, `sentry-sampled`, `sentry-release`, `sentry-environment`, `sentry-transaction`, `sentry-org_id`. ([baggage.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/utils/baggage.ts), [dynamicSamplingContext.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/tracing/dynamicSamplingContext.ts))
- **`traceparent`** (W3C): sent **only** when `propagateTraceparent: true` (for OTel interop). ([Trace Propagation spec](https://develop.sentry.dev/sdk/foundations/trace-propagation/))

Even with **no tracing config** (`tracesSampleRate` unset), the SDK runs "Tracing without Performance" (TwP): it still continues incoming traces, attaches `contexts.trace` to error events, populates the envelope `trace` (DSC) header, and propagates headers. ([Trace Propagation spec](https://develop.sentry.dev/sdk/foundations/trace-propagation/))

For back-end correlation the server must accept and forward the envelope header `trace` (the dynamic sampling context: `trace_id`, `public_key`, `release`, `environment`, `sampled`, `sample_rate`, `transaction`, …), and ideally parse `baggage`/`sentry-trace` on requests it serves. A compatible ingestion server needs to **store and not drop** the `trace` envelope header, since it links transactions, spans, and errors into one trace.

---

## 5. Failure / retry behavior (what a server must not break)

Default transport is `fetch` with an **in-memory** promise buffer (limit **40** in-flight requests in the browser transport; core default 64; `makePromiseBuffer` default 100). There is **no persistent offline store by default**. ([fetch.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser/src/transports/fetch.ts), [base.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/transports/base.ts))

Behavior by response/network condition ([base.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/transports/base.ts), [ratelimit.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/utils/ratelimit.ts)):

- **429**: honors `X-Sentry-Rate-Limits` and `Retry-After`. `X-Sentry-Rate-Limits` format: `retry_after:categories:scope:reason[:namespaces]` (categories are data categories like `error`, `transaction`, `session`; multiple comma-separated). On 429 with no headers → 60 s backoff for all categories. **Rate-limited items are dropped client-side** (recorded as a `ratelimit_backoff` outcome), not re-sent. A compatible server should therefore emit precise `X-Sentry-Rate-Limits`/`Retry-After` rather than relying on the SDK to stop sending.
- **413** (payload too large): envelope dropped, recorded as `send_error` outcome.
- **Network failure**: dropped, recorded as `network_error` outcome (no automatic retry in the default transport).
- **Other 4xx/5xx**: treated as delivered — logged in debug builds, **no retry, no throw**, no outcome recorded. (This means a compatible server should not use `5xx` as a "please retry later" signal; the default browser SDK will not retry.)
- **Buffer full**: envelope dropped, recorded as `queue_overflow` outcome.

### Optional offline transport

`makeBrowserOfflineTransport` (opt-in) wraps fetch and stores failed envelopes in **IndexedDB** (`db "sentry-offline"`, store `"queue"`, max **30** envelopes), flushing on the `online` event and on a backoff timer (5 s start, doubles to 1 h cap, resets on success). It does **not** queue on `>=400` responses (except when `Retry-After`/`X-Sentry-Rate-Limits` are present), and it never queues `client_report` envelopes. ([browser offline.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/browser/src/transports/offline.ts), [core offline.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/core/src/transports/offline.ts))

### Client reports (outcomes)

When events are dropped, the SDK batches a `client_report` envelope item with the discard reason and count:

```json
{ "type": "client_report" }
{ "timestamp": "2023-01-01T00:00:00.000Z", "discarded_events": [ { "reason": "queue_overflow", "category": "error", "quantity": 23 } ] }
```

Discard reasons include `queue_overflow`, `ratelimit_backoff`, `network_error`, `send_error`, `before_send`, `sample_rate`, `event_processor`, `buffer_overflow`, `backpressure`, `no_parent_span`, `invalid`, `ignored`. ([Client Reports spec](https://develop.sentry.dev/sdk/telemetry/client-reports/))

**Server implications (must not break):** accept and correctly answer 429 with rate-limit headers (so the SDK backs off without the server having to reject a flood); accept `client_report` items; do not return 5xx expecting the browser to retry; honor the `sent_at`/`event_id` envelope headers; and be tolerant of bursts up to the in-flight buffer size.

---

## 6. Build tooling (source-map upload)

Source maps are **not** sent by the browser SDK at runtime; they are uploaded at build time by the bundler plugins ([`@sentry/vite-plugin`](https://github.com/getsentry/sentry-javascript/tree/develop/packages/bundler-plugins), `@sentry/webpack-plugin`, `@sentry/rollup-plugin`, `@sentry/esbuild-plugin` — all in the `bundler-plugins` package) or by [`sentry-cli`](https://github.com/getsentry/sentry-cli). ([Source Maps doc](https://docs.sentry.io/platforms/javascript/sourcemaps/))

What the plugins do:

1. **Inject a Debug ID** into each bundle (`//# debugId=<uuid>` and a runtime global `window._sentryDebugIds`). This is the default association method.
2. **Upload** source maps + minified bundles. Both legacy (`release.uploadLegacySourcemaps`) and Debug ID uploads are delegated to **sentry-cli** (`cliInstance.releases.uploadSourceMaps(...)`). ([build-plugin-manager.ts](https://github.com/getsentry/sentry-javascript/blob/develop/packages/bundler-plugins/src/core/build-plugin-manager.ts))

Endpoints the plugins assume (via sentry-cli / the Release & Artifact API):

| Operation | Endpoint |
|---|---|
| Create release | `POST /api/0/organizations/{org}/releases/` |
| Finalize release | `PUT /api/0/organizations/{org}/releases/{version}/` |
| Upload source maps / artifacts | `POST /api/0/organizations/{org}/releases/{version}/files/` (multipart) |
| Debug-ID artifact-bundle upload | `POST /api/0/organizations/{org}/chunk-upload/` (and per-chunk `PUT`/`GET`) |
| Associate commits | `POST /api/0/organizations/{org}/releases/{version}/commits/` |
| Register deploy | `POST /api/0/organizations/{org}/releases/{version}/deploys/` |

([Create a New Release for an Organization](https://docs.sentry.dev/api/releases/create-a-new-release-for-an-organization/), [chunk.py](https://github.com/getsentry/sentry/blob/master/src/sentry/api/endpoints/chunk.py))

Config required by the plugins: `authToken`, `org`, `project`, `url` (self-hosted Sentry base URL), and `release` (`name`, `dist`, `uploadLegacySourcemaps`). A self-hosted BugHan would need to implement the Release/Artifact upload surface (`/api/0/organizations/{org}/releases/…` and `/chunk-upload/`) for source-map uploads to work, in addition to the `/api/{project_id}/envelope/` ingestion endpoint.

---

## Minimum surface a compatible server must accept

1. `POST /api/{project_id}/envelope/` with query-string auth (`sentry_version`, `sentry_key`, `sentry_client`) and content types `application/x-sentry-envelope` / `text/plain`.
2. Envelope items: `event`, `transaction`, `session`, `client_report`, `attachment` (and `span` for streamed span v2).
3. Envelope headers: `event_id`, `sent_at`, `sdk`, `trace` (DSC), and `dsn` when tunneling.
4. Correct 429 handling with `X-Sentry-Rate-Limits`/`Retry-After`, and non-5xx acceptance semantics.
5. For full parity: the Release/Artifact + chunk-upload endpoints for source-map upload, and optionally `/api/{project_id}/store/` for legacy clients.
