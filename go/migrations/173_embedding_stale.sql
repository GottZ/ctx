-- =============================================================================
-- 173_embedding_stale.sql — a title or content change keeps the previous
-- vector searchable until the backfill writes the new one (issue #198, part B)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Until this migration a real title or content change (by-id update, upsert,
-- the issue writers) nulled the block's vector: the embed backfill's queue was
-- the predicate embedding IS NULL, so the clear was how a block got back into
-- it. Both semantic arms of ctx_rrf require embedding IS NOT NULL, so the block
-- was out of them until the backfill reached it; a block rewritten in place
-- was out after every write.
--
-- Since this migration the change marks the vector stale instead:
--   context_blocks.embedding_stale  BOOLEAN NOT NULL DEFAULT false
-- true: the serving vector was made from text the block no longer holds. The
-- vector stays where it is, so the semantic arms keep ranking the block, by
-- its previous text, until the backfill writes the vector of the current text
-- and sets the marker back to false (store.InvalidateEmbeddingTx,
-- store.StoreEmbedding). A block without a vector is never marked; it is
-- pending as unembedded. The change still nulls embedding_next and
-- embed_model_next (a re-embed migration's new space must never hold a vector
-- of other content) and still ends the block's regular backfill memo.
--
-- The embed-backfill pick reads stale blocks in index ranges of their own,
-- one branch per class next to each unembedded branch (store.PendingPickSQL,
-- the shape of migration 171):
--   idx_embedding_stale_scope_sens (scope, sensitivity, created_at, id)
--   idx_embedding_stale_sens       (sensitivity, created_at, id)
-- both WHERE embedding_stale AND NOT is_archived. Neither predicate binds to
-- the embedding column, so neither is in the attnum class of the re-embed
-- cutover: no _next twin, no rename, both survive a cutover as they are.
--
-- ctx_guard_check (generation of 156) gains one candidate predicate, NOT
-- cb.embedding_stale: a stale vector stands for text its block no longer
-- holds, so it must not make a new block a near_duplicate (auto-archive at
-- the duplicate threshold) or needs_review. The block under check is never
-- stale: the guard batch's pending predicate waits for the new vector
-- (guard.guardPendingWhere). Signature, thresholds and result shape are
-- unchanged.
--
-- COLUMN. ADD COLUMN with a constant default is a catalog change (PostgreSQL
-- 11+, no table rewrite): ACCESS EXCLUSIVE for the catalog write only.
--
-- INDEX BUILD AND ITS RUNBOOK (the 171 pattern). CREATE INDEX without
-- CONCURRENTLY takes a SHARE lock on context_blocks for the build: readers go
-- on, writers wait, and a partial index still reads the whole table. The
-- runner runs each file in ONE transaction (store/migrations.go), where
-- CONCURRENTLY is impossible. Therefore, per index:
--   * A usable index of this name (btree, valid, on context_blocks, exactly
--     this key and predicate) makes this a no-op.
--   * A name taken by anything else: WARNING, nothing built.
--   * Below 500 000 rows (pg_class.reltuples) the migration builds inline.
--   * Above, WARNING, nothing built. Prepare out of band BEFORE the binary of
--     this change serves such a corpus, on the running old binary (which
--     does not read the column):
--
--       SET lock_timeout = '3s';
--       ALTER TABLE context_blocks
--           ADD COLUMN IF NOT EXISTS embedding_stale boolean NOT NULL DEFAULT false;
--       CREATE INDEX CONCURRENTLY idx_embedding_stale_scope_sens
--           ON context_blocks (scope, sensitivity, created_at, id)
--           WHERE embedding_stale AND NOT is_archived;
--       CREATE INDEX CONCURRENTLY idx_embedding_stale_sens
--           ON context_blocks (sensitivity, created_at, id)
--           WHERE embedding_stale AND NOT is_archived;
--
--     (abortable; an aborted build leaves an INVALID index behind, which is
--     dropped with DROP INDEX before the next try). Put
--     `SET lock_timeout = '3s';` ahead of the ALTER and repeat it on 55P03:
--     ACCESS EXCLUSIVE waits for every open transaction on the table (the
--     backfill arm holds one over an embed call), and every later statement
--     on the table queues behind the waiting ALTER.
--     Until both indexes exist with exactly this definition, a content change
--     nulls the vector as before this migration (store.InvalidateEmbeddingTx
--     checks both), and the pick's stale branches do not run
--     (store.PendingPickSQL gates each on its index, so a missing or foreign
--     index never turns a pick into a table walk): the backfill works as
--     before, and the schema-contract manifest stays red until the build.
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern 116/117/126/128/140/145/171):
-- the 3 s bound acquiring each lock. A transaction holding the table longer
-- (a writer, or the backfill arm of another ctx instance over an embed call)
-- fails the migration with lock_not_available (55P03), the file rolls back
-- and the boot fails loudly; the next boot tries again.
--
-- REVERT (forward-only tree; to undo temporarily): the column cannot be
-- dropped while a binary of this change runs. The indexes can:
--     DROP INDEX IF EXISTS idx_embedding_stale_scope_sens;
--     DROP INDEX IF EXISTS idx_embedding_stale_sens;
-- (content changes then null the vector again; stale blocks left from before
-- wait for the rebuild).
--
-- One column on context_blocks: the test.sh T07 column count moves by one.
-- The schema-contract manifest gains the column, the two indexes, the new
-- ctx_guard_check definition and migration_max 172 -> 173.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS embedding_stale BOOLEAN NOT NULL DEFAULT false;

