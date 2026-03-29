-- =============================================================================
-- 010_content_dates_drop_generated.sql — Remove GENERATED ALWAYS from content_dates
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- content_dates was GENERATED ALWAYS via extract_dates_from_text(), preventing
-- Go from writing directly. Go's store.ExtractDates is more capable (dot-format,
-- month names, etc.). DROP EXPRESSION converts to a normal column; existing
-- values are preserved.
-- =============================================================================

ALTER TABLE context_blocks ALTER COLUMN content_dates DROP EXPRESSION IF EXISTS;
