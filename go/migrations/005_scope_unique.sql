-- 005_scope_unique.sql — Replace (category, title) unique constraint with scope-aware partial unique index
-- Prevents cross-scope overwrites: a work-key upsert no longer collides with private blocks

-- Drop the old constraint (which was a unique index under the hood)
ALTER TABLE context_blocks DROP CONSTRAINT IF EXISTS uq_context_category_title;
DROP INDEX IF EXISTS uq_context_category_title;

-- Create scope-aware partial unique index (only non-archived blocks)
CREATE UNIQUE INDEX IF NOT EXISTS uq_context_category_title_scope
    ON context_blocks (category, title, scope) WHERE NOT is_archived;
