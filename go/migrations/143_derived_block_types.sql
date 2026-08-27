-- =============================================================================
-- 143_derived_block_types.sql — Block-Typen 'insight' + 'catalog'
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Wissens-Ebenen, Welle W01-Seed. design/01 §3.3/§3.4 (die beiden
-- Registry-Zeilen), §4.1/§4.2 (Feld-für-Feld-Herleitung), §7 W01-2 + W01-3.
--
-- NUMMER: Masterplan §2 K1 — "wer zuerst landet, nimmt die nächste freie".
-- Gelandet sind 141 (checkpoint-untrusted) und 142 (arms_typename), also ist
-- 143 die nächste freie. Die Design-Texte führen die beiden Zeilen noch als
-- "140 (vorläufig)" und "141 (vorläufig)"; verbindlich ist die Nummer hier.
--
-- WAS DIESE MIGRATION IST: reine Registry-DATEN. Zwei Zeilen in
-- context_block_types, kein Schema, kein Index, kein Trigger, kein Schreiber.
-- Kein Bestandsblock trägt heute einen der beiden Typen und keiner trägt einen
-- der beiden Titel-Anker (Live-Negativ-Probe 2026-08-27 über 7 829 Blöcke:
-- 0 Treffer für '%session insights %', 0 für '%katalog #%', 0 Blöcke mit
-- type_name IN ('insight','catalog')). Sie ist damit forward-only und braucht
-- keinen Datenrückbau — anders als 107, die ihren Bestand noch umtypisieren
-- und entarchivieren musste.
--
-- WARUM BEIDE ZEILEN IN EINER MIGRATION:
-- Masterplan K2. Der Design-Text schneidet W01-2 (insight) und W01-3 (catalog)
-- als zwei Wellen; die Konflikt-Auflösung legt sie zusammen, weil beide
-- Registry-Zeilen dieselben Zähler-Pins (9 -> 11) und dasselbe
-- schemacontract-Manifest anfassen. manifest.json ist nicht merge-bar, und
-- zwei Migrations-Wellen parallel sind nach K1 ohnehin verboten — der
-- Zwei-Wellen-Schnitt hätte den Zähler zweimal in Folge angefasst, ohne dass
-- zwischen den beiden Schritten irgendetwas prüfbar gewesen wäre.
--
-- WARUM ZWEI TYPEN UND NICHT EINER (design/01 §4.1):
-- Die beiden Populationen verhalten sich gegensätzlich. 'insight' hängt an
-- Wurzel-Session × Wasserzeichen — streng monoton, stirbt nie, append-only,
-- jede Generation ein neuer Titel, hoher Fremdtext-Anteil (Transkript,
-- Werkzeug-Ausgabe). 'catalog' hängt an einem Cluster-Topic — das driftet,
-- stirbt und verschmilzt —, wird upsert-stabil überschrieben und destilliert
-- bereits kuratierte Korpus-Blöcke. Ein gemeinsamer Typ müsste ein gemeinsames
-- untrusted-Flag und eine gemeinsame Classify-Priorität tragen. Das ist genau
-- das Argument, mit dem 136 'tool-evidence' und 'tool-overview' getrennt hat.
--
-- WARUM retrieval = 'excluded' UND NICHT 'damped':
-- Masterplan K7, auf dem Decision-Board als E-4 bestätigt. Der Erstentwurf
-- (design/01 §3.3/§3.4) sah damped mit 0,50 bzw. 0,60 vor; D-02 schlug 0,35
-- vor, D-03 'excluded bis zur Messung'. Aufgelöst zugunsten von 'excluded' für
-- BEIDE Typen bis zu den Piloten (X-W4/X-W5), danach ein reines
-- Registry-Daten-Update auf den GEMESSENEN Faktor aus dem Sweep M-W8 über
-- {0,25…1,0}. Die Zahlen 0,35/0,50/0,60 sind Sweep-Kandidaten, keine
-- Startwerte — und deshalb steht hier KEIN damping_factor: ein Faktor auf einer
-- excluded-Zeile wäre heute wirkungslos und würde in dem Moment still zum
-- Startwert, in dem jemand die Policy umstellt.
-- 'excluded' und 'damped' sind beide reversible Datenstellungen (M107/M136-
-- Doktrin: ohne Deploy änderbar); 'full-pass' wäre es nicht.
-- Nebeneffekt, der die Welle deploybar macht: p_types_visible bleibt
-- byte-identisch, also ändert sich an ctx_rrf und am Ranking heute nichts.
--
-- WARUM DIE intent_patterns TROTZDEM SCHON HIER STEHEN:
-- Set.DampedTypesFor liest ausschließlich damped-Typen, die Listen sind also
-- inert, solange die Zeilen excluded sind. Sie stehen trotzdem in der Zeile,
-- damit die spätere E-4-Schaltung eine EIN-FELD-Datenänderung über eine bereits
-- GEMESSENE Musterliste ist und nicht der Moment, in dem zum ersten Mal jemand
-- über Muster nachdenkt. Gemessen wird gegen die 47 eval-Fragen
-- (internal/blocktype/derived_types_test.go, Fixture testdata/) — Zielrate
-- < 10 %.
-- WARUM DIESE MUSTER UND NICHT DIE GENERISCHEN: DampedTypesFor hebt den Typ
-- VOLLSTÄNDIG aus den Damping-Arrays, sobald rrf.MatchesAny trifft — es gibt
-- keine Teil-Anhebung —, und MatchesAny ist ein case-insensitiver
-- SUBSTRING-Test. 'katalog' allein trifft 'Katalogisierung', 'insight' trifft
-- 'insights' in jedem englischen Titel. Es bleiben Mehrwort- und Fachformen.
--
-- WARUM retrieval.untrusted BEI 'insight' true UND BEI 'catalog' false:
-- 'insight' destilliert Transkript- und Werkzeug-Material. Die 138er-Doktrin
-- wörtlich: "Summarising attacker-shapable output does not launder it." Ein
-- Insight-Block ist Fremdtext zweiter Ordnung und erbt die Rahmung; 141 hat
-- 'checkpoint' genau deshalb geflaggt, damit diese Ebene die Eigenschaft AN
-- IHRER QUELLE lesen kann. 'catalog' destilliert Korpus-Blöcke, die jemand als
-- Wissen aufgeschrieben hat — der Default false ist hier richtig, und eine
-- einzelne untrusted Quelle wird PRO BLOCK über einen Metadata-Marker gerahmt
-- (design/01 §4.8.3), nicht durch Umstellen des Typs.
-- Beide Werte stehen EXPLIZIT in der Zeile. Absent und false dekodieren gleich;
-- der explizite Schlüssel ist das, worüber ein späterer Sammel-Backfill in der
-- 138/141-Form (Existenz-Guard `NOT (config->'retrieval' ? 'untrusted')`)
-- bewusst hinwegsteigen müsste, statt 'catalog' beiläufig mitzunehmen.
-- Auf einer excluded-Zeile ist das Flag inert, aber gültig (policy.go:
-- "rejecting it would break the one order an operator actually uses").
--
-- WARUM guard.check = false UND guard.candidate = false:
-- Beidseitig, zwei getrennte Felder mit zwei getrennten Konsumstellen — eines
-- allein schließt nur eine Richtung. candidate=false verhindert, dass ein
-- Derivat als Guard-Kandidat sein ORIGINAL archiviert (stiller Datenverlust am
-- Original, jeder Lesepfad filtert NOT is_archived). check=false verhindert,
-- dass ein Derivat SICH SELBST archiviert und seine eigene Regeneration
-- verwaisen lässt — aufeinanderfolgende Generationen derselben Session sind
-- Near-Duplicates by construction, das ist die Form des dangling-manifest-
-- Vorfalls vom 2026-07-20. Präzedenz: 107 und 136.
--
-- WARUM guard.mode = 'archive', OBWOHL ES NIE GREIFEN KANN:
-- Mit check=false erreicht kein Derivat je applyDecision, das Feld ist also
-- toter Text. Es steht trotzdem da, und zwar mit dem Wert 'archive', weil
-- applyDecision bei UNBEKANNTEM oder LEEREM mode ausdrücklich archiviert
-- ("never silently degrades to flag-only"). Das Feld wegzulassen hieße nicht
-- "kein Archivieren", sondern "Archivieren über den Default-Pfad". Ein toter
-- Text, der die richtige Richtung zeigt, falls jemand check später versehentlich
-- einschaltet.
--
-- WARUM dream.linkable = false:
-- Zweiseitig wirksam (Pick-Quelle und Kandidaten-Sieb). Als Quelle würde ein
-- Derivat Links erzeugen, die den Louvain-Input formen — und Louvain liest
-- AUSSCHLIESSLICH context_dream_links, also genau die Partition, aus der der
-- Katalog entsteht. Als Ziel würde es Originale an sich binden. Der
-- system-meta-Seed trägt dieselbe Begründung und eine Zahl: Meta-Blöcke
-- verursachten 85 % des NO_REL-Rauschens.
--
-- WARUM digest.include = false:
-- Der Digest scannt context_blocks OHNE LIMIT und OHNE Cursor und markiert das
-- selbst als Skalierungsproblem; live steht digest.mode='stub'. Eine Ebene in
-- einen Scan zu hängen, der am Ziel-Scale ohnehin umgebaut werden muss, ist
-- eine Schuld ohne Gegenwert.
--
-- WARUM overview.include = false — DIE WICHTIGSTE ZEILE:
-- Ein Katalog ENTSTEHT aus der Topic-Partition. Stünde er in den
-- Overview-Typen, ginge er in den nächsten Cluster-Lauf ein und die Partition
-- würde aus sich selbst abgeleitet. Der Schnitt, der das entscheidet, ist
-- intersect(VisibleTypes, OverviewTypes) — solange die Typen excluded sind,
-- schließt schon die erste Menge sie aus; overview.include=false ist die
-- Zusicherung, die auch NACH der E-4-Sichtbarkeitsschaltung noch trägt. Der
-- Wächter dazu ist negativ geprobt (derived_types_test.go,
-- derived_types_integration_test.go), gegen genau die eine Zeile, die ein
-- Operator per psql umstellen könnte.
--
-- WARUM parent.mode = 'none':
-- parent_id ist ON DELETE CASCADE — ein gelöschter Quellblock würde alle
-- Derivate mitreißen —, und ein Derivat hat N Quellen, während parent_id genau
-- eine trägt. Damit ist auch 'aggregate-to-parent' als Retrieval-Politik für
-- diese Ebene ausgeschlossen: nicht weil der Fold schlecht wäre, sondern weil
-- seine Identitäts-Voraussetzung (EIN Parent) hier nicht existiert.
--
-- WARUM PRIORITÄT 17 UND 16 — UNTER audit-trail (20):
-- Set.Classify läuft AUFSTEIGEND nach (priority, name) und nimmt den ERSTEN
-- Treffer. 'audit-trail' trägt 'session' als Titelmuster (plus 'welle',
-- 'baseline', 'reset'), und der Insight-Titel lautet
-- "Session insights <root_session_id> ab #<watermark>" — bei Priorität > 20
-- gewänne 'audit-trail' und schickte den Block in Guard UND Dream, also in
-- genau die zwei Pipelines, die diese Typen schließen. Gemessen am
-- unveränderten Baum, 2026-08-27: der Anker klassifiziert heute nach
-- 'audit-trail'. Die Umkehrung ist risikofrei, weil die Titelmuster
-- Mehrwort-Ketten sind: 'session insights ' (mit abschließendem Leerzeichen)
-- und 'katalog #' kommen in keinem echten audit-trail-, checkpoint- oder
-- Tool-Typ-Titel als Substring vor. 16 vor 17, weil 'catalog' am Ziel-Scale die
-- häufigere Population ist; die Muster sind disjunkt, die Reihenfolge
-- untereinander also folgenlos.
-- KEINE classify.metadata_flags: 'is_meta' ist die einzige belegte
-- Metadata-Regel im System und gehört 'system-meta'. Eine zweite wäre ein
-- zweiter stiller Umtypisierungs-Pfad — und ein Derivat, das nach 'system-meta'
-- kippt, wird excluded und verliert sein Embedding, ohne dass irgendetwas rot
-- wird.
--
-- CLASSIFY IST DAS NETZ, NICHT DER PRIMÄRPFAD:
-- Der abgeleitete Schreiber ruft UpsertBlock mit explizitem typeName, was
-- type_source='manual' setzt; ClassifyBlockAfterUpsert trägt
-- `AND type_source = 'auto'` und fasst den Block nie wieder an. Die
-- title_patterns fangen einen Schreiber, der den Typ vergisst. Beide Wege
-- werden getrennt geprobt (internal/store/derived_manual_integration_test.go).
--
-- ENVELOPE 'v': 1 IST PFLICHT: DecodePolicy prüft ihn VOR jeder anderen
-- Validierung. Eine Seed-Row ohne das Feld wäre bei jedem Registry-Reload ein
-- Corrupt-Config-Event, der Typ lüde nie, und /health fiele auf
-- blocktype_registry='builtin-fallback' zurück.
--
-- LOCKSTEP MIT internal/blocktype/builtin.go: die beiden Configs hier MÜSSEN
-- byte-äquivalent zum compiled-in Builtin-Set dekodieren.
-- TestRegistryGolden_Integration appliziert die echte Migrationskette aus
-- migrations.FS und diff't die Rows gegen builtinPolicies();
-- TestDerivedSeedsMatchBuiltin macht dasselbe containerfrei gegen die
-- Config-Literale dieser Datei. Ein Drift in beide Richtungen ist rot. Anders
-- als bei 136 gibt es hier KEINE Ausnahme-Normalisierung: 143 seedet den
-- Endzustand, ein einziges abweichendes Feld ist ein Fehler.
--
-- WAS DIESE MIGRATION NICHT TUT — UND WARUM SIE UNDEPLOYT BLEIBT:
-- Sie legt keine Schreib-Sperre an. Zwischen einer gelandeten Registry-Zeile
-- und der Sperre (Welle W01-2a: derived.StratumOf(name) > 0 => 422 in
-- validateTypeNameAgainstSet, reservierte Kategorien => 403,
-- Provenienz-Guard auf dem Konflikt-Zweig) ist der Typ CLIENT-BEANSPRUCHBAR:
-- 'type' ist auf beiden Schreib-Oberflächen ein Client-Feld, und die Validierung
-- prüft ausschließlich Registry-Zugehörigkeit. Ein Client könnte also
-- type='catalog' schreiben und bekäme Guard-Ausnahme, type_source='manual' und
-- Derivat-Optik geschenkt. Deshalb bleibt diese Migration bis zum nächsten
-- RC-Schnitt UNDEPLOYT und W01-2a landet vor jedem Deploy — das Fenster
-- existiert im Repo, nie auf dem Live-System.
--
-- ON CONFLICT (name, scope) DO NOTHING = idempotent (M107-Doktrin): ein Re-Run
-- überschreibt nie das Tuning eines Operators, der einen Faktor oder ein Muster
-- in der laufenden Registry nachgezogen hat.
-- =============================================================================

SET LOCAL lock_timeout = '2s';

INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('insight', '_global', 'Session-Insight', true, false, '{
  "v": 1,
  "retrieval": {"policy": "excluded",
                "untrusted": true,
                "intent_patterns": ["session insight","sitzungs-erkenntnis","was haben wir gelernt",
                                    "erkenntnisse der session","was lief schief","was ist passiert",
                                    "lessons learned","befunde der sitzung"]},
  "guard":     {"check": false, "candidate": false, "mode": "archive", "candidates": "all"},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "classify":  {"priority": 17,
                "title_patterns": ["session insights "]}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;

INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('catalog', '_global', 'Cluster-Katalog', true, false, '{
  "v": 1,
  "retrieval": {"policy": "excluded",
                "untrusted": false,
                "intent_patterns": ["katalog","überblick über","übersicht über","worum geht es bei",
                                    "was gibt es zu","themenübersicht","welche themen",
                                    "gib mir einen überblick"]},
  "guard":     {"check": false, "candidate": false, "mode": "archive", "candidates": "all"},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "classify":  {"priority": 16,
                "title_patterns": ["katalog #"]}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;
