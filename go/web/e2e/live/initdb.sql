-- e2e live-tier first-init hook (design 06 §4.7, wave PV10). Runs ONCE at
-- Postgres cluster init, as the superuser POSTGRES_USER, against POSTGRES_DB.
-- Production's init-data.sh creates these as superuser BEFORE ctxd connects;
-- this mirrors it. Migration 001 also runs CREATE EXTENSION IF NOT EXISTS for
-- vector/pg_trgm/pgcrypto — but store.NewPool registers the pgvector TYPE at
-- connect time (an AfterConnect hook), which needs the `vector` extension to
-- already exist BEFORE the first connection, i.e. before migrations run. So
-- vector must be created here, not left to migration 001. timescaledb has no
-- CREATE EXTENSION in any migration at all. Idempotent.
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
