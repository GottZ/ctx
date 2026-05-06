# Session-Übergabe — Folge-Session nach v1.1.0 + Dream-Vorbereitung (2026-05-06 14:55 CEST)

**Diese Datei nach dem Lesen löschen** (`rm /compose/n8n/temp.md`).

Drift-resistent geschrieben. Jede Aussage ist mit konkretem Pointer (Commit-SHA, ctx-Block-ID, file:line) belegt. Wenn Folge-Session paraphrasiert: das ist Drift.

## Pflicht-Lesen (in dieser Reihenfolge, vor erster Welle)

1. **Warnings** — `/root/.claude/projects/-compose-n8n/memory/warnings.md`
   - W3, W6a-d, W9, W10, W11, W12, W13, W14 sind Welle-relevant
   - 14-Punkt-Self-Audit nach jeder Welle
2. **MEMORY.md** — `/root/.claude/projects/-compose-n8n/memory/MEMORY.md` — "Aktueller Stand" S34 → S30 chronologisch
3. **ctx-Blöcke (in dieser Reihenfolge)**:
   - `019dfa6b-cb82-7bb4-841a-34c9e008c161` — Welle 35/36/37 + v1.1.0-Promote (jüngste)
   - `019df9e4-1070-746f-a0de-4801e164b324` — Welle 34 (vor v1.1.0)
   - `019d3f5a-0f6a-72f4-9ac3-3be356abee47` — S12 Generative-Synthese-Design (für 38c)
   - `019d3f58-3e34-71cd-98bb-b91bb7ee508a` — S12 Quality-Decay-Prevention
   - `019d724e-4508-7140-bd13-8077886aa57f` — S22 ValidateTemporal-Design (schon implementiert)
   - `019d41c4-d2ff-7023-9087-947f2e1272ab` — S15 Dream-Mode-Phase-1
4. **Dream-Vorbereitung Submodule**: `.project/bench-dream-phase0/`
   - `empirie-snapshot-2026-05-06.md` — Quantitative Bestandsaufnahme (Pick × num_dates, Confidence × target_dates, Coverage-Backlog)
   - `BRANCH-HYPOTHESIS-Dream-Mass-Confidence.md` — Welle 38a
   - `BRANCH-HYPOTHESIS-Dream-Cyclic-Linking.md` — Welle 38b
   - `BRANCH-HYPOTHESIS-Dream-Generative.md` — Welle 38c
   - `bench-methodology-dream.md` — eval-dream.sh-Skelett + Skalierbarkeit-Section bis 1M+

## Production-State-Snapshot (verifiziert 2026-05-06 12:55 UTC)

**Pflicht: re-verifizieren wenn Folge-Session > 7 Tage Abstand ODER > 10 Block-Differenz** (siehe Re-Verifikations-Trigger unten).

| Komponente | Stand |
|---|---|
| Hauptrepo HEAD | `089ae8b` — fix: self-audit follow-up W2/W6d/W8 |
| Branch | `root` (lokal 18+ commits ahead origin) |
| Letzter Tag | **v1.1.0** annotated (commit e2df021, pushed) |
| Submodule .project | `143c842acd` — fix(dream-prep): self-audit follow-up |
| DB Migrations | **30** (latest M030 = mass-in-rrf-score) |
| Active blocks | **446** (archived 81) |
| Dream-Links | 1423 (1285 v4, 138 v3) |
| ctx Container | Up 14h (healthy), env: CTX_SCORE_THRESHOLD=0.001 + CTX_CONFIDENT_THRESHOLD=0.008 |
| ctx_rrf-Function | M030 (block_mass CTE + mass_factor multiplier — verified) |

**Re-Verifikations-Trigger** (eines davon → state.sh + diff against snapshot):
- > 7 Tage Abstand zur 2026-05-06 12:55 UTC-Notation oben
- > 10 Block-Differenz vs aktueller 446 active (z.B. > 456 oder < 436)
- Tag-Push seit v1.1.0 (= neue Production-State)
- DB-Migration-Count > 30 (jemand hat M031+ appliziert)

Falls eines getriggert: zuerst `bash state.sh`, `git log --oneline origin/root..HEAD`, dann Empirie-Snapshot 2026-05-06 re-validieren in 38a-Pre-Phase. NICHT auf Folge-Welle starten ohne Re-Validation.

