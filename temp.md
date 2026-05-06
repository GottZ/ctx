# Session-Übergabe — Autonom-Welle 41+ (post-Welle-40-HOLD)

**Diese Datei nach dem Lesen löschen** (`rm /compose/n8n/temp.md`).

Drift-resistent. Pflicht-Lesen + State-Snapshot + Welle-40-HOLD-Status + Folge-Welle-41-Plan + Closure-Pflicht.

## Pflicht-Lesen (in dieser Reihenfolge, vor erster Welle)

1. **Warnings** — `/root/.claude/projects/-compose-n8n/memory/warnings.md`
   - W3, W6a-d, W9, W10, W11, W12, W13, W14
   - 14-Punkt-Self-Audit nach jeder Welle
2. **MEMORY.md** — `/root/.claude/projects/-compose-n8n/memory/MEMORY.md` — "Aktueller Stand" S36 → S35
3. **ctx-Blöcke (in dieser Reihenfolge)**:
   - `019dff8b-f05e-799b-9721-1a2cadc337b7` — Welle 40 HOLD (jüngste, is_meta=TRUE)
   - `019dff4b-5849-79a3-ac23-1de0408eec8e` — Welle 39 + 38b PROMOTE v1.2.0
   - `019dfeca-15f6-7534-b752-fd00b907e304` — Welle 38b implementiert
   - `019d3f5a-0f6a-72f4-9ac3-3be356abee47` — S12 Generative-Synthese-Design (Welle 38c, jetzt machbar)
4. **Welle-40-Artefakte**: `.project/bench-dream-phase0/`
   - `BRANCH-HYPOTHESIS-Welle-40-Klassifikation.md` (Architektur C Decision)
   - `welle-40-sql-audit.json` + `welle-40-classification-audit.json` (Pre-Empirie)
   - `eval-cyclic-w40-it3-damping03.json` + `it4-damping05.json` + `it5-damping10.json`
   - `welle-40-self-audit.md` (14-Warning Self-Audit)

## Production-State-Snapshot (verifiziert 2026-05-07 ~01:08 CEST)

**Re-verifizieren wenn > 7 Tage Abstand ODER > 10 Block-Differenz** (Trigger unten).

| Komponente | Stand |
|---|---|
| Hauptrepo HEAD | `d121df5` (7 Welle-40-Commits ahead von 00126cd, KEIN Tag) |
| Branch | `root` |
| Letzter Tag | **v1.2.0** annotated (Welle 39, unverändert) |
| Submodule .project | post-update (siehe Submodule pointer commit) |
| DB Migrations | **38** (M035 schema + M036/M037/M038 ctx_rrf-iter) |
| Dream Version | **5** (recurrent-Klasse aktiv) |
| Active blocks | **449** (297 knowledge + 121 reference + 19 system-meta + 12 audit-trail) |
| Hooks-Pfad | `/compose/n8n/.hooks` (aktiv) |
| Production-Performance | identisch zu v1.2.0 (verified Iter-5 mean_pass=0.9428) |

**Re-Verifikations-Trigger** (eines davon → state.sh + diff against snapshot):
- Tag-Push seit v1.2.0
- DB-Migration-Count > 38
- > 10 Block-Differenz vs aktueller 449 active

## Welle 40 Endstand: HOLD

**Outcome**: kein v1.3.0-Tag. Production-Performance unverändert. Architektur-Schema deployed.

**Was deployed ist**:
- M035: block_role 4-Klassen-Enum + Backfill (system-meta=is_meta-state, reference=cat=reference, audit-trail=12 hardcoded IDs Sub-Agent-B-confirmed, rest=knowledge)
- M036/M037/M038: ctx_rrf iterations (damping 0.3 → 0.5 → 1.0). M038 = current (no-op damping)
- Filter via `block_role != 'system-meta'` (semantisch identisch zu Welle 39 M033 `NOT is_meta` nach M035-Backfill)

**Was rejected wurde**: Uniform damping (0.3, 0.5) führt -7pp regression durch 5 NEG flips bei explicit-audit-trail-target queries. 0/70 cases mit unterschiedlichem top5 zwischen 0.3 und 0.5 → Damping-Faktor-Tuning ist nicht der Hebel.

**Pre-existing Bug gefixt**: M031+ Migrations recordeten sich selbst, Apply-Code machte 2. INSERT ohne ON CONFLICT → duplicate-key in testdb. Fix: ON CONFLICT (version) DO NOTHING im Apply-Code (commit 9eb5bc9). Production unverändert.

## Folge-Welle 41+ Plan

### Welle 41 — Query-aware Damping (PRIMÄRES Ziel)

