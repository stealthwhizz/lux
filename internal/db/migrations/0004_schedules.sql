-- Schedules come from the agent repository's schedules/ folder and are
-- replaced when a new revision becomes current. `enabled` mirrors the file;
-- `paused` is an administrator's switch in Lux and survives syncs.
ALTER TABLE schedules
    ADD COLUMN mode       TEXT NOT NULL DEFAULT 'repeat' CHECK (mode IN ('repeat','once')),
    ADD COLUMN run_at     TIMESTAMPTZ,
    ADD COLUMN paused     BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN last_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
CREATE INDEX schedules_due_idx ON schedules (next_run_at) WHERE enabled AND NOT paused;
CREATE INDEX tasks_schedule_idx ON tasks (schedule_id, created_at DESC) WHERE schedule_id IS NOT NULL;
