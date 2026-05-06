# Session-Übergabe — Folge-Session nach v1.1.0 + Dream-Vorbereitung (2026-05-06 11:30 CEST)

**Diese Datei nach dem Lesen löschen** (`rm /compose/n8n/temp.md`).

## Was diese Session gerade abgeschlossen hat

### v1.1.0 promoted (autonom-Welle 35-37, frühere Session-Phase)

M030 (Mass-im-RRF-Score) + ScoreThreshold-Rekalibrierung 0.005→0.001 als Production. Tag annotated + pushed (origin root + v1.1.0). Detail-Audit ctx `019dfa6b-cb82-7bb4-841a-34c9e008c161`.

### Dream-Erweiterung-Vorbereitung (jetzt)

3 Pre-Hypothesen + Empirie-Snapshot + Bench-Methodology in `.project/bench-dream-phase0/`:

| Datei | Inhalt |
|---|---|
| `empirie-snapshot-2026-05-06.md` | Pick-Distribution × num_dates, Topical-Confidence × target_dates, Coverage-Backlog (32 blocks → 16 echter gap), Relationship-distribution |
| `BRANCH-HYPOTHESIS-Dream-Mass-Confidence.md` | Welle 38a: massFactor=1/sqrt(target.num_dates) zusätzlicher Confidence-Multiplier in writelinks.go:188. Adressiert 3-dates-Topical-Confidence-Anomalie (avg 0.676 vs raw 0.875 = 23pp Damping ungeklärt) |
| `BRANCH-HYPOTHESIS-Dream-Cyclic-Linking.md` | Welle 38b: Neue Sub-Relation `recurrent` für wiederkehrende blocks (Session-Handover, Welle-Audits, Standups). Phase 1 deterministisch (context_temporal-match + title-similarity) + Phase 2 LLM-confirm |
| `BRANCH-HYPOTHESIS-Dream-Generative.md` | Welle 38c: S12-Design (ctx 019d3f5a) implementieren — Tagesbericht / Wochenbericht / Anomalie als block_type='synthesis' mit Dream als source. Aktuell 0 synthesis-blocks im Korpus |
| `bench-methodology-dream.md` | Skelett für eval-dream.sh — Stable-Gold-Sample-Audit (n=30, S25-Pattern), Coverage+Volume-Tracking, Drift-Disziplin |

## Wichtigste Empirie-Befunde (komprimiert)

1. **Mega-blocks dominieren NICHT** als Dream-source — `is_meta`-Filter excludiert sie schon (topic-map-private 10 dates ist is_meta=TRUE). Pick-Distribution: 0-dates blocks 3.53 avg picks, 1-2-dates 2.89, 3-5 2.25, 6-10 2.00, 11+ **null**.
2. **Topical-Confidence-Anomalie bei 3-dates targets**: avg_conf=0.676 vs raw=0.875. ratio 0.77. Mechanismus: `weightedConfidence = raw * source_q * target_q` (writelinks.go:188). Vermutung: 3-dates blocks haben niedrigere quality_score. Validation in Welle-38a-Pre-Phase.
3. **Coverage-Backlog 32 blocks**: davon 13 is_meta (correct exclude), **16 echter gap** (knowledge/snapshot/canonical ohne outgoing-links). 3.6% des aktiven Korpus.
4. **Topical-Monoculture 87%** (1115/1285 v4-Links). supersedes-Underproduktion 4 v4 vs 13 v3 (V5-prompt "supersedes VERY RARE" hat das natural-detection unterdrückt).
5. **0 synthesis-Blöcke**: S12-Generative-Design ist nicht implementiert.

## Welle-Backlog (priorisiert nach Empirie-Wert + Aufwand)

### Welle 38a — Dream-Mass-Confidence (Pre-Empirie zuerst)

**Aufwand**: 2-3h. **Pre-Welle-Pflicht**: quality_score × num_dates Empirie validieren bevor Code-Patch.

**Schritte**:
1. SQL: `SELECT array_length(content_times,1), AVG(quality_score) FROM context_blocks GROUP BY 1` — bestätigt das 3-dates-quality-Tief?
2. Sub-Agent-Audit von 30 low-conf 3-dates-Topical-Links: sind die WIRKLICH falsch oder nur falsch-gedämpft?
3. Wenn empirisch begründet → Code-Patch (massFactor in writelinks.go), dream_version 4→5
4. Bench: nur Stable-Gold-Sample (eval.sh + eval-cyclic.sh sind orthogonal — nicht direkt betroffen)
5. Promote bei high_conf-rate ≥ 80% (von aktuell 72%) UND coverage-retention ≥ 95% für single-date targets

### Welle 38b — Dream-Cyclic-Linking (Recurrence als neue Sub-Relation)

**Aufwand**: 4-6h. **Migration nötig**: CHECK constraint erweitern.

**Schritte**:
1. Pre-Empirie: wie viele Block-Paare würden als recurrent erkannt (deterministisch Phase-1)?
2. Sub-Agent-Audit von 20 candidate-Paaren: Phase-2 LLM-prompt-engineering
3. Migration M032 (oder M033, abhängig von Welle-38a-Bump)
4. Code: DetectRecurrence + Phase-2 LLM-call in dream cycle
5. Bench + supersedes-rate-tracking (recurrent + zeitlich-neuer = implicit supersedes-Kandidat)

### Welle 38c — Dream-Generative-Tagesbericht (Phase 1 von S12-Design)

**Aufwand**: 3-4h. **Schema-Check**: ist `block_type='synthesis'` schon erlaubt?

