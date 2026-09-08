# BugHan

BugHan is a self-hosted, Sentry-compatible error and performance tracking
platform in the Sentry/GlitchTip class — a single Go binary + Postgres that
ingests envelopes from **unmodified `@sentry/browser` (v8–v10)**, groups
errors into issues, and serves a server-rendered UI for issues, performance,
releases, and settings, with thin alerting and source-map symbolication.

No custom protocol, no custom SDK: point the official Sentry SDK at your
BugHan install and it just works.

- Product design: [`DESIGN.md`](./DESIGN.md) · glossary: [`CONTEXT.md`](./CONTEXT.md)
- Ops runbook (compose, env reference, sizing, backup/restore, upgrades):
  [`docs/self-host.md`](./docs/self-host.md)
- REST API reference: [`docs/api.md`](./docs/api.md)
- SDK compatibility + smoke tests: [`docs/sdk-smoke.md`](./docs/sdk-smoke.md)

## Quickstart (self-host in 5 minutes)

Prerequisites: Docker + Docker Compose.

```bash
cp .env.example .env
# put a long random value in BUGHAN_SECRET_KEY, e.g. openssl rand -hex 32
docker compose up -d --build
```

Open http://localhost:8000 — the first-run setup creates your user (owner)
and your first organization. Then **Projects → New project** gives you a DSN.

Health: `GET /api/health/` answers `{"status":"ok","version":"…","time":"…"}`.
Upgrade: pull the new tag and `docker compose up -d` (migrations run on
boot, forward-only, under lock). Details, sizing, and the backup runbook are
in [`docs/self-host.md`](./docs/self-host.md).

Without Docker: `go run ./cmd/bughan serve` against a local Postgres 16
(`DATABASE_URL=postgres://bughan:bughan@localhost:5432/bughan`); `bughan
migrate` applies migrations without serving. `bughan reset-password <email>`
recovers a locked-out account.

## SDK onboarding

```bash
npm install @sentry/browser
```

```js
import * as Sentry from "@sentry/browser";

Sentry.init({
  dsn: "http://<ingest-key>@localhost:8000/<project-id>", // from Projects → your project
  tracesSampleRate: 1.0, // performance + web vitals
});
```

Throw an error, load a page, and the event lands in **Issues** (errors),
**Performance** (transactions, traces, web vitals), and **Releases**
(session health). Supported: `@sentry/browser` v8, v9, v10; v7 is
accepted-but-unsupported. See [`docs/sdk-smoke.md`](./docs/sdk-smoke.md) for
the support matrix and how to verify with the Vite/webpack hello-world apps.

## Source maps

Upload with **sentry-cli 2.x** (3.x removed the legacy release-files path
BugHan implements — pin 2.x):

```bash
export SENTRY_URL=http://localhost:8000
export SENTRY_AUTH_TOKEN=<API token from User settings>  # write scope
export SENTRY_ORG=<org slug> SENTRY_PROJECT=<project slug>

sentry-cli releases new -p "$SENTRY_PROJECT" 'my-app@1.0.0'
sentry-cli releases files 'my-app@1.0.0' upload-sourcemaps ./dist --url-prefix '~/assets'
sentry-cli releases finalize 'my-app@1.0.0'
```

BugHan symbolicates at view time and caches the result on the event; frames
that can't be matched render minified with a "no source map found" hint
naming the release and file.

## UI tour

- **Issues** — grouped errors with sparklines; filter by status, level,
  environment, release, tags, and free text; saved views; bulk triage
  (resolve / ignore / assign).

  ![Issues list](docs/screenshots/issues.png)

- **Issue detail** — stack trace, breadcrumbs, tags/context, linked user
  feedback, activity feed, and a "View trace" jump when the event carries
  trace context.

  ![Issue detail](docs/screenshots/issue-detail.png)

- **Performance** — transaction summary with p50/p95/p99 and recently-slow
  flags, web-vitals strip, transaction detail (histogram, recent events),
  and the trace waterfall with error ticks and a span drawer.

  ![Performance](docs/screenshots/performance.png)
  ![Trace waterfall](docs/screenshots/trace.png)

- **Releases** — adoption and crash-free session/user rates, issues
  first-seen per release, artifact list with upload.

  ![Releases](docs/screenshots/releases.png)
- **Feedback** — read-only list of `feedback` / `user_report` envelopes
  linked to their issues.
- **Alerts** (project settings) — rules on new issue / regression / event
  count with environment/release/level filters, delivered by email (SMTP)
  and/or HMAC-signed webhook, with a delivery log and send-test.
- **Settings** — organization, projects (DSNs, ingest keys, data wipe),
  user profile, API tokens.

## Configuration

Everything is environment (see [`.env.example`](./.env.example) and the full
table in [`docs/self-host.md`](./docs/self-host.md)):

| Variable | Default | Notes |
|---|---|---|
| `BUGHAN_SECRET_KEY` | (dev fallback, warns) | **Required in production** — session/cookie signing |
| `DATABASE_URL` | `postgres://bughan:bughan@localhost:5432/bughan` | All state lives in Postgres |
| `BUGHAN_URL` | `http://localhost:8000` | Public base URL — builds DSNs and email links |
| `BUGHAN_PORT` | `8000` | Host port (compose) |
| `BUGHAN_SINGLE_ORG` | `false` | Lock the install to one organization |
| `BUGHAN_WORKER_EMBEDDED` | `true` | Alerts + retention + rollups in-process |
| `BUGHAN_RATE_LIMIT_PER_MIN` | `0` (unlimited) | Per-project ingest limit; throttled SDKs get 429 |
| `BUGHAN_RETENTION_*_DAYS` | 90 / 30 / 30 / 90 / 365 | Events / transactions / sessions / feedback / releases |
| `BUGHAN_MAX_EVENT_MB` | `20` | Decompressed envelope cap |
| `BUGHAN_SMTP_*` | (unset) | Enables invites, verification, alert emails |
| `BUGHAN_VERSION` | build tag | Overrides the version reported by `/api/health/` |

## API

The `/api/0/` REST API (Bearer API tokens, `read`/`write` scopes) covers
tenancy, issues + triage, performance, releases + source-map files, alerts,
and a per-project data wipe; SDKs post envelopes to
`POST /api/{project}/envelope/`. Full reference:
[`docs/api.md`](./docs/api.md).

## Status

v0.1 implementation is complete per the
[implementation map](https://github.com/Atash03/BugHan/issues/34): ingest →
grouping → issues API → transactions/sessions → releases/source maps → web
UI → alerting → feedback → this deployment/docs/smoke slice.

Out of scope for v0.1 (see DESIGN.md): replay, profiling, uptime, cron
check-ins, non-browser SDKs, SSO/2FA, multi-instance/HA, UI i18n,
chunk-upload artifact bundles (sentry-cli 3.x path).

## License

MIT (see DESIGN.md §15).