**Anlass**: Welle 40 HOLD-Lehre — uniform damping ist fundamental ineffektiv. Bei den 5 lost cases (L-002/L-009/M-003/M-004/M-015) ist audit-trail-rank 1 und knowledge-rank 2-5; selbst 0.5x-damping dämpft audit-trail unter knowledge.

**Hypothese**: damping abhängig von query-content. Wenn query enthält Pattern wie "session"/"welle"/"audit" als terms → kein damping (audit-trail erscheint normal). Sonst damping (audit-trail unter knowledge).

**Pre-Empirie-Pflicht (W3)**:
1. Query-Pattern-Audit: welche der 70 eval-cyclic-cases haben "session"/"welle"/"audit" als query-tokens?
2. Sub-Agent-Klassifikation: welche cases sind "explicit-audit-target" (expected ist audit-trail) vs "generic-content" (expected ist knowledge)?
3. Cross-check: Match Pattern-Detection mit expected-class. Wenn Pattern-Detection = expected-class für ≥80% der cases → Pattern-Match-Heuristik valid.

**Architektur-Optionen**:
- A. SQL-side Pattern-Match: ctx_rrf bekommt zusätzlichen Parameter `p_target_audit_trail` (boolean). Wenn TRUE → role_factor=1.0 für audit-trail. Sonst → role_factor=0.3.
- B. Go-side Pattern-Detection: synthesize.go macht Pattern-Match auf query-string, setzt Parameter beim Aufruf.
- C. LLM-side Intent-Klassifikation: leichter LLM-call vor RRF, klassifiziert query-intent → Damping-Parameter.

Empfehlung: **A + B (kombiniert)** — SQL-Function flexibel, Go-side macht Pattern-Detection.

### Welle 42 — Audit-Trail-Klassifikation erweitern

35+ unaudited audit-trail-Kandidaten (aus Sub-Agent-A 47-Pool). Sub-Agent-Audit n=35 mit gleicher Taxonomie. Backfill als M0NN.

### Welle 38c — Generative Tagesbericht (S12 Phase 1)

Jetzt machbar mit cleaner block_role-Architecture. synthesis-blocks bekommen block_role='audit-trail' bei Erstellung.

Abhängigkeit: Welle 41 (query-aware damping) sollte ZUERST gemacht sein, sonst ist audit-trail-Damping immer noch broken — neue synthesis-blocks würden gleiche regressions verursachen.

Schritte (siehe ctx 019d3f5a):
1. WriteSynthesisBlock + GenerateDailyReport (LLM-prompt analog evaluate.go)
2. Scheduler: 1× täglich (z.B. 03:00 lokal)
3. block_role='audit-trail' bei Erstellung
4. Tests + Live-Test
5. Bench: orthogonal sein, kein regression

### Welle 43 — ctx_save MCP-Hook für Auto-Klassifikation

Auto-block_role-Zuweisung für synthesis/audit/welle/session blocks bei Erstellung. Verhindert manuelle UPDATE wie aktuell (siehe Welle-39 M032/M034 + Welle-40 M035 hardcoded list).

## Pre-existing Bugs (Backlog)

1. **rrf_mass_test.go**: pre-existing duplicate-key durch unique constraint M005. Helper `insertEmbeddedBlock` benutzt identische titles für 4 blocks. Fix-Pattern: `insertRoleTestBlock` aus rrf_role_test.go (per-id-unique title via fmt.Sprintf).
2. **eval.sh baseline veraltet**: 2026-04-09 baseline (46 tests, 100%) vs current 47 tests + T01 stable-fail. False-positive REGRESSION-Verdict. Fix: `bash eval.sh --update-baseline` post-Welle-41 promote.
3. **parseLinks string-confidence**: dream/parse.go tolerates object-form-drift, aber nicht `"confidence":"high"|"medium"|"low"`. Map "high"→0.9, "medium"→0.7, "low"→0.5.
4. **guard SQLSTATE 42P08 für is_meta-blocks**: parameter-type-cast issue.
5. **synthesis P95 latency drift**: bei parallel-bench (eval.sh + eval-cyclic gleichzeitig) P95 4730→10551ms. Sequenzielle bench-Methodology obligatorisch.

## Closure-Pflicht (vor 06:30 Hard-Stop)

**MUSS gemacht werden, egal welcher Welle-Stand**:

