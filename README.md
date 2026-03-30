# ctx — The memory your LLM pretends to have.

Knowledge store with weighted 4-way RRF retrieval, multi-tenant scope isolation, and temporal awareness. Built for AI workflows.

## Quick Install

```bash
# One-liner: install latest release binary
curl -fsSL https://github.com/GottZ/ctx/releases/latest/download/ctx-$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') -o /usr/local/bin/ctx && chmod +x /usr/local/bin/ctx
```

Or with Go:
```bash
go install github.com/GottZ/ctx/cmd/ctx@latest
```

## Setup

### 1. Configure ctx endpoint

Linux/macOS:
```bash
mkdir -p ~/.config/ctx
cat > ~/.config/ctx/config << 'EOF'
CTX_BASE_URL=https://your-ctx-host.example
CTX_KEY=your-api-key-here
EOF
```

Windows (PowerShell):
```powershell
New-Item -ItemType Directory -Force "$env:APPDATA\ctx"
@"
CTX_BASE_URL=https://your-ctx-host.example
CTX_KEY=your-api-key-here
"@ | Set-Content "$env:APPDATA\ctx\config"
```

### 2. Verify connection

```bash
ctx health
ctx stats
```

### 3. Claude Code statusline (optional)

Add to `~/.claude/settings.json`:

```json
{
  "statusLine": {
    "type": "command",
    "command": "ctx statusline"
  }
}
```

For full setup details including autocompact configuration:
```bash
ctx statusline --help
```

### 4. Claude Code slash commands (optional)

Add to `~/.claude/settings.json` or project `.claude/settings.json`:

```json
{
  "customSlashCommands": [
    {
      "name": "ctx",
      "description": "Query the Context Store",
      "command": "ctx query \"$PROMPT\""
    },
    {
      "name": "ctx-save",
      "description": "Save to Context Store",
      "command": "ctx save $PROMPT"
    },
    {
      "name": "ctx-browse",
      "description": "Browse Context Store",
      "command": "ctx search $PROMPT"
    },
    {
      "name": "ctx-stats",
      "description": "Context Store statistics",
      "command": "ctx stats"
    }
  ]
}
```

## CLI Commands

| Command | Description |
|---------|-------------|
| `ctx query "question"` | Hybrid search + LLM synthesis |
| `ctx save <category> <title> - <content>` | Upsert knowledge block |
| `ctx search [category] [query:text]` | Compact search (no LLM) |
| `ctx stats` | Database statistics |
| `ctx health` | Healthcheck (DB + Ollama) |
| `ctx guard [list\|stats\|resolve]` | Write Guard management |
| `ctx categories` | List categories |
| `ctx get <id>` | Fetch full block |
| `ctx delete <id>` | Delete block |
| `ctx statusline` | Claude Code status bar |
| `ctx ingest <path>` | Ingest Obsidian vault |

## Architecture

- **Go 1.24** — `ctx` CLI + `ctxd` daemon, chi router, pgx v5 + pgvector-go
- **PostgreSQL 18** + pgvector 0.8.2 + TimescaleDB 2.26.0
- **Ollama** — qwen3-embedding:8b (1024d Matryoshka) + qwen3.5:9b (synthesis)
- **4-Way RRF** — Semantic (0.45) + EN-FTS (0.25) + DE-FTS (0.20) + Trigram (0.10)
- **Multi-Tenant** — scope isolation (private/work/shared) via API key
- **Write Guard** — async dedup via PG LISTEN/NOTIFY + HNSW similarity
- **Temporal** — EAV dimension table, deterministic parser (59/60 cases), LLM fallback
- **Gravity Reranker** — physics-inspired temporal scoring, post-RRF on Top-200
- **Dream Mode** (planned) — async quality assessment, knowledge graph ops, draft layer

## API

All endpoints under `/api/*`. Auth via `X-Context-Key` header.

| Endpoint | Description |
|----------|-------------|
| `POST /api/query` | RRF + LLM synthesis |
| `POST /api/store` | Upsert + auto-embedding |
| `POST /api/search` | Lightweight FTS (no LLM) |
| `POST /api/manage` | CRUD + Guard API |
| `POST /api/digest` | Topic map generation |
| `POST /api/ingest` | Obsidian vault ingestion |
| `POST /api/blob/*` | Binary storage (store/fetch/search/manage) |
| `GET /health` | DB + Ollama connectivity |

## Building

```bash
# Local build
go build -o ctx ./cmd/ctx/

# Cross-compile all platforms
./build.sh v0.1.0

# Install locally
./install.sh
```

## By GottZ
