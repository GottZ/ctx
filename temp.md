# Session-Übergabe — Autonom-Welle 43+ (post-Welle-42-PROMOTE-v1.4.0)

**Diese Datei nach dem Lesen löschen** (`rm /compose/n8n/temp.md`).

## Pflicht-Lesen (in dieser Reihenfolge, vor erster Welle)

1. **Warnings** — `/root/.claude/projects/-compose-n8n/memory/warnings.md`
2. **MEMORY.md** — `/root/.claude/projects/-compose-n8n/memory/MEMORY.md` — "Aktueller Stand" S36
3. **ctx-Blöcke** (chronologisch jüngste zuerst):
   - `019e0209-89c5-7362-b464-8565e805046d` — Welle 42 PROMOTE v1.4.0 (Generative Tagesbericht, is_meta=TRUE)
   - `019e0140-102e-7632-ac7c-92ef7b11586e` — Welle 41 PROMOTE v1.3.0 (query-aware damping)
   - `019dff8b-f05e-799b-9721-1a2cadc337b7` — Welle 40 HOLD (Architektur-Schuld-Doku)
   - `019dff4b-5849-79a3-ac23-1de0408eec8e` — Welle 39 + 38b PROMOTE v1.2.0
   - `019d3f5a-0f6a-72f4-9ac3-3be356abee47` — S12 Generative-Synthese-Design (Welle 38c = jetzt Welle 42 implementiert)
4. **Welle-42-Artefakte**: `.project/bench-dream-phase0/`
   - `welle-42-self-audit.md`
   - `eval-cyclic-w42.json` + `eval-w42.log`

## Production-State-Snapshot (verifiziert 2026-05-07 ~12:45 CEST)

| Komponente | Stand |
|---|---|
| Hauptrepo HEAD | post-v1.4.0 tag |
| Branch | `root` |
| Letzter Tag | **v1.4.0** annotated, gepusht origin |
| Submodule .project | post-Welle-42-Bench-Logs (gepusht origin) |
| DB Migrations | **39** (M035 schema + M036/M037/M038 W40-iter + M039 W41) |
| Active blocks | **451** (297 K + 121 R + 20 SM + 13 AT — incl. 1 Welle-42-Tagesbericht-block 019e0200) |
| Production-Performance | T03 fixed (Welle 40), Welle 42 orthogonal (eval-cyclic == v1.3.0) |
| Daily Synthesis | aktiv, 03:00 lokal trigger via scheduler.go runDailySynthesis |

## Welle 42 v1.4.0 Endstand

- Generative Tagesbericht implementiert (dream/synthesize_report.go)
- POST /api/synthesize/daily endpoint (manueller Trigger)
- Scheduler 1× täglich 03:00 lokal
- block_type='synthesis' + block_role='audit-trail' bei Erstellung
- Welle-41-query-aware-damping fängt synthesis-noise bei generic queries naturgemäß
- 3 W12-Commits Welle 42: 9aef111 (core+tests) + 2b32ced (handler+route) + 1fd5961 (scheduler)

## Folge-Welle 43+ Plan

### Welle 43 — Audit-Trail-Klassifikation erweitern (PRIMÄR)

35+ unaudited audit-trail-Kandidaten aus Sub-Agent-A 47-pool (Welle 40 Iter 0). Sub-Agent-Audit n=35 mit 4-Klassen-Taxonomie. UPDATE block_role als M0NN.

Plus: heuristic für Auto-Detect (audit/welle/session in tags ODER title-Pattern → audit-trail). Aber: bedacht wegen 45% FP-Rate aus Welle 40 Pre-Empirie.

### Welle 44 — ctx_save MCP-Hook für Auto-Klassifikation

Auto-block_role-Zuweisung für synthesis/audit/welle/session blocks bei Erstellung. Verhindert manuelle UPDATE wie aktuell für audit-blocks. Plus: synthesis-blocks von dream-pipeline UND von ctx_save MCP path bekommen beide block_role='audit-trail' bei Erstellung.

### Welle 45 — S12 Phase 2 Wochenbericht

