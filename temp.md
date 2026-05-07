# Session-Übergabe — Autonom-Welle 42+ (post-Welle-41-PROMOTE-v1.3.0)

**Diese Datei nach dem Lesen löschen** (`rm /compose/n8n/temp.md`).

Drift-resistent. Pflicht-Lesen + State-Snapshot + Welle-42-Plan + Closure-Pflicht.

## Pflicht-Lesen (in dieser Reihenfolge, vor erster Welle)

1. **Warnings** — `/root/.claude/projects/-compose-n8n/memory/warnings.md`
2. **MEMORY.md** — `/root/.claude/projects/-compose-n8n/memory/MEMORY.md` — "Aktueller Stand" S36
3. **ctx-Blöcke (in dieser Reihenfolge)**:
   - `019e0140-102e-7632-ac7c-92ef7b11586e` — Welle 41 PROMOTE v1.3.0 (jüngste, is_meta=TRUE)
   - `019dff8b-f05e-799b-9721-1a2cadc337b7` — Welle 40 HOLD (Architektur-Schuld-Doku)
   - `019dff4b-5849-79a3-ac23-1de0408eec8e` — Welle 39 + 38b PROMOTE v1.2.0
   - `019d3f5a-0f6a-72f4-9ac3-3be356abee47` — S12 Generative-Synthese-Design (für Welle 38c)
4. **Welle-41-Artefakte**: `.project/bench-dream-phase0/`
   - `welle-41-pattern-audit.json` (Iter 0, 6 Pattern, Recall 0.86)
   - `welle-41-pattern-audit-extended.json` (Iter 4, 10 Pattern, Recall 1.00)
   - `eval-cyclic-w41-it3.json` (Iter 3, 0.9261 mit 6 Pattern)
   - `eval-cyclic-w41-it4.json` (Iter 4, 0.9428 mit 10 Pattern == PROMOTE)
   - `eval-w41-it4-pattern10.log` (eval.sh 46/47)
   - `welle-41-self-audit.md` (14-Warning Self-Audit)

## Production-State-Snapshot (verifiziert 2026-05-07 ~09:05 CEST)

| Komponente | Stand |
|---|---|
| Hauptrepo HEAD | post-v1.3.0-tag (12 Welle-40+41 Commits ahead von 00126cd, jetzt mit annotated v1.3.0) |
| Branch | `root` |
| Letzter Tag | **v1.3.0** annotated, gepusht origin |
| Submodule .project | post-Welle-41-Bench-Logs (gepusht origin) |
| DB Migrations | **39** (M035 schema + M036/M037/M038 Welle-40-iter + M039 Welle-41) |
| Dream Version | **5** |
| Active blocks | **450** (297 knowledge + 121 reference + 20 system-meta + 12 audit-trail) |
| Hooks-Pfad | `/compose/n8n/.hooks` (aktiv, pre-push validiert annotated tags) |
| Production-Performance | T03 fixed (Welle-40-Hauptziel), eval-cyclic == v1.2.0 |

## Welle 41 v1.3.0 Endstand

**Was deployed ist**:
- M035: block_role 4-Klassen-Enum + Backfill (Welle 40)
- M036/M037/M038: ctx_rrf damping iterations (Welle 40 Audit-Trail; M038 = no-op state-restoration)
- M039: ctx_rrf signature mit `p_audit_trail_factor DOUBLE PRECISION DEFAULT 1.0` (Welle 41)
- Go-side: `internal/rrf/pattern.go` (10 Pattern: session/welle/audit/recurrent/handover/self-audit/dream v/performance/reset/baseline)
- Go-side: `handler/query.go` Pattern-aware Caller (`rrf.AuditTrailFactor(query)`)
- Go-side: `dream/dream.go` hardcoded 1.0 (no damping in dream-cycle)

**Architektur-Lehre** (Welle 40 → Welle 41 evolution):
1. Schema-First: M035 block_role-enum war forwards-kompatibles Architecture-Investment. Welle 41 baute darauf, M039 ist nur signature-extension.
2. Uniform damping ineffektiv (Welle 40): 0.3 vs 0.5 → 0/70 cases-Differenz. Mechanismus nicht-Hebel.
3. Query-aware damping ist der wirksame Hebel (Welle 41): Pattern-Recall 0.86 (6 Pattern) → -1.7pp regression. Pattern-Recall 1.00 (10 Pattern) → 0.0pp regression + T03 fix.

## Folge-Welle 42+ Plan

### Welle 42 — Welle 38c Generative Tagesbericht (S12 Phase 1)

Jetzt mit clean Welle-41-Architecture machbar:
1. Schema check: block_type='synthesis' Wert (kein CHECK constraint nötig, VARCHAR(30))
2. Code: dream/synthesize_report.go — function `GenerateDailyReport(ctx, pool, llm-config)` analog evaluate.go pattern (LLM-prompt für Tagesbericht aus context_write_log + recent activity)
3. Scheduler: 1× täglich (z.B. 03:00 lokal) in events/scheduler.go
4. block_role='audit-trail' bei Erstellung — query-aware damping handhabt naturgemäß
5. Tests: writes_synthesis_test.go
6. Live-Test: API-Trigger → 1 synthesis-block → MCP-Query "Was wurde heute gemacht" → check confident response
7. Bench: orthogonal sein (kein regression)

### Welle 43 — Audit-Trail-Klassifikation erweitern

