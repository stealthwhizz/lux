-- Lux control-plane schema, version 1.

CREATE TABLE organizations (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations(id),
    username    TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    disabled    BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, username)
);

CREATE TABLE workspaces (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations(id),
    slug        TEXT NOT NULL,
    name        TEXT NOT NULL,
    -- Workspace policy (approval rules, isolation minimum, fallback, budgets).
    policy      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);

CREATE TABLE memberships (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role         TEXT NOT NULL CHECK (role IN ('viewer','operator','approver','admin','owner')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id)
);

-- API tokens. Only a SHA-256 hash of the secret is stored.
CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    secret_hash  BYTEA NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE TABLE agents (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    slug          TEXT NOT NULL,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    -- Where the agent repository comes from: a git URL or an absolute local path.
    source_kind   TEXT NOT NULL CHECK (source_kind IN ('git','local')),
    source_url    TEXT NOT NULL,
    source_ref    TEXT NOT NULL DEFAULT '',
    runtime       TEXT NOT NULL DEFAULT 'gitagent' CHECK (runtime IN ('gitagent','ira')),
    executor      TEXT NOT NULL DEFAULT 'local',
    current_revision_id TEXT,
    disabled      BOOLEAN NOT NULL DEFAULT false,
    created_by    TEXT REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, slug)
);

-- An immutable, validated snapshot of an agent repository at a commit.
CREATE TABLE agent_revisions (
    id             TEXT PRIMARY KEY,
    agent_id       TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    commit_sha     TEXT NOT NULL,
    manifest       JSONB NOT NULL,
    validation     JSONB NOT NULL,
    valid          BOOLEAN NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, commit_sha)
);

ALTER TABLE agents ADD CONSTRAINT agents_current_revision_fk
    FOREIGN KEY (current_revision_id) REFERENCES agent_revisions(id) DEFERRABLE INITIALLY DEFERRED;

-- Code repositories that agents work on.
CREATE TABLE repositories (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    source_kind   TEXT NOT NULL CHECK (source_kind IN ('git','local')),
    source_url    TEXT NOT NULL,
    default_branch TEXT NOT NULL DEFAULT '',
    test_command  TEXT NOT NULL DEFAULT '',
    -- Name of a credential configured on the luxd host (never the secret itself).
    push_credential TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE conversations (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    agent_id      TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    repository_id TEXT REFERENCES repositories(id) ON DELETE SET NULL,
    title         TEXT NOT NULL,
    created_by    TEXT REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tasks (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    agent_id        TEXT NOT NULL REFERENCES agents(id),
    revision_id     TEXT NOT NULL REFERENCES agent_revisions(id),
    conversation_id TEXT REFERENCES conversations(id) ON DELETE SET NULL,
    repository_id   TEXT REFERENCES repositories(id) ON DELETE SET NULL,
    schedule_id     TEXT,
    prompt          TEXT NOT NULL,
    state           TEXT NOT NULL,
    state_reason    TEXT NOT NULL DEFAULT '',
    result_summary  TEXT NOT NULL DEFAULT '',
    executor        TEXT NOT NULL,
    isolation       TEXT NOT NULL DEFAULT '',
    attempt         INT NOT NULL DEFAULT 0,
    max_attempts    INT NOT NULL DEFAULT 1,
    timeout_seconds INT NOT NULL DEFAULT 3600,
    idempotency_key TEXT,
    created_by      TEXT REFERENCES users(id),
    trigger_kind    TEXT NOT NULL DEFAULT 'manual',
    cancel_requested BOOLEAN NOT NULL DEFAULT false,
    -- Last assigned task_events.seq; incremented atomically when events are appended.
    event_seq       INT NOT NULL DEFAULT 0,
    deadline_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX tasks_state_idx ON tasks (state);
CREATE INDEX tasks_agent_idx ON tasks (agent_id, created_at DESC);
CREATE INDEX tasks_conversation_idx ON tasks (conversation_id, created_at);

CREATE TABLE messages (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role            TEXT NOT NULL CHECK (role IN ('user','assistant','system')),
    content         TEXT NOT NULL,
    task_id         TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    created_by      TEXT REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX messages_conversation_idx ON messages (conversation_id, created_at);

-- One row per execution attempt.
CREATE TABLE task_runs (
    id            TEXT PRIMARY KEY,
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt       INT NOT NULL,
    executor      TEXT NOT NULL,
    handle        JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- sha256 of the run token; the token itself is only given to the runtime.
    token_hash    BYTEA UNIQUE,
    events_offset BIGINT NOT NULL DEFAULT 0,
    exit_code     INT,
    exit_reason   TEXT NOT NULL DEFAULT '',
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at      TIMESTAMPTZ,
    UNIQUE (task_id, attempt)
);

CREATE TABLE task_events (
    id          BIGSERIAL PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    seq         INT NOT NULL,
    type        TEXT NOT NULL,
    payload     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (task_id, seq)
);
CREATE INDEX task_events_ws_idx ON task_events (workspace_id, id);

CREATE TABLE approvals (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN ('tool_call','git_commit','git_push','pull_request','memory_change')),
    summary       TEXT NOT NULL,
    details       JSONB NOT NULL DEFAULT '{}'::jsonb,
    rule          TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','approved','rejected','expired','cancelled')),
    requested_by  TEXT REFERENCES users(id),
    decided_by    TEXT REFERENCES users(id),
    decision_note TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at    TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ
);
CREATE INDEX approvals_pending_idx ON approvals (workspace_id, state);

CREATE TABLE schedules (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    agent_id      TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    -- Name of the file in the agent's schedules/ folder, or '' for Lux-defined schedules.
    source_file   TEXT NOT NULL DEFAULT '',
    name          TEXT NOT NULL,
    cron          TEXT NOT NULL,
    prompt        TEXT NOT NULL,
    enabled       BOOLEAN NOT NULL DEFAULT true,
    next_run_at   TIMESTAMPTZ,
    last_run_at   TIMESTAMPTZ,
    last_task_id  TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, name)
);

CREATE TABLE model_usage (
    id             BIGSERIAL PRIMARY KEY,
    workspace_id   TEXT NOT NULL,
    agent_id       TEXT NOT NULL,
    task_id        TEXT NOT NULL,
    provider       TEXT NOT NULL,
    model          TEXT NOT NULL,
    status         INT NOT NULL,
    usage_known    BOOLEAN NOT NULL,
    input_tokens   BIGINT NOT NULL DEFAULT 0,
    output_tokens  BIGINT NOT NULL DEFAULT 0,
    latency_ms     BIGINT NOT NULL DEFAULT 0,
    fallback_from  TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX model_usage_ws_idx ON model_usage (workspace_id, created_at);
CREATE INDEX model_usage_agent_idx ON model_usage (agent_id, created_at);

CREATE TABLE audit_events (
    id            BIGSERIAL PRIMARY KEY,
    org_id        TEXT NOT NULL,
    workspace_id  TEXT,
    actor_kind    TEXT NOT NULL CHECK (actor_kind IN ('user','system','runtime','schedule')),
    actor_id      TEXT NOT NULL DEFAULT '',
    action        TEXT NOT NULL,
    target_kind   TEXT NOT NULL DEFAULT '',
    target_id     TEXT NOT NULL DEFAULT '',
    outcome       TEXT NOT NULL DEFAULT 'ok',
    details       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_ws_idx ON audit_events (workspace_id, id DESC);

-- Audit rows are append-only for the application role.
CREATE FUNCTION audit_events_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER audit_events_no_update BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_immutable();
