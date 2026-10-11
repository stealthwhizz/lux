-- Agents created in Lux itself: luxd keeps their git repository in its
-- data directory, so people need no repository of their own.
ALTER TABLE agents DROP CONSTRAINT agents_source_kind_check;
ALTER TABLE agents ADD CONSTRAINT agents_source_kind_check CHECK (source_kind IN ('git','local','managed'));
