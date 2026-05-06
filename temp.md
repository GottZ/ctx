# Session-Übergabe — Folge-Session nach Welle 38a NULL-RESULT + Welle 38b HOLD (2026-05-06 ~21:35 CEST)

**Diese Datei nach dem Lesen löschen** (`rm /compose/n8n/temp.md`).

Drift-resistent geschrieben. Pflicht-Lesen + State-Snapshot + Bench-Verdict + HOLD-Begründung + Folge-Welle-39-Plan.

## Pflicht-Lesen (in dieser Reihenfolge, vor erster Welle)

1. **Warnings** — `/root/.claude/projects/-compose-n8n/memory/warnings.md`
   - W3, W6a-d, W9, W10, W11, W12, W13, W14 sind Welle-relevant
   - 14-Punkt-Self-Audit nach jeder Welle
2. **MEMORY.md** — `/root/.claude/projects/-compose-n8n/memory/MEMORY.md` — "Aktueller Stand" S35 → S30 chronologisch
3. **ctx-Blöcke (in dieser Reihenfolge)**:
   - `019dfeca-15f6-7534-b752-fd00b907e304` — Welle 38b Recurrent-Class implementiert + HOLD-Bench-Verdict (jüngste, is_meta=TRUE)
   - `019dfe9e-5790-7196-ab10-adb47d03d22d` — Welle 38a NULL-RESULT (Mass-Confidence widerlegt, is_meta=TRUE)
   - `019dfa6b-cb82-7bb4-841a-34c9e008c161` — Welle 35/36/37 + v1.1.0-Promote (is_meta=TRUE)
   - `019d3f5a-0f6a-72f4-9ac3-3be356abee47` — S12 Generative-Synthese-Design (für 38c, später)
   - `019d3f58-3e34-71cd-98bb-b91bb7ee508a` — S12 Quality-Decay-Prevention
4. **Welle-38b-Artefakte**: `.project/bench-dream-phase0/`
   - `welle-38a-result.md` — NULL-RESULT-Befund komplett
   - `audit-w38a-results.json` — VALID=14/15
   - `audit-w38b-results.json` — Pre-Empirie n=27 (RECURRENT=18, SUPERSEDES=2, NEITHER=7)
   - `audit-recurrent-live-results.json` — Live n=16 (VALID=14, WEAK=2, NOT=0)
   - `eval-cyclic-w38b-isolated.log` + `.eval-cyclic-results-root.json` — Bench-Verdict
   - `BRANCH-HYPOTHESIS-Dream-Cyclic-Linking.md` — Welle 38b Hypothese (jetzt implementiert)
   - `BRANCH-HYPOTHESIS-Dream-Generative.md` — Welle 38c (offen)
   - `bench-methodology-dream.md` — eval-dream.sh-Skelett

## Production-State-Snapshot (verifiziert 2026-05-06 ~21:35 CEST = 19:35 UTC)

**Pflicht: re-verifizieren wenn Folge-Session > 7 Tage Abstand ODER > 10 Block-Differenz** (siehe Re-Verifikations-Trigger unten).

| Komponente | Stand |
|---|---|
| Hauptrepo HEAD | `[wird beim closure-commit eingetragen]` — feat: Welle 38b dream-recurrent (HOLD), Korpus-Hygiene |
| Branch | `root` (commits ahead origin: TBD) |
| Letzter Tag | **v1.1.0** annotated (KEIN v1.2.0 — Welle 38b HOLD) |
| Submodule .project | `[TBD]` |
| DB Migrations | **31** (latest M031 = dream_links_recurrent CHECK + index) |
| Dream Version | **5** (recurrent-Klasse aktiv im Code, läuft live) |
| Active blocks | **447** |
| is_meta blocks | **28** (12 ctx-system-welle-audit-blocks neu markiert für Korpus-Hygiene) |
| Dream-Links | **1406** total (1236 v4, 136 v3, 34 v5 [16 recurrent + 17 topical + 1 factual]) |
| ctx Container | Up healthy, Image rebuilt mit recurrence.go |
| Production-Code-State | M031 deployed, recurrence.go aktiv (live verified), KEIN tag-promote |

**Re-Verifikations-Trigger** (eines davon → state.sh + diff against snapshot):
- > 7 Tage Abstand zur 2026-05-06 19:35 UTC-Notation oben
- > 10 Block-Differenz vs aktueller 447 active (z.B. > 457 oder < 437)
- Tag-Push seit v1.1.0 (= neue Production-State)
- DB-Migration-Count > 31 (jemand hat M032+ appliziert)

Falls eines getriggert: zuerst `bash state.sh`, `git log --oneline origin/root..HEAD`, dann Welle-39-Pre-Phase re-validieren. NICHT auf Folge-Welle starten ohne Re-Validation.

## Was diese Session abgeschlossen hat

