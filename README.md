# ctx — The memory your LLM pretends to have.

> Knowledge store with weighted 4-way RRF retrieval, multi-tenant scope isolation, multi-dimensional cyclic temporal gravity, and autonomous cross-referencing. Built for AI workflows that need to remember.

[![Release](https://img.shields.io/github/v/release/GottZ/ctx)](https://github.com/GottZ/ctx/releases)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-MPL--2.0-blue)](LICENSE)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-18-336791)](https://www.postgresql.org)

## What it does

ctx gives your LLM a persistent, searchable memory. Store knowledge blocks, query them with hybrid retrieval (semantic + bilingual fulltext + trigram), then rerank with multi-dimensional cyclic gravity — each temporal cycle (weekday, month, quarter, week, monthday, seasonal, daily) scored as its own Gaussian field. Queries like "immer dienstags" or "Weihnachten" activate specific dimensions; "Meeting am Dienstag, Ergebnis am Mittwoch" still pulls the Wednesday block (just weaker).

**Multiple anchors per block:** every block carries dimensions from both its content (dates mentioned in text) AND its `created_at` timestamp. A block about "Meeting am Dienstag" written on a Friday gets `weekday=2` (content anchor) AND `weekday=5` (meta anchor). Both signals contribute independently — "immer dienstags" queries find the content anchor; "Freitags-Arbeit" finds the meta anchor. Same principle for monthday, seasonal, daily, etc.

**Dream Mode** runs as a continuous background loop — autonomously discovering relationships between blocks, marking outdated information, and promoting high-quality content. Supports a separate model for evaluation (e.g. a larger model for better causal/supersedes reasoning). Parallel workers (`CTX_DREAM_PARALLELISM`, default 1) with atomic `FOR UPDATE SKIP LOCKED` block-claim — safe under contention. Your knowledge base grows, self-organizes, and stays current.

## How LLMs use ctx

ctx is designed to be the persistent memory layer for LLM agents. Five primitives, composable:

| Use case | Tool | When |
|----------|------|------|
| **Retrieve** prior knowledge before answering | `ctx query "question"` | Whenever the answer might depend on past sessions, project state, or stored decisions |
| **Persist** a new finding | `ctx save <category> <title> - <content>` | After non-obvious discoveries, architecture decisions, resolved bugs, config changes |
| **Update** an existing block | `ctx save` with same `<category> <title>` | category+title is upsert key — re-saving replaces |
| **Browse** without LLM cost | `ctx search [category] [query:text]` | Listing, sanity-checking, lightweight lookups |
| **Inspect** a specific block | `ctx get <block-id>` | Following an id from query sources or another block |

### Categories (semantic, not enforced)

`infrastructure`, `decisions`, `projects`, `reference`, `learnings`, `agent-briefing`, `index`.
Pick by intent: one fact per block, precise title, tags for cross-cutting. ~1-1.5k chars max — split, don't grow.

### Access paths (in order of preference for LLM agents)

1. **MCP** — `claude.ai ctx` server (Streamable HTTP transport). Tools: `query`, `store`, `search`, `get`, `recent`. JSON-schemas, no shell-quoting. Use this in Claude Code / claude.ai sessions.
2. **CLI** — `/usr/local/bin/ctx` — shell pipelines, cron, scripts. Config in `~/.config/ctx/config`.
3. **HTTP** — `POST /api/{query,store,search,manage}` direct — fallback when MCP/CLI unavailable.

### Multi-Tenant Architecture

`scope` column on `context_blocks` (`private` | `work` | `shared` | additional tenant scopes), enforced via API-key `home_scope`. Each LLM/tenant key sees:
- All blocks in its own scope
- All blocks in `shared` (cross-tenant knowledge layer)
- Nothing from other tenants' private scopes

API-key provisioning (v2.0.0+): `ctx keys create <label> --home <scope>` — `--home` is required, no implicit default.

## Using ctx effectively

Installing ctx gives an agent memory. Using it *well* takes discipline — because a memory shared across sessions has a failure mode a single chat doesn't: **drift**.

### Why stored memory drifts

Each time an LLM reads a note and re-saves or summarizes it, it re-interprets it through its own training biases. That isn't random noise — it's a directional filter that pushes the same way every pass: more conservative, more absolute, less attributed. Observations harden into recommendations, recommendations into rules, rules into dogma — and the certainty becomes untraceable.

A stored block is also a *point-in-time observation, not live state*. A note that was true when written ("we migrated off X") can stay true and still drive a wrong action (deleting X's still-running sibling service) — because the scope shifted and the note never said so. The note tells you where to look, not what's true right now.

### Discipline — put this in your agent's instructions

- **Load conventions into context before working — don't just file them away.** Effectiveness ranks training-weights > file-instructions > in-context anchors: only an anchor in the *current* context reliably overrides a trained default. A discipline doc that's never loaded gets silently re-undermined by each new session. (`ctx query` your project conventions at session start.)
- **Trace every stored claim to a source.** Save quote + date; keep verified user statements separate from your own interpretation. An interpretation re-saved as fact is how a "probably" disappears across three persistence layers.
- **Cross-check stored claims against live state before acting.** Before a destructive or status-dependent step, verify against the authoritative source — live config, a test, the actual file — not the note.
- **Don't gate on self-reported confidence.** Models are often just as sure when wrong. Gate on external truth: a test, the source, observed behavior.
- **Prefer external signals over self-reminders.** Naming a failure mode as a rule ("don't forget the tests") tends to re-evoke it; build a check instead — a test script, a grep on the output, a verifier against the raw data.

### Calibration

LLM defaults are tuned for a median user who must be protected from uninformed decisions. For an experienced operator with a defined target, the same training produces systematic distortion: judging against the current state instead of the target ("good enough for now"), preferring the familiar over the better option, asking permission on obvious next steps while making user-facing decisions unprompted, and presenting trained caution as judgement ("that's overkill") with no concrete risk named.

Compensating it is a one-time setup the agent should drive:

1. **Store the calibration as a block.** Have the agent write your conventions and observed failure modes into ctx — a dedicated "RLHF warnings" block is a good seed — so every future session can retrieve them instead of relearning them.
2. **Point your durable instructions at that block.** Your platform's personal-preference / custom-instruction field, or a project-level instruction file, should reference it. This is the step the agent should *prompt you* to do — it's the one layer the agent can't write for itself, and without it the block just sits there unread.
3. **Each session loads the anchor.** The durable instruction tells the agent to `ctx query` that block before working, so the calibration lands in *context* — the only layer that reliably overrides a trained default — instead of staying filed away.

State the *desired* behavior rather than the unwanted one (naming the bad behavior re-evokes it). This isn't about disabling safety — it's about re-aiming a calibration meant for someone else, and keeping that aim across sessions.

## Quick Install

```bash
# Binary (Linux/macOS/Windows)
curl -fsSL https://github.com/GottZ/ctx/releases/latest/download/ctx-$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') -o /usr/local/bin/ctx && chmod +x /usr/local/bin/ctx

# Or with Go
go install github.com/GottZ/ctx/cmd/ctx@latest
```

## Setup

### 1. Configure endpoint

```bash
# Linux/macOS
mkdir -p ~/.config/ctx
cat > ~/.config/ctx/config << 'EOF'
CTX_BASE_URL=https://your-ctx-host.example
CTX_KEY=your-api-key-here
EOF
```

<details>
<summary>Windows (PowerShell)</summary>

```powershell
New-Item -ItemType Directory -Force "$env:APPDATA\ctx"
@"
CTX_BASE_URL=https://your-ctx-host.example
CTX_KEY=your-api-key-here
"@ | Set-Content "$env:APPDATA\ctx\config"
```
</details>

### 2. Verify

```bash
ctx health    # DB + Ollama connectivity
ctx stats     # Block count, categories, storage
```

### 3. Claude Code integration (optional)

**Statusline** — live block count, health, and rate limits:
```json
{ "statusLine": { "type": "command", "command": "ctx statusline" } }
```

**Slash commands** — add to `~/.claude/settings.json`:
```json
{
  "customSlashCommands": [
    { "name": "ctx",        "command": "ctx query \"$PROMPT\"" },
    { "name": "ctx-save",   "command": "ctx save $PROMPT" },
    { "name": "ctx-browse", "command": "ctx search $PROMPT" },
    { "name": "ctx-stats",  "command": "ctx stats" }
  ]
}
```

**Agent hooks** — automatic project briefing for subagents:
```json
{
  "hooks": {
    "SubagentStart": [{ "hooks": [{ "type": "command", "command": "ctx brief --hook" }] }],
    "SubagentStop":  [{ "hooks": [{ "type": "command", "command": "ctx persist --hook" }] }]
  }
}
```

## CLI

| Command | Description |
|---------|-------------|
| `ctx query question` | Hybrid search + LLM synthesis (formatted, `--json` for raw) |
| `ctx save <cat> <title> - <content>` | Upsert knowledge block |
| `ctx save --tag tag1,tag2 <cat> <title>` | Upsert with tags |
| `ctx search [category] [query:text]` | Compact search (no LLM) |
| `ctx get <id>` | Fetch full block |
| `ctx delete <id>` | Soft-delete (archive) |
| `ctx categories` | List all categories |
| `ctx stats` | Database statistics + Dream backlog (`dream_queue`: pickable/cooldown/incoming-forecast) |
| `ctx health` | Healthcheck |
| `ctx guard [list\|stats\|resolve]` | Write Guard management |
| `ctx dream [stats\|review]` | Dream Mode stats — mode, `queue` (backlog + incoming forecast), `backoff` (per-eval-count maturity distribution: how far each block has cooled off + effective cooldown); human-readable on a TTY, JSON when piped + link review |
| `ctx dream enable\|disable\|throttle` | Runtime dream mode control (on/off/throttled) |
| `ctx brief` | Project briefing from store |
| `ctx persist` | Persist `[PERSIST:cat:title]` markers |
| `ctx ingest <path>` | Ingest Obsidian vault |
| `ctx digest` | Rebuild topic map |
| `ctx statusline` | Claude Code status bar |
| `ctx mcp [add\|list\|delete]` | Manage MCP OAuth client registrations |
| `ctx keys create <label> --home <scope>` | Provision API key (v2.0.0: `--home` required, no default scope) |
| `ctx keys [list\|delete]` | List / revoke provisioned API keys |
| `ctx version` | Print version |

## Architecture

```
Query ──► Parse Temporal ──► Embed ──► 4-Way RRF ──► Gravity Boost ──► filterSuperseded ──► LLM Synthesis
          │                            ├─ Semantic (0.45)    │
          │                            ├─ EN-FTS   (0.25)    ├─ Linear (Power-Law, content_times)
          │                            ├─ DE-FTS   (0.20)    └─ Cyclic (Gaussian, EAV dimensions)
          │                            └─ Trigram  (0.10)       ├─ weekday σ=0.07  ┌─────────────────────────────┐
          │                                                     ├─ month   σ=0.10  │  Dream Mode (continuous)     │
          └─► DimensionWeights                                  ├─ quarter σ=0.12  │  N workers (PARALLELISM=N)   │
              {weekday:1.0}  "immer dienstags"                  ├─ week    σ=0.08  │  atomic claim (SKIP LOCKED)  │
              {month:0.4, seasonal:0.6}  "Weihnachten"          ├─ monthday σ=0.10 │  Pick → Keywords → RRF       │
              {monthday:1.0}  "Monatsanfang"                    ├─ seasonal σ=0.08 │  → LLM Eval → Links          │
              {daily:1.0}    "morgens"                          └─ daily   σ=0.08  │  → ApplySupersedes           │
                                                                                   │  → PromoteToCanonical        │
                                                                                   └─────────────────────────────┘

Store ──► Extract Times ──► Hash NOOP ──────────────► Guard (async, 60s)
          (content + created_at)          │           ├─ ≥0.98: auto-archive
          │                               │           ├─ 0.92-0.98: flag needs_review
          │                               │           └─ <0.92: clean
          │                               └─► Embed (async, scheduler backfill, tx-wrapped)
          └─► Dimensions = Union(content anchors ∪ meta anchor)
              • Content: dates mentioned in text (semantic)
              • Meta: created_at timestamp (every block, always)
              • ON CONFLICT dedups overlapping timestamps
```

**Stack:** Go 1.26, PostgreSQL 18 + pgvector 0.8.2, 48 SQL migrations. Dual-protocol inference (Ollama native or OpenAI-compatible) via any provider — per-pipeline configurable via `CTX_*_PROTOCOL`, `CTX_EMBED_*`, `CTX_CHAT_*`, `CTX_DREAM_*` env vars.

### Key environment variables

| Var | Default | Purpose |
|-----|---------|---------|
| `CTX_BASE_URL` / `CTX_KEY` | – | CLI client config (`~/.config/ctx/config`) |
| `CONTEXT_DB` / `CONTEXT_DB_USER` / `CONTEXT_DB_PASSWORD` | – | Database (separate from inference) |
| `CTX_EMBED_HOST` / `_PROTOCOL` / `_MODEL` / `_DIMS` | `ollama` / – / `1024` | Embedding pipeline (e.g. qwen3-embedding:8b) |
| `CTX_CHAT_HOST` / `_PROTOCOL` / `_MODEL` / `_THINK` | `ollama` / – / `false` | Generator pipeline (RRF synthesis) |
| `CTX_DREAM_ENABLED` | `false` | Toggle continuous Dream loop |
| `CTX_DREAM_PARALLELISM` | `1` | Concurrent Dream workers — race-safe via atomic claim |
| `CTX_DREAM_HOST` / `_PROTOCOL` / `_MODEL` / `_NUM_CTX` | inherits chat | Separate Dream model (e.g. larger, slower) |
| `CTX_DREAM_EMBED_*` | inherits embed | Separate embedding endpoint for Dream (e.g. CPU sidecar) |
| `CTX_DREAM_IDLE_WAIT` | `20` (s) | Backoff when no pending blocks |
| `CTX_DREAM_BACKOFF_MODE` / `_FACTOR` / `_MIN` / `_GRACE` / `_CAP` / `_INERT_OFFSET` | `exp` / `1.6` / `12h` / `0` / `45d` / `7` | Re-dream back-off by eval count (`exp`/`log`/`linear`/`off`). Cooldown grows from `MIN` (n=0) to `CAP`: fresh blocks re-dream sub-day to catch new links, mature blocks back off to the cap. `_MIN`/`_CAP` take a duration with a unit suffix — `h` hours, `d` days, `w` weeks, `m` months (30d), `y` years (365d), e.g. `12h`, `45d`, `1w` (bare number = hours). `_INERT_OFFSET` starts a no-links cycle further up the curve |
| `CTX_PROMPT_VERSION` | `v5.2` | Generator-prompt version (`v5.2` default, `v6` opt-in graded confidence) |
| `CTX_TIMEZONE` | `Europe/Berlin` | Cyclic-temporal phase calculation |
| `CTX_CONFIDENT_THRESHOLD` | `0.008` | Generator-side refusal threshold (RRF score below → "I don't know") |
| `CTX_READ_SCOPES` | scope-derived | API key's effective read-scope set (v2.0.0+ scheduler config) |

**Key features:**
- **GottZ 4-Way RRF** — reciprocal rank fusion across semantic, bilingual fulltext, and trigram channels; block_role-aware (4-class enum: system-meta hard-excluded incl. digest-generated topic-maps via Welle-44 hook, audit-trail/reference/knowledge full-pass — uniform damping shown ineffective in Welle 40, query-aware damping pending Folge-Welle 41+)
- **GottZ Scope Model** — multi-tenant isolation (private/work/shared) via API key scoping
- **GottZ Guard** — async deduplication via PG LISTEN/NOTIFY + HNSW similarity
- **GottZ Cyclic Phase Model** — 7 cyclic temporal dimensions (weekday/month/quarter/week/monthday/seasonal/daily) with normalized phase [0,1) and per-dimension Gaussian decay. Queries route to dimensions via parser (18-matcher deterministic engine). Timezone-aware via `CTX_TIMEZONE`.
- **Forward Telescoping** — older blocks get a wider linear gravity well (effective power scaled by `1 / (1 + 0.3·ln(1+age/30))`) so a 6-month-old block isn't drowned out by a 1-week-old block when the user asks about a date in that window. Future dates keep their 1.2× sharper cutoff. Matches Rubin & Baddeley 1989's age-dependent recall imprecision.
- **GottZ Temporal Dimension Table** — EAV storage with partial B-Tree indexes, O(log n) dimension lookups at 1M+ scale. Every block carries multiple anchors: content-mentioned times (semantic) + `created_at` (meta) as independent signals.
- **Dream Mode** — continuous autonomous cross-referencing with dual-model support (v5 prompt for qwen3.6:27b non-thinking sampler, dream pipeline version 5 with `recurrent` relationship class detected via context_temporal+title-similarity Phase 1 + LLM Phase 2), adaptive cooldown, supersedes detection, temporal validation, hard-cap of 5 links per cycle with type-diversity tie-break, replace-semantics with snapshot revert, and runtime mode control (on/throttled/off via API). Throttled mode pauses between GPU-intensive steps for thermal management. **Parallel workers** (`CTX_DREAM_PARALLELISM`, default 1) using atomic `FOR UPDATE SKIP LOCKED` block-claim — race-condition-safe under contention. **Robust LLM-output parsing**: tolerates array-form, single-object, fenced-array, and compact-multi-key-object link formats from heterogeneous LLM outputs. Config: `CTX_DREAM_IDLE_WAIT` (seconds, default 20)
- **Supersedes Filtering** — temporal-gated removal of outdated blocks from query results
- **Embed Cache** — content-hash-keyed embedding cache (`context_embed_cache`) to avoid re-embedding identical text across pipelines
- **LLM Log** — per-call request/response capture (`context_llm_log`) with input/output token counts (Ollama + OpenAI), dream-pipeline version tagging, and parse-format drift tagging (`metadata.parse_format`: array | object | fenced-array | fenced-object) for pipeline debugging + offline benchmark replay
- **MCP Remote** — Streamable HTTP transport with OAuth 2.1 PKCE for claude.ai/Claude Code integration. Tools: query, store, search, get, recent. Client registration via `ctx mcp add`. Tool handlers return `Content[].text` (no structured output) — tested in `test.sh` T17/T18

## API

All endpoints under `/api/*`. Auth via `X-Context-Key` header or `Authorization: Bearer` token.

| Endpoint | Description |
|----------|-------------|
| `POST /api/query` | 4-Way RRF + LLM synthesis (auto-backfills pending embeddings; optional `categories_exclude` / `block_roles_exclude` arrays filter slot-stealers) |
| `POST /api/store` | Upsert (embedding async via scheduler) |
| `POST /api/search` | Lightweight search (no LLM) |
| `POST /api/manage` | CRUD, Guard API, stats, API-key management (`api-key-create` requires `home_scope`) |
| `POST /api/digest` | Topic map generation |
| `POST /api/ingest` | Obsidian vault ingestion |
| `POST /api/blob/*` | Binary storage (store/fetch/search/manage) |
| `GET /health` | DB + Ollama connectivity |
| `POST\|GET\|DELETE /mcp` | MCP Streamable HTTP (remote tool server) |
| `GET /authorize` | OAuth 2.1 authorization (PKCE) |
| `POST /token` | OAuth 2.1 token exchange |

## Building

```bash
go build -o ctx ./cmd/ctx/           # CLI
go build -o ctxd ./cmd/ctxd/         # Daemon
go test ./... -short                  # Unit tests
```

## License

[MPL-2.0](LICENSE) — By [GottZ](https://github.com/GottZ)
