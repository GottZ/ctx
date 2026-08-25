-- =============================================================================
-- 136_tool_evidence_block_types.sql — Block-Typen 'tool-evidence' + 'tool-overview'
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 02, Welle W02-2. design/02 §3.1 (Evidenz-Achse) + design/02a §3
-- (Dimension-Splits, Übersichts-Achse).
--
-- NUMMER: Masterplan §2 K1 — "wer zuerst landet, nimmt die nächste freie".
-- Gelandet sind 134 (ctx_rrf Gen 16) und 135 (distill_run), also ist 136 die
-- nächste freie. Der Design-Text führt die Evidenz-Migration noch als "134
-- (vorläufig)"; verbindlich ist die Nummer hier.
--
-- WAS DIESE MIGRATION IST: reine Registry-DATEN. Zwei Zeilen in
-- context_block_types, kein Schema, kein Index, kein Schreiber. Kein
-- Bestandsblock trägt heute einen der beiden Typen (Live-Negativ-Probe T-08,
-- 2026-08-25: 0 Blöcke mit 'compaction checkpoint tool index', 0 mit
-- 'compaction tool overview' im Titel), also ist sie forward-only und braucht
-- keinen Datenrückbau — anders als 107, die ihren Bestand noch umtypisieren
-- und entarchivieren musste.
--
-- WARUM EIGENE TYPEN UND NICHT 'checkpoint':
-- Der naheliegende Weg wäre, die Tool-Evidenz unter den bestehenden
-- checkpoint-Typ zu hängen — beides sind Compaction-Artefakte desselben
-- Schreibers. Das geht nicht: checkpoint trägt retrieval='excluded'. Ein
-- excluded-Typ steht nicht in p_types_visible, bekommt keinen Embed-Slot
-- (der Backfill überspringt ihn) und ist damit über die Frage
-- "welches Kommando war das nochmal?" NICHT auffindbar. Genau diese
-- Auffindbarkeit ist der ganze Zweck der Evidenz-Achse. checkpoint ist
-- ID-verankert (Auflösung ausschließlich über exakte Block-IDs), diese beiden
-- Typen sind FRAGE-verankert. Zwei verschiedene Auflösungsarten sind zwei
-- verschiedene Typen.
--
-- WARUM ZWEI TYPEN UND NICHT EINER:
-- Die beiden Populationen verhalten sich gegensätzlich. 'tool-evidence' wächst
-- mit jeder Compaction weiter (Rollout-Schätzung 580-1 900+ Blöcke je Bestand)
-- und ist near-duplicate BY CONSTRUCTION: dieselben Werkzeuge, ähnliche
-- Kommandos, überlappende Fenster. 'tool-overview' ist upsert-stabil — ein
-- Block je Achse je Wurzel-Session — und bleibt damit klein. Ein gemeinsamer
-- Typ müsste einen gemeinsamen Dämpfungsfaktor tragen und würde die Übersicht
-- für die Flutung der Evidenz mitbestrafen.
--
-- WARUM DAMPING 0.15 BZW. 0.35 UND NICHT full-pass:
-- Bei 1M+ Blöcken je Tenant (Zielskala) flutet eine konstruktionsbedingt
-- near-duplicate Population die Kandidatenmenge und läuft das Slot-Fenster des
-- Rerankers über — dieselbe Mechanik, die 107 bei den Transkript-Teilen zur
-- vollständigen Ausschluss-Entscheidung gezwungen hat. 0.15 ist schärfer als
-- das historische audit-trail-Maß 0.3 (builtin.go, auditTrailDamping), weil die
-- Near-Duplicate-Dichte hier höher liegt. Für 'tool-overview' gilt das
-- Flutungs-Argument NICHT (upsert-stabil, kleine Population), deshalb 0.35 —
-- knapp über der audit-trail-Linie, in derselben Größenordnung.
--
-- WARUM DIESE intent_patterns UND NICHT DIE GENERISCHEN:
-- Set.DampedTypesFor (set.go:169-183) hebt den Typ VOLLSTÄNDIG aus den
-- Damping-Arrays, sobald rrf.MatchesAny trifft — es gibt keine Teil-Anhebung —
-- und MatchesAny ist ein case-insensitiver SUBSTRING-Test (rrf/pattern.go:21-35).
-- Generische Einzelwörter ('tool', 'output', 'exit', 'command', 'shell',
-- 'terminal', 'failed') treffen damit 'toolchain', 'exiting', 'PowerShell' und
-- machen die Dämpfung in einem technischen Korpus wirkungslos. Es bleiben
-- Mehrwort- und Fachformen. Die Falsch-Lift-Rate ist gemessen, nicht behauptet:
-- W02-2 Gate 4 fährt die 47 eval-Fragen gegen DampedTypesFor
-- (internal/blocktype/tool_evidence_test.go, Fixture testdata/) — Zielrate
-- < 10 %, gemessen 0/47 für beide Typen. Registry-DATEN: die Listen sind ohne
-- Deploy nachziehbar, wenn der Livebetrieb ein Muster nachlegt.
--
-- WARUM guard.check=false UND guard.candidate=false:
-- Identische Begründung wie bei 'checkpoint' (107): aufeinanderfolgende
-- Indizes derselben Wurzel-Session sind Near-Duplicates, der Guard würde sie in
-- der Default-Archive-Bahn (0.98/0.92) still wegarchivieren, und weil jeder
-- Lesepfad NOT is_archived filtert, bräche die ID-Kette Manifest -> Index. Das
-- ist die Form des dangling-manifest-Vorfalls vom 2026-07-20. dream.linkable,
-- digest.include und overview.include sind aus demselben Grund false: die
-- Population ist Evidenz, kein Wissen — sie gehört in keine autonome Pipeline.
--
-- WARUM PRIORITÄT 19 UND 18 — UNTER audit-trail (20), NICHT ÜBER checkpoint (30):
-- Set.Classify (set.go:277) läuft AUFSTEIGEND nach (priority, name) und nimmt
-- den ERSTEN Treffer. Ein älterer Design-Stand sah 25 vor, also zwischen
-- audit-trail und checkpoint. Das wäre falsch: der Titel trägt den Namen der
-- Wurzel-Session, und sobald der 'session', 'audit', 'baseline' oder 'reset'
-- enthält — alles audit-trail-Titelmuster und alles gängige Session-Namen —
-- würde audit-trail bei Priorität 25 zuerst matchen und den Block in Guard UND
-- Dream schicken, also in genau die zwei Pipelines, die dieser Typ schließt.
-- Bei 19/18 gewinnen die Tool-Typen. Die Umkehrung ist risikofrei, weil die
-- Titelmuster hier lange Mehrwort-Ketten sind: 'compaction checkpoint tool
-- index' und 'compaction tool overview' können in keinem echten Audit-Trail-
-- Titel und in keinem checkpoint-Titel als Substring vorkommen. 19 vor 18, weil
-- die Evidenz-Kette die häufigere Population ist; die beiden Muster sind
-- disjunkt, die Reihenfolge untereinander also folgenlos.
--
-- ENVELOPE 'v': 1 IST PFLICHT: DecodePolicy prüft ihn VOR jeder anderen
-- Validierung (policy.go:326-328, Fehlerklasse "unsupported or missing envelope
-- version (want v=1)"). Eine Seed-Row ohne das Feld wäre bei jedem
-- Registry-Reload ein Corrupt-Config-Event, der Typ lüde nie, und /health fiele
-- auf blocktype_registry='builtin-fallback' zurück. W02-2 Gate 1 probt genau
-- das gegen diese Datei.
--
-- LOCKSTEP MIT internal/blocktype/builtin.go: die beiden Configs hier MÜSSEN
-- byte-äquivalent zum compiled-in Builtin-Set decodieren.
-- TestRegistryGolden_Integration appliziert die echte Migrationskette aus
-- migrations.FS und diff't die Rows gegen builtinPolicies() (Drift-Gate,
-- design/01 §4.1 R1); TestToolSeedsMatchBuiltin macht dasselbe ohne Container
-- gegen die Config-Literale dieser Datei. Ein Drift in beide Richtungen ist rot.
--
-- ON CONFLICT (name, scope) DO NOTHING = idempotent (M107-Doktrin): ein Re-Run
-- überschreibt nie das Tuning eines Operators, der einen Faktor oder ein Muster
-- in der laufenden Registry nachgezogen hat.
-- =============================================================================