35+ unaudited audit-trail-Kandidaten aus Sub-Agent-A 47-pool (Welle 40 Iter 0). Sub-Agent-Audit n=35 mit gleicher 4-Klassen-Taxonomie. Backfill als M0NN.

### Welle 44 — ctx_save MCP-Hook für Auto-Klassifikation

Auto-block_role-Zuweisung für synthesis/audit/welle/session blocks bei Erstellung. Verhindert manuelle UPDATE wie aktuell.

### Pre-existing Bug Backlog

1. **rrf_mass_test.go**: pre-existing duplicate-key durch unique constraint M005 (uq_context_category_title_scope). Fix-Pattern: rrf_role_test.go's `insertRoleTestBlock` mit per-id-unique title.
2. **eval.sh baseline veraltet**: 2026-04-09 baseline (46 tests, 100%) vs current 47 tests. False-positive REGRESSION-Verdict. Fix: `bash eval.sh --update-baseline` post-v1.3.0-promote.
3. **parseLinks string-confidence**: dream/parse.go tolerates object-form-drift, aber nicht "high"|"medium"|"low" string. Map "high"→0.9, etc.
4. **guard SQLSTATE 42P08 für is_meta-blocks**: parameter-type-cast issue.
5. **synthesis P95 latency drift bei parallel-bench**: sequenziell isoliert obligatorisch.

## Closure-Pflicht (vor 06:30 Hard-Stop bei autonom-Welle)

**MUSS gemacht werden, egal welcher Welle-Stand**:

1. ctx-Audit-Block(s) speichern für aktuelle Welle MIT `is_meta=TRUE` (W11) — direkt nach store via SQL UPDATE (metadata.is_meta wird NICHT in is_meta-Spalte übersetzt durch MCP)
2. MEMORY.md aktuelle Stand-Eintrag ergänzen (max 200 Zeilen)
3. Folge-temp.md anlegen (überschreibt diese)
4. Submodule .project commit + push
5. Hauptrepo commit + push (Migration + Code) — **nur mit annotated tag wenn Bench grün**
6. Self-Audit gegen 14 Warnings als `welle-XX-self-audit.md` im Submodule
7. Cron deleten falls erstellt
8. State-Verifikation: `bash state.sh` — diff gegen Snapshot oben

## Drift-Disziplin (W11)

1. **ctx_save für audit-blocks**: IMMER metadata.is_meta=TRUE direkt + UPDATE context_blocks SET is_meta=TRUE + block_role='system-meta' direkt nach store. Beachte: metadata.is_meta wird NICHT in SQL is_meta-Spalte übersetzt durch MCP store — manuelle UPDATE PFLICHT.
2. **Bench-Confounding vermeiden**: separate code-only-changes vs Korpus-state-changes.
3. **Re-Baseline obligatorisch** vor Welle-Δ-Bewertung.
4. **Eval-Bench-Methodology**: sequenziell isoliert (eval.sh fertig → eval-cyclic.sh, NIE parallel — Welle 35-Lehre, Session-36-Iter-3-violation).
5. **Self-Audit ohne IKEA-Bias**.
6. **Wall-Clock-Cross-Check**: regelmäßig date+TZ prüfen, mindestens 1× pro Stunde. Session-36-Lehre (cumulative time-tracking-Drift +6h).

## Methodische Cross-Reference (Warnings spezifisch für Folge-Welle)

- **W3**: Pre-Empirie-Pflicht für jede Iter-1-Decision. Welle 38c: prompt-template + write-flow Pre-Empirie nötig.
- **W6a**: Re-Baseline ist Welle-Anker.
- **W6c**: bei klarem Plan direkt Code (Welle 41 Iter-4-Korrektur war diese Lehre).
- **W9**: Sub-Agent für Klassifikation, Bench LIVE im Orchestrator.
- **W11**: jeder neue ctx-Block sofort is_meta=TRUE + block_role=system-meta.
- **W12**: 1 logische Änderung pro Commit. Welle 41 hatte 4 Commits W12.
- **W13**: Sub-Agent-Prompts self-contained.

## Self-Ping-Cron (failsafe)

Empfehlung: `CronCreate` alle 30 min mit Hard-Stop-Check (z.B. cron `*/30 * * * *` oder off-clock `13,43 * * * *`). Pattern aus Session 36 c5a7b48b/8d9206f9.

## Pointer-Sammlung

- v1.3.0 Welle 41 ctx: `019e0140` (is_meta=TRUE)
- Welle 40 HOLD ctx: `019dff8b` (is_meta=TRUE)
- v1.2.0 Welle 39 ctx: `019dff4b` (is_meta=TRUE)
- 38b implementiert ctx: `019dfeca`
- S12-Generative-Design ctx: `019d3f5a`
- M039 (query-aware damping): `go/migrations/039_rrf_query_aware_damping.sql`
- pattern.go: `go/internal/rrf/pattern.go` (10-Pattern set)
- handler/query.go: rrf.AuditTrailFactor caller
- 14-Warnings: `/root/.claude/projects/-compose-n8n/memory/warnings.md`

Die nächste Session liest Pflicht-Lesen-Reihenfolge → prüft Re-Verifikations-Trigger (Tag-Push seit v1.3.0 / DB-Migration > 39 / >10 Block-Differenz vs 450) → liest Welle-41-PROMOTE-Status + Welle-42-Plan (Welle 38c Generative) → wählt Folge-Welle → löscht diese Datei nach dem Lesen.
