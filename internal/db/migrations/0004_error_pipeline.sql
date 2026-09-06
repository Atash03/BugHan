-- Error pipeline (DESIGN.md §6–8, ticket #38): background jobs for the
-- embedded worker and distinct-user tracking for issue counters.

-- Bounded in-process worker queue (alerts, retention sweep, partition
-- maintenance). Drained with FOR UPDATE SKIP LOCKED.
CREATE TABLE jobs (
    id           bigserial PRIMARY KEY,
    kind         text NOT NULL,
    run_at       timestamptz NOT NULL DEFAULT now(),
    payload      jsonb NOT NULL DEFAULT '{}',
    status       text NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'running', 'done', 'failed')),
    attempts     int NOT NULL DEFAULT 0,
    max_attempts int NOT NULL DEFAULT 3,
    last_error   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_due_idx ON jobs (status, run_at);

-- Distinct affected users per issue: one row per (issue, user hash), updated
-- at ingest. Pruned by the retention sweep on the raw-event clock — issues
-- keep their counters, the underlying hash rows age out like events.
CREATE TABLE issue_user_hashes (
    issue_id   uuid NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_hash  text NOT NULL,
    first_seen timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, user_hash)
);
