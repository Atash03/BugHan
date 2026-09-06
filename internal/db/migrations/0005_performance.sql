-- Performance plane (DESIGN.md §9, §11, ticket #40): transaction rollups
-- (percentiles + failure rate), the rollup-maintenance watermark, and HLL
-- distinct-did sketches on session rollups.

-- Hourly per-transaction-name aggregates powering the Performance page.
-- Computed by the rollup_maintenance job from transactions_part (spans and
-- durations live in raw rows), persisting past the 30-day raw retention.
CREATE TABLE transaction_rollups (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       text NOT NULL,
    hour       timestamptz NOT NULL,
    count      bigint NOT NULL DEFAULT 0,
    failures   bigint NOT NULL DEFAULT 0,
    p50_ms     double precision NOT NULL DEFAULT 0,
    p95_ms     double precision NOT NULL DEFAULT 0,
    p99_ms     double precision NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, name, hour)
);
CREATE INDEX transaction_rollups_project_hour_idx ON transaction_rollups (project_id, hour DESC);

-- Progress marker for the rollup-maintenance job: hours before last_hour
-- have been fully recomputed; late arrivals within the trailing window are
-- absorbed by overwriting recent hours on each pass.
CREATE TABLE rollup_state (
    kind       text PRIMARY KEY,
    last_hour  timestamptz NOT NULL DEFAULT now()
);

-- Crash-free user rates need distinct dids per rollup row. HLL sketches
-- (internal/hll, 3KB) merge register-wise, so deliveries of the same session
-- and unions across rows stay correct; crashed_did sketches crashed sessions
-- separately. NULL = no dids seen yet.
ALTER TABLE session_rollups
    ADD COLUMN distinct_did bytea,
    ADD COLUMN crashed_did  bytea;