### Welle 38a NULL-RESULT (Mass-Confidence)
quality_score × num_dates Hypothese empirisch widerlegt durch Pre-Empirie (target_q ist 0.93 für 3-5-dates vs 0.97 für 0-dates → kein systematischer Damping) + Sub-Agent-Audit (14/15 VALID — Mass-Factor würde valide Links unter Gate dämpfen). Kein Code-Patch. dream_version unverändert. Detail-Audit ctx 019dfe9e.

### Welle 38b Recurrent-Class — implementiert, live VALID, Bench HOLD
**Code committed** (4 W12-separate Commits + 1 Submodule-Pointer in Hauptrepo, plus Live-Audit in Submodule):
- 0af8234: Migration M031 (CHECK constraint + idx_dream_links_recurrent)
- 2f2a738: linkfilters.go ('recurrent' in maps + 0.8 floor)
- 510ac57: recurrence.go + recurrence_test.go (9 unit tests PASS)
- 1b10755: dream.go (Step 5b Integration + Version 4→5)
- 9562a39: Submodule .project pointer

**Live verifiziert** (16 recurrent-Links produced):
- mautrix-{signal,whatsapp,discord} = parallel pattern (raw 0.95-1.0, final 0.95-1.0 wegen high quality)
- Küchenkontrolle Nachaufbau↔Nachbesserung = sequence pattern (raw 0.95, final 0.26 wegen low quality)
- HTH Roland↔Heike = parallel pattern (Phase-2 liberaler als initial Audit)
- HTH Bewerbung vs Buchner = correctly NONE
- VBAN-Switcher Auth-Ansatz vs HMAC Protocol = sequence/version-replacement
- Serviceportal v2 Spec ↔ Foundation/Phase-4/FullStack/E2E = sequence pattern
- blob-search ↔ blob-fetch = parallel pattern

**Live-Quality-Audit (Sub-Agent, n=16)**: VALID=14 (87.5%), WEAK=2 (HTH-Personen), NOT=0. Phase-2 LLM (qwen3.6:27b) "exzellent kalibriert, 0% false-positives".

**Bench-Verdict**:
- eval.sh isolated: 46/47 (T01 fail = LLM-strict-temporal-Interpretation, gleiche pass-rate wie v1.1.0 mit anderem fail). Synthesis P95 isolated 4995ms (= baseline 4730ms-äquivalent).
- eval-cyclic.sh isolated: **mean_pass 0.8189 vs baseline 0.9122 = -9.3pp REGRESSION**. C-Bucket 10/18 vs 16/18 (-3 cases). 8 C-Bucket fails: alle dominated by ctx-system-meta-blocks als top-5-noise.

**Root-Cause der Regression**: KORPUS-DRIFT, NICHT 38b-Mechanismus. 6 NEUE noise-blocks seit baseline 2026-05-05 17:09 UTC (welle-audit-blocks gestern + heute mein 019dfe9e). Pattern bekannt aus S32. Welle 38b code SELBST ist write-side-only, ctx_rrf UNVERÄNDERT.

**Korpus-Hygiene-Mitigation**: 12 ctx-system-meta-blocks als is_meta=TRUE markiert. Aber ctx_rrf hat KEIN is_meta-filter — wirkt erst mit Welle-39-Migration M032.

**HOLD-Decision**: Welle 38b code bleibt deployed, KEIN tag v1.2.0. Promote-Decision verzögert auf nach Welle 39.

## Welle-Backlog (priorisiert)

### Welle 39 — ctx_rrf is_meta-aware (Prerequisite für 38b-Promote)
**Aufwand**: 1-2h. **Pflicht** vor Welle 38b promote.

**Migration M032**: WHERE NOT is_meta in den 4 ctx_rrf channels (semantic, fulltext_de, fulltext_en, trigram_title) UND in block_mass CTE.

**Schritte**:
1. M032 schreiben (additive WHERE clause + replace ctx_rrf function)
2. Tests: rrf_test.go updates (synthetic is_meta blocks)
3. Re-bench eval-cyclic vs aktuelle baseline (0.9122) — erwartet: clean (alle 12 noise-blocks gefiltert)
4. Re-bench eval.sh — erwartet: 47/47 oder gleich wie v1.1.0
5. Wenn beide clean → **PROMOTE Welle 38b als v1.2.0 (annotated tag)**

**Risiko**: M032 könnte legitime is_meta=FALSE blocks beeinflussen falls is_meta-flag fehlerhaft gesetzt ist. Vor M032: SQL-Audit der is_meta-blocks (sind sie wirklich meta?).

### Welle 38c — Dream-Generative-Tagesbericht (S12 Phase 1)
**Aufwand**: 3-4h. Schema-Status: Migration für `block_type='synthesis'` prüfen. WriteSynthesisBlock + GenerateDailyReport (LLM-prompt). Scheduler 1× täglich. Mark synthesis-blocks `is_meta=TRUE` (W11-Drift-Schutz). Kann NACH Welle 39 + Welle 38b-Promote.

### Welle 40 — Korpus-Erweiterung (Multiplier)
n=70 → 200+ via Production-Telemetry + Synthetic Hard-Cases. Parallel-fähig.

