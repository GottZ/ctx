# Operations

## Configure the CLI endpoint

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

Verify:

```bash
ctx health    # DB + Ollama connectivity
ctx stats     # Block count, categories, storage
```

## Claude Code integration (optional)

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

## Environment variables

Every var below can also carry a runtime override in `context_settings` (precedence: **DB override > env > default**; sealed `context_secrets` + trigger-fed audit trail in `context_settings_audit`, migration 051). The boot loads the overrides right after the migrations and builds the effective snapshot; sensitive keys take a `secret_ref` (the *name* of a sealed secret), resolved in-memory only. The override layer is never fatal: unknown keys, restart-only/coupled keys (incl. the `CONTEXT_DB_*` group), corrupt values and a missing or wrong master key each degrade to a WARN while the env/default value stays active; `CTX_SETTINGS_DISABLE=1` switches the whole layer off. Live editing goes through the admin-gated [Settings API](api.md#settings-api); direct SQL edits (and break-glass resets) take effect immediately (the 051 triggers NOTIFY a listener that rebuilds the snapshot).

The **`mut` column** is the mutability class per key: **hot** keys take effect without a restart (snapshot consumers pick them up on the next request/cycle); **restart** keys are process wiring (DB connection, listener, worker-goroutine count — runtime writes rejected with 409); **coupled** keys carry a side-effect obligation (embed host/protocol changes flush `context_embed_cache` on apply; an embed **model** change changes the vector space, needs a re-embed migration and stays env-only, 409).

| Var | Default | Mut | Purpose |
|-----|---------|-----|---------|
| `CTX_BASE_URL` / `CTX_KEY` | – | – | CLI client config (`~/.config/ctx/config`), not a server key |
| `CONTEXT_DB` / `CONTEXT_DB_USER` / `CONTEXT_DB_PASSWORD` | – | restart | Database (separate from inference) |
| `CTX_SECRETS_KEY` / `CTX_SECRETS_KEY_PREV` | – | restart | Master key for AES-256-GCM-sealed `context_secrets` (64 hex chars, `openssl rand -hex 32`); `_PREV` only while a rotation sweep is pending. Env-only by design — **copy into your password manager**, key loss = total loss (see [security](security.md#sealed-secrets--break-glass)) |
| `CTX_EMBED_HOST` / `_PROTOCOL` / `_MODEL` | `ollama` / – | coupled | **Bootstrap-only since the backend pool (053):** seeds the `llama-embed` pool row on the first boot with an empty `context_backends`, then the pool owns embedding and these are inert (manage via `ctx backends`). `_API_KEY` / `_NUM_CTX` seed the same row |
| `CTX_CHAT_HOST` / `_PROTOCOL` / `_MODEL` / `_THINK` / `_NUM_CTX` | `ollama` / – / `false` / `0` | hot | **Bootstrap-only since 053:** seeds the `herbert-chat` pool row (synthesis / translate / chat / digest roles), then inert. `_NUM_CTX` (`0`=model default) seeds the row's num_ctx; live it comes from the serving pool row, so chat-role calls resolving onto one row share a single runner |
| `CTX_CHAT_FALLBACK_HOST` / `_PROTOCOL` / `_API_KEY` / `_TIMEOUT` | empty (off) / `openai` / – / `420` | hot | **Bootstrap-only since 053:** seeds the low-priority `llama-cpu` pool row; afterwards the pool chain owns synthesis failover. `_TIMEOUT` (seconds) becomes the row's per-role timeout, sized for CPU inference (27B ≈ 4.5–5.5 min/answer; the body heartbeat keeps proxies alive). See the `llama-cpu` compose service |
| `CTX_DREAM_ENABLED` | `false` | restart | Toggle continuous Dream loop |
| `CTX_DREAM_PARALLELISM` | `1` | restart | Concurrent Dream workers — race-safe via atomic claim |
| `CTX_DREAM_HOST` / `_PROTOCOL` / `_MODEL` / `_NUM_CTX` | inherits chat | hot | **Bootstrap-only since 053:** seeds the dream role — its own `herbert-dream` row when the host diverges from chat, else the `dream` role on `herbert-chat` — then inert. Separate Dream model (e.g. larger, slower) |
| `CTX_DREAM_EMBED_*` | inherits embed | coupled | **Bootstrap-only since 053:** seeds the `dream-embed` role — merged onto `llama-embed` when identical to `CTX_EMBED_*`, else its own row. For a separate Dream embedding endpoint, create a pool row with role `dream-embed` |
| `CTX_DREAM_IDLE_WAIT` | `20` (s) | hot | Backoff when no pending blocks |
| `CTX_DREAM_BACKOFF_MODE` / `_FACTOR` / `_MIN` / `_GRACE` / `_CAP` / `_INERT_OFFSET` | `exp` / `1.6` / `12h` / `0` / `45d` / `7` | hot | Re-dream back-off by eval count (`exp`/`log`/`linear`/`off`). Cooldown grows from `MIN` (n=0) to `CAP`: fresh blocks re-dream sub-day, mature blocks back off to the cap. `_MIN`/`_CAP` take a duration suffix — `h`/`d`/`w`/`m` (30d)/`y` (365d), e.g. `12h`, `45d`, `1w` (bare number = hours). `_INERT_OFFSET` starts a no-links cycle further up the curve |
| `CTX_PROMPT_VERSION` | `v5.2` | hot | Generator-prompt version (`v5.2` default, `v6` opt-in graded confidence) |
| `CTX_TIMEZONE` | `Europe/Berlin` | hot | Cyclic-temporal phase calculation |
| `CTX_CONFIDENT_THRESHOLD` | `0.008` | hot | Generator-side refusal threshold (RRF score below → "I don't know") |
| `CTX_READ_SCOPES` | scope-derived | hot | API key's effective read-scope set (v2.0.0+ scheduler config) |
| `CTX_LLMLOG_RETENTION_DAYS` | `90` | hot | After N days the background janitor NULLs the prompt/response **bodies** in `context_llm_log`; the telemetry row (pipeline / model / tokens / cost / block_ids / backend / trust) survives, so the egress audit stays lossless. `0` = keep bodies forever. Body-NULLing, not a chunk drop. Shares the embed-cache janitor tick (6h) |
| `CTX_LLMLOG_MAX_LIMIT` | `200` | hot | Cap on `GET /api/llmlog?limit=` |
| `CTX_EVENTS_TICK_INTERVAL` / `CTX_EVENTS_QUEUE_STATS_INTERVAL` | `5` (s) / `30` (s) | hot | Status-collector cadence: the cheap sources (health / pool / dream mode / gaming / llm-24h) refresh at most once per tick; the O(n) dream-queue scan decouples to its own slower interval. Also the SSE diff cadence (one snapshot+diff per tick, fanned to every connection) |
| `CTX_EVENTS_PING_INTERVAL` / `CTX_EVENTS_MAX_CONNECTIONS` | `25` (s) / `8` | hot | SSE knobs (`GET /api/events`): the `: ping` keepalive cadence — MUST stay below the fronting proxy's read timeout (nginx 60s) — and the concurrent-stream cap (429 above it). `MAX_CONNECTIONS` is parse-strict (a malformed cap aborts boot) |
| `CTX_WEBCHAT_ENABLED` / `_MAX_ITERATIONS` / `_MAX_TOKENS` / `_COMPLETION_BUDGET` / `_TOOL_RESULT_MAX_CHARS` / `_HISTORY_BUDGET_CHARS` / `_LLM_TIMEOUT` / `_CONCURRENT_TURNS` / `_SESSION_RETENTION` | `true` / `6` / `2048` / `8192` / `8000` / `60000` / `900` (s) / `1` / `0` (off) | hot | Web-chat harness (`POST /api/chat/stream`). `ENABLED` gates the endpoint (off ⇒ 404). The budgets cap one turn. `CONCURRENT_TURNS` is the per-`home_scope` semaphore (429 above it; parse-strict). `SESSION_RETENTION` takes a duration suffix (`h/d/w/m/y`); `0` keeps sessions forever |
| `LISTEN_ADDR` | `:8080` | restart | HTTP listen address; also read raw by the `-health` container healthcheck mode |
| `CTX_BOOTSTRAP_ADMIN_KEY` / `CTX_BOOTSTRAP_RUN_ID` | – | restart | **Fail-closed first-key bootstrap** (read raw, not via the settings registry). On boot, WHEN `CTX_BOOTSTRAP_ADMIN_KEY` is set AND `context_api_keys` is **empty**, ctxd mints a single server-admin key whose hash is this plaintext, labelled `e2e-bootstrap-<run-id>` (`RUN_ID` defaults to `unknown`). On a **populated** table it mints nothing and only logs — the credential is never injected into a real deployment. Solves the henhouse-egg gap on a fresh DB (no key exists yet to create the first key); primary consumer is the e2e live-tier compose stack (per-run random key). Unset in every normal deployment ⇒ no-op. The plaintext is never logged |
| `CTX_GRAPH_EXPAND_ENABLED` / `_*` | `true` | hot | Query-time Dream-graph traversal (default-on since Wave 3). Fail-open. Knobs: `_DIRECTED` / `_HOP_DEPTH` / `_SEED_COUNT` / `_SEED_SCORE_FLOOR` / `_PER_SEED_CAP` / `_MAX_INJECTED` / `_MIN_CONFIDENCE`(`_RECURRENT`) / `_BOOST_WEIGHT` / `_HUB_DAMPING` / `_WEIGHT_{TOPICAL,FACTUAL,CAUSAL,RECURRENT}` / `_NEW_PLACEMENT_FRAC` |
| `CTX_RERANK_ENABLED` / `_HOST` / `_*` | `true` | hot | Post-RRF rerank (default-on since Wave 3.5, fail-open). `_HOST` / `_MODEL` / `_API_KEY` are **Bootstrap-only since 053** (seed the `herbert-rerank` row, then inert); `_ENABLED` / `_MAX_DOCS` / `_BLEND_WEIGHT` stay live query knobs. `_HOST` empty → LLM-as-judge on the chat model; default `http://ctx-rerank:8082` → local bge-reranker-v2-m3 sidecar. `_MAX_DOCS` (default 50; CPU ≈1s/doc), `_BLEND_WEIGHT` (default 0.5; 1.0 = pure cross-encoder). See `docker-compose.yml` for the sidecar |

**Compose gap.** An env var only reaches the container if the `docker-compose.yml` `environment:` block declares it. Eighteen parsed keys are deliberately *not* declared there (`CTX_DREAM_IDLE_WAIT`, `CTX_DREAM_PARALLELISM`, the six `CTX_DREAM_BACKOFF_*`, `CTX_PROMPT_VERSION`, `CONTEXT_DB_SSLMODE`, the five `CTX_DREAM_EMBED_*`, and the three dashboard knobs `CTX_LLMLOG_MAX_LIMIT` / `CTX_EVENTS_TICK_INTERVAL` / `CTX_EVENTS_QUEUE_STATS_INTERVAL`) — setting them in `.env` alone does nothing. The boot dump makes this visible: a var that never arrived shows `"default"` as its source. To use one, add it to the compose `environment:` block (or set a `context_settings` override).

## Boot-time validation & config dump

`ctxd` parses all `CTX_*`/`CONTEXT_*` env vars through a typed registry (`internal/config`) and logs one `config: effective` record at startup: every setting with its origin (`settings` for a DB override, `env`, or `default`), secrets masked (`api_key`s render a short sha256 fingerprint so key rotation is provable from logs without leaking the value; the DB password renders presence-only).

Invalid configurations abort the boot **after logging every finding** with field + reason. Beyond the long-standing fatal parses (malformed ints, unknown timezone, missing DB password), these previously-booting-but-broken-at-runtime states are now startup errors: unknown `_PROTOCOL` values (used to silently select the Ollama wire path → 404 on llama.cpp), malformed host URLs / trailing slashes / embedded `user:pass@` credentials (use `_API_KEY` instead), `CTX_SCORE_THRESHOLD` above `CTX_CONFIDENT_THRESHOLD`, out-of-range knobs (`_BLEND_WEIGHT` outside [0,1], negative rate limits), and cross-host credential inheritance in the `CTX_DREAM_EMBED_*` fallback chain. Malformed values on tolerant knobs keep their defaults but now log a WARN instead of failing silently.

## Backups & disaster recovery

`backup.sh` archives only the pg_dumps — the sealed-secret ciphertexts are in every dump, the master key (`CTX_SECRETS_KEY`) is in **none**, by design. Disaster recovery needs both the dump and the separately-stored master key. See [security](security.md#sealed-secrets--break-glass) for master-key setup, rotation and break-glass extraction.

## Deploy & migrations

`docker compose build ctx` builds the multi-stage Go binary (incl. the embedded Svelte SPA — see [development](development.md)); `docker compose up -d ctx` starts ctx + PostgreSQL. Migrations run at boot in order. Rolling the multi-tenant line out to a running deployment (migrating the production DB from 057 across the 058–068 chain) is a separate operational step; the single-tenant default tenant keeps every path byte-identical until tenants are provisioned — see [multi-tenancy](multi-tenancy.md#self-service-onboarding-v411).
