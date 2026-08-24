# Storage Engine Choice — Research (Ticket #18)

> Status: research findings + recommendation. This is **not** a final design
> commitment — it records the facts and the recommended direction, and the
> trade-offs it deliberately leaves open.

## 1. Scope and target

BugHan is a self-hosted, Sentry-compatible error + performance tracker. The
storage engine must serve, on a **single node** running Docker Compose:

| Dimension | Value |
|---|---|
| Event volume | ≤ **1M events/day** across ~10s of projects |
| Event kinds | error events **and** transactions/spans |
| Retention | ~30 days (so up to ~30M events resident) |
| Query shape 1 | **Issue list** — group events by issue + filter by time window |
| Query shape 2 | **Time-bucket aggregates** for dashboards |
| Query shape 3 | **Trace-by-id span lookups** |

The three candidate designs under comparison:

1. **Postgres alone** — events as `JSONB` + generated columns/indexes; separate
   tables for issues and pre-computed aggregates.
2. **ClickHouse single node** — the industry-standard event/column store for this
   data shape.
3. **Hybrid** — Postgres for org/issue/aggregate state, ClickHouse for raw
   events and spans.

## 2. What the reference systems actually use

This is the single most informative input: two mature, Sentry-compatible systems
have already answered this question at different scales.

### 2.1 GlitchTip — Postgres only

GlitchTip is the closest architectural analog to BugHan (Django-based,
Sentry-SDK-compatible, self-hosted, single-box). Its documented stack is:

- **PostgreSQL (14+) is the only required datastore.** A single app service (or
  web + worker split) is sufficient; **Valkey/Redis 7+ is optional** and can be
  disabled entirely, in which case Postgres also provides the task queue, cache,
  and session storage. There is **no ClickHouse and no Kafka**.
- Events, issues, transactions, and aggregates all live in Postgres; search uses
  **PostgreSQL full-text search**; ingest-time grouping/rollups and partition
  maintenance run in the worker.
- Disk guide from upstream: **"a 1 million event per month instance may require
  30 GB of disk"** — a useful order-of-magnitude anchor for Postgres footprint.
- Migrations run **automatically on container startup** (`./manage.py migrate`).
- Retention is configurable (`GLITCHTIP_RETENTION_DAYS`, default 90), and — this
  is telling — GlitchTip recently added a **hot/cold split**: recent events stay
  in Postgres ("hot", default 30 days) while older data is **archived to Parquet
  on S3/local disk and queried via DuckDB**. That feature exists precisely
  because Postgres gets expensive to grow for long retention.

