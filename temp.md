# Session-Übergabe — Autonom-Welle 45+ (post-Welle-44-PROMOTE-v1.4.2)

**Diese Datei nach dem Lesen löschen** (`rm /compose/n8n/temp.md`).

## Pflicht-Lesen

1. **Warnings** — `/root/.claude/projects/-compose-n8n/memory/warnings.md`
2. **MEMORY.md** — "Aktueller Stand" S36
3. **ctx-Blöcke** (chronologisch jüngste zuerst):
   - `019e0261-529c-77f9-96a5-b9173299a9e9` — Welle 44 PROMOTE v1.4.2 (ctx_save Auto-Klassifikation, is_meta=TRUE automatic)
   - `019e023f-e42b-7b3f-97be-e7cb9fe7186a` — Welle 43 PROMOTE v1.4.1 (audit-trail-Erweiterung)
   - `019e0209-89c5-7362-b464-8565e805046d` — Welle 42 PROMOTE v1.4.0 (Generative Tagesbericht)
   - `019e0140-102e-7632-ac7c-92ef7b11586e` — Welle 41 PROMOTE v1.3.0 (query-aware damping)
   - `019dff8b-f05e-799b-9721-1a2cadc337b7` — Welle 40 HOLD
4. **Welle-44-Artefakte**: `.project/bench-dream-phase0/`
   - `welle-44-self-audit.md`
   - `eval-cyclic-w44.json` + `eval-w44.log`

## Production-State-Snapshot (verifiziert 2026-05-07 ~14:30 CEST)

| Komponente | Stand |
|---|---|
| Hauptrepo HEAD | post-v1.4.2 tag |
| Letzter Tag | **v1.4.2** annotated, gepusht origin |
| DB Migrations | **40** (M035-M040, kein neues für Welle 44) |
| Active blocks | **454** (288 K + 121 R + 23 SM + 22 AT) |
| Production-Performance | identisch zu v1.4.1 (eval-cyclic 0.9428, eval.sh 46/47) |
| Daily Synthesis | aktiv 03:00 lokal |
| Auto-Classify Hook | **AKTIV** (ctx_save sets block_role/is_meta automatic) |

## Welle 44 v1.4.2 Endstand

- store.ClassifyBlockAfterUpsert: 4-Branch-Decision-Tree
- Hook-Integration in handler/context_store + handler/mcp
- Self-referential validated: Welle-44-Audit-Block (019e0261) automatic system-meta
- Welle 40-43 manuelle-UPDATE-Schulden adressiert: künftige ctx-Audit-Blocks brauchen kein post-store-UPDATE mehr
- 2 W12-Commits: ce69d77 + 86006c4

## Folge-Welle 45+ Plan

### Welle 45 — S12 Phase 2 Wochenbericht (PRIMÄR)

Analog zu Welle 42 (Tagesbericht) aber 7-Tage-Window. Trends/Patterns über mehrere Tagesberichte.

**Plan**:
1. dream/synthesize_weekly_report.go (analog synthesize_report.go)
2. SQL: aggregiere 7-Tage-activity (decisions, neue blocks, dream-links, plus existing daily-synthesis-blocks)
3. LLM-Prompt: deutscher Wochenbericht (300-500 Worte) mit Trend-Analyse
4. Block-Klassifikation: synthesis + audit-trail (via Welle-44-Auto-Hook!)
5. Scheduler: 1× wöchentlich Sonntag 04:00 lokal
6. handler/synthesize.go endpoint POST /api/synthesize/weekly
7. Tests + Live-Test
8. Bench: orthogonal verify

### Welle 46 — LLM-Intent-Klassifikation als 2. Pattern-Layer

Für edge cases die nicht via 10-Token-Pattern detected werden. LLM-call vor RRF, klassifiziert query-intent → boolean audit_target → audit_trail_factor.

**Plan**:
1. dream/intent_classifier.go: leichter LLM-call (z.B. structured-output prompt)
2. Cache-Layer: vermeide LLM-call bei wiederholten queries
3. Integration: handler/query.go probiert pattern-match first (fast), dann LLM-fallback
4. Tests + Bench: M-003-style cases sollten verbesserung zeigen

### Pre-existing Bug Backlog

1. **rrf_mass_test.go** duplicate-key fix (per-id-unique title pattern)
2. **eval.sh baseline** regenerate post-v1.4.2 (`bash eval.sh --update-baseline`)
3. **parseLinks string-confidence**: dream/parse.go map "high"→0.9, etc.
4. **guard SQLSTATE 42P08** für is_meta-blocks parameter-type-cast

## Closure-Pflicht

1. ctx-Audit-Block(s) speichern (Welle 44 Hook: metadata.is_meta=true → automatic system-meta!)
2. MEMORY.md aktuelle Stand-Eintrag (max 200 Zeilen)
3. Folge-temp.md anlegen
4. Submodule commit + push
5. Hauptrepo commit + push **mit annotated tag wenn grün**
6. Self-Audit gegen 14 Warnings als `welle-XX-self-audit.md`
7. State-Verifikation: `bash state.sh`

## Drift-Disziplin (W11)

1. **ctx_save für audit-blocks**: Welle 44 macht das automatic. Aber: weiterhin metadata.is_meta=true setzen für system-meta intent. Pattern-Detection greift bei title.
2. **Re-Baseline obligatorisch** vor Welle-Δ-Bewertung.
3. **Eval-Bench-Methodology**: sequenziell isoliert.
4. **Wall-Clock-Cross-Check**: regelmäßig (Session 36 Lehre).

## Pointer-Sammlung

- v1.4.2 Welle 44 ctx: `019e0261` (auto-system-meta)
- v1.4.1 Welle 43 ctx: `019e023f`
- v1.4.0 Welle 42 ctx: `019e0209`
- v1.3.0 Welle 41 ctx: `019e0140`
- store/classify.go: `go/internal/store/classify.go` (ClassifyBlockAfterUpsert)
- handler/context_store.go: HandleStore Hook-Aufruf
- handler/mcp.go: mcpStoreHandler Hook-Aufruf
- 14-Warnings: `/root/.claude/projects/-compose-n8n/memory/warnings.md`

Die nächste Session liest Pflicht-Lesen → Re-Verifikations-Trigger (Tag-Push v1.4.2 / DB-Migration > 40 / >10 Block-Differenz vs 454) → Welle-44-PROMOTE-Status + Welle-45-Plan → Folge-Welle wählen (recommend Welle 45 = S12 Phase 2 Wochenbericht) → diese Datei nach Lesen löschen.
