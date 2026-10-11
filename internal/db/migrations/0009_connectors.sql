-- A person's connection to an outside service (Google...). Tokens are
-- encrypted with luxd's secret key; agents never receive them.
CREATE TABLE connections (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL,
    account     TEXT NOT NULL DEFAULT '',
    scopes      TEXT NOT NULL DEFAULT '',
    token_enc   BYTEA NOT NULL,
    last_error  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, provider)
);

-- Sign-in attempts in progress: the state sent to the provider, tied to the
-- person who started it. Only a hash of the state is kept.
CREATE TABLE oauth_states (
    state_hash  BYTEA PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL,
    verifier    TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL
);

-- An approval for an action a connector performs is used once.
ALTER TABLE approvals ADD COLUMN used_at TIMESTAMPTZ;