Analog Welle 42 aber 7-Tage-Window. Trends/Patterns über mehrere Tagesberichte. Scheduler 1× wöchentlich (z.B. Sonntag 04:00 lokal).

### Welle 46 — Pattern-List Evolution

LLM-Intent-Klassifikation als 2. Layer für edge cases. Wenn user fragt nach altem block ohne pattern-match in 10-Token-Liste → LLM klassifiziert query-intent → boolean audit-target.

### Pre-existing Bug Backlog

1. **rrf_mass_test.go**: pre-existing duplicate-key durch unique constraint M005. Fix-Pattern: rrf_role_test.go's `insertRoleTestBlock` mit per-id-unique title.
2. **eval.sh baseline veraltet**: 2026-04-09 baseline (46 tests, 100%) vs current 47 tests. False-positive REGRESSION-Verdict. Fix: `bash eval.sh --update-baseline` post-v1.4.0.
3. **parseLinks string-confidence**: dream/parse.go tolerates object-form-drift, aber nicht "high"|"medium"|"low" string. Map "high"→0.9, etc.
4. **guard SQLSTATE 42P08 für is_meta-blocks**: parameter-type-cast issue.
5. **synthesis P95 latency drift bei parallel-bench**: sequenziell isoliert obligatorisch.

## Closure-Pflicht (vor Hard-Stop)

1. ctx-Audit-Block(s) speichern für aktuelle Welle MIT `is_meta=TRUE` (W11)
2. MEMORY.md aktuelle Stand-Eintrag ergänzen (max 200 Zeilen)
3. Folge-temp.md anlegen (überschreibt diese)
4. Submodule .project commit + push
5. Hauptrepo commit + push (Migration + Code) — **nur mit annotated tag wenn Bench grün**
6. Self-Audit gegen 14 Warnings als `welle-XX-self-audit.md` im Submodule
7. Cron deleten falls erstellt
8. State-Verifikation: `bash state.sh` — diff gegen Snapshot oben

## Drift-Disziplin (W11)

1. **ctx_save für audit-blocks**: IMMER metadata.is_meta=TRUE direkt + UPDATE context_blocks SET is_meta=TRUE + block_role='system-meta' direkt nach store. Beachte: metadata.is_meta wird NICHT in SQL is_meta-Spalte übersetzt durch MCP store — manuelle UPDATE PFLICHT (bis Welle 44 ctx_save MCP-Hook das automatisiert).
2. **Re-Baseline obligatorisch** vor Welle-Δ-Bewertung.
3. **Eval-Bench-Methodology**: sequenziell isoliert (eval.sh fertig → eval-cyclic.sh, NIE parallel — Welle 35-Lehre).
4. **Wall-Clock-Cross-Check**: regelmäßig date+TZ prüfen, mindestens 1× pro Stunde (Session-36-Lehre cumulative time-tracking-Drift +6h).

## Pointer-Sammlung

- v1.4.0 Welle 42 ctx: `019e0209` (is_meta=TRUE)
- v1.3.0 Welle 41 ctx: `019e0140`
- Welle 40 HOLD ctx: `019dff8b`
- v1.2.0 Welle 39 ctx: `019dff4b`
- S12-Generative-Design ctx: `019d3f5a` (jetzt implementiert in Welle 42)
- Tagesbericht 2026-05-07 ctx: `019e0200-bf45-7b58-8d82-d7412d703140` (block_type='synthesis')
- dream/synthesize_report.go: `go/internal/dream/synthesize_report.go`
- handler/synthesize.go: POST /api/synthesize/daily endpoint
- scheduler.go: runDailySynthesis goroutine
- 14-Warnings: `/root/.claude/projects/-compose-n8n/memory/warnings.md`

Die nächste Session liest Pflicht-Lesen → prüft Re-Verifikations-Trigger (Tag-Push seit v1.4.0 / DB-Migration > 39 / >10 Block-Differenz vs 451) → liest Welle-42-PROMOTE-Status + Welle-43-Plan → wählt Folge-Welle (recommend Welle 43 = Audit-Trail-Klassifikations-Erweiterung) → löscht diese Datei nach dem Lesen.
