-- 004_notify_triggers.sql — LISTEN/NOTIFY triggers for the Go event pipeline
-- Replaces n8n cron-based guard/digest with PG-native event-driven approach.

CREATE OR REPLACE FUNCTION notify_block_write() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('ctx_block_write', json_build_object('id', NEW.id, 'op', TG_OP)::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_block_write ON context_blocks;
CREATE TRIGGER trg_block_write
    AFTER INSERT OR UPDATE ON context_blocks
    FOR EACH ROW EXECUTE FUNCTION notify_block_write();
