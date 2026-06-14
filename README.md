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

API-key provisioning (v2.0.0+): `ctx keys create <label> --home <scope>` — `--home` is required, no implicit default. Scope names starting with `_` are rejected (the underscore namespace is system-reserved; `_global` anchors the server-global settings identity in `context_settings`).

### Admin tier (BREAKING, migration 052)

Keys carry an `is_admin` flag (default `false`, no key is auto-promoted). The following `/api/manage` actions now require an admin key — **BREAKING for previously-working non-admin keys**: `api-key-create`, `api-key-list`, `api-key-delete`, `mcp-client-create`, `mcp-client-list`, `mcp-client-delete`, and `dream-mode` when mutating (reading the current mode stays open). Rationale: before this gate, ANY valid key of any home_scope could mint keys for arbitrary scopes — read access to foreign tenants — and the upcoming settings/secrets API must not inherit that model.

**Admin bootstrap** (one-time, host access required). Promote by `id`, never by label — `label` has no UNIQUE constraint and an UPDATE by label would escalate every same-named key, including inactive ones:

```bash
# 1. Inspect candidates:
docker exec -e PGPASSWORD="$CONTEXT_DB_PASSWORD" n8n-db-1 \
  psql -U "$CONTEXT_DB_USER" -d "$CONTEXT_DB" \
  -c "SELECT id, label, active, home_scope, is_admin FROM context_api_keys;"
# 2. Promote EXACTLY one key by id:
docker exec -e PGPASSWORD="$CONTEXT_DB_PASSWORD" n8n-db-1 \
  psql -U "$CONTEXT_DB_USER" -d "$CONTEXT_DB" \
  -c "UPDATE context_api_keys SET is_admin = true WHERE id = '<uuid>';"
```

**Admin-key hygiene:** the OAuth/MCP flow hands the API key ITSELF out as the bearer token — a key used as an MCP remote token circulates through claude.ai/Cloudflare and is stored in external connector storage. Create a **dedicated admin key that is never used as an MCP/OAuth token**; the claude.ai MCP key stays non-admin. Test/eval script keys stay non-admin too (least privilege).

### Sealed secrets & break-glass

Provider credentials live AES-256-GCM-sealed in `context_secrets` (encrypted in Go — never via pgcrypto, the master key must not cross the SQL wire). The AAD binds each ciphertext to its `name`+`scope` row identity, so a ciphertext copied onto another row fails authentication. Writes go through the admin-gated, **write-only** `/api/secrets` (set/rotate/delete — values never appear in any response, list shows metadata + `referenced_by` only, no fingerprints); settings reference a secret by *name* (`secret_ref`), resolved to plaintext exclusively inside the in-memory snapshot. A rotation or revocation reloads the snapshot immediately — no settings write needed, the incident-response path is never silently inert. Deleting a secret that settings still reference is a `409` listing the keys.

**Master key setup** (one-time):

```bash
# generate and append to .env:
echo "CTX_SECRETS_KEY=$(openssl rand -hex 32)" >> .env
```

**Mandatory: copy `CTX_SECRETS_KEY` into your password manager when you set it.** `backup.sh` archives only the pg_dumps — the ciphertexts are in every dump, the master key is in none (deliberate: the key stays spatially separated from the ciphertexts it opens, so disaster recovery needs both places). **Key loss = total loss of all sealed secrets, by design.** No recovery mechanism; re-enter the provider keys instead.

**Master-key rotation:** generate a new key, move the old value to `CTX_SECRETS_KEY_PREV`, put the new one in `CTX_SECRETS_KEY`, restart ctx. The boot sweep re-seals every secret it can open with the previous key (`key_version` bump, log line per name, one transaction per row); it logs a completion line — `re-encrypt sweep complete` means remove `CTX_SECRETS_KEY_PREV` from `.env`, a `finished with failures` WARN means keep it set and investigate. Secrets that open with neither key are left untouched (WARN per name, no boot abort, no data loss). The *value* rotation of a single provider key is `PUT /api/secrets/{name}` (or `ctx secrets rotate`) — no restart, propagates immediately.

**Break-glass extraction** (host access; works even when the ctx container crash-loops — the decrypt mode reads ONLY env + stdin, no DB):

```bash
./break-glass.sh secret <name> [scope]     # prints the plaintext
./break-glass.sh reset-settings [key]      # factory-reset settings overrides (audited via DB trigger)
```

`openssl enc` cannot do AES-GCM, so extraction pipes the row through the ctxd binary itself: `psql -At … | docker run --rm -i -e CTX_SECRETS_KEY -e CTX_SECRETS_KEY_PREV n8n-ctx -secret-decrypt`. PostgreSQL's `encode(bytea,'base64')` is MIME (RFC 2045) and wraps every 76 chars — the script strips the wraps SQL-side, and the decrypt mode additionally reads stdin to EOF and strips CR/LF, so every realistic provider-key length survives the pipe (negatively probed: a line-based reader fails on exactly those records).

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
| `ctx settings [list\|get\|set\|unset]` | Runtime settings overrides (alias `cfg`; **admin key**, reads included). TTY: table, pipe: JSON; `set` takes the value as argument or stdin; API failures (422/409/403) exit 1 with the server's reason |
| `ctx secrets [list\|set\|rotate\|rm]` | Sealed provider credentials (alias `sec`; **admin key**). Write-only: values go in via stdin ONLY (`echo "$KEY" \| ctx secrets set <name>` — an argv value is rejected, it would leak via /proc and shell history); list shows metadata + `referenced_by`, never values; `rm` exits 1 with a 409 while settings reference the secret |
| `ctx backends [list\|create\|update\|delete\|test]` | LLM backend pool (**admin key**). TTY: table with live status, pipe: JSON; `create`/`update` take a JSON spec as argument or stdin; API failures exit 1 |
| `ctx gaming [on\|off]` | Gaming toggle: drop the GPU-host backends from every chain so the GPU is free to game (CPU/external stay in as failover). No arg = status (any valid key); `on`/`off` need an **admin key**. Persists in settings — survives a restart (unlike dream-mode) and hits the next chain without one; a typo in the disabled list surfaces as `unknown_backends` |
| `ctx blocks audit [status\|sample\|start]` | Sensitivity LLM audit (**admin key**): classify `sensitivity_source='default'` blocks over the hard-local `classify` chain. `sample --n 30` = dry-run verdicts without writes (the sample gate), `start [--limit N]` = live run, bare/`status` = progress (`pending`, by-source counts, run state) |
| `ctx blocks classify [status\|dry-run\|start]` | Credentials PATTERN re-audit (**admin key**): the deterministic detector raises every home-scope hit to `credentials` (`sensitivity_source='pattern'`), upgrade-only. `dry-run [--limit N]` = full scan WITHOUT writes (the FP gate — run it first), `start [--limit N]` = live run, bare/`status` = progress (by-source counts, run state) |
| `ctx mcp [add\|list\|delete]` | Manage MCP OAuth client registrations |
| `ctx keys create <label> --home <scope>` | Provision API key (v2.0.0: `--home` required, no default scope; admin key required since 052) |
| `ctx keys [list\|delete]` | List / revoke provisioned API keys (admin key required since 052) |
| `ctx version` | Print version |

## Architecture

