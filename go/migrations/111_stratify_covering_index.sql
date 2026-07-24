-- 111_stratify_covering_index.sql
-- Achse 01 W01-1b (K3): partieller Covering-Index für die
-- Stratifizierungs-/loo-Zugriffe der recall_check-Probe (Achse 01) und —
-- read-only mitgenutzt — die künftige Kardinalitäts-Schätzung des
-- Strategy-Selektors (Achse 02). Ohne ihn ist die per-Scope-Zählung
-- embeddeter aktiver Blöcke am Ziel-Scale ein Full-Heap-Scan
-- (~14 GB @10M); mit ihm ein Index-Only-Scan über (scope, type_name).
-- Eigentum: Achse 01 (K3) — Achse 02 legt KEINEN eigenen Index an.

CREATE INDEX IF NOT EXISTS idx_blocks_stratify_covering
    ON context_blocks (scope, type_name)
    WHERE NOT is_archived AND embedding IS NOT NULL;
