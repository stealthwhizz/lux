-- Imported agents used their manifest name ("release-notes") as the name
-- people see; give existing ones the readable form ("Release notes").
UPDATE agents
SET name = upper(left(replace(name, '-', ' '), 1)) || substr(replace(name, '-', ' '), 2)
WHERE source_kind <> 'managed' AND name ~ '^[a-z0-9]+(-[a-z0-9]+)*$';
