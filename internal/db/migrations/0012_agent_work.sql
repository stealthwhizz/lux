-- What an agent says it is working on, shown in its Work panel. The agent
-- keeps it current with the lux__update_status tool.
CREATE TABLE agent_status (
    agent_id    TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    status      JSONB NOT NULL,
    task_id     TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Follow-ups an agent schedules for itself are one-time schedules marked
-- with this kind, kept apart from the routines people make.
ALTER TABLE schedules ADD COLUMN kind TEXT NOT NULL DEFAULT 'routine' CHECK (kind IN ('routine','follow_up'));
