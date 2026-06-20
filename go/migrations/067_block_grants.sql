-- =============================================================================
-- 067_block_grants.sql — row-level read grant for single blocks (MT, Achse 07)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T39 (Achse 07-W1). The THIRD level of the 3-level
-- architecture (user 2026-06-15): after tenant (059) and scope/department, the
-- single-block share. "einzelne bloecke an andere freigeben". Exact schema
-- vorlage: design/07-block-level-sharing.md §3.1.
--
-- A block STAYS in its scope; this grant makes it additively READABLE for one
-- grantee tenant — read-only, revocable, immediate. FINER than
-- context_tenant_grants (061, scope-level):
--   scope-level: (grantee_tenant, granted_scope)   -- 061
--   block-level: (grantee_tenant, block_id)        -- THIS table
--
-- Mechanism = code (the OR-arm in the switch point), policy = data (THIS table).
-- ADDITIVE table with NO consumer yet — the VisibilityPredicate OR-arm (T40a)
-- and the ctx_rrf sixfold OR (T40b/068) read it LATER. 067 only lays the table.
-- With an empty grant set the whole mechanism is a byte-identical no-op to the
-- scope-only state (pausability invariant, conflicts.md §7).
--
-- Backfill-free (0 existing grants — block-level sharing is opt-in,
-- least-privilege). NEW table + index on an EMPTY relation → no context_blocks
-- lock, no 1M index build, no CONCURRENTLY problem (the decisive advantage of
-- the join over the array variant, inventory/rowlevel-acl-scale.md §5.3 /
-- design/07 §3.2). Idempotent (CREATE ... IF NOT EXISTS, _migrations ON
-- CONFLICT), self-registering, forward-only, one Tx.
--
-- ON DELETE policies:
--   block_id        FK → context_blocks(id)   ON DELETE CASCADE
--     — a deleted block cannot hold a grant; the grant vanishes with it.
--       KONTRAST to context_dream_links (no ON DELETE → blocks the delete);
--       here the cleaner offboarding wins (design/07 §3.1).
--   grantee_tenant  FK → context_tenants(id)  ON DELETE CASCADE
--     — grantee offboarding clears the grants it HOLDS (design/01 §6.3).
--   granted_by      FK → context_api_keys(id) ON DELETE SET NULL
--     — provenance pointer; key delete ANONYMIZES it, never deletes the grant
--       (the share survives the key that minted it; FK politik db-tenant-surface §8).
--
-- lock_timeout (R-MIG2): CREATE-only, no ALTER on a hot table → no long-held
-- lock. The Runner (internal/store/migrations.go) wraps EACH migration in its
-- own Tx, so SET LOCAL is transaction-scoped and self-reverting. Set for
-- consistency with 058-063 and fail-fast hygiene.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_block_grants (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    block_id        UUID NOT NULL REFERENCES context_blocks(id)  ON DELETE CASCADE, -- the shared block
    grantee_tenant  UUID NOT NULL REFERENCES context_tenants(id) ON DELETE CASCADE, -- gains read access (Modell C, UUID-FK)
    granted_by      UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,        -- audit pointer (FK politik db-tenant-surface §8)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata        JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_block_grant UNIQUE (block_id, grantee_tenant)   -- no double-grant; also covers block_id-leading lookup
);
-- TENANT-DECISION(block-grant-grantee): grantee_tenant (UUID FK on context_tenants) —
--   Alternative grantee_scope (department) OR grantee_key (single key), umentscheidbar
--   weil die Spalte additiv erweiterbar ist (mehrere grantee_*-Spalten mit XOR-CHECK)
--   und der Switch-Point-OR nur die aufgeloeste id-Liste sieht. Tenant-Granularitaet
--   ist der minimale Schnitt (analog scope-level grantee_tenant, 061); feiner (scope/key)
--   ist additiv nachruestbar. design/07 §3.1 / §8, inventory rowlevel-acl-scale ED-B2.
-- TENANT-DECISION(block-grant-permission): KEINE permission-Spalte in 067 — read-only by
--   construction (the write path gates on home_scope, design/07 §2.5). Alternative
--   permission TEXT DEFAULT 'read' CHECK (permission IN ('read')) JETZT, umentscheidbar
--   weil die Tabelle KLEIN ist → ein spaeteres `ADD COLUMN permission DEFAULT 'read'` bei
--   der ersten Schreib-/Comment-Grant-Welle (Achse 05) ist lock-arm. Eine Single-Value-
--   CHECK-Enum gatet HEUTE nichts (jede Zeile traegt zwangslaeufig 'read') → minimaler
--   Schnitt (M052-is_admin-Doktrin) spricht GEGEN sie jetzt. Wenn 'write'-Grants kommen,
--   MUSS der Lese-OR dann `AND permission IN ('read',...)` filtern. design/07 §3.1.

-- Two lookup directions (analog context_dream_links 016: PK + idx_target):
--   (a) "which blocks are granted to tenant A?" → grantee-leading (HOT read, the resolver)
CREATE INDEX IF NOT EXISTS idx_block_grants_grantee
    ON context_block_grants (grantee_tenant, block_id);
--   (b) "who sees this block?" / revoke / audit → block_id-leading, covered by uq_block_grant

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (67, '067_block_grants.sql', now())
  ON CONFLICT (version) DO NOTHING;
