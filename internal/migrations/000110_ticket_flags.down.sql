DROP TABLE flag_outbox;
ALTER TABLE projects DROP COLUMN flags_agent_id, DROP COLUMN flag_cleanup_days;
ALTER TABLE columns DROP COLUMN flag_action;
DROP VIEW tickets;
ALTER TABLE tickets_all DROP COLUMN needs_flag, DROP COLUMN flag_name, DROP COLUMN flag_agent_id,
    DROP COLUMN flag_error, DROP COLUMN flag_created_at, DROP COLUMN flag_cleanup_id;
DO $$
DECLARE cols text;
BEGIN
    SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) INTO cols
    FROM pg_attribute WHERE attrelid = 'tickets_all'::regclass AND attnum > 0 AND NOT attisdropped AND attname <> 'deleted_at';
    EXECUTE format('CREATE VIEW tickets AS SELECT %s FROM tickets_all WHERE deleted_at IS NULL', cols);
END $$;
