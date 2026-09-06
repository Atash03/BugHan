-- Release artifact storage (DESIGN.md §10, ticket #41): the classic
-- release-files API. Bodies live in Postgres bytea (50 MB/file, 500 MB/release
-- caps); object storage is a v0.2 seam. debug_id and sourcemap_url are
-- extracted at upload for symbolication matching.

CREATE TABLE release_files (
    id            uuid PRIMARY KEY,
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    release       text NOT NULL,
    dist          text NOT NULL DEFAULT '',
    name          text NOT NULL,
    headers       jsonb NOT NULL DEFAULT '{}',
    body          bytea NOT NULL,
    size          bigint NOT NULL,
    sha256        text NOT NULL,
    debug_id      text NOT NULL DEFAULT '',
    sourcemap_url text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, release, dist, name)
);

-- debug_id lookup is the fast path when events carry debug_meta.images.
CREATE INDEX release_files_debug_idx
    ON release_files (project_id, release, debug_id) WHERE debug_id <> '';