```
Query ──► Parse Temporal ──► Embed ──► 4-Way RRF ──► Gravity Boost ──► Graph Expand ──► filterSuperseded ──► LLM Synthesis
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

**Stack:** Go 1.26, PostgreSQL 18 + pgvector 0.8.2, 55 SQL migrations. Dual-protocol inference (Ollama native or OpenAI-compatible) via any provider — per-pipeline configurable via `CTX_*_PROTOCOL`, `CTX_EMBED_*`, `CTX_CHAT_*`, `CTX_DREAM_*` env vars.

### Key environment variables

Every var below can also carry a runtime override in `context_settings` (precedence: **DB override > env > default**; sealed `context_secrets` + trigger-fed audit trail in `context_settings_audit`, migration 051). The boot loads the overrides right after the migrations and builds the effective snapshot from them; sensitive keys take a `secret_ref` (the *name* of a sealed secret), resolved in-memory only — logs show keys and sources, never resolved values. The override layer is never fatal: unknown keys, restart-only/coupled keys (incl. the `CONTEXT_DB_*` group), corrupt values and a missing or wrong master key each degrade to a WARN while the env/default value stays active; `CTX_SETTINGS_DISABLE=1` switches the whole layer off (env-only boot, one log line). Live editing goes through the admin-gated [Settings API](#settings-api); direct SQL edits (and break-glass resets) take effect immediately too — the 051 triggers NOTIFY a listener that rebuilds the snapshot.

The `mut` column is the registry's mutability class per key: **hot** keys take effect without a restart once changed at runtime (snapshot consumers pick them up on the next request/cycle; the settings API will accept live writes for exactly these), **restart** keys are process wiring (DB connection, listener, worker-goroutine count — runtime writes are rejected with 409), **coupled** keys carry a side-effect obligation: embed **host/protocol** changes are runtime-writable and automatically flush `context_embed_cache` on apply (stale vectors from the old backend must never serve against the new one), while an embed **model** change changes the vector space, needs a re-embed migration and stays env-only (409).

| Var | Default | Mut | Purpose |
|-----|---------|-----|---------|
| `CTX_BASE_URL` / `CTX_KEY` | – | – | CLI client config (`~/.config/ctx/config`), not a server key |
| `CONTEXT_DB` / `CONTEXT_DB_USER` / `CONTEXT_DB_PASSWORD` | – | restart | Database (separate from inference) |
| `CTX_SECRETS_KEY` / `CTX_SECRETS_KEY_PREV` | – | restart | Master key for AES-256-GCM-sealed `context_secrets` (64 hex chars, `openssl rand -hex 32`); `_PREV` only while a rotation sweep is pending. Env-only by design — **copy into your password manager**, key loss = total loss (see Sealed secrets & break-glass) |
| `CTX_EMBED_HOST` / `_PROTOCOL` / `_MODEL` | `ollama` / – | coupled | **Bootstrap-only since the backend pool (053):** seeds the `llama-embed` pool row on the first boot with an empty `context_backends`, then the pool owns embedding and these are inert (manage via `ctx backends`). `_API_KEY` / `_NUM_CTX` seed the same row |
| `CTX_CHAT_HOST` / `_PROTOCOL` / `_MODEL` / `_THINK` / `_NUM_CTX` | `ollama` / – / `false` / `0` | hot | **Bootstrap-only since the backend pool (053):** seeds the `herbert-chat` pool row (synthesis / translate / chat / digest roles), then inert — the pool chain owns the chat roles. `_NUM_CTX` (`0`=model default) seeds the row's num_ctx; live it comes from the serving pool row, so chat-role calls resolving onto one row share a single runner |
| `CTX_CHAT_FALLBACK_HOST` / `_PROTOCOL` / `_API_KEY` / `_TIMEOUT` | empty (off) / `openai` / – / `420` | hot | **Bootstrap-only since the backend pool (053):** seeds the low-priority `llama-cpu` pool row on the first boot with an empty `context_backends`; afterwards the pool chain owns synthesis failover and these vars are inert. `_TIMEOUT` in seconds becomes the row's per-role timeout, sized for CPU inference (27B ≈ 4.5–5.5 min/answer; the body heartbeat keeps proxies alive). See the `llama-cpu` compose service |
| `CTX_DREAM_ENABLED` | `false` | restart | Toggle continuous Dream loop |
| `CTX_DREAM_PARALLELISM` | `1` | restart | Concurrent Dream workers — race-safe via atomic claim |
| `CTX_DREAM_HOST` / `_PROTOCOL` / `_MODEL` / `_NUM_CTX` | inherits chat | hot | **Bootstrap-only since the backend pool (053):** seeds the dream role — its own `herbert-dream` row when the host diverges from chat, else the `dream` role on `herbert-chat` — then inert. Separate Dream model (e.g. larger, slower) |
| `CTX_DREAM_EMBED_*` | inherits embed | coupled | **Bootstrap-only since the backend pool (053):** seeds the `dream-embed` role — merged onto `llama-embed` when identical to `CTX_EMBED_*`, else its own row — then inert. For a separate Dream embedding endpoint (e.g. CPU sidecar), create a pool row with role `dream-embed` rather than these vars |
| `CTX_DREAM_IDLE_WAIT` | `20` (s) | hot | Backoff when no pending blocks |
| `CTX_DREAM_BACKOFF_MODE` / `_FACTOR` / `_MIN` / `_GRACE` / `_CAP` / `_INERT_OFFSET` | `exp` / `1.6` / `12h` / `0` / `45d` / `7` | hot | Re-dream back-off by eval count (`exp`/`log`/`linear`/`off`). Cooldown grows from `MIN` (n=0) to `CAP`: fresh blocks re-dream sub-day to catch new links, mature blocks back off to the cap. `_MIN`/`_CAP` take a duration with a unit suffix — `h` hours, `d` days, `w` weeks, `m` months (30d), `y` years (365d), e.g. `12h`, `45d`, `1w` (bare number = hours). `_INERT_OFFSET` starts a no-links cycle further up the curve |
| `CTX_PROMPT_VERSION` | `v5.2` | hot | Generator-prompt version (`v5.2` default, `v6` opt-in graded confidence) |
| `CTX_TIMEZONE` | `Europe/Berlin` | hot | Cyclic-temporal phase calculation |
| `CTX_CONFIDENT_THRESHOLD` | `0.008` | hot | Generator-side refusal threshold (RRF score below → "I don't know") |
| `CTX_READ_SCOPES` | scope-derived | hot | API key's effective read-scope set (v2.0.0+ scheduler config) |
| `CTX_LLMLOG_RETENTION_DAYS` | `90` | hot | After N days the background janitor NULLs the prompt/response **bodies** in `context_llm_log`; the telemetry row (pipeline / model / tokens / cost / block_ids / backend / trust) survives, so the egress audit stays lossless and only the plaintext shadow corpus is dropped. `0` = keep bodies forever (no retention). Body-NULLing, **not** a chunk drop — the audit is never destroyed. Shares the embed-cache janitor tick (6 h) |
| `CTX_LLMLOG_MAX_LIMIT` | `200` | hot | Cap on `GET /api/llmlog?limit=` (G33 status dashboard) |
| `CTX_EVENTS_TICK_INTERVAL` / `CTX_EVENTS_QUEUE_STATS_INTERVAL` | `5` (s) / `30` (s) | hot | Status-collector cadence (G33): the cheap sources (health / pool / dream mode / gaming / llm-24h) refresh at most once per tick; the O(n) dream-queue scan decouples to its own slower interval so it never rides the base tick. Also the SSE diff cadence in G34 (one snapshot+diff per tick, fanned out to every connection) |
| `CTX_EVENTS_PING_INTERVAL` / `CTX_EVENTS_MAX_CONNECTIONS` | `25` (s) / `8` | hot | SSE knobs (G34, `GET /api/events`): the `: ping` keepalive cadence — MUST stay below the fronting proxy's read timeout (nginx 60s) — and the concurrent-stream cap (429 above it; the client degrades to polling). `MAX_CONNECTIONS` is parse-strict — a malformed cap aborts boot instead of silently falling back to the default |
| `CTX_WEBCHAT_ENABLED` / `_MAX_ITERATIONS` / `_MAX_TOKENS` / `_COMPLETION_BUDGET` / `_TOOL_RESULT_MAX_CHARS` / `_HISTORY_BUDGET_CHARS` / `_LLM_TIMEOUT` / `_CONCURRENT_TURNS` / `_SESSION_RETENTION` | `true` / `6` / `2048` / `8192` / `8000` / `60000` / `900` (s) / `1` / `0` (off) | hot | Web-chat harness (F6-C4, `POST /api/chat/stream`). `ENABLED` gates the endpoint + session routes (off ⇒ 404). The budgets cap one turn (iterations, per-call + per-turn tokens, tool-result truncation, prompt-history chars, per-call timeout). `CONCURRENT_TURNS` is the per-`home_scope` semaphore (429 above it; parse-strict like the other ceilings — multi-tenant fairness on the single slot). `SESSION_RETENTION` takes a duration suffix (`h/d/w/m/y`); `0` keeps sessions forever |
| `LISTEN_ADDR` | `:8080` | restart | HTTP listen address; also read raw by the `-health` container healthcheck mode |
| `CTX_GRAPH_EXPAND_ENABLED` / `_*` | `true` | hot | Query-time Dream-graph traversal (Wave 1): 1-hop confidence/type-gated expansion of inferred links, fused post-gravity / pre-rerank. **Default-on since Wave 3** (only arm that moves the recall ceiling, ~0s; magnitude partly circular vs the link-derived eval gold). Fail-open. Knobs: `_DIRECTED` / `_HOP_DEPTH` / `_SEED_COUNT` / `_SEED_SCORE_FLOOR` / `_PER_SEED_CAP` / `_MAX_INJECTED` / `_MIN_CONFIDENCE`(`_RECURRENT`) / `_BOOST_WEIGHT` / `_HUB_DAMPING` / `_WEIGHT_{TOPICAL,FACTUAL,CAUSAL,RECURRENT}` / `_NEW_PLACEMENT_FRAC` |
| `CTX_RERANK_ENABLED` / `_HOST` / `_*` | `true` | hot | Post-RRF rerank (fail-open). **Default-on since Wave 3.5**: the surface-gold counter-probe (judge-annotated real-user queries) showed the cross-encoder is where it earns its keep (nDCG@10 +0.164, MRR +0.169) while blend 0.5 keeps it neutral on latent gold — `graph+ce-bw0.5` is the best arm on both gold sets; the ~80-90s query path stays proxy-safe via the body heartbeat. `_HOST` / `_MODEL` / `_API_KEY` are **Bootstrap-only since the pool (053)** — they seed the `herbert-rerank` row, then inert; `_ENABLED` / `_MAX_DOCS` / `_BLEND_WEIGHT` stay live query knobs. `_HOST` empty → LLM-as-judge on the chat model; default `http://ctx-rerank:8082` → local **bge-reranker-v2-m3** cross-encoder sidecar (Wave 2, cohere-style `/v1/rerank`, all-local/$0). Knobs: `_MODEL` / `_MAX_DOCS` (default 50; CPU ≈1s/doc, latency not gated) / `_BLEND_WEIGHT` (default 0.5; 1.0 = pure cross-encoder, lower mixes RRF back in — Wave-3: pure hurts on latent-relevance gold and is destructive as final arbiter over graph neighbors) / `_API_KEY`. See `docker-compose.yml` for the sidecar service. |

