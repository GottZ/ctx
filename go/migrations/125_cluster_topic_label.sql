-- =============================================================================
-- 125_cluster_topic_label.sql — Label-Spalten auf graph_cluster_topic
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Getrennt von 124, weil Identität und Label getrennt deploybar sind: 124+W3
-- liefern eine identitätsstabile Karte ohne Label-Änderung, 125+W5/W6 legen das
-- Label darauf. Zwei Migrationen statt einer, damit jede Welle einzeln
-- rückrollbar-durch-Nichtnutzung bleibt.
--
-- LABEL-QUELLEN-VOKABULAR (label_source), Rangfolge von stark nach schwach:
--   'manual'   — vom Menschen gesetzt; von KEINEM Automatik-Pfad überschrieben
--   'llm'      — von der Label-Pipeline erzeugt (Rolle digest)
--   'fallback' — deterministisch aus Tags/Kategorien/Repräsentant, in der
--                persist-Tx geschrieben; die Garantie "die Karte ist nie leer"
--   'none'     — Zustand vor dem ersten Rebuild nach dieser Migration
-- Der Automatik-Pfad schreibt NUR über 'fallback' und 'none'; 'llm' wird nur
-- von der Label-Pipeline selbst ersetzt, 'manual' von niemandem. 'manual' steht
-- hier bereits im Vokabular, obwohl der Schreib-Endpoint eine spätere Welle ist
-- (E5-01): der Automatik-Pfad muss den Wert respektieren können, bevor ihn
-- jemand setzen kann — sonst wäre das Pinning beim Einbau ein Schema-Nachzug
-- mitten in einer laufenden Label-Pipeline.
--
-- label_core_hash ist der Drift-Anker: die Pipeline labelt genau dann neu, wenn
-- graph_cluster_node.core_hash von label_core_hash abweicht. Fließende
-- Mitgliedschaft im RAND des Clusters kostet damit KEINEN LLM-Call — der
-- Substanz-Kern (E4-01) flattert am semantisch leichtesten Rand zuerst.
--
-- label_stale ist die MATERIALISIERTE Form dieses Vergleichs. Der Vergleich
-- selbst (label_core_hash IS DISTINCT FROM n.core_hash) ist ein Prädikat über
-- ZWEI Tabellen und damit nicht index-adressierbar; als OR-Zweig neben
-- label_source IN ('none','fallback') zwänge er die Selektion in einen
-- Seq-Scan über graph_cluster_topic — die Tabelle, in der Grabsteine mit
-- lebenden Topics koexistieren, und der Scan liefe pro Intervall, pro Tenant.
-- label_stale wird in DERSELBEN persist-Tx gesetzt, in der core_hash
-- geschrieben wird (W5 fasst die Zeile ohnehin an, der Zusatz kostet keine
-- eigene Passe). DEFAULT true: eine Zeile, die noch nie einen Kern-Vergleich
-- gesehen hat, ist per Definition label-bedürftig — fail-closed in Richtung
-- "labeln", nicht in Richtung "übersehen".
--
-- KEIN Confidence-Feld. Projekt-Empirie (Session 24, dream v3): die
-- Selbsteinschätzung des Modells ist als Gate unbrauchbar. Die Validierung ist
-- rein strukturell (nicht leer, <= 120 Runen, keine Kontroll-Token) und lebt im
-- Code, nicht in einer Zahl vom Modell. label_model hält fest, WELCHES Modell
-- das Label geschrieben hat — Provenienz pro Topic, nicht auf dem Overview-Wire
-- (E4-02/E6-01).
--
-- lock_timeout (Muster 116/119): ADD COLUMN nullable bzw. mit konstantem
-- DEFAULT auf einer Tabelle in Cluster-Größenordnung; seit PG11 schreibt der
-- DEFAULT nicht in die Heap-Tuples, es gibt also keinen Table-Rewrite.
-- Idempotent: ADD COLUMN IF NOT EXISTS; die CHECKs per DO-Block guarded (ein
-- nacktes ADD CONSTRAINT bräche beim zweiten Lauf mit 42710);
-- CREATE INDEX IF NOT EXISTS. Der Runner hält die Datei in EINER Tx
-- (store/migrations.go:97-115), SET LOCAL ist damit selbst-revertierend.
-- Forward-only. Additive Spalten ⇒ test.sh-Tabellenzahl UNVERÄNDERT (51);
-- Manifest trotzdem regenerieren (neue Spalten + neuer Index).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE graph_cluster_topic
    ADD COLUMN IF NOT EXISTS label           TEXT,
    ADD COLUMN IF NOT EXISTS label_source    TEXT        NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS label_built_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS label_core_hash TEXT,
    ADD COLUMN IF NOT EXISTS label_model     TEXT,
    ADD COLUMN IF NOT EXISTS label_attempts  INT         NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS label_stale     BOOLEAN     NOT NULL DEFAULT true;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'gct_label_source_vocab') THEN
        ALTER TABLE graph_cluster_topic
            ADD CONSTRAINT gct_label_source_vocab
            CHECK (label_source IN ('none','fallback','llm','manual'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'gct_label_len') THEN
        -- 120 Runen wie repr_title (057). char_length zählt Zeichen, nicht
        -- Bytes — dieselbe Rune-Genauigkeit, die digest.truncateTitle gegen
        -- SQLSTATE 22021 herstellt (Issue #4). Ein 120-Umlaut-Label wiegt 240
        -- Bytes und muss trotzdem passen.
        ALTER TABLE graph_cluster_topic
            ADD CONSTRAINT gct_label_len
            CHECK (label IS NULL OR (char_length(label) BETWEEN 1 AND 120));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'gct_label_present') THEN
        -- label_source <> 'none' verlangt ein Label. Verhindert den Zustand
        -- "als gelabelt markiert, aber leer" — der wäre in der Wurzel-Map eine
        -- unsichtbare Lücke statt eines sichtbaren Fehlers.
        ALTER TABLE graph_cluster_topic
            ADD CONSTRAINT gct_label_present
            CHECK (label_source = 'none' OR label IS NOT NULL);
    END IF;
END $$;

-- Der Selektions-Index der Label-Pipeline. Das Index-Prädikat deckt GENAU die
-- Selektionsbedingung ab: lebend, nicht manuell gepinnt. Die Spaltenliste deckt
-- die beiden verbleibenden Filter (label_stale, label_attempts) plus den
-- Scope-Schnitt ab; die Kaltstart-Sortierung nach n.size ist ein Join-Attribut
-- und bleibt ein Sort. Kein OR-Zweig, kein tabellenübergreifendes Prädikat,
-- keine Grabstein-Zeile im Index.
CREATE INDEX IF NOT EXISTS idx_gct_label_pending
    ON graph_cluster_topic (scope, label_stale, label_attempts)
    WHERE retired_at IS NULL AND label_source <> 'manual';

COMMENT ON COLUMN graph_cluster_topic.label_source IS
    'Herkunft des Labels: manual > llm > fallback > none. Der Automatik-Pfad schreibt nur über fallback und none; manual überschreibt niemand (E5-01).';
