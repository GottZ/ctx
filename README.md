# ctx — The memory your LLM pretends to have.

> Knowledge store with weighted 4-way RRF retrieval, multi-tenant scope isolation, multi-dimensional cyclic temporal gravity, and autonomous cross-referencing. Built for AI workflows that need to remember.

[![Release](https://img.shields.io/github/v/release/GottZ/ctx)](https://github.com/GottZ/ctx/releases)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-MPL--2.0-blue)](LICENSE)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-18-336791)](https://www.postgresql.org)

## What it does

ctx gives your LLM a persistent, searchable memory. Store knowledge blocks, query them with hybrid retrieval (semantic + bilingual fulltext + trigram), then rerank with multi-dimensional cyclic gravity — each temporal cycle (weekday, month, quarter, week, monthday, seasonal, daily) scored as its own Gaussian field. Queries like "immer dienstags" or "Weihnachten" activate specific dimensions.

Every block carries anchors from both its content (dates mentioned in text) AND its `created_at` timestamp, so a note about "Meeting am Dienstag" is findable by the day it discusses and the day it was written. **Dream Mode** runs as a continuous background loop — autonomously discovering relationships between blocks, marking outdated information, and promoting high-quality content. Your knowledge base grows, self-organizes, and stays current.

It speaks **MCP, a CLI, and a plain HTTP API**, ships an embedded web UI, and isolates multiple tenants down to the individual block.

## Features

| Capability | What it is |
|---|---|
| **4-way RRF retrieval** | Reciprocal-rank fusion of semantic + EN/DE fulltext + trigram, type-policy-parameterised — [architecture](docs/architecture.md#4-way-rrf-retrieval) |
| **Cyclic temporal gravity** | 7 cyclic dimensions with per-dimension Gaussian decay + forward telescoping — [architecture](docs/architecture.md#temporal-gravity--cyclic-phases) |
| **Dream Mode** | Continuous autonomous cross-referencing, supersedes detection, parallel race-safe workers — [architecture](docs/architecture.md#dream-mode) |
| **Block-type registry** | Declarative per-type behaviour (retrieval, guard, dream, digest), hot-reloadable — [architecture](docs/architecture.md#block-type-registry-migration-072) |
| **Multi-tenancy** | Three-level tenant/scope/block isolation, grants, quotas, self-service onboarding — [multi-tenancy](docs/multi-tenancy.md) |
| **Sealed secrets** | AES-256-GCM provider credentials + trust×sensitivity egress gating — [security](docs/security.md) |
| **Web UI** | Embedded Svelte 5 admin SPA: settings, graph, corpus, status, chat — [development](docs/development.md#web-ui-svelte-5--typescript--vite-bun) |
| **MCP / CLI / HTTP** | Three access paths to the same store, OAuth 2.1 for remote MCP — [api](docs/api.md) |

## Quick install

```bash
# Binary (Linux/macOS/Windows)
curl -fsSL https://github.com/GottZ/ctx/releases/latest/download/ctx-$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') -o /usr/local/bin/ctx && chmod +x /usr/local/bin/ctx

# Or with Go
go install github.com/GottZ/ctx/cmd/ctx@latest
```

Point the CLI at your ctx host:

```bash
mkdir -p ~/.config/ctx
cat > ~/.config/ctx/config << 'EOF'
CTX_BASE_URL=https://your-ctx-host.example
CTX_KEY=your-api-key-here
EOF
ctx health    # DB + Ollama connectivity
```

Running the server (Go daemon + PostgreSQL 18 + pgvector) is `docker compose up -d ctx`. Full setup, env vars and Claude Code integration are in [operations](docs/operations.md); building from source in [development](docs/development.md).

## How LLMs use ctx

ctx is designed to be the persistent memory layer for LLM agents. Five primitives, composable:

| Use case | Tool | When |
|----------|------|------|
| **Retrieve** prior knowledge before answering | `ctx query "question"` | Whenever the answer might depend on past sessions, project state, or stored decisions |
| **Persist** a new finding | `ctx save <category> <title> - <content>` | After non-obvious discoveries, architecture decisions, resolved bugs, config changes |
| **Update** an existing block | `ctx save` with same `<category> <title>` | category+title is the upsert key — re-saving replaces |
| **Browse** without LLM cost | `ctx search [category] [query:text]` | Listing, sanity-checking, lightweight lookups |
| **Inspect** a specific block | `ctx get <block-id>` | Following an id from query sources or another block |

**Categories** (semantic, not enforced): `infrastructure`, `decisions`, `projects`, `reference`, `learnings`, `agent-briefing`, `index`. Pick by intent: one fact per block, precise title, tags for cross-cutting. ~1–1.5k chars max — split, don't grow.

**Access paths** (in order of preference for LLM agents):

1. **MCP** — `claude.ai ctx` server (Streamable HTTP transport). Tools: `query`, `store`, `search`, `get`, `recent`. JSON-schemas, no shell-quoting. Use this in Claude Code / claude.ai sessions.
2. **CLI** — `/usr/local/bin/ctx` — shell pipelines, cron, scripts. Config in `~/.config/ctx/config`.
3. **HTTP** — `POST /api/{query,store,search,manage}` direct — fallback when MCP/CLI unavailable.

Using a shared memory *well* takes discipline against drift — see [using ctx effectively](docs/using-ctx-effectively.md).

## Documentation

| Doc | Contents |
|---|---|
| [Architecture](docs/architecture.md) | Retrieval pipeline, cyclic temporal model, Dream Mode, Guard, block-type registry, backend pool |
| [Multi-tenancy](docs/multi-tenancy.md) | Model C (tenant/scope/block), grants, admin tiers, per-tenant settings/secrets/quotas, self-service onboarding |
| [HTTP API](docs/api.md) | Endpoints, manage actions, graph API, settings API, backend pool, web-chat sessions |
| [CLI reference](docs/cli.md) | Every `ctx` subcommand, incl. tenant / quota / keys / block-grant |
| [Security](docs/security.md) | Admin tier, sealed secrets & break-glass, trust×sensitivity gating, sensitivity classification |
| [Operations](docs/operations.md) | Setup, environment variables, boot validation, backups, deploy & migrations |
| [Development](docs/development.md) | Building, the Svelte web UI, tests, visual baseline governance, git hooks |
| [Using ctx effectively](docs/using-ctx-effectively.md) | Memory drift, agent discipline, RLHF calibration |

## Built with AI agents

ctx is built by AI agents working against a published RLHF-warnings calibration — a 22-axis map of LLM failure modes (memory drift, unattributed certainty, median-user caution) with concrete exemplars. It is the methodology reference behind this project's way of working: **[gottz.de/warnings.md](https://gottz.de/warnings.md)**. Background in [using ctx effectively](docs/using-ctx-effectively.md).

## License

[MPL-2.0](LICENSE) — By [GottZ](https://github.com/GottZ)
