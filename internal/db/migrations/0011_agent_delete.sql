-- Deleting an agent hides it and frees its name; its tasks stay for the record.
ALTER TABLE agents ADD COLUMN deleted_at TIMESTAMPTZ;
ALTER TABLE agents DROP CONSTRAINT agents_workspace_id_slug_key;
CREATE UNIQUE INDEX agents_workspace_slug_live ON agents (workspace_id, slug) WHERE deleted_at IS NULL;
