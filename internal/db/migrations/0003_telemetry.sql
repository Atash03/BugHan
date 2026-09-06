-- Telemetry storage (DESIGN.md §7, ticket #32): releases, issues, partitioned
-- event/transaction/session tables, rollups, feedback, ingest counters.

CREATE TABLE releases (
    id             uuid PRIMARY KEY,
    project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    version        text NOT NULL,
    first_event_at timestamptz,
    last_event_at  timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, version)
);
CREATE INDEX releases_project_recent_idx ON releases (project_id, created_at DESC);

CREATE TABLE issues (
    id               uuid PRIMARY KEY,
    project_id       uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    fingerprint      text NOT NULL,
    grouping_version int NOT NULL DEFAULT 1,
    title            text NOT NULL DEFAULT '',
    culprit          text NOT NULL DEFAULT '',
    type             text NOT NULL DEFAULT 'error',
    level            text NOT NULL DEFAULT 'error',
    status           text NOT NULL DEFAULT 'unresolved'
                     CHECK (status IN ('unresolved', 'resolved', 'ignored')),
    substatus        text NOT NULL DEFAULT '',
    first_seen       timestamptz NOT NULL,
    last_seen        timestamptz NOT NULL,
    count            bigint NOT NULL DEFAULT 0,
    user_count       int NOT NULL DEFAULT 0,
    assignee_id      uuid REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (project_id, fingerprint)
);
CREATE INDEX issues_project_last_seen_idx ON issues (project_id, last_seen DESC);
CREATE INDEX issues_project_status_idx ON issues (project_id, status);

-- Error events: monthly partitions; old partitions are dropped by the
-- retention sweep rather than DELETEd.
CREATE TABLE events_part (
    id            uuid NOT NULL,
    project_id    uuid NOT NULL REFERENCES projects(id),
    issue_id      uuid REFERENCES issues(id),
    received_at   timestamptz NOT NULL DEFAULT now(),
    timestamp     timestamptz NOT NULL,
    platform      text NOT NULL DEFAULT '',
    level         text NOT NULL DEFAULT 'error',
    environment   text NOT NULL DEFAULT 'production',
    release       text NOT NULL DEFAULT '',
    dist          text NOT NULL DEFAULT '',
    title         text NOT NULL DEFAULT '',
    culprit       text NOT NULL DEFAULT '',
    message       text NOT NULL DEFAULT '',
    type          text NOT NULL DEFAULT 'error',
    trace_id      uuid,
    span_id       text NOT NULL DEFAULT '',
    user_hash     text NOT NULL DEFAULT '',
    tags          jsonb NOT NULL DEFAULT '{}',
    payload       jsonb NOT NULL,
    symbolicated  jsonb,
    PRIMARY KEY (id, timestamp)
) PARTITION BY RANGE (timestamp);

CREATE INDEX events_issue_ts_idx ON events_part (issue_id, timestamp DESC);
CREATE INDEX events_project_ts_idx ON events_part (project_id, timestamp DESC);
CREATE INDEX events_project_trace_idx ON events_part (project_id, trace_id);

CREATE TABLE transactions_part (
    id           uuid NOT NULL,
    project_id   uuid NOT NULL REFERENCES projects(id),
    received_at  timestamptz NOT NULL DEFAULT now(),
    timestamp    timestamptz NOT NULL,
    start_ts     timestamptz NOT NULL,
    duration_ms  double precision NOT NULL DEFAULT 0,
    trace_id     uuid,
    span_id      text NOT NULL DEFAULT '',
    name         text NOT NULL DEFAULT '',
    source       text NOT NULL DEFAULT '',
    status       text NOT NULL DEFAULT '',
    environment  text NOT NULL DEFAULT 'production',
    release      text NOT NULL DEFAULT '',
    dist         text NOT NULL DEFAULT '',
    measurements jsonb NOT NULL DEFAULT '{}',
    payload      jsonb NOT NULL,
    PRIMARY KEY (id, timestamp)
) PARTITION BY RANGE (timestamp);

CREATE INDEX transactions_project_start_idx ON transactions_part (project_id, start_ts DESC);
CREATE INDEX transactions_project_trace_idx ON transactions_part (project_id, trace_id);
CREATE INDEX transactions_project_name_idx ON transactions_part (project_id, name, start_ts DESC);

CREATE TABLE sessions_part (
    sid         uuid NOT NULL,
    started     timestamptz NOT NULL,
    project_id  uuid NOT NULL REFERENCES projects(id),
    received_at timestamptz NOT NULL DEFAULT now(),
    status      text NOT NULL DEFAULT 'ok',
    errors      int NOT NULL DEFAULT 0,
    duration    double precision,
    did_hash    text NOT NULL DEFAULT '',
    release     text NOT NULL DEFAULT '',
    environment text NOT NULL DEFAULT 'production',
    init        boolean NOT NULL DEFAULT false,
    PRIMARY KEY (sid, started)
) PARTITION BY RANGE (started);

CREATE INDEX sessions_project_started_idx ON sessions_part (project_id, started DESC);

-- Hourly session health rollups (project × release × env): fed by both
-- individual `session` items and pre-aggregated `sessions` items.
CREATE TABLE session_rollups (
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    release     text NOT NULL,
    environment text NOT NULL,
    hour        timestamptz NOT NULL,
    total       bigint NOT NULL DEFAULT 0,
    crashed     bigint NOT NULL DEFAULT 0,
    abnormal    bigint NOT NULL DEFAULT 0,
    errored     bigint NOT NULL DEFAULT 0,
    exited      bigint NOT NULL DEFAULT 0,
    duration_sum double precision NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, release, environment, hour)
);

-- Hourly event counts per issue (sparklines) and per project (charts).
CREATE TABLE event_rollups (
    issue_id uuid NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    hour     timestamptz NOT NULL,
    count    bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (issue_id, hour)
);

CREATE TABLE project_event_rollups (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour       timestamptz NOT NULL,
    count      bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, hour)
);

CREATE TABLE feedbacks (
    id                  uuid PRIMARY KEY,
    project_id          uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    issue_id            uuid REFERENCES issues(id) ON DELETE SET NULL,
    associated_event_id uuid,
    name                text NOT NULL DEFAULT '',
    contact_email       text NOT NULL DEFAULT '',
    message             text NOT NULL,
    url                 text NOT NULL DEFAULT '',
    received_at         timestamptz NOT NULL DEFAULT now(),
    payload             jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX feedbacks_project_idx ON feedbacks (project_id, received_at DESC);
CREATE INDEX feedbacks_issue_idx ON feedbacks (issue_id) WHERE issue_id IS NOT NULL;

-- Fixed-window ingest rate limiting (project × window start).
CREATE TABLE ingest_counters (
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    window_start timestamptz NOT NULL,
    count        bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, window_start)
);

-- Unknown envelope item types are counted, never rejected (spec rule).
CREATE TABLE unknown_item_stats (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    item_type  text NOT NULL,
    day        date NOT NULL DEFAULT current_date,
    count      bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, item_type, day)
);
