-- One reply per task in a conversation, so a retried finalize can never
-- add a second assistant message.
CREATE UNIQUE INDEX messages_one_per_task_role ON messages (task_id, role) WHERE task_id IS NOT NULL;
