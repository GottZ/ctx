-- =============================================================================
-- 091_dispatch_telemetry.sql — Dispatch-Telemetrie im LLM-Log (Vorhaben E, MW10/A5-W4)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Persistiert die Lease-Telemetrie des Admission-Layers (internal/dispatch)
-- pro llmlog-Zeile (design/05 §3.1/§3.2):
--   queue_wait_ms  — Lease-Wartezeit des zeilen-prägenden Attempts (admitted −
--                    enqueued). 0 ist ein ECHTER Messwert (Sofort-Admission,
--                    auch im Durchreiche-Zustand) und wird als 0 persistiert,
--                    nie zu NULL gedroppt (B-R4 — sonst wäre jede p95-
--                    Auswertung nach oben verzerrt). NULL = Zeile aus
--                    Vor-Verdrahtungs-Zeit oder lease-freier Sonderpfad.
--   dispatch_class — 'interactive' | 'background' (vom Caller gebundene
--                    Admissions-Klasse der Sequenz). NULL = Vor-Verdrahtung.
--   dispatch_abort — 'preempted' | 'reaped' (Dispatcher-Abbruch eines
--                    laufenden Attempts) | 'acquire_expired' | 'queue_full'
--                    (K9-Abweis-Zeile: nie-admittierter background-Acquire,
--                    duration_ms NULL — kein physischer Call). NULL = kein
--                    Dispatcher-Eingriff (auch bei gewöhnlichen Fehlern).
--                    Klassen-Invariante: nur background-Zeilen tragen je einen
--                    Wert (I-D1: interactive wird nie dispatcher-gecancelt).
-- KEIN CHECK-Constraint auf die Wertemengen (Bestandsmuster backend_trust/
-- required_sensitivity: freie text-Spalten); die Vokabulare pinnt ein Go-Test
-- (llmlog/chain-Testgates, A5-W4).
--
-- Hypertable-Kosten (B-R6): alle drei Spalten nullable OHNE Default ⇒ reine
-- Katalog-Operation ohne Chunk-Rewrite auf der TimescaleDB-Hypertable;
-- Bestands-Zeilen bleiben byte-gleich lesbar (Integrations-Probe auf chunk-
-- bestückter Test-Hypertable). Der Partial-Index folgt exakt dem Muster
-- idx_llm_log_error (M025): Abbruch-/Abweis-Ereignisse sind selten,
-- "Preemptions/Starvation-Abweise pro Tag/Ziel" müssen am 1M+-Ziel-Scale
-- trotzdem ohne Chunk-Vollscan zählbar sein.
--
-- lock_timeout (R-MIG2): ADD COLUMN nullable ohne Default ist katalog-only;
-- der Runner wickelt jede Migration in eine eigene Transaktion, SET LOCAL ist
-- transaktions-gebunden und selbst-revertierend.
-- Idempotent: IF NOT EXISTS überall; _migrations ON CONFLICT DO NOTHING.
-- Forward-only. Additive Spalten, keine neue Tabelle → test.sh table count
-- UNCHANGED. EvictBodies NULLt die neuen Spalten bewusst NICHT (Telemetrie
-- überlebt die Body-Retention, Bestands-Doktrin llmlog.go).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_llm_log
    ADD COLUMN IF NOT EXISTS queue_wait_ms  INTEGER,
    ADD COLUMN IF NOT EXISTS dispatch_class TEXT,
    ADD COLUMN IF NOT EXISTS dispatch_abort TEXT;

CREATE INDEX IF NOT EXISTS idx_llm_log_dispatch_abort
    ON context_llm_log (created_at DESC) WHERE dispatch_abort IS NOT NULL;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (91, '091_dispatch_telemetry.sql', now()) ON CONFLICT (version) DO NOTHING;
