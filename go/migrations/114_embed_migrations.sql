-- 114_embed_migrations.sql
-- Achse 04 W04-3: Modell-Registry + Re-Embed-Migrations-Statemachine (Daten-
-- Seite) + Dual-Spalten-Paar für den Zwei-Phasen-Cutover + FK-Nachzug auf
-- die W04-2-Tabelle (113_embed_failures.sql). Design: design/04-reembed-
-- migration.md §3.2b. Go-Seite derselben Welle: Paket internal/embedmigration
-- (Statemachine + create-Validierung) + ClearEmbedding-_next-Erweiterung
-- (blocks.go).

-- (1) Modell-Registry — Clean-Room-Gegenstück zu pgContext
--     register_model_version/model_versions (MPL-2.0 Konzeptübernahme, kein
--     Code-Port, User-Entscheid 2026-07-22). model_key == der Backend-
--     Modell-String (chain[0].ModelFor(role).Model) == Cache-Key-Bestandteil.
--     Quantisierung/Serving-Varianten, die den Vektor-Raum ändern, MÜSSEN im
--     String unterscheidbar sein (design §5 Bruchpfad 5) — sonst kollidieren
--     zwei Räume im selben model_key und der Cache-Key sowie die Provenienz
--     (Migration 109) belügen sich gegenseitig.
CREATE TABLE IF NOT EXISTS context_embed_models (
    model_key     TEXT PRIMARY KEY,
    family        TEXT NOT NULL,               -- z. B. 'qwen3-embedding'
    native_dims   INT  NOT NULL,                -- z. B. 4096
    stored_dims   INT  NOT NULL,                -- z. B. 1024 (Matryoshka-Truncation)
    matryoshka    BOOLEAN NOT NULL DEFAULT false,
    quantization  TEXT,                         -- z. B. 'Q4_K_M'
    notes         TEXT,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Bestandsdaten-Zeile: das kanonische Live-Label seit Migration 109 (der
-- Zeitpunkt, ab dem embed_model wahrheitsgemäß gepflegt wird). Ehrlichkeits-
-- Fußnote wie in 109 bereits dokumentiert: umfasst per Betriebs-Annahme
-- 2026-06-10 auch die Ollama-Ära 'qwen3-embedding:8b' (kein Re-Embed beim
-- Engine-Wechsel Ollama→llama.cpp, gleiches Modell + Quantisierung).
INSERT INTO context_embed_models (model_key, family, native_dims, stored_dims, matryoshka, quantization, notes)
VALUES ('qwen3-embedding-8b', 'qwen3-embedding', 4096, 1024, true, 'Q4_K_M',
        'Kanonisches Bestandslabel seit M109; umfasst per Betriebs-Annahme 2026-06-10 auch die Ollama-Ära qwen3-embedding:8b (kein Re-Embed beim Engine-Wechsel).')
ON CONFLICT (model_key) DO NOTHING;

-- (2) Migrations-Zeilen — Clean-Room-Gegenstück zu
--     create_embedding_migration/embedding_migrations. Der Vektor-Raum ist
--     global und scope-frei, deshalb trägt diese Tabelle keinen scope/tenant.
CREATE TABLE IF NOT EXISTS context_embed_migrations (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    from_model        TEXT NOT NULL REFERENCES context_embed_models(model_key),
    to_model          TEXT NOT NULL REFERENCES context_embed_models(model_key),
    -- to_backend: context_backends.name. Create-Validierungs-Anker (§4.2 —
    -- Existenz, Locality local, global-scoped, model_map-Key 'embed_next'→
    -- to_model), danach informativ/Audit. Laufzeit-Enforcement ist der
    -- Model-Guard (aufgelöster String, W04-4), NICHT diese Spalte — Failover-
    -- Ketten mit mehreren to_model-Backends (§6.2) bleiben damit zulässig.
    to_backend        TEXT NOT NULL,
    mode              TEXT NOT NULL DEFAULT 'dual' CHECK (mode IN ('dual')),   -- 'inplace' bewusst NICHT in v1 (E-04-1)
    status            TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending','running','paused','verifying','done','aborted','rolled_back')),
    total_blocks      BIGINT NOT NULL DEFAULT 0, -- Snapshot bei Start (Basis der arithmetischen Pending-Ableitung, §6.3)
    migrated_count    BIGINT NOT NULL DEFAULT 0, -- Batch-aktualisiert, §6.3
    failed_count      BIGINT NOT NULL DEFAULT 0,
    skipped_count     BIGINT NOT NULL DEFAULT 0, -- oversize / sensitivity_ineligible (§4.4)
    cursor_created_at TIMESTAMPTZ,             -- persistenter Peek-Cursor (High-Water, Wrap-around pro Durchlauf, §4.3)
    verify_started_at TIMESTAMPTZ,             -- Watermark: Vollständigkeits-Scope des Verify/Confirm (§4.7)
    verify_report     JSONB,
    last_error        TEXT,                    -- normalisiert wie context_embed_failures.last_error (§3.2a)
    abort_reason      TEXT,
    rollback_reason   TEXT,                    -- Pflicht beim Übergang done→rolled_back (§4.10)
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at        TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    CHECK (from_model != to_model)
);

-- Invariante: höchstens EINE aktive Migration systemweit (der Vektor-Raum
-- ist global, scope-frei — anders als context_backends/context_blocks trägt
-- diese Tabelle keine Scope-Dimension, die den Index partitionieren könnte).
CREATE UNIQUE INDEX IF NOT EXISTS idx_embed_migration_single_active
    ON context_embed_migrations ((true))
    WHERE status IN ('pending','running','paused','verifying');

-- (3) FK-Nachzug auf die W04-2-Tabelle (dort noch nicht möglich —
--     context_embed_migrations existierte in Migration 113 noch nicht,
--     design §3.2a/§3.2b). "ADD CONSTRAINT IF NOT EXISTS" gibt es in
--     PostgreSQL nicht — Idempotenz-Guard via pg_constraint-Lookup im
--     DO-Block, NOT VALID + separates VALIDATE hält den ADD selbst kurz
--     (kein Full-Table-Scan unter dem Lock, der bei 10M relevant würde).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'fk_embed_failures_migration'
    ) THEN
        ALTER TABLE context_embed_failures
            ADD CONSTRAINT fk_embed_failures_migration
            FOREIGN KEY (migration_id) REFERENCES context_embed_migrations(id) ON DELETE CASCADE
            NOT VALID;
    END IF;
END $$;
ALTER TABLE context_embed_failures VALIDATE CONSTRAINT fk_embed_failures_migration;

-- (4) Dual-Spalten-Paar für den Zwei-Phasen-Cutover (§4.5 Option c). Permanent
--     angelegt (NULL kostet nichts), kein Runtime-DDL beim Migrations-Start.
--     KEIN HNSW-Index auf embedding_next hier — der wird erst in der
--     Verify-Phase per CREATE INDEX CONCURRENTLY gebaut (W04-5, §4.7 Stufe 3),
--     damit der Backfill nicht Millionen inkrementelle Index-Inserts bezahlt.
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS embedding_next   vector(1024);
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS embed_model_next TEXT;
ALTER TABLE context_blocks ALTER COLUMN embedding_next SET STORAGE EXTERNAL;