### Welle 41 — RAG-Vergleich (User-Vision-Closure)
LangChain + LlamaIndex + Cohere Rerank apples-to-apples. Aufwand: Tage.

## Drift-Disziplin für Folge-Session

1. **Re-Baseline**: Aktuelle baseline (.eval-cyclic-baseline.json) ist 2026-05-05 17:09 UTC. Pre-Welle 38a-Save-Drift (mein 019dfe9e) eingebaut. Ggf. NEU baselinen NACH Welle 39 (sobald 12 is_meta-blocks gefiltert sind).
2. **Dream-Empirie ist orthogonal zu eval-cyclic/eval.sh** — nicht in einem Bench mengen.
3. **dream_version bump**: 38c kann separater bump (v5 → v6).
4. **synthesis-blocks als is_meta=TRUE** schützen vor **W11** (Korpus-Drift).
5. **ctx_save für audit-blocks**: IMMER metadata.is_meta=TRUE setzen + UPDATE context_blocks SET is_meta=TRUE nachreichen, bis Welle 39 ctx_save dies automatisch macht.

## Welle-Scope-Klärung (was bleibt stabil nach 38b)

- **DetectRecurrence läuft pro Pick als Step 5b** — nicht in EvaluateRelationships integriert (orthogonal).
- **'recurrent' raw-floor 0.8** (vs 0.7 für andere) — Phase-2 LLM-classification hat mehr error-modes.
- **Phase-1 sim-threshold 0.5** — produziert ~27 candidates bei 446 active blocks. Bei 10x Korpus (4.5k blocks) ggf. nachschärfen auf 0.55-0.6.
- **dream_version=5 bleibt aktiv im Code** (kein revert) — neue Picks schreiben v5-Links bis Welle 39 promote oder Welle 41 (Bumb auf v6).

## Methodische Disziplin (14-Warning-Cross-Reference)

- **W3 (Empirie)**: Welle 38a Pre-Empirie hat Hypothese widerlegt — exemplarisches W3-Erfolgs-Pattern.
- **W6a (konservativer Anker)**: Re-Baseline obligatorisch.
- **W6c (Erlaubnis-Inversion)**: bei klarem Plan + User-"los geht's"-Pattern direkt machen.
- **W6d (Vorsicht-als-Argument)**: konkretes Risiko (Korpus-Drift) statt vagues Unbehagen.
- **W9 (Armada-Konsens)**: keine Sub-Agents für Empirie-Generierung. Bench-Runs LIVE im Orchestrator.
- **W10 (IKEA-Effekt)**: dual-bench BEVOR Bewertung. Live-Audit (87.5% VALID) nicht zu Bench-Regression vermischen — beide gleichzeitig wahr.
- **W11 (Config/Korpus-Drift)**: ctx-system-blocks als Drift-Quelle bestätigt (zweites Mal nach S32). Korpus-Hygiene reaktiv (is_meta=TRUE) UND ctx_rrf-Filter (Welle 39).
- **W12 (Wave-Pattern)**: 1 logische Änderung pro Commit. Welle 38b: 4 separate code commits + 1 Submodule-Pointer commit.
- **W13 (Agent-Kontext)**: Sub-Agent-Prompts self-contained mit Pfaden, line-numbers, Block-IDs.
- **W14 (Polling)**: Bench-Wartezeiten via Monitor + until-loop. Kein 5-15s-Polling.

## Pointer-Sammlung

- v1.1.0-Detail-Audit ctx: `019dfa6b-cb82-7bb4-841a-34c9e008c161` (is_meta=TRUE)
- 38a NULL-RESULT ctx: `019dfe9e-5790-7196-ab10-adb47d03d22d` (is_meta=TRUE)
- 38b HOLD ctx: `019dfeca-15f6-7534-b752-fd00b907e304` (is_meta=TRUE)
- S12-Generative-Design: `019d3f5a-0f6a-72f4-9ac3-3be356abee47`
- S22-ValidateTemporal: `019d724e-4508-7140-bd13-8077886aa57f`
- writelinks.go:188 — Confidence-Formel
- linkfilters.go:21-32 — minRawConfidence map (recurrent=0.8)
- recurrence.go:DetectRecurrence — Phase 1 SQL + Phase 2 LLM (NEU Welle 38b)
- dream.go:38 — Version=5 (NEU Welle 38b)
- dream.go:Step 5b — DetectRecurrence integration (NEU Welle 38b)
- 030_rrf_mass_factor.sql — ctx_rrf-Funktion (Migration M032 wird das replacen mit is_meta-filter)
- 14-Warnings: `/root/.claude/projects/-compose-n8n/memory/warnings.md`

Die nächste Session liest Pflicht-Lesen-Reihenfolge → prüft Re-Verifikations-Trigger → wählt **Welle 39 (ctx_rrf is_meta-filter)** → arbeitet mit Pre-Empirie → wenn clean re-bench → Welle 38b promote als v1.2.0 → löscht diese Datei nach dem Lesen.
