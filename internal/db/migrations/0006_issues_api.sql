-- T5: Issues API and lifecycle — activity feed, saved views, list-support indexes.

-- One feed per issue: human notes plus machine-recorded triage actions.
CREATE TABLE issue_activity (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id  uuid NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    author_id uuid REFERENCES users(id) ON DELETE SET NULL,
    type      text NOT NULL CHECK (type IN ('note', 'status', 'assignment')),
    body      text NOT NULL DEFAULT '',
    data      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX issue_activity_issue_idx ON issue_activity (issue_id, created_at DESC);

-- Bookmarkable issue-list queries per project.
CREATE TABLE saved_views (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       text NOT NULL,
    query      text NOT NULL DEFAULT '',
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

-- Trigram support for issue-list text search (title/culprit ILIKE).
CREATE INDEX issues_title_trgm_idx ON issues USING gin (title gin_trgm_ops);
CREATE INDEX issues_culprit_trgm_idx ON issues USING gin (culprit gin_trgm_ops);