SET LOCAL lock_timeout = '2s';

INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('tool-evidence', '_global', 'Tool-Evidence', true, false, '{
  "v": 1,
  "retrieval": {"policy": "damped", "damping_factor": 0.15,
                "intent_patterns": ["tool index","tool output","tool call","tool-ausgabe",
                                    "stderr","stdout","exit code","exit-code","traceback",
                                    "kommando","befehl","kommandozeile",
                                    "welches kommando","welcher befehl",
                                    "terminal-ausgabe","shell-befehl"]},
  "guard":     {"check": false, "candidate": false, "mode": "archive", "candidates": "all"},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "classify":  {"priority": 19,
                "title_patterns": ["compaction checkpoint tool index"]}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;

INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('tool-overview', '_global', 'Tool-Overview', true, false, '{
  "v": 1,
  "retrieval": {"policy": "damped", "damping_factor": 0.35,
                "intent_patterns": ["tool overview","werkzeug-übersicht","welche werkzeuge",
                                    "welche kommandos","welche befehle","welche dateien",
                                    "tool-fehler","fehlgeschlagene calls","artefakt-übersicht",
                                    "was wurde bearbeitet"]},
  "guard":     {"check": false, "candidate": false, "mode": "archive", "candidates": "all"},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "classify":  {"priority": 18,
                "title_patterns": ["compaction tool overview"]}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;
