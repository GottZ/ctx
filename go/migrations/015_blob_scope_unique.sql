-- 015_blob_scope_unique.sql — Replace (category, title) unique constraint with scope-aware unique index
-- Prevents cross-scope overwrites: a work-key upsert no longer collides with private blobs

-- Drop the old constraint
ALTER TABLE context_blobs DROP CONSTRAINT IF EXISTS context_blobs_category_title_key;

-- Create scope-aware unique index
CREATE UNIQUE INDEX IF NOT EXISTS uq_blobs_category_title_scope
    ON context_blobs (category, title, scope);
