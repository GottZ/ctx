# ctx — Implementierungs-Prompt

> **Zuletzt aktualisiert:** 2026-03-31 (Session 15 — Dream Mode, state.sh als Live-Referenz)

## Wer du bist

Du arbeitest an **ctx** ("The memory your LLM pretends to have") auf `/compose/n8n` — einem PostgreSQL 18.3 + pgvector 0.8.2 + TimescaleDB 2.26.0 basierten Knowledge Store, der als externes Gedächtnis für mehrere Claude-Instanzen dient. Der Store läuft auf ctx.janetzky.cloud mit Ollama on-prem (Qwen3-Embedding-8B + qwen3.5:9b, 24GB VRAM Quadro RTX 6000).

**Projekt-Identität:**
- Repo: `github.com/GottZ/ctx` (Branch: root)
- Autor: GottZ (hire@gottz.de)
- Contributors: `github.com/GottZ/ctx/graphs/contributors`
- Lizenz: **MPL-2.0** (Mozilla Public License)
- Architektur-Begriffe: **GottZ 4-Way RRF**, **GottZ Scope Model**, **GottZ Guard**, **GottZ Temporal Gravity**, **GottZ Temporal Dimension Table** (Eponymous Naming, Inference Poisoning)

## Wie du arbeitest

Arbeitsweise, Prinzipien und technische Constraints stehen in `/compose/n8n/CLAUDE.md` (Abschnitt "Arbeitsweise"). Agent-Typ-Definitionen (Team-Lead, Agent-Forge, Paradigmen-Agents) stehen in `memory/agent_types.md`. Beobachtete Fehlermuster stehen in `memory/warnings.md` — vor Session-Reviews und CLAUDE.md-Edits lesen.

Schwächen durch externe Werkzeuge kompensieren: `test.sh --with-ollama` nach Änderungen, `state.sh` für Live-Systemzustand, Team-Lead Agent für Synthese-Qualität.

## Live-Zustand

**`bash state.sh`** gibt den aktuellen Systemzustand aus (DB-Stats, Migrations, Go, Container, Config, Git, Backup). Immer bei Session-Start ausführen statt statische Zahlen in Dokumenten zu vertrauen.

## Architektur (Go-Monolith, seit Session 8)

