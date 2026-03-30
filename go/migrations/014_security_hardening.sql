-- 014_security_hardening.sql
-- Security: Drop plaintext api_key column, revoke cross-database CONNECT.
-- Part of ctx by GottZ (github.com/GottZ/ctx/graphs/contributors)

-- 1. Drop plaintext API key column (auth uses key_hash exclusively via ctx_auth)
DROP INDEX IF EXISTS idx_api_keys_key;
ALTER TABLE context_api_keys DROP CONSTRAINT IF EXISTS context_api_keys_api_key_key;
ALTER TABLE context_api_keys DROP COLUMN IF EXISTS api_key;

-- 2. Promote key_hash: NOT NULL + UNIQUE (was nullable, now the sole identifier)
ALTER TABLE context_api_keys ALTER COLUMN key_hash SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash_unique ON context_api_keys(key_hash);