## Was diese Session abgeschlossen hat

### v1.1.0 promoted (autonom-Welle 35-37)
M030 (Mass-im-RRF-Score) + ScoreThreshold-Rekalibrierung 0.005→0.001 als Production. Tag annotated + pushed (origin root + v1.1.0). Detail-Audit ctx `019dfa6b`.

### Dream-Erweiterung-Vorbereitung
3 Pre-Hypothesen + Empirie-Snapshot + Bench-Methodology in `.project/bench-dream-phase0/` (siehe Pflicht-Lesen oben).

## Wichtigste Empirie-Befunde (komprimiert, Stand 2026-05-06 12:55 UTC)

1. **Mega-blocks dominieren NICHT als Dream-source** — `is_meta`-Filter excludiert sie schon (PickBlock dream.go:283 `AND NOT is_meta`). Pick-Distribution: 0-dates 3.53 avg, 1-2-dates 2.89, 3-5 2.25, 6-10 2.00, 11+ **null**. Korrektur einer initialen Vermutung — siehe Empirie-Snapshot für Begründung.
2. **Topical-Confidence-Anomalie bei 3-dates targets**: avg_conf=0.676 vs raw=0.875 (23pp Damping). Mechanismus: `weightedConfidence = raw * source_q * target_q` (writelinks.go:188). Vermutung: 3-dates blocks haben niedrigere quality_score. **Validation in 38a-Pre-Phase pflicht (W3)**.
3. **Coverage-Backlog**: 32 blocks ohne outgoing-links → 13 is_meta (correct exclude), **16 echter gap** (knowledge/snapshot/canonical). 3.6% des aktiven Korpus.
4. **Topical-Monoculture 87%** (1115/1285 v4). supersedes-Underproduktion: 4 v4 vs 13 v3.
5. **0 synthesis-Blöcke** (S12-Generative nicht implementiert).

## Welle-Backlog (priorisiert)

### Welle 38a — Dream-Mass-Confidence
**Aufwand**: 2-3h. **Pre-Welle-Pflicht (W3)**: quality_score × num_dates Empirie validieren bevor Code-Patch.

**Schritte**:
1. SQL: `SELECT array_length(content_times,1), AVG(quality_score) FROM context_blocks GROUP BY 1` — bestätigt das 3-dates-quality-Tief?
2. Sub-Agent-Audit von 30 low-conf 3-dates-Topical-Links (**W13**: Sub-Agent-Prompt self-contained, mit konkreten Block-IDs + Pfaden + Zielfrage). Sind die WIRKLICH falsch oder nur falsch-gedämpft?
3. Wenn empirisch begründet → Code-Patch (massFactor in writelinks.go), dream_version 4→5
4. Bench: nur Stable-Gold-Sample (eval.sh + eval-cyclic.sh sind orthogonal — nicht direkt betroffen). **W14**: kein 5s-polling, until-loop + run_in_background.
5. Promote bei high_conf-rate ≥ 80% (von aktuell 72%) UND coverage-retention ≥ 95% für single-date targets
6. **W12**: 1 logische Änderung pro Commit. Pre-Empirie-Audit + Code-Patch + dream_version-Bump + Test-Update können in 2-3 separate commits.

### Welle 38b — Dream-Cyclic-Linking
**Aufwand**: 4-6h. **Migration nötig**: CHECK constraint erweitern (M032 oder M033).

**Schritte**:
1. Pre-Empirie (W3): wie viele Block-Paare würden als recurrent erkannt (deterministisch Phase-1)?
2. Sub-Agent-Audit von 20 candidate-Paaren (**W13**: candidates als block-id-pairs in Prompt mitgeben).
3. Migration M032 + Code: DetectRecurrence + Phase-2 LLM-call in dream cycle
4. Bench + supersedes-rate-tracking. Recurrent-confidence-Floor 0.8 vs implicit-supersedes-promotion 0.85 als zwei-stufige Hürde (siehe W6d-Mitigation in Cyclic-Hypothese).
5. **W12**: Migration + Code + Tests = 3 commits, nicht 1.

### Welle 38c — Dream-Generative-Tagesbericht (S12 Phase 1)
**Aufwand**: 3-4h.

