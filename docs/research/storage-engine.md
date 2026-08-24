# Storage engine choice

> Research ticket #18 — establishes the **facts** and ends with a **recommendation** (no final design commitment).
> Question: what is BugHan's primary event store at modest self-host scale?

## 1. Target scale and workload

| Dimension | Value |
|---|---|
| Topology | Single node, single-instance Docker Compose |
| Throughput | ≤ 1M events/day (~12 events/sec sustained, bursty to a few hundred/sec) |
| Tenancy | ~10s of projects |
| Event types | Error events + transactions/spans |
| Retention | ~30 days |
| Peak stored volume | ~30M events (plus spans) |

The three query shapes that must work well:

1. **Issue list** — group events by fingerprint, filter by project + time window, order by count/last-seen.
2. **Time-bucket aggregates** — dashboard rollups (counts, unique issues, error rates, p50/p95 duration) per hour/day.
3. **Trace-by-id span lookups** — fetch a trace tree by `trace_id` (error event → root transaction → child spans).

---

## 2. Candidate A — Postgres alone

**Model.** Keep raw event/spans in `event` tables as `JSONB`, plus:
- **Generated columns** (`GENERATED ALWAYS AS (payload->>'fingerprint') STORED`) with B-tree indexes for the hot filter/sort/group keys — the standard way to make JSONB "look like" typed columns to the planner. Expression/functional indexes (`payload->>'fingerprint'`) give the same effect without the stored-column cost; MVP Factory reports ~80% P99-latency cuts from this pattern ([generated columns & expression indexes for multi-tenant SaaS](https://mvpfactory.io/blog/postgresql-generated-columns-and-expression-indexes-for-multi-tenant-saas)).
- A `GIN` index over the JSONB payload for arbitrary tag/context lookups (`jsonb_path_ops`).
- **Separate aggregate/rollup tables** for issue state and time-bucket aggregates, maintained on ingest (or by a periodic rollup job), so dashboards never scan raw events.
- Table partitioning (or at least composite `(project_id, timestamp)` indexes) to keep the working set bounded.

**Query shapes.**

- *Issue list* — indexed generated columns on `project_id`, `fingerprint`, `timestamp` make this a plain indexed `GROUP BY`. Fine.
- *Time-bucket aggregates* — works only if aggregates are **precomputed** into rollup tables; `date_trunc(...)` over 30M JSONB rows is the failure mode. Acceptable, but it means you are designing your own mini-OLAP layer.
- *Trace-by-id span lookups* — an index on a generated `trace_id` column makes this a few index scans + one join. Fine at 30M rows.

**Write throughput vs schema flexibility.** Postgres comfortably ingests tens of thousands of rows/sec with batched/multi-row inserts and `unlogged`-free designs; 12/sec sustained (hundreds/sec burst) is far below the ceiling. JSONB gives maximal **schema flexibility** (Sentry's event schema is large, nested, and SDK-version-dependent), and you keep one transactional store for events *and* org/issue state. The cost is that everything is row-oriented and uncompressed-by-default (PGLZ/TOAST help, but nowhere near columnar).

**Storage footprint (estimate).** A typical Sentry-style error event JSON is ~2–5 KB (stacktraces, breadcrumbs, contexts). 30M events × ~3 KB ≈ **90 GB** of raw payload, plus indexes and TOAST overhead — realistically **60–150 GB**, and spans add on top. Single disk is fine, but JSONB is the fat option; retention beyond 30 days multiplies it directly.

**Ops burden (single instance).** Lowest of the three: one database, the app's own migration framework (`ALTER TABLE`), no compaction step, no separate TTL system (retention = a scheduled `DELETE`/partition-drop job). Well-trodden.

**Backup/restore.** `pg_dump`/`pg_basebackup`/WAL archiving + PITR — the most mature backup story of any datastore in existence. Zero surprises.

**Self-host docs maturity.** Excellent — Postgres is the default assumption of nearly every Django/Node self-host project.

---

## 3. Candidate B — ClickHouse single node

**Model.** Column-oriented MergeTree family tables for events and spans. It is the industry-standard store for exactly this shape (append-heavy, wide events, few update patterns, column scans over time windows). Retention is built in: `TTL ... DELETE WHERE timestamp + INTERVAL 30 DAY` drops expired **parts** in the background, so storage self-manages ([ClickHouse TTL](https://clickhouse.com/docs/reference/statements/alter/ttl), [TTL data-retention walkthrough](https://oneuptime.com/blog/post/2026-03-31-clickhouse-what-is-ttl-data-lifecycle/)). There is no VACUUM/compaction to schedule — MergeTree merges parts in the background by design.

**Query shapes.**

- *Issue list* — `SELECT ... GROUP BY fingerprint ORDER BY count() DESC` with a time filter is a first-class MergeTree aggregation; fast even over 30M rows.
- *Time-bucket aggregates* — this is ClickHouse's home turf (`toStartOfHour(timestamp)` group-bys, `quantile()` for p50/p95). No pre-aggregation layer needed; you can also add `AggregatingMergeTree` materialized views later.
- *Trace-by-id span lookups* — an index/sort key on `trace_id` (or a `trace_id → span` keyed table) gives sub-second lookups. Fine.

**Write throughput vs schema flexibility.** Writes: millions of rows/sec sustained — wildly beyond target. Schema: ClickHouse is schema-first; you must map event fields to columns. Semi-structured flexibility exists via the newer **JSON/Object** type and **Map** columns, but it is younger and less ergonomic than JSONB, and the docs themselves advise a "use JSON where appropriate" judgment call rather than a blanket dump ([ClickHouse: use JSON where appropriate](https://clickhouse.com/docs/best-practices/use-json-where-appropriate)). Evolving a Sentry-compatible schema means writing a schema-sync/migration layer for ClickHouse — more work than `ALTER TABLE ... ADD COLUMN` on JSONB, because unknown future SDK fields have to be anticipated (Map/JSON columns) or added per release.

**Storage footprint (estimate).** Columnar layout + block-level compression typically yields **5–10×** smaller than row-oriented JSONB for this data ([ClickHouse compression](https://clickhouse.com/resources/engineering/database-compression)). The same 30M-event/30-day window lands around **~10–30 GB**, and TTL keeps it bounded automatically.

**Ops burden (single instance).** One more service in Compose, but genuinely single-node capable (unlike Kafka-backed pipelines). Higher than Postgres: schema/migrations are more manual (no Django-migration equivalent), you must understand MergeTree engines + part merges, and you now run **two** operational domains (SQL dialect, backups, auth, monitoring) even if ClickHouse is the *only* store for events. TTL removes the retention-job burden Postgres leaves you.

**Backup/restore.** Solid but less turnkey than Postgres: native `BACKUP`/`RESTORE` (newer versions), `ALTER TABLE FREEZE` + filesystem copy, or the `clickhouse-backup` tool ([ClickHouse backup overview](https://clickhouse.com/docs/operations/backup/overview)). Works, but it is one more thing to script and rehearse.

**Self-host docs maturity.** Good and improving, but clearly a second-tier citizen for small self-host deployments compared to Postgres; most small self-host guidance assumes Postgres. ClickHouse's docs are aimed at large analytical deployments.

---

## 4. Candidate C — Hybrid (Postgres for state + ClickHouse for events)

**Model.** Postgres holds orgs/projects/teams, issue state, and aggregate pointers; ClickHouse holds raw events + spans. A small ingest path writes events to ClickHouse and updates Postgres state. This is the **Sentry production architecture** (see §6).

**Assessment against target scale.** Every query shape is *best* served by this split — but you pay for it with a second datastore, a second dialect, a second backup path, and (in Sentry's case) a whole message bus. For ≤1M events/day on a single node, the ingestion volume does **not** justify the added moving parts: the same three queries are comfortably served by either A or B alone. Hybrid is the right answer at 10–100× this scale, not at this scale.

**Write throughput vs schema flexibility.** Best of both (ClickHouse ingest, JSONB flexibility in Postgres for state) — at the cost of two code paths and eventual-consistency questions between event store and issue counters.

**Ops burden.** Highest: two databases to migrate, back up, monitor, and version. Docker Compose grows to 4+ services (app, Postgres, ClickHouse, optional queue) and backup/restore becomes a multi-datastore consistency exercise (Postgres state + ClickHouse events must be restored coherently, or issue counters drift from events).

---

## 5. Side-by-side comparison

| Criterion | A. Postgres alone | B. ClickHouse single node | C. Hybrid |
|---|---|---|---|
| Issue list (group-by + window) | Good (generated col indexes) | Excellent | Excellent |
| Time-bucket aggregates | Needs precomputed rollups | Excellent (native) | Excellent |
| Trace-by-id span lookups | Good (indexed `trace_id`) | Good | Good |
| Write throughput | Plenty at 12/sec | Extreme overkill | Plenty |
| Schema flexibility | Best (JSONB) | Weaker (schema-first; JSON type younger) | Good (split) |
| Storage footprint @30M | ~60–150 GB | ~10–30 GB (TTL-bounded) | ~10–30 GB events |
| Ops burden (single node) | Lowest | Medium | Highest |
| Migrations | App framework | Manual schema-sync | Both |
| Retention/TTL | Scheduled job | Built-in TTL | Built-in TTL |
| Backup/restore | Best-in-class (pg_dump/PITR) | Good (BACKUP/clickhouse-backup) | Hardest (cross-store) |
| Self-host docs maturity | Highest | Good, analytics-focused | Poor for small self-host |
| Precedent at this scale | GlitchTip | PostHog/Signoz (larger) | Sentry self-hosted |

---

## 6. What the precedents actually use

### GlitchTip (closest Sentry-compatible self-host)
GlitchTip is **Postgres (+ Redis) only** — Django ORM, events stored in Postgres (see its [`events/models.py`](https://gitlab.com/glitchtip/glitchtip-backend/-/blob/302436c8c2de372f535dae104409224d9e11dae9/events/models.py)). It deliberately avoids a columnar database; when it needs cheap columnar analytics over *old* data it archives events to **DuckDB + Parquet** ("columnar analytics without the columnar database") rather than running ClickHouse ([GlitchTip: DuckDB/Parquet archives](https://glitchtip.com/blog/2026-04-20-duckdb-parquet-archives)). Its performance-monitoring docs describe this as **cold storage** ([GlitchTip performance / cold storage](https://glitchtip.com/documentation/performance/#cold-storage)). GlitchTip is routinely cited as the "self-host Sentry without the 16GB RAM bill" — a direct contrast to Sentry's heavyweight stack ([GlitchTip on Railway vs Sentry's RAM bill](https://dev.to/greatsage_sh/self-hosted-sentry-error-tracking-without-the-16gb-ram-bill-glitchtip-on-railway-46if)).

### Sentry self-hosted
Sentry is the **hybrid** reference: **Postgres** for relational/org/project state (and the default `nodestore` for large payload blobs — a known source of unbounded growth, see [getsentry/self-hosted#783](https://github.com/getsentry/self-hosted/issues/783)), **ClickHouse via Snuba** for events/transactions/spans, **Redis** for cache/queues, and **Kafka** for the ingest pipeline ([Sentry self-hosted data flow](https://develop.sentry.dev/self-hosted/data-flow/#event-ingestion-pipeline); [system architecture](https://deepwiki.com/getsentry/self-hosted/2-system-architecture)). The official Compose stack runs ~70 containers — a frequently-cited operations burden that is wildly over-provisioned for ≤1M events/day ([Blendbyte: "Sentry Self-Hosting Runs 71 Containers. We Wanted One."](https://www.blendbyte.com/blog/sentry-self-hosting-71-containers-tindra-one)).

**Takeaway.** The two Sentry-compatible self-host projects embody the two ends of this decision: GlitchTip proves **Postgres alone is sufficient and operationally cheap** at modest scale; Sentry proves **hybrid ClickHouse is what you need at large scale**, and what you want to avoid at small scale.

---

## 7. Recommendation

**Start with Postgres alone (Candidate A), with a documented, reversible path to hybrid ClickHouse (Candidate C).**

Rationale:

1. **Scale is the deciding factor.** ~12 events/sec sustained (bursts of a few hundred/sec) is 2–3 orders of magnitude below Postgres's ingest ceiling. None of the three query shapes needs ClickHouse at 30M rows/30 days — issue list and trace lookups are indexed queries, and time-bucket aggregates are solved with modest precomputed rollup tables.
2. **Lowest total ops burden for a single-instance Compose.** One database, app-native migrations, `pg_dump`/PITR backups. This directly serves the "modest self-host" target.
3. **Precedent.** GlitchTip — the closest Sentry-compatible self-host that actually fits on one node — runs this exact model and is widely deployed. Sentry's hybrid is the *anti-goal* at this scale (71 containers).
4. **Schema flexibility wins.** Sentry's event payload is deep, nested, and SDK-version-dependent; JSONB + generated columns absorbs that without a parallel schema-sync system, which ClickHouse forces you to build.

**Trade-offs deliberately left open (design decision to revisit):**

- **Retention/aggregation growth.** If retention rises well past 30 days, or projects grow into the hundreds, JSONB's storage footprint (~60–150 GB and climbing) becomes the first thing to bite. The escape hatch is to move *raw events/spans only* to ClickHouse (hybrid) while Postgres keeps org/issue state — the migration is incremental and reversible.
- **Time-bucket aggregates.** If precomputed rollups prove limiting (new dimensions, arbitrary filters), a single-node ClickHouse (or GlitchTip-style DuckDB/Parquet cold storage for old events) is the targeted upgrade rather than a rewrite.
- **Cold storage.** GlitchTip's DuckDB/Parquet archive pattern is a third option for "cheap analytics over expired data" that keeps the live store at Postgres. Worth evaluating before committing to ClickHouse.
- **Two-store consistency.** If/when hybrid is adopted, the coherence of issue counters (Postgres) vs raw events (ClickHouse) during backup/restore must be specified.

### References

- ClickHouse — Table TTL: <https://clickhouse.com/docs/reference/statements/alter/ttl>
- ClickHouse — Backup and restore: <https://clickhouse.com/docs/operations/backup/overview>
- ClickHouse — Use JSON where appropriate: <https://clickhouse.com/docs/best-practices/use-json-where-appropriate>
- ClickHouse — Database compression: <https://clickhouse.com/resources/engineering/database-compression>
- OneUptime — ClickHouse TTL for data retention: <https://oneuptime.com/blog/post/2026-03-31-clickhouse-what-is-ttl-data-lifecycle/>
- Postgres generated columns & expression indexes (multi-tenant SaaS): <https://mvpfactory.io/blog/postgresql-generated-columns-and-expression-indexes-for-multi-tenant-saas>
- When does PostgreSQL JSONB performance break down (scale thresholds): <https://docs.bswen.com/blog/2026-04-24-jsonb-performance-scale/>
- GlitchTip — Columnar analytics without the columnar database (DuckDB/Parquet archives): <https://glitchtip.com/blog/2026-04-20-duckdb-parquet-archives>
- GlitchTip — Performance monitoring / cold storage: <https://glitchtip.com/documentation/performance/#cold-storage>
- GlitchTip backend `events/models.py`: <https://gitlab.com/glitchtip/glitchtip-backend/-/blob/302436c8c2de372f535dae104409224d9e11dae9/events/models.py>
- GlitchTip on Railway (vs Sentry's RAM bill): <https://dev.to/greatsage_sh/self-hosted-sentry-error-tracking-without-the-16gb-ram-bill-glitchtip-on-railway-46if>
- Sentry self-hosted — data flow (event ingestion pipeline): <https://develop.sentry.dev/self-hosted/data-flow/#event-ingestion-pipeline>
- Sentry self-hosted — system architecture: <https://deepwiki.com/getsentry/self-hosted/2-system-architecture>
- Sentry self-hosted — data storage: <https://deepwiki.com/getsentry/self-hosted/2.3-data-storage>
- Sentry self-hosted #783 (unbounded Postgres nodestore growth): <https://github.com/getsentry/self-hosted/issues/783>
- Blendbyte — Sentry self-hosting runs 71 containers: <https://www.blendbyte.com/blog/sentry-self-hosting-71-containers-tindra-one>