**Compose gap:** an env var only reaches the container if the `docker-compose.yml` `environment:` block declares it. Eighteen parsed keys are deliberately *not* declared there (`CTX_DREAM_IDLE_WAIT`, `CTX_DREAM_PARALLELISM`, the six `CTX_DREAM_BACKOFF_*`, `CTX_PROMPT_VERSION`, `CONTEXT_DB_SSLMODE`, the five `CTX_DREAM_EMBED_*` — the latter Bootstrap-only since the backend pool and dedup'd onto the embed row when identical, see above — and the three G33 dashboard knobs `CTX_LLMLOG_MAX_LIMIT` / `CTX_EVENTS_TICK_INTERVAL` / `CTX_EVENTS_QUEUE_STATS_INTERVAL`, whose defaults ship correct) — setting them in `.env` alone does nothing. The boot dump makes this visible: a var that never arrived shows `"default"` as its source. To use one, add it to the compose `environment:` block (or set a `context_settings` override).

### Boot-time validation & config dump

`ctxd` parses all `CTX_*`/`CONTEXT_*` env vars through a typed registry (`internal/config`) and logs one `config: effective` record at startup: every setting with its origin (`settings` for a DB override, `env`, or `default` — a var you set in the shell but forgot to declare in compose shows up as `default`), secrets masked (`api_key`s render a short sha256 fingerprint so key rotation is provable from logs without leaking the value; the DB password renders presence-only).

Invalid configurations abort the boot **after logging every finding** with field + reason — fix the named fields in `.env` and restart. Beyond the long-standing fatal parses (malformed ints, unknown timezone, missing DB password), these previously-booting-but-broken-at-runtime states are now startup errors: unknown `_PROTOCOL` values (used to silently select the Ollama wire path → 404 on llama.cpp), malformed host URLs / trailing slashes / embedded `user:pass@` credentials (use `_API_KEY` instead), `CTX_SCORE_THRESHOLD` above `CTX_CONFIDENT_THRESHOLD`, out-of-range knobs (`_BLEND_WEIGHT` outside [0,1], negative rate limits), and cross-host credential inheritance in the `CTX_DREAM_EMBED_*` fallback chain. Malformed values on tolerant knobs keep their defaults as before, but now log a WARN instead of failing silently.

**Key features:**
- **GottZ 4-Way RRF** — reciprocal rank fusion across semantic, bilingual fulltext, and trigram channels; block_role-aware (4-class enum: system-meta hard-excluded incl. digest-generated topic-maps via Welle-44 hook, audit-trail/reference/knowledge full-pass — uniform damping shown ineffective in Welle 40, query-aware damping pending Folge-Welle 41+)
- **GottZ Scope Model** — multi-tenant isolation (private/work/shared) via API key scoping
- **GottZ Guard** — async deduplication via PG LISTEN/NOTIFY + HNSW similarity
- **GottZ Cyclic Phase Model** — 7 cyclic temporal dimensions (weekday/month/quarter/week/monthday/seasonal/daily) with normalized phase [0,1) and per-dimension Gaussian decay. Queries route to dimensions via parser (18-matcher deterministic engine). Timezone-aware via `CTX_TIMEZONE`.
- **Forward Telescoping** — older blocks get a wider linear gravity well (effective power scaled by `1 / (1 + 0.3·ln(1+age/30))`) so a 6-month-old block isn't drowned out by a 1-week-old block when the user asks about a date in that window. Future dates keep their 1.2× sharper cutoff. Matches Rubin & Baddeley 1989's age-dependent recall imprecision.
- **GottZ Temporal Dimension Table** — EAV storage with partial B-Tree indexes, O(log n) dimension lookups at 1M+ scale. Every block carries multiple anchors: content-mentioned times (semantic) + `created_at` (meta) as independent signals.
- **Dream Mode** — continuous autonomous cross-referencing with dual-model support (v5 prompt for qwen3.6:27b non-thinking sampler, dream pipeline version 5 with `recurrent` relationship class detected via context_temporal+title-similarity Phase 1 + LLM Phase 2), adaptive cooldown, supersedes detection, temporal validation, hard-cap of 5 links per cycle with type-diversity tie-break, replace-semantics with snapshot revert, and runtime mode control (on/throttled/off via API). Throttled mode pauses between GPU-intensive steps for thermal management. **Parallel workers** (`CTX_DREAM_PARALLELISM`, default 1) using atomic `FOR UPDATE SKIP LOCKED` block-claim — race-condition-safe under contention. **Robust LLM-output parsing**: tolerates array-form, single-object, fenced-array, and compact-multi-key-object link formats from heterogeneous LLM outputs. Config: `CTX_DREAM_IDLE_WAIT` (seconds, default 20)
- **Supersedes Filtering** — temporal-gated removal of outdated blocks from query results
- **Dream-Graph Traversal** (Wave 1, default-on since Wave 3, `CTX_GRAPH_EXPAND_ENABLED`) — query-time 1-hop expansion of the Dream-inferred link graph (topical/factual/causal/recurrent), confidence/type-gated + hub-damped, fused as a scale-invariant post-gravity boost before rerank. Turns the inferred links into positive recall instead of write-only metadata; fully parameterized for A/B sweeps, fail-open
- **Transport Retry** — all inference HTTP calls (chat ollama/openai, embed, rerank) retry exactly once on transient transport failures (connection reset / EOF before any response bytes) via `internal/httpx`. Covers the keep-alive race with llama.cpp's cpp-httplib servers (~5s idle close vs Go connection reuse); HTTP status errors and context deadlines are never retried. Inference POSTs are stateless, so a replay is safe
- **Synthesis on the pool chain** (054) — query-path synthesis walks the role chain from `context_backends` (priority-ordered, cooldown-sorted; the chain is the ONLY way to a backend, so the trust gate sits structurally before prompt transmission). Transport-class failures advance to the next backend (e.g. the `llama-cpu` sidecar at priority 10: same GGUF, CPU speed, its own per-role timeout); HTTP-500 and attempt timeouts stop the chain — the server *ran* the request, slow-but-alive is not down. The response heartbeat starts whenever synthesis is on (`synthesize != false`), so a CPU-leg answer survives buffering proxies even with rerank off. "Es sollte immer ein Weg zu finden sein" — answers degrade to minutes, never to errors. Since 055 the WHOLE query path resolves through the chain (translate, temporal, query-embed, rerank dispatch, inline backfill) with a real requirement: `max(query sensitivity, sensitivity of the FINAL prompt set)` — measured after rank filtering, so a credentials block on rank 180 that never enters the prompt cannot lock the failover. The background paths followed: dream cycles (temporal/keywords/eval/recurrence at `max` over the involved blocks' floor-adjusted sensitivity), keyword embeds and the scheduler's embed backfill (role `dream-embed` when configured, `embed` otherwise, per-block requirement), and BOTH daily-digest callers (03:00 scheduler + manual `POST /api/synthesize/daily`) at constant `internal` — titles and aggregate counts are structure, not content. An empty dream chain (gaming/disabled/trust) skips the cycle BEFORE the block pick: no claim, no cooldown touch, so a gaming session never smears the back-off statistics; num_ctx now comes from the serving pool row, so every chat-role call resolving onto the same row shares the single runner by construction
- **Streaming Tool-Call Wire** (`llm.ChatStream`) — streaming OpenAI-compatible chat with function calling, the wire layer for the upcoming web-chat harness (no consumer yet). Multi-turn message arrays, per-delta events, index-keyed tool-call assembly, arguments normalisation (llama.cpp JSON-string fragments and whole-object form yield identical calls), hardened against OpenRouter SSE comment frames and mid-stream error events inside HTTP-200 streams; usage falls back to llama.cpp `timings` incl. MTP draft-acceptance
- **Embed Cache** — content-hash-keyed embedding cache (`context_embed_cache`) to avoid re-embedding identical text across pipelines
- **LLM Log** — per-call request/response capture (`context_llm_log`) with input/output token counts (Ollama + OpenAI), dream-pipeline version tagging, and parse-format drift tagging (`metadata.parse_format`: array | object | fenced-array | fenced-object) for pipeline debugging + offline benchmark replay. Since 054 each chained call carries backend provenance: `backend_name`/`backend_trust`/`backend_locality` of the backend that **actually answered** (the pre-pool code logged the primary host even when the fallback served), `attempt` + the full per-attempt `metadata.chain`, and a partial index on `backend_locality='external'` as the egress audit trail; `cost_usd` carries OpenRouter's `usage.cost` since the G29 wave (NULL on local backends); `api_key_id` is reserved for caller attribution. Since 055 the formerly unlogged query-path roles (translate, temporal, query-embed, rerank, inline backfill) write **slim rows** — full backend/trust/locality/`required_sensitivity`/attempt telemetry plus `block_ids` where block content was sent, NO prompt bodies (~0 storage; embed-cache hits contact no backend and write no row). The background wave completed the coverage: every dream/digest row now carries the chain provenance, and the background embed wire-calls (`dream-keyword-embed`, scheduler `embed-backfill`) write the same slim rows with their block ids. Rows whose `required_sensitivity` is `credentials` get the body slim across ALL pipelines (synthesis and dream alike): the egress trace stays ID-exact while the hottest tier leaves no plaintext shadow corpus
- **MCP Remote** — Streamable HTTP transport with OAuth 2.1 PKCE for claude.ai/Claude Code integration. Tools: query, store, search, get, recent. Client registration via `ctx mcp add`. Tool handlers return `Content[].text` (no structured output) — tested in `test.sh` T17/T18

## API

All endpoints under `/api/*`. Auth via `X-Context-Key` header or `Authorization: Bearer` token.

| Endpoint | Description |
|----------|-------------|
| `POST /api/query` | 4-Way RRF + LLM synthesis (auto-backfills pending embeddings; optional `categories_exclude` / `block_roles_exclude` arrays filter slot-stealers; optional `sensitivity` classifies the query text for trust gating — default settings key `pool.default_query_sensitivity`; optional `include_content` attaches a <=1500-char snippet per source on the retrieval-only path `synthesize:false`, default off so eval/sweep responses stay byte-identical — the F6 chat harness's ctx_query tool sets it). Whenever synthesis is on (`synthesize != false`, 054 — any pool-chain leg can exceed 60s, not just the ~80s reranker path) the response commits `200` up front and streams a whitespace keepalive every 25s so buffering reverse proxies don't hit their read timeout; the body stays valid JSON (leading whitespace, RFC 8259) and a late synthesis failure reports `success:false` inside the 200 body |
| `POST /api/store` | Upsert (embedding async via scheduler). Optional `sensitivity` (`credentials`\|`personal`\|`internal`\|`public`) classifies the block manually (`sensitivity_source='manual'`); absent ⇒ settings key `pool.default_block_sensitivity` (fail-closed `credentials`). On an upsert conflict an explicit value applies upgrade-only — downgrades go through `manage update` with `confirm_sensitivity_downgrade`. A credentials pattern in the content forces `credentials` upgrade-only regardless of the requested level (G40 detector, `sensitivity_source='pattern'`) |
| `POST /api/search` | Lightweight search (no LLM) |
| `GET /api/graph/ego` | Scope-filtered k-hop ego subgraph over dream links (read-only, no LLM — see [Graph API](#graph-api)) |
| `GET /api/graph/overview` | Scope-pure Louvain cluster supergraph ("landkarte"); reads precomputed scope-partitioned aggregates, gated on `graph_overview.enabled` (off → 404). Read-only, no LLM (see [Graph API](#graph-api)) |
| `GET /api/whoami` | Calling key's identity: `label`, `home_scope`, `read_scopes`, `admin` tier flag — the SPA login gate probes it and derives its read-only degradation from `admin` |
| `POST /api/manage` | CRUD, Guard API, stats, API-key management (`api-key-create` requires `home_scope`; key/MCP-client management and mutating `dream-mode` require an **admin key** since 052 — see Admin tier) |
| `GET\|PUT\|DELETE /api/settings[/{key}]` | Runtime config overrides, **admin-gated incl. reads** (see [Settings API](#settings-api)) |
| `GET\|PUT\|DELETE /api/secrets[/{name}]` | Write-only sealed credentials, **admin-gated**: PUT creates/rotates (value never returned), GET lists metadata + `referenced_by`, DELETE 409s while referenced (see Sealed secrets & break-glass) |
| `GET /api/status` | **Admin-only** dashboard aggregate from the process-wide status collector: health, backend pool (`pool.Status()` shape), dream queue + mode, 24h LLM telemetry (with a `llm_24h_complete` attribution flag), gaming toggle. Served from a cache (N pollers cost one collection; the O(n) dream-queue scan decouples on its own interval) — carries hostnames, so it is admin-gated where `/health` stays anonymous |
| `GET /api/llmlog` | **Admin-only** LLM telemetry table (`?limit=`/`pipeline=`/`errors_only=`). NEVER returns the `request_system`/`request_user`/`response_content` body columns (the prompt shadow corpus); the `error` is normalized to a class + 256-char-capped detail so a provider body can't leak prompt fragments |
| `GET /api/events` | **Admin-only** SSE live stream (`text/event-stream`) for the dashboard (G34). The process-wide collector diffs its snapshot ONCE per tick and fans `status` / `backends` / `llmcall` events to every connection (N panels cost one build); a new connection gets the full state first, then diffs. `: ping` keepalive (`CTX_EVENTS_PING_INTERVAL`), a rolling 90 s write deadline that outlives the absolute server `WriteTimeout`, the `CTX_EVENTS_MAX_CONNECTIONS` cap → 429 (client degrades to polling), and an in-stream re-auth every 12th tick that ends the stream on key revocation. Same body-free shapes as `/api/status` + `/api/llmlog` |
| `POST /api/digest` | Topic map generation |
| `POST /api/ingest` | Obsidian vault ingestion |
| `POST /api/blob/*` | Binary storage (store/fetch/search/manage) |
| `GET /health` | DB + pool role reachability, aggregated to anonymous service classes (no backend names, no states — topology is admin-only via `backend-list`) |
| `POST\|GET\|DELETE /mcp` | MCP Streamable HTTP (remote tool server) |
| `GET /authorize` | OAuth 2.1 authorization (PKCE) |
| `POST /token` | OAuth 2.1 token exchange |
| `GET /` (unregistered paths) | Embedded admin SPA (Svelte 5 + Vite, served from the binary). History-API fallback answers HTML navigations (`Accept: text/html`) only — mistyped API URLs stay 404 for JSON clients. Hashed `/assets/*` are immutable-cached and pre-compressed (`.br`/`.gz`); binaries built without the frontend (plain `go install`) serve a 503 placeholder while all APIs stay functional — the Docker image is the channel that ships the real UI. Areas: **Settings** (generic config editor + the **Backends** sub-route `/settings/backends` — backend-pool editor with a trust dropdown + elevation-confirm dialog, roles multi-select, model_map line editor, priority up/down and per-row reachability test, plus the write-only **secrets vault** with reference tracking; all over the existing `backend-*` / `/api/secrets` admin actions), **Graph**, **Blocks** (corpus browser — full-text search + category/tag/scope facets + newest-first list over the scope-gated `/api/search`, with a read-only detail panel; editing lands in a later wave), **Status** dashboard + SSE, **Chat** |

### Graph API

`GET /api/graph/ego?block=<uuid>` returns the k-hop ego subgraph of a focus block over the dream-link graph — the server side of the graph viewer. Designed for 1M+ blocks: the server only ever ships budgeted subgraphs, never the full graph.

```
GET /api/graph/ego?block=<uuid>&hops=2&per_node_cap=25&limit=500
                  &min_confidence=0.5&link_class=topical,causal
                  &category=learnings&created_after=2026-01-01T00:00:00Z
                  &edge_limit=4000
```

| Param | Default | Range | Meaning |
|-------|---------|-------|---------|
| `block` | — (required) | full UUID | focus node (hop 0) |
| `hops` | 1 | 1–3 | BFS depth |
| `per_node_cap` | 25 | 1–100 | top-N edges per frontier node by `raw_confidence` — slots count only visible, filter-passing edges |
| `limit` | 500 | 1–1500 | total node budget (truncation: closer hop wins, then higher confidence, then id) — ceiling set by the G39 1M benchmark (p95 < 500ms; was 5000) |
| `min_confidence` | 0 | 0–1 | gate on weighted confidence (traversal + displayed edges) |
| `link_class` | all 5 | topical,factual,causal,recurrent,supersedes | `supersedes` is display-only, never traversed |
| `category` | all | CSV | filter on neighbor blocks (focus always included) |
| `created_after` / `created_before` | open | RFC3339 | window on neighbor `created_at` |
| `edge_limit` | 4000 | 1–20000 | budget for edges within the node set, strongest first |

Out-of-range values are a `400`, never silently clamped. Response: `nodes` (id, title capped at 120 chars, category, scope, visible `degree` — capped at 201, rendered "200+" — and `hop`), `edges` as compact index tuples `[srcIdx, dstIdx, relIdx, confidence]` into `nodes`/`rels`, and `stats` (`nodes`, `edges`, `truncated`, `elapsed_ms`). The payload never contains block `content` (load it lazily via `manage get`).

Security semantics: the visibility triple (not archived, not system-meta, scope readable by the key) is applied inside every hop **and** inside the per-node cap legs — a node reachable only through a foreign private bridge is never delivered, and invisible edges never consume cap slots. `degree` counts only visible neighbors (scan budget 1000 raw edges/direction). "Does not exist" and "not visible" answer with an identical `404` (no existence oracle), and only successful calls write an access-log row (`action='graph'`, `block_id=NULL` — graph browsing never feeds access-count ranking).

#### Overview (cluster "landkarte")

`GET /api/graph/overview` returns the cluster supergraph: a few hundred meta-nodes (precomputed Louvain communities over the dream-link graph) with `size`, `top_categories`, a representative block, and aggregated inter-cluster meta-edges. Click a meta-node → drill into its representative's ego net (`GET /api/graph/ego`). The Louvain rebuild runs offline in the scheduler (`internal/overview`, gonum); the endpoint only reads precomputed tables.

```
GET /api/graph/overview?min_cluster_size=1&min_inter_cluster_weight=0&node_limit=500&edge_limit=2000
```

| Param | Default | Range | Meaning |
|-------|---------|-------|---------|
| `min_cluster_size` | 1 | ≥1 | hide meta-nodes whose visible size is below this |
| `min_inter_cluster_weight` | 0 | ≥0 | hide meta-edges below this aggregated weight |
| `node_limit` | 500 | 1–2000 | max meta-nodes (largest first) |
| `edge_limit` | 2000 | 1–20000 | max meta-edges (strongest first) |

Response: `nodes` (`cluster` ordinal, `size`, `top_categories`, `repr_id`/`repr_title`, `scope_mix`), `edges` as compact tuples `[srcOrdinal, dstOrdinal, link_count, weight]`, and `stats` (`computed_at` = last rebuild, `null` if never built). The feature is gated on the hot setting `graph_overview.enabled` (default off → `404`).

Security semantics (the solved scope-count-leak, [design 07](.project/plan-web-2026-06-10/design/07-graph-overview.md)): aggregates are **scope-partitioned** — each precomputed row belongs to exactly one scope (nodes) or scope-pair (edges), and a request sums only rows whose scope(s) lie entirely within the caller's `read_scopes` (edges need **both** endpoint scopes visible, like induced edges). No global total is ever exposed, so a private member count cannot be recovered by difference. The internal `cluster_id` (the smallest member UUID, scope-agnostic) is **never** emitted — clients see a per-request ordinal, so the identifier itself is not an existence oracle over foreign blocks. Like the ego endpoint, only successful calls write an access-log row (`action='graph-overview'`, `block_id=NULL`).

### Settings API

Runtime config editing over the `context_settings` override layer. **Admin-gated including reads** — the effective config (hosts, models, thresholds) is operational intelligence, and a non-admin key that can read it can also enumerate what to attack.

```
GET    /api/settings           # every registry key: value, source, type, mutability, default
GET    /api/settings/{key}     # single key + last 10 audit rows (action, actor, via)
PUT    /api/settings/{key}     # body {"value": <scalar>} — validated BEFORE persist
DELETE /api/settings/{key}     # drop the override, revert to env/default
```

Semantics:

- **Validation before persist.** A PUT builds the candidate config through the same path the reload uses; a value the build would reject or ignore is a `422` and never reaches the table (no row, no audit entry). Unknown keys are `404`; `restart`/`coupled` keys are `409` with the env var to set instead. String inputs are normalized to their registry type before persist (`"0.7"` is stored as the number `0.7`).
- **Hot effect.** After commit the handler swaps the snapshot — the next request/cycle runs with the new value, no restart. Direct `psql` edits arrive through the NOTIFY listener with the same effect, and the trigger audit records them as `via='sql'`.
- **Masking rule.** Any response position carrying the effective value of a sensitive key renders `"(set via env)"` when the value comes from env — including `previous.value` on PUT and the post-revert `value` on DELETE (the standard migrate-to-secret_ref flow would otherwise echo the `.env` plaintext). DB-sourced sensitive values render the secret **name** (`secret_ref`), never resolved material.
- **secret_ref gate.** Sensitive keys (`*.api_key`, `server.db_password`) accept only the *name* of an existing sealed secret — a provider-key-shaped value is rejected with `422` so plaintext can never land in `context_settings` or its append-only audit trail.
- **Embed-cache coupling.** Writes (and reverts) that change the effective embed/dream-embed host or protocol flush `context_embed_cache` automatically — vectors computed by the old backend must never blend with the new one's. The response `warnings` array also flags a `.host` change whose sibling `.protocol` still comes from env: **change host + protocol + api_key together** (a lone host flip onto a different wire format 404s at request time).

```bash
curl -s -X PUT "$CTX/api/settings/rerank.blend_weight" \
  -H "X-Context-Key: $ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"value":0.6}'
# → {"success":true,"key":"rerank.blend_weight","value":0.6,"source":"db",
#    "previous":{"value":0.5,"source":"env"},"warnings":[]}
```

### Backend pool (F3, migrations 053–055)

`context_backends` replaces the hardwired primary+fallback pair with a declarative, role-routed, priority-ordered pool. Each row is one backend: `base_url`, wire `protocol` (`openai`/`ollama`/`rerank`), `provider_class` (`generic`/`llamacpp`/`openrouter`), a **trust level**, an egress `locality`, a `roles` list (`synthesis`, `translate`, `embed`, `rerank`, `dream`, `digest`, `chat`, `classify`, free-form), a per-role `model_map` (string short form or `{"model":…,"params":{…}}`), per-role `timeouts`, `priority` and `enabled`. Order/priority are pure DATA — no code path references backend names or priority constants.

On first boot with an empty table, ctxd seeds it from the effective config snapshot (settings > env precedence); afterwards the **table is the source of truth** and the `CTX_*_HOST` env vars only feed that one-time bootstrap.

**Trust × sensitivity matrix (fail-closed).** A backend with trust `T` may receive content of sensitivity `S` iff `rank(S) ≤ maxRank(T)` — `full-trust` ≥ credentials, `no-credentials` ≥ personal, `non-personal` ≥ internal, `public` = public only. Empty/unknown sensitivity counts as `credentials`; an empty chain is an error, **never a silent escalation across trust borders**.

**Block sensitivity (055).** Every block carries `sensitivity` (default `credentials` — unclassified content never leaves full-trust backends; normal operation is untouched while all backends are full-trust, only a future external leg stays dark until classification opens it block by block) plus `sensitivity_source` (`default`/`llm-audit`/`pattern`/`manual`; `manual` is untouchable for the audit wave) and `sensitivity_audited_at`. The query path batch-annotates **all** RRF candidates after graph expansion (a supersedes/graph straggler from beyond rank 50 still carries its level into the gate; a lookup miss acts as `credentials`), applies the scope floor `pool.scope_sensitivity_floor` (a JSON map `scope → minimum level`; it can only RAISE — blanket protection for friend-tenant scopes without block mutation), and gates each role with its real requirement: query-only roles (translate, temporal, query-embed) with the query sensitivity, rerank with `max(query, judged docs)`, synthesis with `max(query, final prompt set)`, inline backfill per block. **Downgrade guard (both directions of the same border):** lowering a block's sensitivity needs `confirm_sensitivity_downgrade:true` on `manage update` (audited to `metadata.sensitivity_audit`), exactly like raising a backend's trust needs `confirm_trust_elevation`; the settings defaults `pool.default_block_sensitivity`/`pool.default_query_sensitivity` are guard-marked the same way (`PUT` body flag, CLI `ctx settings set --confirm-sensitivity-downgrade`). `ctx save --sensitivity LEVEL` classifies on write.

**Gaming toggle (F3-P6, `gaming.active` + `gaming.disabled_backends`).** `ctx gaming on` flips the GPU-host backends (default `herbert-chat` + `herbert-rerank`) out of EVERY chain so the GPU is free to game; `llama-cpu` and any external backend stay in as failover. The flip is a settings write — admin-gated (an ungated toggle would let any tenant key flip the system's egress topology), and persistent: it SURVIVES a restart (the dream-mode break path, where a restart drops the GPU lock, is the anti-pattern it avoids) and takes effect on the next chain without one, via a synchronous reload. In-flight requests finish normally; the dream cycle-skip (above) already covers the back-off-curve integrity. A name in the disabled list matching no live backend surfaces as `unknown_backends` (a typo would otherwise leave the GPU busy). Per-backend runtime detail stays in the admin-gated `ctx backends`; `/health` never carries the gaming flag (it would be an "admin sits at the GPU host" presence oracle).

**Sensitivity LLM audit (G41).** `ctx blocks audit start` (manage action `blocks-audit-start`, **admin**) classifies every home-scope block still at `sensitivity_source='default'` out of the fail-closed credentials default: two SEPARATE yes/no questions per block over the `classify` role chain — "beinhaltet dieser block möglicherweise schützenswerte credentials?" and "beinhaltet dieser block möglicherweise personenbezogene daten?" — answered as strict JSON booleans (deliberately NO confidence field: local-model self-reported confidence is uncalibrated). Verdict table: credentials-ja keeps `credentials` (the personal question is skipped), nein+personal-ja → `personal`, nein×2 → `internal`; `public` is **never** assigned by the audit — that stays manual. A parse failure is no verdict (the block keeps the credentials default and a 24h retry cooldown via `sensitivity_audited_at`); a chain/backend failure **aborts the run** instead of cooling down blocks the model never judged. `manual` rows are untouchable by the SQL predicate itself (`WHERE sensitivity_source='default'`), a concurrent manual classification between pick and verdict discards the verdict. The `classify` role is **hard-local**: `backend-create`/`update` rejects `classify` on `locality='external'` with 422 and — unlike embed — there is no metadata escape hatch, because audit prompts carry unclassified block content by definition (full-trust ZDR included); the chain executor additionally drops external rows at call time. Before a bulk run, gate with a sample: `ctx blocks audit sample --n 30` classifies 30 random pending blocks WITHOUT writing and reports the would-be verdicts in `blocks-audit-status` for manual accuracy review. Every wire call writes a slim llmlog row (pipeline `sensitivity-audit`, block id attached, no bodies).

**Credentials pattern detector (G40).** A deterministic, LLM-free scanner (`internal/sensitivity`) that only ever RAISES content to `credentials` — never downgrades. It runs at two points automatically: on `POST /api/store` (a content hit forces `credentials` with `sensitivity_source='pattern'` and records the secret-free reason in `metadata.sensitivity_detector`) and on `POST /api/query` (a hit in the query text raises the operation's required sensitivity, so a query carrying a secret can never reach a lower-trust backend). Rule set, precision over recall (a false positive permanently blocks external failover for that block, so generic blobs that collide with this corpus's git SHAs and content hashes are avoided): AWS key ids, PEM private-key headers, JWTs, vendor token prefixes (`sk-`/`ghp_`/`xox…`/`AIza…`/`glpat-`), entropy- and placeholder-gated secret assignments, high-entropy base64 blobs (≥32 chars, >4.5 bits/char), long hex blobs (≥64). The bulk re-audit `ctx blocks classify start` (manage action `blocks-classify-start`, **admin**) keyset-walks every home-scope block that is not already `credentials` and not `manual`, raising hits to `credentials`/`pattern` — the deterministic **veto** against the G41 audit (a `pattern` row is outside the audit's `source='default'` pick set, so the LLM can never downgrade a pattern hit). `manual` stays untouchable; `credentials` blocks are left intact (upgrade-only); the write predicate re-checks both invariants race-safe. Always `dry-run` first (`ctx blocks classify dry-run`) — it scans the real corpus WITHOUT writing and lists exactly what would be raised, the empirical false-positive gate before committing. Once the corpus is classified, `pool.default_block_sensitivity` can be lowered to `personal` via the guarded settings write.

Manage actions (all **admin-gated, reads included** — the list discloses egress topology):

```
POST /api/manage {"action":"backend-list"}                 # rows + live status (effective_state, cooldown, sanitized last_error)
POST /api/manage {"action":"backend-create","data":{…}}    # full validation, see below
POST /api/manage {"action":"backend-update","id":…,"data":{…}}   # single-field patch
POST /api/manage {"action":"backend-delete","id":…}        # hard delete (llmlog history stays readable)
POST /api/manage {"action":"backend-test","id":…,"data":{"probe":"chat"}}  # reachability dry-run
```

Validation guards (create AND update, `422` with field errors): credential-carrier headers in `extra_headers` (`Authorization`, `Cookie`, `*-key`, `*-token`, …) and credential-semantic `extra_body` fields are rejected — provider keys go through `api_key_ref`, the *name* of a sealed F2 secret, resolved in-memory only; `locality` is cross-validated against `base_url` (a publicly routable host must be `external` — the egress audit depends on it); embed roles on external backends are blocked without `metadata.embed_equivalence_verified=true` (foreign quantization corrupts the shared vector space irreversibly). Raising `trust` (create above `public`, or update toward `full-trust`) requires `confirm_trust_elevation:true`. Every mutation reloads the pool snapshot synchronously — `backend-update {"enabled":false}` is an instant brake, no restart; psql edits converge via the 053 NOTIFY trigger.

**OpenRouter (first external backend, G29).** `provider_class: "openrouter"` refines the openai wire: the request **always** carries `provider.zdr:true` + `provider.data_collection:"deny"`, **independent of the trust level** — trust decides WHICH content may flow to a backend, the provider class decides whether the provider may store it. Raising the backend to full-trust therefore never silently drops the ZDR guarantee; `extra_body.provider` entries merge but can only tighten (the force runs after the merge). The single escape is `metadata.allow_data_collection: true`, and arming it requires `confirm_data_collection:true` on create/update — never implicit. Responses feed the telemetry: `usage.cost` → llmlog `cost_usd` (local backends stay NULL), the top-level `model` (the model that **actually** answered — OpenRouter's models-fallback can differ from the request) overwrites the row's model column, the response `id` lands in `metadata.provider_request_id` for async audit via `GET /api/v1/generation`. A request rejected because the zdr/deny filter leaves no provider ("no providers") classifies as configuration-permanent: 1h cooldown, error log, no retry storm. `backend-test` on an openrouter-class row additionally reports `credits_remaining`/`usage_usd` (from `GET /v1/key`) and `zdr_endpoints` — the default model's ZDR endpoint count (from `GET /v1/endpoints/zdr`), which predicts whether the forced `zdr:true` leaves a non-empty provider set *before* the first failover needs it. **`base_url` is the API root WITHOUT the version segment** — the wire paths append `/v1/...` themselves (llama.cpp `http://host:port`, OpenRouter `https://openrouter.ai/api`); a `base_url` ending in `/v1` double-segments to a 404.

### Web chat sessions (F6, migration 056)

The persistence layer, the server-side tool harness, and the streaming HTTP endpoint for web chat. `context_chat_sessions` is **scope-owned**: list and delete key on the creating tenant's home scope, so a key never sees a foreign tenant's chats. It snapshots the creating key's `read_scopes` and carries a **monotone `max_sensitivity` high-water-mark**. Because a tool result may hold cross-scope content, reading or continuing a session requires `session.read_scopes ⊆ caller.ReadScopes` (else 404, indistinguishable from non-existent — no oracle), closing the shadow-corpus channel against future least-privilege keys. The HWM rises with **every** appended message (raised in the same short transaction that assigns the message `seq`, so the trust gate is structurally unforgettable); a credentials-touched session therefore stays full-trust-only for its whole life. `context_chat_messages` records per-message `sensitivity` (fail-closed `credentials` default), tool-call metadata, telemetry and a gapless `seq` (`UNIQUE(session_id, seq)`). A turn claims its session via a short `busy_until` CAS — a second concurrent turn gets 409 without blocking, a crashed turn self-heals on expiry — instead of holding a connection-long transaction (which would starve the pgxpool). Retention is off by default (`CTX_WEBCHAT_SESSION_RETENTION` — duration suffix `h/d/w/m/y`; a background janitor on the embed-cache tick deletes older sessions, messages cascade).

The **harness** (`internal/chat`) drives the model loop: model call → tool execution → next call, re-resolving the F3 chat chain each iteration on `max(request, session HWM)` sensitivity so a credentials-touched session can only ever reach a full-trust backend (an empty chain ends the turn — never a silent escalation). Four read-only tools run under the session's `read_scopes` snapshot: **ctx_query** (hybrid retrieval, delegated to the query pipeline — see the `include_content` flag below), **ctx_search**, **ctx_get** (full block, paged past the window via a resumable `offset`), **ctx_recent**. Each result is annotated with `max(sensitivity)` of the blocks it carried, raising the session HWM; tools are offered only to a full-trust backend, and the closing call after the tool-budget cap carries no tools array (never `tool_choice:none`, which leaks tool syntax as text). Tool errors return to the model as `{"error":…}` and never abort the turn. Events flow through a narrow `Sink` interface, so a future headless agent runner can drive the same loop without HTTP.

**Endpoints** (auth required; `CTX_WEBCHAT_ENABLED=false` ⇒ 404):

| Route | What |
|---|---|
| `POST /api/chat/stream` | Run one turn, response `text/event-stream`. Body `{session_id?, message, sensitivity?, tools_enabled?, max_tokens?}` — empty `session_id` creates a session. Pre-stream failures are JSON (`404` unknown/foreign session or feature off, `409` session busy, `429` scope semaphore); once the first event flows the status is spent and later failures are `error` events. SSE events: `session, backend, delta, tool_call_start, tool_call, tool_result, usage, done, error` + a `: hb` keepalive every 15s of silence. Errors are **laundered** to class code + backend NAME — the raw backend URL never reaches the client. Wrapped in the scheduler signal so dream yields the single llama.cpp slot during a turn |
| `GET /api/chat/sessions?limit=50` | List the caller's home-scope sessions (metadata + `message_count`, newest first; no content) |
| `GET /api/chat/sessions/{id}?after_seq=0&limit=0` | One session + its messages (full tool-result contents); gated by `read_scopes ⊆ caller` → 404 on miss. Pagination additive |
| `DELETE /api/chat/sessions/{id}` | Hard-delete (messages cascade); home-scope-owned → 404 on miss. Complete because llmlog logs `web-chat` **metadata-only** (no conversation bodies in the un-scoped `context_llm_log`, §R9) |

A per-`home_scope` semaphore (`CTX_WEBCHAT_CONCURRENT_TURNS`, default 1) bounds concurrent turns — multi-tenant fairness on the single slot (429 before stream start). The `ctx_query` tool delegates to the same `/api/query` handler with `synthesize:false` + `include_content:true`, run under the session's `read_scopes` but attributed to the real key.

## Building

```bash
go build -o ctx ./cmd/ctx/           # CLI
go build -o ctxd ./cmd/ctxd/         # Daemon
go test ./... -short                  # Unit tests
```

### Web UI (Svelte 5 + TypeScript + Vite, Bun)

The admin SPA lives in `go/web/` and is embedded into the ctxd binary via
`go:embed`. The Docker image builds it in its own stage (`oven/bun:1.3-alpine`,
`bun install --frozen-lockfile`, `svelte-check` gate) — `docker compose build ctx`
is the channel that ships the real UI. Plain `go build` / `go install .../cmd/ctxd`
need no Bun and produce a binary that serves a 503 placeholder instead of the UI;
the CLI (`cmd/ctx`) never depends on the frontend at all.

The **Settings area** renders the full [Settings API](#settings-api) catalog
generically from the registry metadata — one category card per key prefix,
widgets dispatched by registry type (an unknown future type degrades to a
read-only rendering), source badge (`default`/`env`/`db`) and env-var name per
field. Hot and `coupled:embed-cache` keys edit live (save = one `PUT` per
changed key, a `422` lands inline at exactly that field); restart/coupled keys
render read-only with the same hint the API's `409` carries. Fields with a
`db` override get a reset affordance (`DELETE`, revert to env/default).
Sensitive keys show masked values only and take a secret *name*. The three
cross-field rules (thresholds, dual-runner `num_ctx`, `blend_weight`×graph)
are mirrored client-side as inline previews while the server-side candidate
build stays authoritative. Non-admin keys get a read-only banner — the
catalog itself is 403 for them.

The **Graph area** (`/graph?focus=<uuid>`, deep-linkable) renders dream-link
ego networks via sigma (WebGL) over one graphology instance as the single
source of truth — deliberately outside Svelte reactivity, the runes proxy
overhead on thousands of node objects is the documented reason. Entry is the
FTS search (`POST /api/search`); a hit click or node click focuses that
block's ego net (`GET /api/graph/ego`, 2 hops). Edge index tuples resolve to
UUIDs at merge time (they are response-local), re-merges keep node positions,
and the payload carries titles only — block content never travels through the
graph endpoint. Read-only: no LLM is touched from this area.

Double-clicking a node expands it (+1 hop merge, focus stays); the layout is
ForceAtlas2 in a web worker (Blob-URL — the CSP carries `worker-src blob:`
for this), running 3–10s scaled by graph size after every merge. Client
memory is hard-capped: over 5 000 nodes / 20 000 edges the nodes farthest
from the focus (BFS distance, LRU tie-break) are evicted down to 4 000 —
pinned nodes and the focus survive. Each node label carries a `· +N` badge
for visible-but-unloaded incidences (`200+` past the server's degree cap).

One filter state (link class, min confidence, category, created window)
drives both sides: loaded elements filter instantly through the sigma
reducers — zero server roundtrips — while new focus/expand fetches mirror
the same filters as ego-query params. Degree badges stay unfiltered by
design (the server counts all visible incidences). Single-clicking a node
opens the detail sidebar: metadata from the loaded attributes, full content
lazy through the existing scope-checked `manage get` (graph payloads never
carry content), plus focus/expand/pin actions — pinned nodes are exempt
from eviction. Content renders as a text node, never `{@html}`.

The **Chat area** (`/chat`) streams a turn from [`POST /api/chat/stream`](#web-chat-sessions-f6-migration-056) over fetch + `eventsource-parser` — **no reconnect** (a turn is one-shot; a reconnect would re-run it). The thread shows the user message, collapsible tool-call cards (`ctx_query · "…" · N blocks · ms`; arguments + block list as text, each block linking `/graph?focus=<id>`), the streamed assistant answer and a backend badge (which backend served, whether tools were offered + why not). Assistant markdown goes through the sanitizing pipeline — **markdown-it `html:false` + DOMPurify**, with `[title](ctx:<id>)` citations rewritten to `/graph?focus=<id>` BEFORE sanitizing so DOMPurify's allowlist stays intact (raw HTML in a quoted block is escaped, never parsed; `markdown.ts` carries the XSS suite). The left sidebar lists sessions (newest first, message count, a 🔒 on credentials-touched ones); a turn is abortable and aborts on navigate-away/`beforeunload` (frees the single llama.cpp slot). A pre-stream `409`/`429` is a JSON error (busy / scope semaphore); a mid-stream failure is an `error` event that keeps the partial + offers a retry.

```bash
cd go/web
bun install                           # once; bun.lock is committed
bun run dev                           # Vite on :5173, proxies /api → ctxd
bun run check && bun run build        # typecheck + production build into dist/
```

The dev proxy targets `http://localhost:8080`; the compose ctx service publishes
no ports by default — add a local port mapping (see
`docker-compose.override.yml.example`) and override with
`CTX_DEV_PROXY=http://127.0.0.1:<port>` if you map a different port.

## License

[MPL-2.0](LICENSE) — By [GottZ](https://github.com/GottZ)