**Schritte**:
1. Schema-Status: Migration für `block_type='synthesis'` prüfen, ggf. Migration erstellen
2. WriteSynthesisBlock + GenerateDailyReport (LLM-prompt)
3. Scheduler-integration: 1× täglich (z.B. 03:00 lokal nach normalem Dream-cycle)
4. Mark synthesis-blocks `is_meta=TRUE` damit ctx_rrf sie nicht als Sources liefert (**W11**-Drift-Schutz)
5. 7-Tage-Validation: prüfen Tagesberichte werden korrekt erstellt + superseded

### Reihenfolge-Empfehlung
1. **38a** zuerst (geringster Risiko, klärbar-empirisch)
2. **38b** dann (Migration nötig, dream_version-bump kann mit 38a kombiniert werden)
3. **38c** parallel oder nach 38b (orthogonal)

### Welle 39 — Korpus-Erweiterung (Multiplier)
**Anlass**: bei n=70 ist 1 Case = 1.43pp Sensitivity. Welle-36 L-Bucket -1pp ist innerhalb noise.

**Optionen**: Production-Telemetry (`context_access_log.query_text` 30d, anonymisiert) + Synthetic Hard-Cases (**W10**-IKEA-Risiko: synthetic-bias gegen real queries beachten). Aufwand: ~1 Session pro Quelle. Parallel-fähig zu 38a/b/c.

### Welle 40 — RAG-Vergleich (User-Vision-Closure)
**User-Ziel**: "ctx besser als andere RAG systeme".

**Schritte**: Vergleichsmetriken (TopK-overlap@5, MRR, Latency p95, Coverage), Test-Korpus (70-case + ggf 39-Erweiterung), Vergleichs-Systeme (LangChain BM25+dense, LlamaIndex+reranker, Cohere Rerank), apples-to-apples (gleicher Korpus, Embeddings Qwen3-8b 1024d, Test-Queries), Audit als reference-block. Aufwand: Tage.

## Quantitative Ziele post-Dream-Erweiterung

| Metrik | Aktuell (12:55 UTC) | Konservativ-Ziel | Ambition-Ziel | Pfad |
|---|---|---|---|---|
| Topical avg_conf | 0.769 | ≥ 0.80 | **≥ 0.85** | 38a Mass-Damping |
| High-conf-rate (≥0.7) | 72% | ≥ 80% | **≥ 90%** | 38a + 38b (recurrent-extraction filtert low-conf-Tail) |
| supersedes-share | 0.3% | ≥ 1% | **≥ 3%** | 38b implicit-supersedes via recurrent |
| recurrent-share | 0% | ≥ 3% | **≥ 8%** | 38b Phase-1+Phase-2 |
| synthesis-blocks/Woche | 0 | ≥ 7 | **≥ 14 (Tages+Wochen)** | 38c Phase-1+2 |
| Coverage-Gap (echte) | 16 | ≤ 10 | **≤ 5** | 38a + Pick-Logik-Audit |

**W6a/W8-Hinweis**: konservativ-Ziele sind Methodology-Sensitivität-Schwelle. Ambition-Ziele sind Production-Maturity-Indikatoren. Bei Ziel-Scale 1M+ blocks: Methodology-Wechsel zu Cluster-Aggregat-Metriken (siehe `bench-methodology-dream.md` Skalierbarkeit-Section). NICHT bei aktuellen 446-Block-Werten als "perfekt" werten — die n=70 Sensitivity macht +1pp ununterscheidbar von Rauschen.

## Drift-Disziplin für Folge-Session

1. **Re-Baseline**: bei Welle-Δ-Bewertung muss Re-Baseline auf aktuellem state. Empirie aus diesem temp.md ist 2026-05-06 12:55 UTC stand. Bei > 7 Tage Abstand oder > 10 Block-Differenz → re-verifizieren (siehe Re-Verifikations-Trigger oben).
2. **Dream-Empirie ist orthogonal zu eval-cyclic/eval.sh** — nicht in einem Bench mengen. eval-dream.sh-Skelett in `bench-methodology-dream.md`.
3. **dream_version bump**: 38a + 38b ggf. in EINEM bump (v4 → v5). 38c kann separater bump (v5 → v6). Deciding-Faktor: ob gleicher Reset-Cycle möglich ist.
4. **synthesis-blocks als is_meta=TRUE** schützen vor **W11** (Korpus-Drift in Retrieval-Bench).
5. **Sub-Agent-Audit-Pattern aus S25** (n=30, stratifiziert per-relationship + per-confidence-bucket, reproducible-seed) wiederverwenden. **Bei Ziel-Scale > 50k blocks**: Wechsel zu Cluster-Aggregat (siehe Methodology Skalierbarkeit).

