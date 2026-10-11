-- Files a person attached to a message: [{"path": "uploads/x.pdf", "name": "x.pdf", "size": 1234}].
-- The files themselves live in the agent's uploads folder on the server.
ALTER TABLE messages ADD COLUMN attachments JSONB NOT NULL DEFAULT '[]'::jsonb;