DO $do$
DECLARE
    v_idx   RECORD;
    v_def   TEXT;
    v_valid BOOLEAN;
    v_rows  BIGINT;
BEGIN
    SELECT c.reltuples::BIGINT INTO v_rows
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relname = 'context_blocks';
    IF v_rows IS NULL OR v_rows < 0 THEN
        -- reltuples < 0: never analysed. A bounded count, never a full scan.
        SELECT count(*) INTO v_rows FROM (SELECT 1 FROM context_blocks LIMIT 500000) t;
    END IF;

    FOR v_idx IN
        SELECT * FROM (VALUES
            ('idx_embedding_stale_scope_sens', 'scope, sensitivity, created_at, id'),
            ('idx_embedding_stale_sens',       'sensitivity, created_at, id')
        ) AS t(name, cols)
    LOOP
        SELECT pg_get_indexdef(c.oid), i.indisvalid INTO v_def, v_valid
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
          JOIN pg_index     i ON i.indexrelid = c.oid
         WHERE n.nspname = 'public' AND c.relname = v_idx.name;

        IF FOUND THEN
            -- The definition as PostgreSQL deparses it, so a same-named index
            -- on another table, column list or predicate is not adopted.
            IF v_valid AND v_def = format(
                'CREATE INDEX %s ON public.context_blocks USING btree (%s) WHERE (embedding_stale AND (NOT is_archived))',
                v_idx.name, v_idx.cols)
            THEN
                CONTINUE;               -- usable, nothing to do
            END IF;
            RAISE WARNING '% exists but is not the stale-pick index (valid=%): %. Content changes null the vector as before this migration until it is built. Runbook in the header of 173_embedding_stale.sql: rename or drop it, then build it CONCURRENTLY; the schema-contract manifest stays red until then.',
                v_idx.name, v_valid, v_def;
            CONTINUE;
        END IF;

        IF v_rows < 500000 THEN
            EXECUTE format(
                'CREATE INDEX IF NOT EXISTS %I ON context_blocks (%s) WHERE embedding_stale AND NOT is_archived',
                v_idx.name, v_idx.cols);
        ELSE
            RAISE WARNING '% is missing at % rows: build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 173_embedding_stale.sql). Content changes null the vector as before this migration until then; the schema-contract manifest stays red.',
                v_idx.name, v_rows;
        END IF;
    END LOOP;
END
$do$;

