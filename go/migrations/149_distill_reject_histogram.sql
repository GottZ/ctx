-- =============================================================================
-- 149_distill_reject_histogram.sql — Reject-Histogramm + Gruppen-Verkleinerungen
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Wissens-Ebenen, Welle C4-1 (Entscheid E5-1, Checkpoint #4). Behebt Befund N-6
-- aus reports/bau/c3-3-re-pilot.md §10.
--
-- NUMMER: Masterplan §2 K1 — "wer zuerst landet, nimmt die naechste freie".
-- Gelandet ist 148 (write.internal_only), also ist 149 die naechste freie; sie
-- ist fuer diese Welle reserviert, und K1 ist geprueft (keine zweite
-- Migrations-Welle laeuft parallel).
--
-- ── DER BEFUND ───────────────────────────────────────────────────────────────
-- Der Destillat-Arm hat ein Abbruchkriterium, das an der Verwurfsrate haengt
-- (design/02 §4.8; zweimal ausgeloest, zuletzt bei einer Verankerungs-Rate von
-- 0,3780 gegen eine Schwelle von 0,80). Welche TORE die Verwuerfe produzieren,
-- war im Betrieb nicht ablesbar: die Stufenzaehler g1…g7 verlassen
-- distillOneCall ausschliesslich als slog.Debug, und cmd/ctxd/main.go pinnt den
-- Handler auf slog.LevelInfo. Dieselbe Luecke hat die C3-1-Call-Planung: der
-- Rune-Meter loggt per WARN, DASS er bremst, aber die Verkleinerung einer
-- Gruppe nur per Debug. Damit war nicht entscheidbar, ob eine Prompt-Iteration
-- wirkt oder nur die Fehlklassen tauscht.
--
-- ── WARUM SPALTEN UND NICHT EIN LOG-SCHALTER ─────────────────────────────────
-- Ein Env-Schalter auf den slog-Level haette die Zeilen sichtbar gemacht, aber
-- nicht ZAEHLBAR. Drei konkrete Gruende, nicht der Aufwand:
--
--   1. KEINE ZUORDNUNG. Die Debug-Zeile fuehrt kein run_id
--      (distill_extract.go:1169-1171). Ein Verwurf liesse sich weder einem Lauf
--      noch einem Wasserzeichen-Bereich noch einer Quelle zuordnen; die
--      Auswertung waere ein Zeitstempel-Abgleich ueber nebenlaeufige Quellen
--      (max_sessions_per_run > 1 laesst mehrere Laeufe im selben Tick).
--   2. KEINE AGGREGATION. Die Frage der Folge-Welle lautet "welches Tor hat
--      sich zwischen zwei Prompt-Fassungen bewegt" — das ist ein sum() ueber
--      einen Zeitraum, keine Log-Suche.
--   3. FLUECHTIGKEIT. Das Journal ist DB-persistent (heute unbegrenzt:
--      distill.retention_days ist deklariert und validiert, hat aber keinen
--      Konsumenten im Baum — Befund N-13, die Retention-Welle steht aus,
--      135_distill_run.sql:53); Log-Zeilen leben so lange wie der Container.
--      Ein Vorher/Nachher ueber zwei Prompt-Fassungen braucht die aeltere
--      Haelfte noch.
--
-- Die Zahlen gehoeren damit in die Zeile, die der Lauf ohnehin schreibt.
--
-- ── WAS DIESE MIGRATION IST ──────────────────────────────────────────────────
-- Neun additive Zaehlerspalten auf distill_run, alle NOT NULL DEFAULT 0, kein
-- Index, kein Trigger, kein Constraint auf Bestandszeilen, keine Datenzeile
-- angefasst. Sie sind die ZERLEGUNG einer Zahl, die schon dasteht:
-- insights_rejected. Bestehende Zeilen behalten ihre Aggregat-Zahl und tragen
-- ein Null-Histogramm — sie stammen aus einer Zeit ohne Instrument, und ein
-- Backfill waere eine erfundene Messung.
--
-- ── WARUM KEIN CHECK AUF DIE SUMME ───────────────────────────────────────────
-- Die Invariante sum(rej_g1..rej_g7) + rej_schema = insights_rejected gilt
-- exakt (distillDecode setzt offered = len(lines), distillOneCall bucht
-- res.rejected += offered - len(kept), und jede verworfene Zeile faellt in
-- genau einen Eimer). Sie steht hier trotzdem NICHT als CHECK, aus drei
-- Gruenden:
--
--   1. Bestandszeilen verletzen sie per Konstruktion (Aggregat > 0, Histogramm
--      = 0). Ein CHECK muesste zur schwachen Form "<=" abgeschwaecht werden.
--   2. Genau die schwache Form faengt den Fehlerfall NICHT, der hier zaehlt:
--      ein Histogramm, das still auf null stehen bleibt, weil die
--      Verdrahtung reisst. 0 <= n ist immer wahr — der CHECK waere Zusicherung
--      ohne Deckung, also schlimmer als keiner.
--   3. Ein verletzter CHECK toetet den GANZEN Lauf ueber einen Buchungsfehler
--      in einer reinen Beobachtungsgroesse. Das Journal ist das Instrument,
--      nicht der Wirkpfad.
--
-- Die Invariante wird stattdessen dort durchgesetzt, wo sie den stillen
-- Nullfall faengt: als Gate-Sonde ueber den echten Schreibpfad
-- (internal/events/distill_reject_n6_integration_test.go, Sonde 2).
--
-- ── KOSTEN ───────────────────────────────────────────────────────────────────
-- Neun INTEGER-Spalten mit nicht-volatilem DEFAULT: PostgreSQL schreibt die
-- Tabelle dafuer seit 11 nicht neu (die Defaults landen in
-- pg_attribute.attmissingval), der ADD COLUMN ist ein Katalog-Eintrag. Die
-- Tabelle liegt ohnehin in der Groessenordnung "einige tausend Zeilen/Jahr je
-- Quelle" (§6.2). Kein Leser bekommt eine neue Index-Last, weil kein Index
-- entsteht — die Auswertung ist ein sum() ueber einen Zeitraum, also ohnehin
-- ein Scan ueber die Zeilen dieses Zeitraums.
--
-- Additiv, forward-only, IF NOT EXISTS (Muster 119/130/135). Ein Re-Run ist
-- folgenlos.
-- =============================================================================

SET LOCAL lock_timeout = '2s';

-- ── Das Reject-Histogramm (§4.3, die Tore G1-G7) ─────────────────────────────
-- Die Schluessel sind die des Arms (distillGate), NICHT die der
-- derived.CiteGate: dort gibt es zusaetzlich ein G0 ("die Quelle steht in
-- provenance.source_block_ids"), und das Tor hat der Arm nicht — er loest
-- gegen `shown` auf, die einzige Autoritaet dafuer, was eine prompt-lokale
-- Adresse bedeutet hat. Ein rej_g0 waere eine Spalte, die per Konstruktion
-- immer 0 bliebe.
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g1 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g2 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g3 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g4 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g5 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g6 INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g7 INTEGER NOT NULL DEFAULT 0;

-- Der achte Eimer ist KEIN Tor, sondern der Parser: Zeilen, die distillDecode
-- abgewiesen hat (unbekanntes Feld, Pflichtfeld leer, Steuerzeichen in claim
-- oder quote). Er gehoert ins selbe Histogramm, weil insights_rejected ihn
-- mitzaehlt — ohne ihn waere die Summe nicht die Zerlegung dieser Zahl.
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_schema INTEGER NOT NULL DEFAULT 0;

-- ── Die Call-Planung (C3-1 Teil B) ───────────────────────────────────────────
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS call_groups_shrunk INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN distill_run.rej_g1 IS
    'G1: (block, chunk) ist kein Paar DIESES Calls — die Adresse stand nicht im Prompt.';
COMMENT ON COLUMN distill_run.rej_g2 IS
    'G2: das Zitat liegt unter derived.MinQuoteRunes (32 Runen).';
COMMENT ON COLUMN distill_run.rej_g3 IS
    'G3: das Zitat steht nicht woertlich in genau dem Chunk, den das Modell gesehen hat.';
COMMENT ON COLUMN distill_run.rej_g4 IS
    'G4: claim oder quote spricht als Prompt-Struktur (promptguard.Neutralize hat gebrochen).';
COMMENT ON COLUMN distill_run.rej_g5 IS
    'G5: der Credential-Scan schlaegt auf claim oder quote an (zwei getrennte Scans).';
COMMENT ON COLUMN distill_run.rej_g6 IS
    'G6: das Zitat ist Plugin-Boilerplate oder besteht in der Substanz aus Redaktions-Marken.';
COMMENT ON COLUMN distill_run.rej_g7 IS
    'G7: die Aussage traegt den Chunk nicht (Art, lexikalische Deckung, Imperativ-Negativliste).';
COMMENT ON COLUMN distill_run.rej_schema IS
    'Kein Tor, sondern der Parser: von distillDecode abgewiesene Zeilen (unbekanntes Feld, leeres Pflichtfeld, Steuerzeichen). sum(rej_g1..rej_g7) + rej_schema = insights_rejected.';
COMMENT ON COLUMN distill_run.call_groups_shrunk IS
    'Wie oft die Call-Planung eine Gruppe auf den Rest-Platz des Blocks verkleinert hat (min(rows_per_call, room())). Zaehlt EREIGNISSE, nicht eingesparte Zeilen — die Frage ist, wie oft der Cap die Yield-Achse gesteuert hat.';
