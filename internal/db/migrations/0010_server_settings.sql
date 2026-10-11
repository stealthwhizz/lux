-- Settings an administrator makes in the app (for example the Google OAuth
-- client). Values are encrypted with luxd's secret key where they hold secrets.
CREATE TABLE server_settings (
    key         TEXT PRIMARY KEY,
    value       BYTEA NOT NULL,
    updated_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
