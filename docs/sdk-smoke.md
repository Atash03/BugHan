# SDK compatibility & smoke tests

BugHan speaks the Sentry envelope protocol to official browser SDKs —
no custom SDK, no custom protocol in v1 (DESIGN.md §5).

## Support matrix

| SDK | Status |
|---|---|
| `@sentry/browser` v8, v9, v10 | **Validated** — see smoke tests below |
| `@sentry/browser` v7 | Accepted, unsupported (no smoke coverage; will likely work until the protocol moves) |
| v11+ | Added when stable, per the evolution rule |

**Evolution rule:** watch sentry-js release notes for envelope-protocol
changes; the smoke suite is the tripwire — a red smoke run against a new
SDK major is the trigger to bump the compat floor, never the other way
around.

Item types BugHan parses: `event` (errors), `transaction`, `session` +
`sessions` (aggregates), `feedback` + `user_report`, `client_report`
(counted, discarded). Unknown item types are tolerated, never rejected;
`event`, `transaction`, and `session` rows dedupe on `event_id` / `sid`, so
SDK retries are safe.

## Automated smoke (CI)

`TestSDKSmokeErrorTransactionSession` (`internal/httpapi/sdk_smoke_test.go`)
runs on every CI push: it posts sentry-js-v10-shaped error, transaction
(+ LCP/CLS vitals, child span, shared `trace_id`), and session envelopes
through the real `POST /api/{project}/envelope/` handler and asserts each
flow is visible via the REST API — issues list → issue events, performance
summary → transaction detail, trace view (transaction + span + error tick),
release health (crash-free rate), and a final `/api/health/` probe.

The CI `smoke` job additionally boots the real binary against Postgres and
probes `/api/health/` for `{"status":"ok"}` plus a stamped version.

## Manual smoke (real SDK, Vite + webpack)

Run this when touching ingest, grouping, or the compat floor. You need a
running BugHan (the quickstart in the README) with a project DSN.

`/tmp/bughan-smoke/vite-app` (repeat with `webpack-app` using your usual
webpack template — the assertions are identical):

```bash
npm create vite@latest vite-app -- --template vanilla
cd vite-app && npm install && npm install @sentry/browser
```

`main.js`:

```js
import * as Sentry from "@sentry/browser";

Sentry.init({
  dsn: "<your project DSN>",
  tracesSampleRate: 1.0,
});

document.querySelector("#app").innerHTML = `<button id="boom">boom</button>`;
document.querySelector("#boom").addEventListener("click", () => {
  throw new TypeError("smoke boom");
});
setInterval(() => { /* keep pageload transaction + heartbeat sessions flowing */ }, 1000);
```

```bash
npm run dev   # open the page, click "boom", wait ~30s
```

Then assert (replace slugs):

```bash
BASE=http://localhost:8000 ORG=<org> PROJ=<project>
AUTH="Authorization: Bearer <write-scope API token>"

# 1. the error grouped into an issue
curl -s -H "$AUTH" "$BASE/api/0/projects/$ORG/$PROJ/issues/?query=smoke+boom" | python3 -m json.tool
# 2. the pageload transaction in the summary
curl -s -H "$AUTH" "$BASE/api/0/projects/$ORG/$PROJ/performance/summary/" | python3 -m json.tool
# 3. release health shows the release with sessions
curl -s -H "$AUTH" "$BASE/api/0/projects/$ORG/$PROJ/releases-health/" | python3 -m json.tool
```

All three must show the smoke traffic; the issue page in the UI must show
the stack trace, and the trace view (follow "View trace") the transaction
with the error tick.
