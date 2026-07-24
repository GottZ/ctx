-- 108_migrations_checksum.sql
-- Contract-Achse W03-1: pin the applied SQL artifact (Konzept: pgContext
-- Contract-Registry, 00b §1 — Clean-Room). NULL = vor-108 appliziert;
-- der Boot-Backfill stempelt den Hash des EINGEBETTETEN Files nach.
ALTER TABLE _migrations ADD COLUMN IF NOT EXISTS checksum CHAR(64);
COMMENT ON COLUMN _migrations.checksum IS
  'sha256(hex) of the embedded migration file at record/backfill time; W11: backfill attests the present file, not the historic apply';
