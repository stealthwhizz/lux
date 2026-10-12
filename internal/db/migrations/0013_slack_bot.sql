-- The Lux bot in Slack. A person's Slack user id (from their Slack
-- connection) links Slack messages to their Lux account.
ALTER TABLE connections ADD COLUMN external_id TEXT NOT NULL DEFAULT '';
CREATE INDEX connections_external_idx ON connections (provider, external_id) WHERE external_id <> '';

-- Conversations someone talked to in Slack: replies and approvals go back there.
CREATE TABLE slack_routes (
    conversation_id TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    channel         TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- What has been posted to Slack, so each reply or approval goes once even
-- with several luxd replicas.
CREATE TABLE slack_deliveries (
    kind       TEXT NOT NULL,
    item_id    TEXT NOT NULL,
    channel    TEXT NOT NULL,
    ts         TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, item_id)
);