Sources: [GlitchTip install docs](https://glitchtip.com/documentation/install),
[glitchtip-backend (GitLab)](https://gitlab.com/glitchtip/glitchtip-backend).

### 2.2 Sentry self-hosted — Postgres + ClickHouse (+ Kafka/Redis)

Sentry self-hosted's documented data flow is a full hybrid pipeline:

- **Postgres** holds org/project/issue ("group") state and is the default
  **nodestore** (raw event payload blob store) backend; on self-hosted the
  nodestore front is often SeaweedFS.
- **ClickHouse** (queried through **Snuba**) is the analytics/event store — error
  and transaction events are written there and all issue-list, aggregate, and
  trace queries run against it.
- **Kafka** buffers ingestion between Relay → Sentry ingest consumers → Snuba
  consumers → ClickHouse; **Redis** holds project config/cache; **Memcached**
  caches; **Symbolicator** resolves stack traces.
- Ingestion: Relay validates DSN/project → Kafka → ingest consumers (symbolicate,
  save payload to nodestore) → republish to `events` topic → `snuba-consumer`
  writes to ClickHouse → post-process-forwarder computes issue groupings.

So **Sentry = hybrid** (Postgres for state, ClickHouse for events/spans), with
several extra services that exist for Sentry-scale ingestion, not because the
storage model requires them.

Sources: [Sentry self-hosted data flow](https://develop.sentry.dev/self-hosted/data-flow/),
[Snuba troubleshooting](https://develop.sentry.dev/self-hosted/troubleshooting/snuba/),
[reference architecture: simple single node](https://develop.sentry.dev/self-hosted/reference-architecture/simple-single-box/).

## 3. Option-by-option analysis

### 3.1 Postgres alone

**Model.** Store each event's full payload in a `JSONB` column; extract the
fields you query (`project_id`, `group_id`/fingerprint, `level`, `environment`,
`release`, `timestamp`, `trace_id`) into **generated columns** with B-tree
indexes, or expression indexes on `(payload->>'...')`. Keep a transactional
`issue` table (fingerprint → metadata) and a pre-computed aggregate/rollup table
maintained at ingest.

- **Issue list** — fast *if* you pre-aggregate into an `issue` + `issue_events`
  rollup table (GROUP BY on an indexed `group_id` + time range). Slow if the
  query has to scan raw JSONB events.
- **Time-bucket aggregates** — `date_trunc('hour', ts) ... GROUP BY` over raw
  events is a sequential scan; realistically you maintain a rollup table (at
  ingest or via cron) and read that.
- **Trace-by-id lookups** — a generated `trace_id UUID` column + B-tree index is
  a fine point lookup at moderate size; the spans table is the largest table and
  the most likely to stress a single Postgres.

**Write throughput vs flexibility.** JSONB gives total schema flexibility (any
Sentry SDK payload shape) — that's its real appeal. The cost: every new field you
need to filter on is a new generated column/expression index, i.e. a real
migration. Write throughput is adequate at the low end but JSONB + several
indexes + periodic deletes (30-day TTL) causes write amplification and table
bloat that **autovacuum** must keep up with. Generated columns measurably help
read latency (one vendor measured an ~80% P99 cut using them vs. raw
`->>` scans), but they don't remove the index-bloat/delete problem.

**Storage footprint.** The pain point at this scale. Upstream's rough guide of
~30 GB per 1M events/month implies **hundreds of GB** for 30M resident events —
even with a more optimistic 2–5 KB/row (payload + indexes), 30M events lands in
the **~100–300 GB** range, and grows linearly with payload size. This is the
single biggest reason Postgres-alone is questionable at the 1M/day ceiling.

**Ops burden.** Lowest: one container, the most battle-tested single-node
database in existence, framework migrations (`Django`/`Alembic`) are routine,
retention is a simple `DELETE`/partition-drop job. No compaction engine to
operate (VACUUM is automatic). Backup = `pg_dump`/`pg_basebackup`/WAL, with PITR.

**Backup/restore.** Gold standard. Logical or physical dumps, point-in-time
recovery, trivially understood single-node restore.

**Self-host docs maturity.** Highest possible; decades of docs and community.

**Verdict.** Simplest and proven (GlitchTip) — but the storage/query curve bends
up sharply toward the 1M-events/day ceiling, which is why GlitchTip itself added
a DuckDB/Parquet cold tier.

Sources: [PostgreSQL JSON types](https://www.postgresql.org/docs/current/datatype-json.html),
[generated columns](https://www.postgresql.org/docs/current/ddl-generated-columns.html),
[generated columns + expression indexes for multi-tenant SaaS](https://dev.to/software_mvp-factory/postgresql-generated-columns-and-expression-indexes-for-multi-tenant-saas-58l1),
[generated columns cut P99 latency 80%](https://mvpfactory.io/blog/postgresql-generated-columns-and-expression-indexes-for-multi-tenant-saas).

### 3.2 ClickHouse single node

**Model.** MergeTree-family tables with an ordering key chosen for the dominant
lookup (e.g. `ORDER BY (project_id, timestamp, group_id)` for events,
`ORDER BY (project_id, trace_id, timestamp)` for spans). Semi-structured `JSON`/
`Map` types allow flexible payloads while key columns stay typed and indexed
sparsely. A **TTL clause** drops old data automatically; background merges handle
compaction.

- **Issue list** — `GROUP BY group_id` over a filtered time window is a fast
  column scan; or use an `AggregatingMergeTree`/`SummingMergeTree` materialized
  view to keep a live per-issue rollup.
- **Time-bucket aggregates** — `toStartOfHour(ts)` + `GROUP BY` is ClickHouse's
  home turf; raw-scans are cheap because only the relevant columns are read.
- **Trace-by-id lookups** — an ordering key starting with `trace_id` makes this
  an index seek over sorted data; spans map naturally to ClickHouse.

**Write throughput vs flexibility.** Write throughput is the strength — async
batched inserts comfortably absorb hundreds of thousands of rows/sec on one box;
1M events/day is trivial. Flexibility is *good but different*: payloads can stay
semi-structured, but which columns you aggregate/filter on is effectively fixed
by the table schema + ordering key, so you design the schema up front rather
than adding indexes lazily (adding a column is a light `ALTER ... ADD COLUMN`,
but re-ordering a key is a rewrite).

**Storage footprint.** The strongest win: columnar layout + LZ4/ZSTD compression
shrinks repetitive event data dramatically (often 5–20× vs. row/JSONB storage).
30M events that cost ~100–300 GB in Postgres typically land in **~10s of GB**
here. ClickHouse's own JSON benchmark places it far ahead of Postgres JSONB on
both size and query speed for document-shaped analytics data.

**Ops burden.** Still a single container with an official image, but a *different*
ops vocabulary: schema is managed via `.sql`/migration tooling (less standardized
than an ORM migration), compaction is automatic background merges (little to
tune at this scale), and retention is declarative **TTL** rather than a cron job.
Backup/restore and upgrades are well documented but are a second skill to learn.

**Backup/restore.** Mature but distinct: `BACKUP`/`RESTORE` commands (full or
incremental, sync/async, compressed, to local disk/S3/Azure), plus the popular
`clickhouse-backup` tool (`ALTER TABLE ... FREEZE` + copy parts). Restore =
re-attach parts; requires practicing the procedure (atomicity across parts and
ReplicatedMergeTree metadata are the usual gotchas).

**Self-host docs maturity.** Strong official docs and a first-party Docker image;
single-node deployment is a first-class, well-documented path, though the
operator community is smaller than Postgres's.

**Verdict.** Best-in-class for the event/span query shapes and footprint, but it
is *not* a general-purpose transactional store — you still need something else
for org/team/issue state with strong consistency (across-tenancy FKs, unique
fingerprints, counts).

Sources: [ClickHouse TTL](https://clickhouse.com/docs/guides/developer/ttl),
[backup & restore](https://clickhouse.com/docs/operations/backup/overview),
[compression](https://clickhouse.com/resources/engineering/database-compression),
[JSON benchmark vs Postgres](https://clickhouse.com/blog/json-bench-clickhouse-vs-mongodb-elasticsearch-duckdb-postgresql),
[choosing a primary key](https://clickhouse.com/docs/best-practices/sparse-primary-indexes).

### 3.3 Hybrid (Postgres + ClickHouse single node)

**Model.** Postgres = source of truth for orgs/teams/projects/members, issue
groups (fingerprint → group), and lightweight per-issue/aggregate rollups.
ClickHouse = raw error events + transactions/spans with a 30-day TTL. Ingest
writes the transactional bits to Postgres and the raw event/spans to ClickHouse
(batched); issue grouping is computed at ingest (Sentry's post-process pattern)
and its result lands in Postgres.

- **Issue list** — reads the Postgres issue/rollup tables (fast, transactional);
  ClickHouse serves the raw event detail + time-windowed counts.
- **Time-bucket aggregates** — ClickHouse.
- **Trace-by-id** — ClickHouse spans table.

**Write throughput vs flexibility.** Gets ClickHouse's write headroom and
Postgres's transactional guarantees; schema flexibility is the same as the sum
of parts (semi-structured payloads in ClickHouse, rigid relational state in
Postgres). The cost is that **you now write to two stores per event** — with the
attendant consistency question (see trade-offs).

**Storage footprint.** Best: raw events/spans compressed in ClickHouse (~10s of
GB), Postgres stays small because it holds only state + rollups.

**Ops burden.** Highest of the three: two containers, two migration systems, two
backup/restore procedures, and a boundary to reason about (which store answers
which query). At 1M/day you do **not** need Sentry's Kafka/Snuba/Redis/memcached
ceremony — a bounded in-process batch buffer or a tiny queue is enough — but the
two-store boundary itself is irreducible.

**Backup/restore.** Two procedures + a consistency checkpoint between them
(issue state in Postgres vs. raw events in ClickHouse). Backing up both is
straightforward; a perfectly consistent cross-store restore is the subtle part.

**Self-host docs maturity.** No single "hybrid" doc — you compose Postgres and
ClickHouse docs; Sentry self-hosted's docs are the closest worked example.

**Verdict.** The industry-standard shape (Sentry), best query/footprint profile,
most moving parts. The sensible target *if and when* volume and retention justify
it.

## 4. Comparison matrix

| Criterion | Postgres alone | ClickHouse single node | Hybrid (PG + CH) |
|---|---|---|---|
| Issue list (group + time) | Good *with* rollup tables; weak on raw JSONB | Strong (GROUP BY / materialized rollups) | Strong (PG rollups + CH raw counts) |
| Time-bucket aggregates | Needs rollup table/cron | Native, strongest | Native (CH) |
| Trace-by-id spans | OK index lookup; biggest table | Index seek on `trace_id` key | Index seek (CH) |
| Write throughput | Fine at low end; bloat risk at ceiling | Excellent (async batched) | Excellent |
| Schema flexibility | Highest (JSONB) | Good (typed + semi-structured) | Highest overall |
| Storage @30M events | ~100–300 GB (hundreds of GB) | ~10s of GB | ~10s of GB (events) + small PG |
| Ops burden (1 box) | Lowest | Low-medium (new vocabulary) | Highest (2 stores) |
| Migrations | Framework-standard | `.sql`/tooling, less standardized | Two systems |
| Retention/compaction | DELETE job + autovacuum | Declarative TTL + auto merges | TTL (CH) + small PG deletes |
| Backup/restore | Gold standard (PITR) | Good (BACKUP/clickhouse-backup) | Two + consistency checkpoint |
| Self-host docs maturity | Highest | Strong | Composed; Sentry as example |
| Used by | GlitchTip | (standalone event stores) | Sentry self-hosted |

## 5. Recommendation

**Recommend the hybrid (Postgres + ClickHouse single node) as the target
architecture for the stated ≤1M events/day ceiling** — Postgres for
org/issue/aggregate state, ClickHouse for raw error events + transactions/spans
with a 30-day TTL. This is exactly the split Sentry self-hosted converged on, and
it is the only one of the three that stays comfortably within a single node while
handling all three query shapes at 30-day × 1M/day retention.

**But sequence it:** ship the error-tracking core **first on Postgres-only**, the
way GlitchTip does, behind a small storage interface, and add ClickHouse when
transactions/spans and retention pressure actually arrive. That keeps v0.1 to a
single database (lowest ops risk) while not locking the design out of the hybrid
end-state. If real traffic settles at the map's "tens of thousands of events/day"
rather than 1M/day, Postgres-only may remain correct for a long time.

The decisive inputs behind this: (a) GlitchTip proves Postgres-only works but
shows the footprint pain (~30 GB/1M events/month, hence its Parquet+DuckDB cold
tier), and (b) Sentry proves the hybrid is the shape that survives at higher
volume — at 1M/day BugHan sits an order of magnitude above GlitchTip's comfort
zone, so the hybrid is warranted at the ceiling but optional at the floor.

## 6. Trade-offs left open (not decided here)

1. **Sequencing threshold.** The exact volume/retention trigger at which to
   introduce ClickHouse (the map says "tens of thousands/day", the ticket says
   ≤1M/day — the answer changes which starting point is right).
2. **Cross-store consistency.** One transaction cannot span Postgres and
   ClickHouse; the retry/idempotency and "which store wins if one write fails"
   semantics are an open design question.
3. **Whether a queue/buffer is needed.** Sentry uses Kafka; BugHan at 1M/day
   likely doesn't need it — an in-process batched writer may suffice — but the
   cut-over point is unset.
4. **ClickHouse schema/ordering keys.** `ORDER BY` choices for the events vs
   spans tables (e.g. `(project_id, timestamp)` vs `(project_id, trace_id)`)
   are left to the storage/query design ticket.
5. **Grouping/fingerprint source of truth.** Whether issue-group metadata and
   per-issue counts live only in Postgres or are also mirrored as ClickHouse
   materialized views.
6. **Backup strategy.** Whether BugHan documents one combined backup runbook or
   accepts a loose cross-store restore guarantee at this scale.

## 7. References

- GlitchTip install docs (system requirements, retention, hot/cold DuckDB) — <https://glitchtip.com/documentation/install>
- GlitchTip backend source — <https://gitlab.com/glitchtip/glitchtip-backend>
- Sentry self-hosted data flow — <https://develop.sentry.dev/self-hosted/data-flow/>
- Sentry self-hosted Snuba troubleshooting — <https://develop.sentry.dev/self-hosted/troubleshooting/snuba/>
- Sentry self-hosted reference architecture (simple single node) — <https://develop.sentry.dev/self-hosted/reference-architecture/simple-single-box/>
- PostgreSQL JSON types — <https://www.postgresql.org/docs/current/datatype-json.html>
- PostgreSQL generated columns — <https://www.postgresql.org/docs/current/ddl-generated-columns.html>
- PostgreSQL generated columns + expression indexes for multi-tenant SaaS — <https://dev.to/software_mvp-factory/postgresql-generated-columns-and-expression-indexes-for-multi-tenant-saas-58l1>
- PostgreSQL generated columns cut P99 latency 80% (MVP Factory) — <https://mvpfactory.io/blog/postgresql-generated-columns-and-expression-indexes-for-multi-tenant-saas>
- ClickHouse TTL — <https://clickhouse.com/docs/guides/developer/ttl>
- ClickHouse backup & restore — <https://clickhouse.com/docs/operations/backup/overview>
- ClickHouse compression — <https://clickhouse.com/resources/engineering/database-compression>
- ClickHouse JSON benchmark vs Postgres/DuckDB/others — <https://clickhouse.com/blog/json-bench-clickhouse-vs-mongodb-elasticsearch-duckdb-postgresql>
- ClickHouse choosing a primary key — <https://clickhouse.com/docs/best-practices/sparse-primary-indexes>
