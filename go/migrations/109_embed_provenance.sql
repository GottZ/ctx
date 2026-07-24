-- 109_embed_provenance.sql
-- Achse 04 W04-1: repariert die tote Provenienz-Spur, BEVOR die
-- Re-Embed-Statemachine (W04-3+) auf ihr aufbaut.

-- (1) embed_model: Default fällt — der Wert wird ab jetzt vom Writer gesetzt.
--     Semantik neu: "Modell-String, der den AKTUELLEN Vektor erzeugt hat; NULL = kein Vektor".
ALTER TABLE context_blocks ALTER COLUMN embed_model DROP DEFAULT;

-- (2) Bestandsdaten-Backfill. Ehrlichkeits-Fußnote: das Label behauptet
--     Raum-Zugehörigkeit unter der Äquivalenz-Annahme des Engine-Wechsels
--     2026-06-10 (Ollama->llama.cpp, gleiches Modell Q4_K_M, kein Re-Embed).
--     Vektoren von davor stammen von einem Backend mit String
--     'qwen3-embedding:8b'; die Annahme "gleicher Raum" ist seither
--     Betriebsgrundlage und wird hier fortgeschrieben, nicht neu bewiesen.
--     Deploy-Zeitpunkt-Annahme: zwei Full-Table-UPDATEs in einer Tx sind beim
--     Klein-Korpus (~1,8k Zeilen) trivial; fällt das Deploy-Fenster wider
--     Erwarten erst bei >=1M Zeilen, MÜSSEN diese UPDATEs vorher auf
--     Batch-UPDATEs (z.B. 50k-Chunks per ctid-Range, eigene Txen) umgeschnitten
--     werden.
UPDATE context_blocks SET embed_model = 'qwen3-embedding-8b' WHERE embedding IS NOT NULL;
UPDATE context_blocks SET embed_model = NULL                 WHERE embedding IS NULL;

-- (3) Tote Objekte: embed_status (nie von Go-Code beschrieben; DDL-Default
--     'done' seit 001) und sein Partial-Index.
DROP INDEX IF EXISTS idx_embed_pending;
ALTER TABLE context_blocks DROP COLUMN IF EXISTS embed_status;

-- (4) Tragender Pending-Index für beide Backfill-Pfade + künftigen
--     Migrations-Scan (K2: dieser Index gehört Achse 04):
CREATE INDEX IF NOT EXISTS idx_embedding_pending
    ON context_blocks (created_at)
    WHERE embedding IS NULL AND NOT is_archived;
