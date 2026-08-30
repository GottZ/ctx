-- =============================================================================
-- 150_distill_g3_decomposition.sql — G3 zerlegt nach dem ORT des Zitats
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Wissens-Ebenen, Welle C5-A (Entscheid C5-3, Checkpoint #5). Fuehrt den Befund
-- aus reports/bau/c4-r-prompt-iteration.md §15 fort.
--
-- NUMMER: Masterplan §2 K1 — "wer zuerst landet, nimmt die naechste freie".
-- Gelandet ist 149 (Reject-Histogramm), also ist 150 die naechste freie; sie ist
-- fuer diese Welle reserviert, und K1 ist geprueft (die zweite laufende Welle,
-- C5-B/N-13-Janitor, fasst die RETENTION von distill_seen/distill_run an, nicht
-- das Schema der Zaehler).
--
-- ── DER BEFUND ───────────────────────────────────────────────────────────────
-- Seit 149 ist ablesbar, WELCHES Tor die Verwuerfe produziert. Die Messung C4-R
-- hat damit den naechsten Hebel benannt: nach dem G7-Satz traegt g3 — "das Zitat
-- steht nicht woertlich in genau dem Chunk, den das Modell gesehen hat" —
-- 56,5 % der Rest-Verwuerfe (96 von 170 in Lauf 2). §15 des Berichts:
--
--   "Der Prompt adressiert das Zitat, aber nicht die Adressierung: mehrere
--    gezeigte Segmente teilen sich dieselbe block="N" und unterscheiden sich nur
--    in chunk="M". Ob das Modell falsch adressiert oder ueber Segmentgrenzen
--    zitiert, ist mit dem heutigen Histogramm nicht trennbar — beide Faelle
--    fallen in g3."
--
-- Beide Faelle verlangen entgegengesetzte Antworten: eine falsche Adresse ist
-- eine Prompt-Frage (die Marke chunk="M" binden), ein Zitat ueber eine
-- Chunk-Grenze ist eine Chunking-Frage (Ueberlappung, oder G3 auf Part-Ebene),
-- und ein Zitat, das nirgends steht, ist eine Generator-Frage. Solange die drei
-- in einer Zahl liegen, ist der naechste Schritt nicht entscheidbar.
--
-- ── WARUM VIER SPALTEN UND NICHT ZWEI ────────────────────────────────────────
-- Der operative Schnitt, den §15 vorschlaegt, lautet "Zitat im Nachbar-Chunk
-- desselben Parts vs. nirgends". Er beantwortet seine eigene Frage nur halb:
--
--   1. Ein Zitat, das ueber die NAHT zwischen Chunk M und M+1 laeuft, steht in
--      keinem einzelnen Chunk. Ein Zwei-Wege-Schnitt bucht es unter "nirgends",
--      also neben der Halluzination — von der es sich in der Abhilfe
--      unterscheidet. Die Chunks eines Parts setzen sich byte-identisch zum
--      Part-Body zusammen (ctxcheckpoint/parse.go:121-125), das Zitat stand also
--      sehr wohl im Material.
--   2. "Nirgends im gezeigten Material" ist nur wahr, wenn das GANZE gezeigte
--      Material durchsucht wurde. Ein Zitat aus Part 3, adressiert als Part 2,
--      ist ebenfalls ein Adressierungsfehler; es als Halluzination zu buchen
--      waere eine falsche Messung, keine grobe.
--
-- Vier Spalten, vier verschiedene naechste Schritte. Die Zuordnung ist exklusiv
-- und wird in der PRAEZEDENZ "kleinster Adressierungsfehler zuerst" entschieden
-- (distill_extract.go, distillG3Index.classify): erst falscher Chunk-Index, dann
-- ueberschrittene Chunk-Grenze desselben Parts, dann falsche Part-Nummer, dann
-- nichts davon.
--
-- ── WAS DIESE MIGRATION IST ──────────────────────────────────────────────────
-- Vier additive Zaehlerspalten auf distill_run, alle NOT NULL DEFAULT 0, kein
-- Index, kein Trigger, kein Constraint auf Bestandszeilen, keine Datenzeile
-- angefasst. Sie sind die ZERLEGUNG einer Zahl, die schon dasteht: rej_g3, das
-- seinerseits ein Achtel der Zerlegung von insights_rejected ist. Die Zeilen der
-- C4-R-Laeufe behalten ihr rej_g3 und tragen eine Null-Zerlegung — sie stammen
-- aus einer Zeit ohne dieses Instrument, und ein Backfill waere eine erfundene
-- Messung. Aus demselben Grund traegt auch der C4-R-Vergleich die neuen Spalten
-- erst ab dem naechsten Lauf.
--
-- ── WARUM KEIN CHECK AUF DIE SUMME ───────────────────────────────────────────
-- Die Invariante rej_g3_chunk + rej_g3_span + rej_g3_part + rej_g3_none = rej_g3
-- gilt exakt: distillGate klassifiziert JEDE Zeile, die es unter "g3" bucht, in
-- genau einen der vier Eimer, und classify hat keinen Rueckgabewert ausserhalb
-- der vier. Sie steht hier trotzdem NICHT als CHECK, aus den drei Gruenden, die
-- 149 fuer dieselbe Frage nennt und die hier unveraendert gelten:
--
--   1. Bestandszeilen verletzen sie per Konstruktion (rej_g3 > 0, Zerlegung = 0).
--      Ein CHECK muesste zur schwachen Form "<=" abgeschwaecht werden.
--   2. Genau die schwache Form faengt den Fehlerfall NICHT, der hier zaehlt: eine
--      Zerlegung, die still auf null stehen bleibt, weil die Verdrahtung reisst.
--   3. Ein verletzter CHECK toetet den GANZEN Lauf ueber einen Buchungsfehler in
--      einer reinen Beobachtungsgroesse.
--
-- Die Invariante wird dort durchgesetzt, wo sie den stillen Nullfall faengt: als
-- Gate-Sonde ueber den echten Schreibpfad
-- (internal/events/distill_g3class_integration_test.go, Sonde 2, die BEIDE
-- Gleichungen in einer Ablesung prueft).
--
-- ── KOSTEN ───────────────────────────────────────────────────────────────────
-- Vier INTEGER-Spalten mit nicht-volatilem DEFAULT: PostgreSQL schreibt die
-- Tabelle dafuer seit 11 nicht neu (die Defaults landen in
-- pg_attribute.attmissingval), der ADD COLUMN ist ein Katalog-Eintrag. Kein
-- Leser bekommt eine neue Index-Last, weil kein Index entsteht — die Auswertung
-- ist ein sum() ueber einen Zeitraum.
--
-- Die LAUFZEIT-Kosten stehen im Arm, nicht hier, und sie fallen nur an, wo schon
-- verworfen wird: die Normalisierung des gezeigten Materials wird je Call
-- HOECHSTENS EINMAL gebaut und nur dann, wenn der Call ueberhaupt einen
-- G3-Verwurf hatte (distill_extract.go, distillNewG3Index, lazy). Ein sauberer
-- Call zahlt nichts.
--
-- Additiv, forward-only, IF NOT EXISTS (Muster 119/130/135/149). Ein Re-Run ist
-- folgenlos.
-- =============================================================================

SET LOCAL lock_timeout = '2s';

-- ── Die Zerlegung von G3 (§4.3, das Tor G3) ──────────────────────────────────
-- Die Schluessel sind die des Arms (distillG3Keys). Sie beschreiben den ORT des
-- Zitats im gezeigten Material und NIE seinen Inhalt: die verworfenen Texte
-- verlassen den Arm nicht (distill_extract.go, Kopf von distillGate), und ein
-- Zaehler ueber Orte haelt diese Haltung.
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g3_chunk INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g3_span  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g3_part  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE distill_run ADD COLUMN IF NOT EXISTS rej_g3_none  INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN distill_run.rej_g3_chunk IS
    'G3-Ort: das Zitat steht woertlich in einem ANDEREN Chunk DESSELBEN Parts. Adressierungsfehler — chunk="M" ist falsch, das Material ist echt.';
COMMENT ON COLUMN distill_run.rej_g3_span IS
    'G3-Ort: das Zitat steht in keinem einzelnen Chunk, aber woertlich in einem zusammenhaengenden Lauf der gezeigten Chunks des adressierten Parts — es laeuft ueber eine Chunk-Grenze. Chunking-Frage, keine Generator-Frage.';
COMMENT ON COLUMN distill_run.rej_g3_part IS
    'G3-Ort: das Zitat steht woertlich in einem FREMDEN Part desselben Calls. Adressierungsfehler auf der block-Marke statt auf der chunk-Marke.';
COMMENT ON COLUMN distill_run.rej_g3_none IS
    'G3-Ort: das Zitat steht nirgends im Material dieses Calls — weder in einem Chunk noch ueber einer Naht, in keinem Part. Der Halluzinations-Eimer. sum(rej_g3_chunk, rej_g3_span, rej_g3_part, rej_g3_none) = rej_g3.';