-- ctx_guard_check, generation of 156 plus the stale-candidate predicate.
CREATE OR REPLACE FUNCTION ctx_guard_check(
    p_block_id            UUID,
    p_threshold_duplicate REAL,                   -- Pflicht (01 §3.6, kein Default)
    p_threshold_review    REAL,                   -- Pflicht (01 §3.6, kein Default)
    p_candidate_types     TEXT[],                 -- Pflicht; NULL ⇒ 0 Kandidaten (fail-closed)
    p_same_scope_only     BOOLEAN DEFAULT FALSE   -- Achse-02-Anteil (02 §4.7, K1/I-J)
)
RETURNS TABLE (
    decision        VARCHAR,
    top_similarity  NUMERIC,
    matched_id      UUID,
    matched_title   VARCHAR,
    matched_scope   VARCHAR,
    is_cross_scope  BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    v_embedding     vector(1024);
    v_scope         VARCHAR(50);
    v_tenant        UUID;
    v_matched_id    UUID;
    v_matched_title VARCHAR(255);
    v_matched_scope VARCHAR(50);
    v_similarity    NUMERIC;
BEGIN
    -- Load the block's embedding and scope
    SELECT cb.embedding, cb.scope
    INTO v_embedding, v_scope
    FROM context_blocks cb
    WHERE cb.id = p_block_id;

    -- If block not found or has no embedding, return clean with no match
    IF v_embedding IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- The tenant that owns the block's scope (Modell C: one scope, one
    -- tenant). NULL for a scope registered to no tenant ⇒ the candidate
    -- predicate below degenerates to same-scope only (fail-closed).
    SELECT ts.tenant_id INTO v_tenant
    FROM context_tenant_scopes ts
    WHERE ts.scope = v_scope;

    -- Filtered ANN (02 §4.7): iterate the HNSW graph until the predicates
    -- yield candidates instead of returning a silent false-clean. Always on
    -- since 156 — the tenant predicate filters every call. LOCAL: resets at
    -- transaction end.
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    -- Find Top-1 nearest neighbor (excluding self, excluding archived,
    -- excluding chunks, excluding stale vectors since 173) — restricted to
    -- the POLICY candidate set (p_candidate_types allowlist; NULL ⇒
    -- `= ANY(NULL)` never true ⇒ 0 candidates, fail-closed), to the OWN
    -- TENANT's scopes (156) and, for the issue axis, to the own scope.
    SELECT
        cb.id,
        cb.title,
        cb.scope,
        round(
            (1 - (cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)))::numeric,
            4
        )
    INTO v_matched_id, v_matched_title, v_matched_scope, v_similarity
    FROM context_blocks cb
    WHERE cb.id != p_block_id
      AND NOT cb.is_archived
      AND cb.embedding IS NOT NULL
      AND NOT cb.embedding_stale
      AND cb.lifecycle_state != 'chunk'
      AND cb.type_name = ANY(p_candidate_types)
      AND (NOT p_same_scope_only OR cb.scope = v_scope)
      AND (
            cb.scope = v_scope
         OR (v_tenant IS NOT NULL AND cb.scope IN (
                SELECT ts2.scope FROM context_tenant_scopes ts2 WHERE ts2.tenant_id = v_tenant))
      )
    ORDER BY cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)
    LIMIT 1;

    -- No neighbors found
    IF v_matched_id IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Apply thresholds (policy parameters — the 011 literals left the body)
    IF v_similarity >= p_threshold_duplicate THEN
        decision := 'near_duplicate';
    ELSIF v_similarity >= p_threshold_review THEN
        decision := 'needs_review';
    ELSE
        decision := 'clean';
    END IF;

    -- Determine cross-scope status
    -- Cross-scope = match is NOT in same scope AND match is NOT shared
    top_similarity := v_similarity;
    matched_id     := v_matched_id;
    matched_title  := v_matched_title;
    matched_scope  := v_matched_scope;
    is_cross_scope := (v_matched_scope != v_scope AND v_matched_scope != 'shared');

    RETURN NEXT;
    RETURN;
END;
$$;