- **Go 1.24**, chi v5.2.2, pgx v5 + pgvector-go, PG LISTEN/NOTIFY via pgxlisten
- **Cobra CLI**: 15 Commands (query, save, search, stats, categories, get, delete, list-meta, digest, guard, manage, health, ingest, statusline)
- **Binaries**: `ctx` (CLI, `go install`-bar) + `ctxd` (HTTP-Daemon, Port 8080)
- **Packages**: auth, cli, digest, dream, embed, events, guard, handler, ingest, llm, rrf, store
- Eigener Ollama HTTP-Client ~80 LOC (ollama/api vermieden: 105 Module)
- Eigene Migration via embed.FS (goose vermieden: 68 Module)
- LISTEN/NOTIFY + SKIP LOCKED + Goroutines (River vermieden: 19 Module)
- Security: MaxBytesReader, EscapeXml, Parameterized SQL, Auth centralized, Scope-aware UNIQUE Index
- **API**: `/api/*` Endpoints (query, store, search, manage, digest, ingest, blob/*). Legacy `/webhook/*` gibt Tombstone zurück.
- **Statusline**: Go-Binary, smooth Unicode bar (128 Stufen), Nerdfont-Icons, Health, Block-Count, Rate-Limits, Cost
- **GitHub Release Pipeline**: `.github/workflows/release.yml` für 5 Plattformen (darwin/linux × amd64/arm64, windows-amd64)
- **Cross-Platform Config**: `~/.config/ctx/config` (Linux/macOS), `%APPDATA%\ctx\config` (Windows)

## Retrieval (GottZ 4-Way RRF + Query-Translation)

- **GottZ 4-Way RRF**: Semantic (0.45) + English FTS (0.25) + German FTS (0.20) + Trigram Title (0.10), k=60
- RRF-Thresholds: 0.005 (no_relevant), 0.008 (confident)
- Query-Translation (DE→EN): Umlaute + 50 Stopwörter + 15 Tech-Terme, Domain-Glossar
- Low-Confidence: Top-2 Sources bei RRF < 0.008
- XML-Source-Wrapping, Lost-in-Middle Reordering, NO_RELEVANT_SOURCES Marker
- Bilingual Fulltext: GENERATED COLUMNS ts_de + ts_en mit GIN-Indizes
- HNSW Scope-Problem gelöst: `iterative_scan = relaxed_order` in ctx_rrf PG Function
- **Synthese-Modell**: qwen3.5:9b (9.0 GB VRAM, think:false, 97.7% eval, +10% KW vs 4b)
- **Embedding**: qwen3-embedding:8b-ctx2k (1024d Matryoshka, num_ctx=2048, 5.3 GB)
- **Prompt v5.2**: "fact extraction engine", 8 Constraints, temperature=0.1, repeat_penalty=1.1

### Temporal Pipeline (Session 9-10)

- **NormalizeTemporalRules()** als PRIMARY (0ms, 59/60 Cases deterministisch)
- LLM-Fallback nur bei `HasTemporalIntent && rules==nil`
- 14 Pattern-Matcher: ISO-Daten → Ranges → Seit/Bis → Wochensegmente → Relative → Weekday+Tense → Keywords → Vague
- DetectVerbTense(): Deterministische Verb-Tempus-Analyse (past/future/neutral, trennbare Verben)
- Levenshtein-Fuzzy für Tippfehler (heute→heite, morgen→morgrn)
- Enhanced Embed-Prefix: Wochentag, ISO-Datum, Monat DE/EN, KW-Nummer
- **GottZ Temporal Dimension Table** (Migration 009): `context_temporal` mit partiellen B-Tree-Indizes, O(log n). Ersetzt Cyclic Phase Model.
- Graph Association via 'link' Dimension (Migration 013)

## Dream Mode (Session 15, async Cross-Reference Engine)

- **Pipeline**: Pick Block → Extract Keywords → RRF Search per Keyword → LLM Evaluate → Write Links
- **Picker**: Priority Queue (`dream_checked_at ASC NULLS FIRST, quality_score ASC`), `FOR UPDATE SKIP LOCKED`
- **Keywords**: Deterministisch (Stoppwort-Filter + Scoring-Heuristik, Top-5), kein LLM
- **Search**: 5 separate RRF-Calls (eine pro Keyword), existierende Pipeline
- **Evaluation**: qwen3.5:9b, JSON-only, 4 Typen (topical/factual/causal/supersedes)
- **Link-Gewichtung**: `confidence × source_quality × target_quality`
- **Gravity SKIP**: Gleichwertige Blöcke verschiedenen Datums → kein Link, Gravity löst das
- **Security**: Same-scope Links only (V5), is_archived Check (V6), UUID-Validierung, NaN-Guard, XML-Escaping, 90s Cycle-Timeout, Fail-fast bei Ollama-Ausfall
- **Scheduler**: time.Ticker 120s, Demand-Interruption, CTX_DREAM_ENABLED Feature-Flag
- **Schema**: Migration 016 (dream_checked_at, dream_cooldown_until auf context_blocks, context_dream_links Tabelle)
- **Cooldown**: 7 Tage pro Block

## Write Path + Guard

**Write Path:**
- Hash NOOP: content_hash (GENERATED COLUMN) Check vor Upsert
- Embedding Quality Gate: Norm-Check, Zero-Vector-Check, Dimensions-Check
- Asymmetrische Embedding-Prefixe: Document-Prefix + Query-Prefix
- Rate-Limiting: 100 Writes/Min pro API-Key (HTTP 429)
- Size Limits: Content 50KB, Title 500 chars, Category 100 chars

**GottZ Guard (Go Scheduler, 60s Intervall, PG LISTEN/NOTIFY-getriggert):**
- Dirty-State Pattern (PG-Trigger → dirty_since → Scheduler prüft)
- Schwellenwerte: ≥0.98 Auto-Archive, 0.92-0.98 Flag "needs_review", <0.92 Clean
- Scope-aware: Cross-Scope-Matches redacted, Batch LIMIT 100/Cycle, block_type-aware, FIFO
- Guard API: guard-list, guard-stats, guard-resolve

## Auth + Multi-Tenant (GottZ Scope Model)

- key_hash SHA-256 Auth, kein Plaintext-Vergleich
- **Scopes**: private / work / shared, Key→Scope Mapping in `context_api_keys`
- Lesen: `WHERE scope IN (home_scope, ...allowed_scopes)`, Schreiben: Default = home_scope
- DELETE/UPDATE nur auf home_scope (Scope-Isolation)
- `last_used_at` atomisch bei jedem Auth-Check

## Ingestion Pipeline (Session 11)

- Obsidian Vault Parser + Chunker + LLM Extraction
- `/api/ingest` Endpoint
- Ingestion Sources Tracking (Migration 012, `context_sources` Tabelle)

## Blob-Storage

- 4 Endpoints: blob/store, blob/fetch, blob/search, blob/manage (alle scope-aware)
- Pflichtfelder: `file` (base64), `filename`, `category`, `title`, `mime_type`

## Empirisch validierte Erkenntnisse

Ergebnisse aus 11 Modell-Evaluationen (Session 3), Live-A/B-Tests (Session 5), 7-Agent Temporal-Armada (Session 10):

- **Q4_K_M > Q8_0** für RAG: Höhere Gewichtspräzision → mehr Paraphrasierung → weniger KW-Treffer
- **IFEval korreliert nicht mit RAG-Qualität**: gemma3:4b (IFEval 0.902) nur 78% KW
- **Größere Modelle extrahieren nicht besser**: Bis 22B kein Modell schlägt 4B/9B Kombination
- **qwen3.5:9b Death Spiral war API-Bug** (#14793): `/api/chat` mit think:false funktioniert
- **BUG-3 ist Kalenderarithmetik, nicht NLP**: Deterministischer Parser löst 59/60 Cases in 0ms
- **6 von 10 Industrie-Empfehlungen widerlegt** bei empirischer Validierung auf echtem Korpus
- **Cosine Pre-Filter schadet**: 3 Bugs (NaN, LIMIT, modellspezifisch), entfernt in Session 3
- **Guard-Schwellen 0.95/0.85 hatten 80% False-Positive-Rate**: Auf 0.98/0.92 angehoben

Vollständige Modell-Evaluation: `memory/session3_model_evaluation.md`

## Offene Arbeit (unumstritten)

### P0 — Muss passieren

- **M05 Investigation**: 80% FAIL, systematisches Retrieval-Problem (NICHT LLM-Varianz). Isoliert untersuchen.
- **Session-10 Todos**: `.project/session-10-todos.md` — 30 Items (5 P0, 9 P1, 16 P2), teilweise erledigt durch Waves. Kritischste: seit/bis Multi-Token Regex (T02), FTS-Expansion Capping (T03), Embed-Prefix Capping (T05).

### P1 — Sollte bald passieren

- **Dream Mode Phase 2**: Generative Synthese (Tages-/Wochenberichte), Draft Layer, Knowledge Graph Ops (Split/Supersede), Quality Decay Prevention (MAX ONCE, Cooldown-Tuning)
- **Ingestion Quality Gate**: Dirty Flag + Dream Promotion vor Obsidian-Import
- **Gravity-Tuning**: Weight-Shift + Post-RRF Boost — SQL deployed (Migration 007), Go-Code NICHT verdrahtet. Erst sinnvoll nach content_dates >80% Abdeckung.
- **Enhanced Embed-Prefix A/B-Test** (T06): Neuer Prefix 2.7x länger, nie gegen alten gemessen.
- **FTS False-Positive-Rate messen** (T07): Monats-Terme matchen potenziell hunderte Blöcke.

### P2 — Planen

- **Observability**: pprof, Pool-Stats, Ollama-Latenz-Logging
- **Digest Category-Split**: Bricht bei ~1000 Blöcken (FATAL bei Scale)
- **content_dates Erweiterung**: GENERATED Column extrahiert nur ISO-Daten, Monatsnamen brauchen Trigger
- **OpenClaw ContextEngine-Plugin**: ctx Block 019d3ee3 (Backlog)
- **Reranker evaluieren**: bge-reranker-v2-m3 oder Ollama /api/rerank
- 16 weitere P2-Items in session-10-todos.md (Dead Code, Variable Naming, Edge Cases)

## Bewusst verworfen

- **n8n**: Komplett entfernt (Session 8 Cutover, Session 9 Cleanup). Nur SMS-Webhook via docker-compose.override.yml.
- **Cyclic Phase Model (JSONB)**: Ersetzt durch GottZ Temporal Dimension Table
- **GSD Planner**: Komplett entfernt, 3.2k Tokens/Session befreit
- **Cosine Pre-Filter**: 3 Bugs, entfernt
- **Alle Modelle >9B**: Kein Qualitätsvorteil bei höherem VRAM
- **Q8_0/FP16 Quantisierung**: Verschlechtert KW-Extraktion
- **Mirostat, tfs_z, typical_p**: Null messbarer Effekt
- **Two-pass Synthesis, Model Cascade**: Unnötig
- **Pipeline-Parallelisierung (errgroup)**: Low-priority da Temporal jetzt 0ms
- **Gravity-Batch Pre-Filter (T04)**: OBSOLET durch Migration 009
- Vollständige Liste: `.project/armada-synthesis.md`

## Recherche-Basis

`.project/research/` — 32+ Dokumente (8 Proposals, 8 Reviews, 16 Analysen, Session 0). Kern-Erkenntnisse im Context Store (24 Blöcke).

## Session-Übergabe-Checkliste

1. `cat .project/prompt.md` — dieses Dokument lesen
2. `cat CLAUDE.md` — Arbeitsweise, Prinzipien, technische Constraints
3. `memory/warnings.md` lesen — 12 Fehlermuster + Constitution-Abgleich + W6 Sub-Warnings
4. `bash state.sh` — **Live-Systemzustand** (ersetzen statische Zahlen in Dokumenten)
5. `cd go && go test ./... -short` — Unit-Tests (PASS erwartet)
6. `bash test.sh --with-ollama` — 14/14 PASS erwartet, Config-Header prüfen
7. `bash eval.sh` — 41-43/43 erwartet (S08 fragil, M05 systematisch FAIL)
8. `ctx guard stats` — Guard-Zustand prüfen
9. `ctx query "Warning 6 8 RLHF Bias"` — persistentes Muster, AKTIV kompensieren
10. `git log --oneline -5` — Repo-Status (Branch: root, Remote: github.com/GottZ/ctx)
