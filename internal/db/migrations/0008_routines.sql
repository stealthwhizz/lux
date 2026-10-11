-- Routines: schedules made in Lux (source_file = ''), owned by the person
-- who created them. Their tasks run as that person's and post into the
-- agent's thread.
ALTER TABLE schedules ADD COLUMN created_by TEXT REFERENCES users(id) ON DELETE SET NULL;
