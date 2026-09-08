# BugHan self-host runbook (v0.1)

Two containers, one binary, one volume. All state lives in Postgres.

```
┌────────────── compose ──────────────┐
│  db   postgres:16-alpine + pgdata   │
│  web  bughan (UI + API + ingest     │
│       + embedded worker; migrations │
│       on boot)                      │
└─────────────────────────────────────┘
```

## Install

```bash
cp .env.example .env
# set BUGHAN_SECRET_KEY to something long and random:
openssl rand -hex 32
docker compose up -d --build
```

Open `BUGHAN_URL` (default http://localhost:8000). The setup page creates
the first user as **owner** of the first organization; afterwards it
redirects to login. Create a project under **Projects** and copy its DSN
into your SDK (see [README](../README.md#sdk-onboarding)).

`GET /api/health/` is the liveness + DB-readiness probe (`200
{"status":"ok","version":"…","time":"…"}`; `503` while Postgres is
unreachable). Both the compose `db` healthcheck ordering and the image
`HEALTHCHECK` use it.

## Environment reference

| Variable | Default | Notes |
|---|---|---|
| `BUGHAN_SECRET_KEY` | dev fallback (stderr warning) | **Required in production.** Signs sessions/CSRF; rotating logs everyone out |
| `DATABASE_URL` | `postgres://bughan:bughan@localhost:5432/bughan` | Compose overrides to the `db` service |
| `BUGHAN_URL` | `http://localhost:8000` | Public base URL: DSN host, email links, webhook payload URLs. Set to your real origin behind a proxy |
| `BUGHAN_PORT` | `8000` | Host-side port mapping |
| `BUGHAN_BIND` | `:8000` | Listen address inside the container (leave alone) |
| `BUGHAN_VERSION` | image build arg | Overrides the version string in `/api/health/` |
| `BUGHAN_SINGLE_ORG` | `false` | `true` locks signup/creation to the first organization (strict self-host) |
| `BUGHAN_WORKER_EMBEDDED` | `true` | `false` disables background work (alert evaluation, retention sweep, rollup maintenance) in that process. There is no worker-only mode in v0.1 — leave `true` |
| `BUGHAN_RATE_LIMIT_PER_MIN` | `0` (unlimited) | Per-project envelope limit; excess gets `429` + `Retry-After` + `X-Sentry-Rate-Limits` |
| `BUGHAN_RETENTION_EVENT_DAYS` | `90` | Error events (issues survive) |
| `BUGHAN_RETENTION_TRANSACTION_DAYS` | `30` | Raw transactions (hourly rollups persist) |
| `BUGHAN_RETENTION_SESSION_DAYS` | `30` | Raw sessions (rollups keep 3× this) |
| `BUGHAN_RETENTION_FEEDBACK_DAYS` | `90` | User feedback |
| `BUGHAN_RETENTION_RELEASE_DAYS` | `365` | Release files after last event |
| `BUGHAN_MAX_EVENT_MB` | `20` | Decompressed envelope cap (oversize → `413`) |
| `BUGHAN_SMTP_HOST/PORT/USER/PASS/FROM` | unset | Unset = invites/verification/alert emails disabled; password recovery via `bughan reset-password` |

Release-file artifact caps are fixed in v0.1: 50 MB/file, 500 MB/release
(SHA-256 dedupe).

## Sizing

Single instance, tens of projects (DESIGN.md §14):

| Load | Shape | Disk |
|---|---|---|
| ≤ 50k events/day | 1 vCPU / 1 GB RAM | 10 GB |
| ≤ 200k events/day | 2 vCPU / 2 GB RAM | 50 GB |
| Approaching 1M events/day | 4 vCPU / 4 GB RAM | 200 GB+ |

Past that — sustained > 200k events/day or raw event storage > 100 GB —
is the documented trigger for the hybrid ClickHouse cut-over (not built in
v0.1). Multi-instance/HA and Kubernetes are unsupported: one `web`, one
`db`.

## Upgrade

```bash
docker compose pull   # or: docker compose build --build-arg VERSION=vX.Y.Z
docker compose up -d
```

Migrations are versioned SQL, forward-only, applied under lock on boot —
safe to re-run, never downgrade. Check `/api/health/` afterwards.

## Backup & restore

Postgres holds **all** state (tenancy, events, rollups, artifacts, jobs).
Nightly `pg_dump` is the backup story:

```bash
# backup (custom format, timestamped)
docker compose exec -T db pg_dump -U ${BUGHAN_DB_USER:-bughan} -Fc ${BUGHAN_DB_NAME:-bughan} > bughan-$(date +%F).dump

# restore into a fresh stack (stop web first so nothing writes mid-restore)
docker compose stop web
cat bughan-2026-09-08.dump | docker compose exec -T db pg_restore -U ${BUGHAN_DB_USER:-bughan} -d ${BUGHAN_DB_NAME:-bughan} --clean --if-exists
docker compose start web
```

Keep the `.dump` files off-host (object storage, restic, whatever you
already use) and rehearse the restore quarterly — an untested backup is a
rumor. The `pgdata` Docker volume is the only thing that must survive host
moves; everything else is rebuildable from the image + `.env`.

Also worth backing up: your `.env` — the `BUGHAN_SECRET_KEY` in particular.
Sessions and CSRF tokens are keyed by it, so restoring with a different
secret logs everyone out (data itself is unaffected).

## TLS / reverse proxy

No proxy ships in the box. Terminate TLS in front (Caddy/nginx/Traefik),
forward to `web:8000`, and set `BUGHAN_URL=https://bugs.example.com` so
DSNs, invite links, and webhook payload URLs come out right. Browser SDKs
need the ingest endpoint reachable with CORS — BugHan answers permissive
CORS on `/api/{project}/envelope/` itself, so a plain reverse proxy needs
no extra CORS config. Session cookies are `Secure` by default, which is
correct behind a TLS proxy (the browser sees https); for plain-HTTP access
use `BUGHAN_DEV_INSECURE_COOKIES=true`.

## Source-map uploads

Pin **sentry-cli 2.x** — 3.x removed the legacy release-files path BugHan
implements (`GET chunk-upload/` + artifact bundles are v0.2). Full command
sequence is in the [README](../README.md#source-maps).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `web` exits: `set BUGHAN_SECRET_KEY in .env` | Empty secret — fill it in |
| `/api/health/` 503 `degraded` | `db` not ready — `docker compose logs db`; data volume perms |
| SDK gets 403 on envelopes | Wrong/revoked ingest key, or project id mismatch in the DSN — re-copy from Projects |
| SDK gets 429 | `BUGHAN_RATE_LIMIT_PER_MIN` tripped — raise it or drop the knob (0) |
| SDK gets 413 | Envelope over `BUGHAN_MAX_EVENT_MB` decompressed — raise or trim SDK payload (fewer breadcrumbs) |
| Login loops / session won't stick | `BUGHAN_URL` scheme mismatch (http vs https) or `Secure` cookies over plain HTTP — set `BUGHAN_DEV_INSECURE_COOKIES=true` for local plain-HTTP dev |
| Locked out, no SMTP | `docker compose exec web bughan reset-password <email>` (or `BUGHAN_NEW_PASSWORD=…` non-interactively) |
| No alert emails | `BUGHAN_SMTP_*` unset — verification/invites/alert mail all gate on it |