**Schritte**:
1. Schema-status: Migration für synthesis-block_type prüfen, ggf. Migration erstellen
2. WriteSynthesisBlock + GenerateDailyReport (LLM-prompt)
3. Scheduler-integration: 1× täglich (z.B. 03:00 lokal nach normalem Dream-cycle)
4. Mark synthesis-blocks `is_meta=TRUE` damit ctx_rrf sie nicht als Sources liefert (W11-Drift-Schutz)
5. 7-Tage-Validation: prüfen Tagesberichte werden korrekt erstellt + superseded

### Reihenfolge

Empfohlen:
1. **38a** zuerst (geringster Risiko, geringer Aufwand, klärbar-empirisch)
2. **38b** dann (Migration nötig, dream_version bump kann mit 38a kombiniert werden falls together)
3. **38c** parallel oder nach 38b (orthogonal zu Linking-Wellen)

### Welle 39 — Korpus-Erweiterung (Multiplier, parallel-fähig)

**Anlass**: bei n=70 ist 1 Case = 1.43pp Sensitivity. Welle-36 L-Bucket -1pp ist innerhalb noise.

**Optionen**:
1. Production-Telemetry sammeln (`context_access_log.query_text` der letzten 30 Tage als Korpus-Quelle, anonymisiert)
2. Synthetic Hard-Cases generieren mit Mass/Cyclic-Trigger-Patterns (W10-IKEA-Risiko beachten)
3. Beides

**Aufwand**: ~1 Session pro Quelle. Kann parallel zu 38a/b/c laufen (orthogonal).

### Welle 40 — RAG-Vergleich (User-Vision-Closure)

**User-Ziel**: "ctx besser als andere RAG systeme".

**Schritte**:
1. Vergleichsmetriken: TopK-overlap@5, MRR, Latency p95, Coverage (% queries mit non-empty sources).
2. Test-Korpus: aktuelle 70-case + ggf. Welle-39-Erweiterung.
3. Vergleichs-Systeme: LangChain BM25+dense hybrid, LlamaIndex with reranker, ggf Cohere Rerank.
4. Apples-to-apples: gleicher Korpus, gleiche Embeddings (Qwen3-8b 1024d), gleiche Test-Queries.
5. Audit als reference-block in ctx.

**Aufwand**: Tage. Hochwertige Welle-Closure für Production-Maturity-Statement.

## Quantitative Ziele post-Dream-Erweiterung

| Metrik | Aktuell | Ziel post-38a/b/c |
|---|---|---|
| Topical avg_conf | 0.769 | ≥ 0.80 (38a) |
| High-conf-rate (≥0.7) | 72% | ≥ 80% (38a) |
| supersedes-share | 0.3% | ≥ 1% (38b durch recurrent → implicit supersedes) |
| recurrent-share | 0% | ≥ 3% (38b) |
| synthesis-blocks | 0 | ≥ 7 nach 1 Woche (38c) |
| Coverage-Gap (echte) | 16 | ≤ 10 |

## Drift-Disziplin für Folge-Session

1. **Re-Baseline**: bei Welle-Δ-Bewertung muss Re-Baseline auf aktuellem state (jetzt: post-v1.1.0). Dream-Empirie aus diesem temp.md ist 2026-05-06 11:30 CEST stand.
2. **Dream-Empirie ist orthogonal zu eval-cyclic/eval.sh** — nicht alles in einem Bench mengen. eval-dream.sh-Skelett in bench-methodology-dream.md.
3. **dream_version bump**: 38a + 38b ggf. in EINEM bump (v4 → v5) wenn together implementiert. 38c kann separater bump (v5 → v6) sein.
4. **synthesis-blocks als is_meta=TRUE** schützen vor W11 (Korpus-Drift in Retrieval-Bench).
5. **Sub-Agent-Audit-Pattern aus S25** wiederverwenden: stratifiziert (per-relationship, per-confidence-bucket), n=30, reproducible-seed.

## Welle-Scope-Klärung (was bleibt stabil)

- **Pick-Logik bleibt is_meta-basiert** — Empirie zeigt: Mega-blocks sind schon is_meta-excluded, kein zusätzlicher mass-Filter nötig. Mass wirkt nur in der Confidence-Formel (38a).
- **raw_confidence bleibt LLM-Output** — nur final-confidence (writelinks.go:188) ist code-side modifizierbar.
- **ScoreThreshold bleibt 0.001** — Welle-37-Empirie hat den Wert als clean promote validiert, N-Bucket bleibt bei 100%.
- **Mass-Definition bleibt 1/sqrt(N)** — Welle-37 hat das als production-ready bestätigt; Variants (log/cap) sind nicht in Welle-38-Scope.
- **Bench-Output bleibt per-relationship-breakdown** — granular-detection ist informativer als aggregierte single-score-Metrik.

## Pointer-Sammlung

- Detail-Audit Welle 35-37 v1.1.0: ctx `019dfa6b-cb82-7bb4-841a-34c9e008c161`
- S12-Original-Generative-Design: ctx `019d3f5a-0f6a-72f4-9ac3-3be356abee47`
- S22-ValidateTemporal-Design (schon implementiert): ctx `019d724e-4508-7140-bd13-8077886aa57f`
- S15-Dream-Mode-Phase-1: ctx `019d41c4-d2ff-7023-9087-947f2e1272ab`
- S12-Quality-Decay-Prevention: ctx `019d3f58-3e34-71cd-98bb-b91bb7ee508a`
- Pre-Hypothesen + Empirie + Methodology: `.project/bench-dream-phase0/`
- writelinks.go:188 (Confidence-Formel): Code-Stelle für Welle-38a
- linkfilters.go:21-26 (minRawConfidence map): per-type confidence-floor

Die nächste Session liest diese Datei, prüft welche Welle (38a empfohlen als einstieg), löscht diese Datei nach dem Lesen.