## Welle-Scope-Klärung (was bleibt stabil)

- **Pick-Logik bleibt is_meta-basiert** — Mega-blocks sind schon excluded; Mass wirkt nur in Confidence-Formel (38a).
- **raw_confidence bleibt LLM-Output** — nur final-confidence (writelinks.go:188) ist code-side modifizierbar.
- **ScoreThreshold bleibt 0.001** — Welle-37-Empirie hat den Wert validiert, N-Bucket bei 100%.
- **Mass-Definition bleibt 1/sqrt(N)** — Welle-37 production-ready, Variants (log/cap) nicht in 38-Scope.
- **Bench-Output bleibt per-relationship-breakdown** — granular > aggregierte single-score.

## Methodische Disziplin (14-Warning-Cross-Reference)

- **W3 (Empirie)**: jede Hypothese vor Implementation in BRANCH-HYPOTHESIS-*.md mit Zeitstempel + quantitative Targets. Pre-Welle-Empirie-Pflicht in 38a/b/c-Schritten markiert.
- **W6a (konservativer Anker)**: Re-Baseline obligatorisch. Quantitative Ziele oben haben "Konservativ" + "Ambition" — NICHT auf Konservativ stehen bleiben.
- **W6c (Erlaubnis-Inversion)**: bei klarem Plan + User-"los geht's"-Pattern direkt machen. Bei zwei Wegen: A/B-Test, nicht User fragen. Vorbereitung dieser temp.md ist genug — NICHT vor Welle-Start nochmal user-konfirmieren.
- **W6d (Vorsicht-als-Argument)**: konkretes Risiko oder schweigen. Risiko-Sections in den Pre-Hypothesen sind konkret (FilterSuperseded.go-Pfad, separate confidence-Floors).
- **W9 (Armada-Konsens)**: keine Sub-Agents für Empirie-Generierung. Bench-Runs LIVE im Orchestrator. Sub-Agents nur für Code-Patches + Sample-Audits.
- **W10 (IKEA-Effekt)**: dual-bench (oder per-Welle entsprechende Bench-Klasse) BEVOR Bewertung. Keine "EXTREM STARK"-Phrasen pre-empirisch.
- **W11 (Config-Drift)**: feature-Branch-Migrations isoliert (revert-SQL bereitliegt). state.sh nach jeder Welle. synthesis-blocks `is_meta=TRUE` für Welle 38c.
- **W12 (Wave-Pattern)**: 1 logische Änderung pro Commit. Migration + Code + Audit-Doku in separaten Commits.
- **W13 (Agent-Kontext)**: Sub-Agent-Prompts self-contained mit Pfaden, line-numbers, konkreten Block-IDs, Zielfrage. Sub-Agents droppen CLAUDE.md + gitStatus.
- **W14 (Polling)**: Bench-Wartezeiten via until-loop + run_in_background. Kein 5-15s-Polling.

## Pointer-Sammlung

- v1.1.0-Detail-Audit ctx: `019dfa6b-cb82-7bb4-841a-34c9e008c161`
- S12-Generative-Design: `019d3f5a-0f6a-72f4-9ac3-3be356abee47`
- S22-ValidateTemporal: `019d724e-4508-7140-bd13-8077886aa57f`
- S15-Dream-Mode-Phase-1: `019d41c4-d2ff-7023-9087-947f2e1272ab`
- S12-Quality-Decay-Prevention: `019d3f58-3e34-71cd-98bb-b91bb7ee508a`
- Pre-Hypothesen + Methodology: `.project/bench-dream-phase0/`
- writelinks.go:188 — Confidence-Formel (Code-Stelle für 38a)
- linkfilters.go:21-26 — minRawConfidence map (per-type confidence-floor)
- dream.go:283 — PickBlock-Filter (is_meta-exclude bestätigt)
- 14-Warnings: `/root/.claude/projects/-compose-n8n/memory/warnings.md`

Die nächste Session liest Pflicht-Lesen-Reihenfolge → prüft Re-Verifikations-Trigger → wählt 38a (empfohlen) → arbeitet mit Pre-Empirie → löscht diese Datei nach dem Lesen.
