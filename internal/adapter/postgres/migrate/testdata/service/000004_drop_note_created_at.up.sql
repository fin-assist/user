DROP INDEX IF EXISTS note_created_at_idx;
ALTER TABLE note DROP COLUMN created_at;