1. ctx-Audit-Block(s) speichern für aktuelle Welle MIT `is_meta=TRUE` (W11-Drift-Schutz, sofort beim store + via SQL nachreichen)
2. MEMORY.md aktuelle Stand-Eintrag ergänzen
3. Folge-temp.md anlegen (überschreibt diese)
4. Submodule .project commit + push
5. Hauptrepo commit + push (Migration + Code) — **nur mit annotated tag wenn Bench grün**
6. Self-Audit gegen 14 Warnings als `welle-XX-self-audit.md` im Submodule
7. Cron deleten falls erstellt
8. State-Verifikation: `bash state.sh` — diff gegen Snapshot oben

## Drift-Disziplin (W11 weiter zentral)

1. **ctx_save für audit-blocks**: IMMER metadata.is_meta=TRUE direkt + UPDATE context_blocks SET is_meta=TRUE + block_role='system-meta' direkt nach store. Beachte: metadata.is_meta wird NICHT in SQL is_meta-Spalte übersetzt durch MCP store — manuelle UPDATE PFLICHT.
2. **Bench-Confounding vermeiden**: separate code-only-changes vs Korpus-state-changes. Strict-Attribution via isolated bench.
3. **Re-Baseline obligatorisch** vor Welle-Δ-Bewertung.
4. **Eval-Bench-Methodology**: sequenziell isoliert (eval.sh fertig → eval-cyclic.sh, NIE parallel — Welle 35-Lehre, Welle-40-Iter-3-violation).
5. **Self-Audit ohne IKEA-Bias**: Sub-Agent-Audit-VALID-rates nicht als Code-Quality-Beweis missdeuten. Bench ist primär. Live-Audit ist Sekundär.

## Methodische Cross-Reference (Warnings spezifisch für Welle 41)

- **W3**: Pre-Empirie-Pflicht für Query-Pattern-Audit + expected-class-Cross-check.
- **W6a (konservativer Anker)**: Re-Baseline ist Welle-41-Anker. Welle 40 HOLD ist NICHT die neue Baseline (DB-Performance == v1.2.0).
- **W6b (Kreativität)**: query-aware damping ist NEUER ansatz, nicht "M037 mit kleinem Tweak".
- **W9 (Armada-Konsens)**: Sub-Agent NUR für Klassifikations-Audit + Pattern-Match-Audit. Bench LIVE im Orchestrator.
- **W11 (Drift)**: jeder neue audit-block sofort is_meta=TRUE + block_role=system-meta markieren. Auto-Hook bei ctx_save fehlt noch (Welle 43 backlog).
- **W12 (Wave-Pattern)**: 1 logische Änderung pro Commit. Migration + Code + Tests = mind. 3 Commits pro Iteration.
- **W13 (Agent-Kontext)**: Sub-Agent-Prompts self-contained mit Pfaden, line-numbers, Block-IDs.
- **W14 (Polling)**: Bench-Wartezeiten via Monitor + until-loop. Cron-Self-Ping wie Session 36 (z.B. "13,43 * * * *", off-clock).

## Self-Ping-Cron (failsafe)

Empfehlung: `CronCreate` alle 30 min mit Hard-Stop-Check (z.B. cron `*/30 * * * *` zwischen 00:30-06:30, dann delete). Pattern aus Session 36 c5a7b48b.

## Pointer-Sammlung

- v1.2.0 Detail-Audit ctx: `019dff4b` (is_meta=TRUE)
- Welle 40 HOLD ctx: `019dff8b` (is_meta=TRUE) — diese Session
- 38b implementiert ctx: `019dfeca`
- 38a NULL-RESULT ctx: `019dfe9e`
- S12-Generative-Design ctx: `019d3f5a`
- writelinks.go:188 — Confidence-Formel
- linkfilters.go:21-32 — minRawConfidence map
- recurrence.go:DetectRecurrence — Phase 1 + Phase 2
- 035_block_role_classification.sql — Welle 40 Schema
- 038_rrf_role_damping_revert.sql — Welle 40 HOLD state
- BRANCH-HYPOTHESIS-Welle-40-Klassifikation.md — Decision-Doku
- 14-Warnings: `/root/.claude/projects/-compose-n8n/memory/warnings.md`

## Hard-Stop-Disziplin

- **Ab 06:00**: aktuelle Iteration zu Ende, KEINE neue Iteration starten
- **Ab 06:30**: nur noch Closure-Sequenz, keine Code-Änderungen
- **Bis 07:00**: alle commits + push + ctx-Audit + MEMORY.md + Folge-temp.md + Self-Audit + Cron-Delete

Die nächste Session liest Pflicht-Lesen-Reihenfolge → prüft Re-Verifikations-Trigger → liest Welle-40-HOLD-Status + Welle-41-Plan → wählt Folge-Welle (recommend Welle 41) → löscht diese Datei nach dem Lesen.
