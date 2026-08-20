-- =============================================================================
-- 132_embed_cache_coupled_fingerprint.sql — der Boot-Abgleich des Embed-Caches
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Entflechtung Stufe 2, Achse 04 (Konsumenten & Test-Vehikel), Welle A04-W4.
-- design/04 §3.2b + §7 Zeile 4. Fortsetzung von A04-W3 (Mig-frei):
-- internal/events/listener.go difft seit W3 die Coupled-Menge — die Menge der
-- (host, protocol)-Paare über serving-eligible embed/dream-embed-Rows — nach
-- jedem Pool-Reload und flusht context_embed_cache bei Änderung.
--
-- WARUM EINE PERSISTIERTE ZEILE UND KEIN REINER IN-MEMORY-VERGLEICH:
-- Der W3-Diff lebt im NOTIFY-Trichter. Der Trichter sieht NUR Writes, die bei
-- laufendem ctxd passieren. Eine Pool-Row, die im Wartungsfenster per psql
-- editiert wird (ctxd gestoppt), erzeugt beim nächsten Boot keinen NOTIFY —
-- die Baseline wird aus genau diesem editierten Pool genommen, difft leer, und
-- der Cache serviert die Vektoren des ALTEN Hosts unter unverändertem
-- Modellnamen weiter (design/04 §5.1 R5: cosine 0.997 != 1.0, stumme
-- Retrieval-Degradation, Evict nur über Recency). Der Fingerprint ist der
-- einzige Zeuge, der ein Stopp/Start überlebt.
--
-- EINE ZEILE, KEIN JOURNAL: Der Fingerprint ist ZUSTAND, kein Verlauf — beim
-- Boot gelesen, nach jedem erfolgreichen Flush ersetzt. Muster und Formgeber
-- ist graph_overview_meta (Mig 057:67): singleton BOOLEAN PRIMARY KEY mit
-- CHECK, damit eine zweite Zeile schema-seitig unmöglich ist statt nur
-- konventionell unerwünscht.
--
-- coupled_fingerprint IST NULLABLE, UND DAS IST DIE ERST-BOOT-SEMANTIK:
-- Weder Zeile noch Wert werden hier geseedet. Fehlende Zeile ODER NULL heißt
-- "auf dieser Installation nie gestempelt" — der Boot seedet dann den
-- Fingerprint und flusht NICHT (User-Entscheid E12 = design/04 §8 E-A04-4
-- Variante b). Ein Erst-Boot-Flush (Variante a) hätte auf JEDER Installation
-- des Upgrades einen vollständigen Cache-Warmup erzwungen, um die Minderheit
-- mit historisch stale Cache zu heilen. Diese Minderheit heilt stattdessen ein
-- dokumentierter Einmal-Schritt (docs/operations.md, "Migration 132"):
--
--     DELETE FROM context_embed_cache;   -- einmalig, wenn der Embed-Host je gewechselt hat
--
-- Ab dem geseedeten Stempel ist das Offline-Fenster deterministisch zu.
--
-- KEIN BACKFILL. Ein Bestands-Fingerprint wäre nicht rekonstruierbar: welche
-- (host, protocol)-Menge die Vektoren im Cache geschrieben hat, steht nirgends
-- — context_embed_cache keyed auf (text_hash, model) und trägt keine Herkunft.
-- Ein geratener Stempel wäre schlimmer als keiner: er behauptete Deckung.
--
-- Additiv, forward-only, CREATE TABLE IF NOT EXISTS — gefahrloser Re-Run
-- (Muster 119/130). SET LOCAL lock_timeout, obwohl eine NEUE Tabelle keine
-- fremden Locks braucht: der Runner fährt die Datei in einer Transaktion, und
-- die COMMENT-Strecke unten berührt dieselbe Relation (Muster 130).
--
-- ⚠ EINE TABELLE KOMMT: test.sh T07_EXPECT_TABLES 55 → 56 nachgezogen und das
--   Schema-Contract-Manifest regeneriert
--   (go test ./internal/schemacontract -tags=genmanifest -run TestGenerateManifest).
-- =============================================================================

SET LOCAL lock_timeout = '2s';

CREATE TABLE IF NOT EXISTS context_embed_cache_meta (
    -- Muster graph_overview_meta (057:67): der CHECK macht die zweite Zeile
    -- unmöglich, nicht bloß unerwünscht.
    singleton           BOOLEAN     PRIMARY KEY DEFAULT true CHECK (singleton),

    -- sha256-hex über die sortierte Coupled-Menge (Version + Paar-Trenner im
    -- Hash-Input, internal/events/coupled_fingerprint.go). NULL = nie
    -- gestempelt ⇒ Erst-Boot seedet ohne Flush (E12 / E-A04-4 Variante b).
    coupled_fingerprint TEXT,

    -- Anzahl der Paare im gestempelten Stand. Rein diagnostisch: der Vergleich
    -- läuft NUR über den Hash. Beantwortet beim Nachsehen "war der Pool leer,
    -- als gestempelt wurde?" ohne den Hash rückwärts lesen zu müssen.
    coupled_pair_n      INTEGER     NOT NULL DEFAULT 0,

    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE context_embed_cache_meta IS
    'Einzeiliger Zustand des Embed-Cache-Coupled-Abgleichs (Mig 132, design/04 §3.2b). Beim Boot gegen die Coupled-Menge des frisch geladenen Backend-Pools geprüft: Mismatch ⇒ context_embed_cache flushen (deckt Pool-Edits bei gestopptem ctxd), Gleichstand ⇒ nichts. Fortgeschrieben NUR nach erfolgreichem Flush.';

COMMENT ON COLUMN context_embed_cache_meta.coupled_fingerprint IS
    'sha256-hex über die sortierten (host, protocol)-Paare aller serving-eligiblen embed/dream-embed-Backends des zuletzt erfolgreich geflushten bzw. geseedeten Stands. NULL = nie gestempelt: der nächste Boot seedet ohne Flush (E-A04-4 Variante b) — Bestands-Altlasten räumt der einmalige Runbook-Schritt DELETE FROM context_embed_cache.';

COMMENT ON COLUMN context_embed_cache_meta.coupled_pair_n IS
    'Anzahl der Paare im gestempelten Stand — diagnostisch; der Abgleich läuft ausschließlich über coupled_fingerprint.';
