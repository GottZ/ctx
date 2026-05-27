# ctx — The memory your LLM pretends to have.

> Knowledge store with weighted 4-way RRF retrieval, multi-tenant scope isolation, multi-dimensional cyclic temporal gravity, and autonomous cross-referencing. Built for AI workflows that need to remember.

[![Release](https://img.shields.io/github/v/release/GottZ/ctx)](https://github.com/GottZ/ctx/releases)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-MPL--2.0-blue)](LICENSE)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-18-336791)](https://www.postgresql.org)

## What it does

ctx gives your LLM a persistent, searchable memory. Store knowledge blocks, query them with hybrid retrieval (semantic + bilingual fulltext + trigram), then rerank with multi-dimensional cyclic gravity — each temporal cycle (weekday, month, quarter, week, monthday, seasonal, daily) scored as its own Gaussian field. Queries like "immer dienstags" or "Weihnachten" activate specific dimensions; "Meeting am Dienstag, Ergebnis am Mittwoch" still pulls the Wednesday block (just weaker).

**Multiple anchors per block:** every block carries dimensions from both its content (dates mentioned in text) AND its `created_at` timestamp. A block about "Meeting am Dienstag" written on a Friday gets `weekday=2` (content anchor) AND `weekday=5` (meta anchor). Both signals contribute independently — "immer dienstags" queries find the content anchor; "Freitags-Arbeit" finds the meta anchor. Same principle for monthday, seasonal, daily, etc.

**Dream Mode** runs as a continuous background loop — autonomously discovering relationships between blocks, marking outdated information, and promoting high-quality content. Supports a separate model for evaluation (e.g. a larger model for better causal/supersedes reasoning). Your knowledge base grows, self-organizes, and stays current.

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
    "SubagentStart": [{ "type": "command", "command": "ctx brief --hook" }],
    "SubagentStop":  [{ "type": "command", "command": "ctx persist --hook" }]
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
| `ctx stats` | Database statistics |
| `ctx health` | Healthcheck |
| `ctx guard [list\|stats\|resolve]` | Write Guard management |
| `ctx dream [stats\|review]` | Dream Mode stats (incl. mode) + link review |
| `ctx dream enable\|disable\|throttle` | Runtime dream mode control (on/off/throttled) |
| `ctx brief` | Project briefing from store |
| `ctx persist` | Persist `[PERSIST:cat:title]` markers |
| `ctx ingest <path>` | Ingest Obsidian vault |
| `ctx digest` | Rebuild topic map |
| `ctx statusline` | Claude Code status bar |
| `ctx mcp [add\|list\|delete]` | Manage MCP OAuth client registrations |
| `ctx version` | Print version |

## Architecture

```
Query ──► Parse Temporal ──► Embed ──► 4-Way RRF ──► Gravity Boost ──► filterSuperseded ──► LLM Synthesis
          │                            ├─ Semantic (0.45)    │
          │                            ├─ EN-FTS   (0.25)    ├─ Linear (Power-Law, content_times)
          │                            ├─ DE-FTS   (0.20)    └─ Cyclic (Gaussian, EAV dimensions)
          │                            └─ Trigram  (0.10)       ├─ weekday σ=0.07  ┌─────────────────────────┐
          │                                                     ├─ month   σ=0.10  │  Dream Mode (continuous) │
          └─► DimensionWeights                                  ├─ quarter σ=0.12  │  Pick → Keywords → RRF  │
              {weekday:1.0}  "immer dienstags"                  ├─ week    σ=0.08  │  → LLM Eval → Links     │
              {month:0.4, seasonal:0.6}  "Weihnachten"          ├─ monthday σ=0.10 │  → ApplySupersedes      │
              {monthday:1.0}  "Monatsanfang"                    ├─ seasonal σ=0.08 │  → PromoteToCanonical   │
              {daily:1.0}    "morgens"                          └─ daily   σ=0.08  └─────────────────────────┘

Store ──► Extract Times ──► Hash NOOP ──────────────► Guard (async, 60s)
          (content + created_at)          │           ├─ ≥0.98: auto-archive
          │                               │           ├─ 0.92-0.98: flag needs_review
          │                               │           └─ <0.92: clean
          │                               └─► Embed (async, scheduler backfill)
          └─► Dimensions = Union(content anchors ∪ meta anchor)
              • Content: dates mentioned in text (semantic)
              • Meta: created_at timestamp (every block, always)
              • ON CONFLICT dedups overlapping timestamps
```

**Stack:** Go 1.26, PostgreSQL 18 + pgvector 0.8.2, 43 SQL migrations. Dual-protocol inference (Ollama native or OpenAI-compatible) via any provider — per-pipeline configurable via `CTX_*_PROTOCOL`, `CTX_EMBED_*`, `CTX_CHAT_*`, `CTX_DREAM_*` env vars

**Key features:**
- **GottZ 4-Way RRF** — reciprocal rank fusion across semantic, bilingual fulltext, and trigram channels; block_role-aware (4-class enum: system-meta hard-excluded, audit-trail/reference/knowledge full-pass — uniform damping shown ineffective in Welle 40, query-aware damping pending Folge-Welle 41+)
- **GottZ Scope Model** — multi-tenant isolation (private/work/shared) via API key scoping
- **GottZ Guard** — async deduplication via PG LISTEN/NOTIFY + HNSW similarity
- **GottZ Cyclic Phase Model** — 7 cyclic temporal dimensions (weekday/month/quarter/week/monthday/seasonal/daily) with normalized phase [0,1) and per-dimension Gaussian decay. Queries route to dimensions via parser (18-matcher deterministic engine). Timezone-aware via `CTX_TIMEZONE`.
- **Forward Telescoping** — older blocks get a wider linear gravity well (effective power scaled by `1 / (1 + 0.3·ln(1+age/30))`) so a 6-month-old block isn't drowned out by a 1-week-old block when the user asks about a date in that window. Future dates keep their 1.2× sharper cutoff. Matches Rubin & Baddeley 1989's age-dependent recall imprecision.
- **GottZ Temporal Dimension Table** — EAV storage with partial B-Tree indexes, O(log n) dimension lookups at 1M+ scale. Every block carries multiple anchors: content-mentioned times (semantic) + `created_at` (meta) as independent signals.
- **Dream Mode** — continuous autonomous cross-referencing with dual-model support (v5 prompt for qwen3.6:27b non-thinking sampler, dream pipeline version 5 with `recurrent` relationship class detected via context_temporal+title-similarity Phase 1 + LLM Phase 2), adaptive cooldown, supersedes detection, temporal validation, hard-cap of 5 links per cycle with type-diversity tie-break, replace-semantics with snapshot revert, and runtime mode control (on/throttled/off via API). Throttled mode pauses between GPU-intensive steps for thermal management. Config: `CTX_DREAM_IDLE_WAIT` (seconds, default 20)
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
| `POST /api/manage` | CRUD, Guard API, stats |
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
