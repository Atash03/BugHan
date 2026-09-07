-- Thin alerting (DESIGN.md §12, ticket #44): project-scoped rules,
-- per-issue quiet period (alert_dedupe), and a delivery log.

CREATE TABLE alert_rules (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id        uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name              text NOT NULL DEFAULT '',
    trigger           text NOT NULL CHECK (trigger IN ('new_issue', 'regression', 'event_count')),
    enabled           boolean NOT NULL DEFAULT true,
    environments      text[] NOT NULL DEFAULT '{}',
    releases          text[] NOT NULL DEFAULT '{}',
    levels            text[] NOT NULL DEFAULT '{}',
    threshold_count   int NOT NULL DEFAULT 10,
    threshold_minutes int NOT NULL DEFAULT 60,
    quiet_minutes     int NOT NULL DEFAULT 30,
    email_to          text[] NOT NULL DEFAULT '{}',
    webhook_url       text NOT NULL DEFAULT '',
    webhook_secret    text NOT NULL DEFAULT '',
    created_by        uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX alert_rules_project_idx ON alert_rules (project_id);

-- Per-(rule, issue) quiet period: no timers, just last-sent timestamps.
CREATE TABLE alert_dedupe (
    rule_id      uuid NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    issue_id     uuid NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    last_sent_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (rule_id, issue_id)
);

-- Delivery log: one row per attempted action (email recipient or webhook).
CREATE TABLE alert_deliveries (
    id         bigserial PRIMARY KEY,
    rule_id    uuid REFERENCES alert_rules(id) ON DELETE SET NULL,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    issue_id   uuid REFERENCES issues(id) ON DELETE SET NULL,
    event_id   uuid,
    trigger    text NOT NULL DEFAULT '',
    action     text NOT NULL CHECK (action IN ('email', 'webhook', 'test')),
    target     text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT 'sent' CHECK (status IN ('sent', 'failed')),
    error      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX alert_deliveries_project_idx ON alert_deliveries (project_id, created_at DESC);
CREATE INDEX alert_deliveries_rule_idx ON alert_deliveries (rule_id, created_at DESC);
