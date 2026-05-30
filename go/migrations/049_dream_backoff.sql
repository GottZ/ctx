-- 049_dream_backoff.sql
-- Wave-2 (W49c): gradual back-off cooldown for the Dream re-dream loop.
--
-- The effective re-dream interval grows with the number of completed eval cycles
-- per block (dream_eval_count). Mature/legacy blocks (dreamed many times) are
-- pulled progressively less often, while NEW blocks (count 0) keep catching up
-- at the base rate. Curve shape (log/exp/linear), factor, grace, and cap are
-- runtime-configurable; see SetDreamCooldown. Transient GPU/LLM failures use the
-- minutes-cooldown path and do NOT increment the count, so the back-off reflects
-- real completed work only.
--
-- dream_eval_count is back-filled from the existing dream-eval LLM log so legacy
-- blocks enter the long-cooldown regime immediately on deploy. The deploy
-- timestamp is the "history split": pre/post back-off behavior is cleanly
-- separated in context_llm_log for the 1-month STEP-0 re-measurement.

ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS dream_eval_count INTEGER NOT NULL DEFAULT 0;

-- Back-fill from completed dream-eval cycles. The source block is block_ids[1]
-- (EvaluateRelationships logs source first, then candidates). Guarded so a fresh
-- install without context_llm_log still applies cleanly (column stays at 0).
DO $$
BEGIN
    IF to_regclass('public.context_llm_log') IS NOT NULL THEN
        UPDATE context_blocks b
        SET dream_eval_count = sub.cnt
        FROM (
            SELECT block_ids[1] AS sid, count(*)::int AS cnt
            FROM context_llm_log
            WHERE pipeline = 'dream-eval'
              AND error IS NULL
              AND block_ids IS NOT NULL
              AND array_length(block_ids, 1) >= 1
            GROUP BY block_ids[1]
        ) sub
        WHERE b.id = sub.sid;
    END IF;
END $$;
