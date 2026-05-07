# Session-Übergabe — Autonom-Welle 44+ (post-Welle-43-PROMOTE-v1.4.1)

**Diese Datei nach dem Lesen löschen** (`rm /compose/n8n/temp.md`).

## Pflicht-Lesen (in dieser Reihenfolge)

1. **Warnings** — `/root/.claude/projects/-compose-n8n/memory/warnings.md`
2. **MEMORY.md** — "Aktueller Stand" S36
3. **ctx-Blöcke** (chronologisch jüngste zuerst):
   - `019e023f-e42b-7b3f-97be-e7cb9fe7186a` — Welle 43 PROMOTE v1.4.1 (Audit-Trail-Erweiterung, is_meta=TRUE)
   - `019e0209-89c5-7362-b464-8565e805046d` — Welle 42 PROMOTE v1.4.0 (Generative Tagesbericht)
   - `019e0140-102e-7632-ac7c-92ef7b11586e` — Welle 41 PROMOTE v1.3.0 (query-aware damping)
   - `019dff8b-f05e-799b-9721-1a2cadc337b7` — Welle 40 HOLD (Architektur-Schuld-Doku)
   - `019dff4b-5849-79a3-ac23-1de0408eec8e` — Welle 39 + 38b PROMOTE v1.2.0
4. **Welle-43-Artefakte**: `.project/bench-dream-phase0/`
   - `welle-43-self-audit.md`
   - `welle-43-unaudited-candidates.json` (34 candidates)
   - `welle-43-classification.json` (Sub-Agent decisions)
   - `eval-cyclic-w43.json` + `eval-w43.log`

## Production-State-Snapshot (verifiziert 2026-05-07 ~13:50 CEST)

| Komponente | Stand |
|---|---|
| Hauptrepo HEAD | post-v1.4.1 tag |
| Letzter Tag | **v1.4.1** annotated, gepusht origin |
| DB Migrations | **40** (M035-M040) |
| Active blocks | **454** (288 K + 121 R + 22 SM + 22 AT + Welle-43-audit-block) |
| Production-Performance | identisch zu v1.4.0 (eval-cyclic 0.9428, eval.sh 46/47) |
| Daily Synthesis | aktiv via scheduler.runDailySynthesis (03:00 lokal) |

## Welle 43 v1.4.1 Endstand

- M040: 9 IDs UPDATE block_role='audit-trail' (Sub-Agent-Klassifikation n=34, 25 als knowledge belassen, 2 whitelisted)
- Whitelist-Schutz für 019d33fd + 019defb1 (Bench-Anchor M-013/L-023)
- 0 Bench-Regression (eval-cyclic + eval.sh identisch zu v1.4.0)
- 1 W12-Commit Welle 43 (a22add6 M040 + README)

## Folge-Welle 44+ Plan

### Welle 44 — ctx_save MCP-Hook für Auto-block_role (PRIMÄR)

Aktuell: synthesis-blocks von dream-pipeline bekommen block_role='audit-trail' (siehe Welle 42). Aber: ctx_save MCP-tool macht KEIN auto-classification. Audit-blocks die ich in dieser Session via ctx_save erstelle (z.B. 019dff8b, 019e0140, 019e0209, 019e023f) brauchten manuelle UPDATE.

**Plan**:
1. Server-side hook: bei MCP `ctx_save` mit metadata.is_meta=true → automatic UPDATE is_meta=TRUE + block_role='system-meta'.
2. Plus: Pattern-Erkennung im Title (analog rrf.HasAuditTrailIntent): wenn title matched audit-trail-pattern → block_role='audit-trail'.
3. Test: ctx_save mit verschiedenen titles → check block_role/is_meta.
4. Bench: orthogonal.

### Welle 45 — S12 Phase 2 Wochenbericht

Analog zu Welle 42 aber 7-Tage-Window. Trends/Patterns über mehrere Tagesberichte. Scheduler 1× wöchentlich (z.B. Sonntag 04:00 lokal).

### Welle 46 — LLM-Intent-Klassifikation als 2. Pattern-Layer

Für edge cases die nicht via 10-Token-Pattern detected werden (z.B. M-003 wäre fail ohne Welle-41-Iter-4-Erweiterung). LLM-call vor RRF, klassifiziert query-intent → audit_trail_factor.

### Pre-existing Bug Backlog

1. **rrf_mass_test.go**: pre-existing duplicate-key durch unique constraint M005. Fix-Pattern: rrf_role_test.go's `insertRoleTestBlock` mit per-id-unique title.
2. **eval.sh baseline veraltet**: 2026-04-09 baseline (46 tests, 100%) vs current 47 tests. Fix: `bash eval.sh --update-baseline` post-v1.4.1.
3. **parseLinks string-confidence**: dream/parse.go tolerates object-form-drift, aber nicht "high"|"medium"|"low" string. Map "high"→0.9, etc.
4. **guard SQLSTATE 42P08 für is_meta-blocks**: parameter-type-cast issue.

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

1. **ctx_save für audit-blocks**: IMMER metadata.is_meta=TRUE direkt + UPDATE context_blocks SET is_meta=TRUE + block_role='system-meta' direkt nach store. Beachte: metadata.is_meta wird NICHT in SQL is_meta-Spalte übersetzt durch MCP store — manuelle UPDATE PFLICHT (bis Welle 44 das automatisiert).
2. **Re-Baseline obligatorisch** vor Welle-Δ-Bewertung.
3. **Eval-Bench-Methodology**: sequenziell isoliert (eval.sh fertig → eval-cyclic.sh, NIE parallel — Welle 35-Lehre).
4. **Wall-Clock-Cross-Check**: regelmäßig date+TZ prüfen, mindestens 1× pro Stunde (Session-36-Lehre cumulative time-tracking-Drift).

## Pointer-Sammlung

- v1.4.1 Welle 43 ctx: `019e023f` (is_meta=TRUE)
- v1.4.0 Welle 42 ctx: `019e0209`
- v1.3.0 Welle 41 ctx: `019e0140`
- Welle 40 HOLD ctx: `019dff8b`
- v1.2.0 Welle 39 ctx: `019dff4b`
- M040 (audit-trail-Klassifikation): `go/migrations/040_audit_trail_classification_extension.sql`
- pattern.go (Welle 41): `go/internal/rrf/pattern.go` (10-Pattern set)
- synthesize_report.go (Welle 42): `go/internal/dream/synthesize_report.go`
- 14-Warnings: `/root/.claude/projects/-compose-n8n/memory/warnings.md`

Die nächste Session liest Pflicht-Lesen → prüft Re-Verifikations-Trigger (Tag-Push seit v1.4.1 / DB-Migration > 40 / >10 Block-Differenz vs 454) → liest Welle-43-PROMOTE-Status + Welle-44-Plan → wählt Folge-Welle (recommend Welle 44 = ctx_save MCP-Hook) → löscht diese Datei nach dem Lesen.
