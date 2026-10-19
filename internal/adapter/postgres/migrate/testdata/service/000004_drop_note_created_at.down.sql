-- irreversible
DO $$ BEGIN RAISE EXCEPTION '000004_drop_note_created_at is irreversible'; END $$;
