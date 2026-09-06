-- Tenancy, auth, projects, and credentials (DESIGN.md §3–4).

CREATE TABLE organizations (
    id         uuid PRIMARY KEY,
    name       text NOT NULL,
    slug       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    email         text NOT NULL UNIQUE,          -- stored lowercased
    name          text NOT NULL DEFAULT '',
    password_hash text NOT NULL,
    verified      boolean NOT NULL DEFAULT false,
    verify_token  text,                           -- sha256 hex of the emailed token
    reset_token   text,                           -- sha256 hex of the emailed token
    reset_expires timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    id         uuid PRIMARY KEY,
    org_id     uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       text NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, user_id)
);
CREATE INDEX memberships_user_idx ON memberships(user_id);

CREATE TABLE invitations (
    id          uuid PRIMARY KEY,
    org_id      uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email       text NOT NULL,
    role        text NOT NULL CHECK (role IN ('admin', 'member')),
    token_hash  text NOT NULL UNIQUE,             -- sha256 hex of the emailed token
    invited_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX invitations_org_idx ON invitations(org_id) WHERE accepted_at IS NULL;

CREATE TABLE projects (
    id         uuid PRIMARY KEY,
    org_id     uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name       text NOT NULL,
    slug       text NOT NULL,
    platform   text NOT NULL DEFAULT 'javascript',
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);
CREATE INDEX projects_org_idx ON projects(org_id);

-- The key id doubles as the DSN public key (sentry_key).
CREATE TABLE ingest_keys (
    id         uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       text NOT NULL DEFAULT 'Default',
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
CREATE INDEX ingest_keys_project_idx ON ingest_keys(project_id);

CREATE TABLE api_tokens (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         text NOT NULL,
    token_hash   text NOT NULL UNIQUE,            -- sha256 hex of the full token
    token_prefix text NOT NULL,                   -- display prefix, e.g. bghan_ab12cd
    scope        text NOT NULL DEFAULT 'write' CHECK (scope IN ('read', 'write')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);

CREATE TABLE auth_sessions (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   text NOT NULL UNIQUE,            -- sha256 hex of the cookie value
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
