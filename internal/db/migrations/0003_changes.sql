-- Name of a git credential configured on the luxd host (LUX_GIT_CREDENTIAL_<NAME>)
-- used to fetch a private agent repository and to publish memory changes.
-- Never the secret itself.
ALTER TABLE agents ADD COLUMN credential TEXT NOT NULL DEFAULT '';

-- Commit of the task's repository a run started from ('' without a repository).
ALTER TABLE task_runs ADD COLUMN repo_commit TEXT NOT NULL DEFAULT '';

-- Changes a run made, captured by luxd after the run ended:
-- 'repository' is the task's repository checkout, 'memory' the agent's
-- memory/ and skills/ folders. Publishing pushes exactly the stored patch.
CREATE TABLE task_changes (
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    target        TEXT NOT NULL CHECK (target IN ('repository','memory')),
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    attempt       INT NOT NULL,
    base_commit   TEXT NOT NULL,
    files         JSONB NOT NULL,
    -- NULL when the patch was larger than luxd keeps; such changes cannot be published.
    patch         BYTEA,
    patch_bytes   BIGINT NOT NULL,
    state         TEXT NOT NULL DEFAULT 'proposed'
                  CHECK (state IN ('proposed','publishing','published','discarded')),
    branch        TEXT NOT NULL DEFAULT '',
    commit_sha    TEXT NOT NULL DEFAULT '',
    pull_request_url TEXT NOT NULL DEFAULT '',
    last_error    TEXT NOT NULL DEFAULT '',
    decided_by    TEXT REFERENCES users(id),
    decided_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, target)
);
CREATE INDEX task_changes_ws_state_idx ON task_changes (workspace_id, state);
