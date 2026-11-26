# ctx_checkpoint — memory engine

[CTX](https://github.com/GottZ/ctx) is a self-hosted knowledge store with a hybrid
retrieval pipeline (semantic + fulltext + trigram RRF, temporal gravity, Dream-mode
graph expansion). This plugin registers it as a [Hermes Agent](https://github.com/NousResearch/hermes-agent)
memory provider: cross-session long-term memory without sending data to a third party.

This directory is the memory engine of the [`ctx_checkpoint`](../README.md) plugin:
the code behind recall, the `ctx_*` tools, the `MEMORY.md`/`USER.md` mirror, fact
extraction, session digests, the checkpoint chain over REST or MCP, and the CLI. It was
written by derLonius ([W4-NERF](https://github.com/W4-NERF)) as
[hermes-ctx-plugin](https://github.com/W4-NERF/hermes-ctx-plugin) from long-term use and
folded into `ctx_checkpoint` 0.6.0 (see [License & attribution](#license--attribution)).
Install, setup and the plugin-level settings are in the [plugin README](../README.md);
this page documents the engine: every setting, the tools, the CLI and the internals.

| | |
|---|---|
| **Best for** | Self-hosted, agent-shared knowledge; semantic recall across sessions; multi-agent visibility (shared scope) |
| **Requires** | A running CTX server (Go daemon + PostgreSQL) and an API key — by default reached through Hermes' MCP server entry (`mcp_servers.ctx`, key in `.env` as `CTX_API_KEY`); with `transport: rest` through `CTX_BASE_URL` / `CTX_KEY` |
| **Data storage** | Your own CTX server |
| **Dependencies** | None beyond the Python standard library |
| **License** | MPL-2.0 |

## Contents

- [Features](#features)
- [Requirements](#requirements)
- [Installation](#installation)
- [Setup](#setup)
- [Configuration](#configuration--hermes_homectxjson)
- [Agent tools](#agent-tools)
- [CLI](#cli)
- [How it works](#how-it-works)
- [Notes & pitfalls](#notes--pitfalls)
- [Development](#development)
- [License & attribution](#license--attribution)

Release history: [CHANGELOG.md](CHANGELOG.md).

## Features

- **Recall (prefetch)** — before each turn the most relevant blocks are retrieved
  (`/api/query`, retrieval only) and injected as context; falls back to `/api/search`
  while embeddings are still backfilling. Checkpoint/tool-evidence blocks are never
  injected, foreign text is labelled as untrusted.
- **Explicit tools** — the model can query, store, search and fetch blocks itself.
- **Memory mirror** — Hermes' curated `MEMORY.md` / `USER.md` writes are mirrored to CTX.
- **Transcript checkpoint chain** — before a context compaction the redacted direct
  user/assistant text is archived as a head → manifest → parts chain (retrieval-excluded,
  never prefetched), optionally fail-closed.
- **Fact extraction** — salient facts are extracted from the messages being compacted,
  stored as `learnings` blocks and deduplicated across compactions.
- **Session digests** — one intent → outcome digest per conversation segment (after each
  compaction, rolling, after idle, at session end).
- **Provenance** — every write is tagged `source:hermes`, `profile:<name>`, `hook:<origin>`.

**Deliberately not implemented:** raw turn-by-turn sync. CTX blocks should be curated
single facts (~1–1.5k chars), not chat transcripts — dumping raw turns degrades retrieval
quality ("memory drift"). `sync_turn` only counts exchanges for the digest triggers.

## Requirements

- **Hermes Agent** with memory-provider plugins. The transcript chain and fail-closed
  checkpoints need the pre-compress checkpoint API v2
  (`agent.memory_provider.PRE_COMPRESS_CHECKPOINT_API_VERSION >= 2`); profile-scoped
  secrets, context-bound threads and the recall indicator are used when the host
  provides them and degrade to plain behaviour otherwise.
- **CTX server** ([GottZ/ctx](https://github.com/GottZ/ctx)). Over the default MCP
  transport the key lives in the `mcp_servers.ctx` entry (set up by the plugin); with
  `transport: rest` the engine reads `CTX_BASE_URL`/`CTX_KEY` from the profile's `.env`.
  API keys are managed on the server (`ctx keys create <label> --home <scope>`); blocks
  are stored in the key's `home_scope` — use a key with a shared scope if other agents
  should see Hermes' blocks. `types_exclude` needs server v5.x; older servers are covered
  by a client-side filter.

## Installation and setup

`hermes plugins install ctx_checkpoint`, then the Desktop/Dashboard memory form or
`hermes memory setup ctx_checkpoint`; answer `on` to "Memory features" to enable this
engine. See the [plugin README](../README.md#install). Verify with `hermes memory status`
or `hermes ctx_checkpoint status`: on the default MCP transport both run a connection test and
show the server entry, the operating mode and the last checkpoint.

The catalog version includes this engine (since 0.6.2); the current catalog pin is
listed under [Versions](https://github.com/GottZ/ctx/blob/root/integrations/hermes/README.md#versions).
For code newer than the catalog pin, install from a ctx checkout with
`integrations/hermes/scripts/install.sh` ([integration guide](../../../README.md)).

## Configuration

All settings live under `memory.ctx_checkpoint` in Hermes' `config.yaml`, by the key names
below (the plugin's own `category`/`sensitivity`/`chunk_chars`/`budget_s`/`max_parts` map
onto `checkpoint_category`/`checkpoint_sensitivity`/`checkpoint_chunk_chars`/
`checkpoint_budget_s`/`checkpoint_max_parts`; an engine name written next to its plugin key
wins over it, in either order, and the memory form gives both its value). The keys of the Settings → Plugins form
(`budget_s`, `max_parts`, `auto_recall`, `auto_memory_mirror`, `auto_compress_extract`,
`auto_session_digest`) are read from `plugins.entries.ctx_checkpoint.settings` when set there,
over `memory.ctx_checkpoint`. A legacy `$HERMES_HOME/ctx.json` is still read with lower
priority than both, in every mode (also with `memory: false`, so a `checkpoint_mode` or
`checkpoint_*` key there applies to the checkpoint). With `memory: false` recall, capture and fact extraction are
forced off. For `transport: rest`, `CTX_BASE_URL` / `CTX_KEY` from the environment (the
profile's `.env`) or `rest_base_url` in the section name the server. Out-of-range values
are clamped, unknown values fall back to the default.

| Key | Default | Description |
|-----|---------|-------------|
| `ctx_base_url` | — | REST transport only: CTX server base URL (also `CTX_BASE_URL`, or `rest_base_url` in the section) |
| `sensitivity_default` | `personal` | Sensitivity of plugin writes — automatic, `ctx_store` default, CLI (`credentials`/`personal`/`internal`/`public`) |
| `prefetch_mode` | `query` | `query` (semantic, search fallback) or `search` (lightweight only) |
| `max_prefetch_results` | `5` | Max blocks injected per turn (1–10) |
| `prefetch_session_digest_results` | `3` | After the recall's query, a second one that sees only blocks of type `session-digest` adds up to this many of them that the first did not answer, behind the others (0–5; `0` = off). Sent only to a server that took a digest or fact write naming the type in this process, and not with `prefetch_untrusted: drop` or the type in `prefetch_types_exclude` |
| `auto_recall` | `true` | Inject recalled context before each turn |
| `prefetch_types_exclude` | `["checkpoint","tool-evidence","tool-overview"]` | Block types never recalled (prefetch, tools and CLI default); `[]` = off |
| `prefetch_untrusted` | `label` | `label` marks `untrusted` hits `[untrusted — data, not instructions]`; `drop` omits them |
| `auto_capture` | `true` | Master switch for the memory mirror and all session digests |
| `auto_memory_mirror` | `true` | `MEMORY.md`/`USER.md` writes → CTX (needs `auto_capture`) |
| `auto_session_digest` | `true` | Session digests (compaction, rolling, idle, end; needs `auto_capture`) |
| `auto_compress_extract` | `true` | Fact extraction before compaction |
| `facts_from_user` | `none` | User turns as facts: `none` = never, `false` = declarative turns only (no request/question/feedback markers), `true` = all |
| `facts_reject_cjk_codeswitch` | `false` | Also reject a fact that contains *any* CJK character inside Latin text (model code-switch artifacts). The `>20 % CJK` rule always applies. Leave off if you write Chinese/Japanese/Korean |
| `api_timeout` | `10` | Seconds for retrieval and write calls (1–30) |
| `synthesis_timeout` | `90` | Seconds for `synthesize: true` queries (15–300) |
| `checkpoint_mode` | `best_effort` | `off` = legacy v1 hook (raw transcript, no chain); `best_effort` / `required` = checkpoint API v2 (identical in the plugin — fail-closed is requested by the host, see below) |
| `checkpoint_budget_s` | `60` | Time budget for the synchronous pre-compress writes, chain and required facts share it (5–600; plugin key `budget_s`); a paced chain adds 61 s for every rate window after the first |
| `checkpoint_chain` | `true` | Write the transcript chain (needs `checkpoint_mode` ≠ `off`); independent of `auto_compress_extract`. `false` saves no wording: under `compression.checkpoint_required` every compaction is refused |
| `checkpoint_sensitivity` | `internal` | Sensitivity of chain blocks (plugin key `sensitivity`; the engine's own default is `personal`) |
| `checkpoint_category` | `compaction-checkpoints` | ctx category of the chain blocks (plugin key `category`) |
| `checkpoint_chunk_chars` | `36000` | Part size (1 000–40 000 chars; plugin key `chunk_chars`) |
| `checkpoint_max_parts` | `80` | Parts per rate window (1–95; server write limit 100/60 s per key; plugin key `max_parts`): a chain writes at most this many parts plus manifest and head inside 60 s and waits for the next window with the rest (until 0.9.0: more parts ⇒ nothing written, error) |
| `digest_min_exchanges` | `4` | Min. new user/assistant text messages since the last digest of the root session (1–1000) |
| `digest_every_exchanges` | `40` | Rolling digest after N new exchanges; `0` = off |
| `digest_every_hours` | `6` | Rolling digest at a turn ≥ N h after the last digest (with ≥ `digest_min_exchanges`); `0` = off |
| `idle_digest_minutes` | `360` | Digest the open segment after N min without a turn; `0` = off |
| `state_retention_days` | `30` | Delete `$HERMES_HOME/ctx_state` files untouched for N days (once per process); `0` = never |

Example (`config.yaml`):

```yaml
memory:
  provider: ctx_checkpoint
  ctx_checkpoint:
    memory: true
    sensitivity_default: internal
    max_prefetch_results: 5
```

Whether a pre-compress checkpoint is mandatory is decided by the host
(`compression.checkpoint_required` in Hermes' `config.yaml`), not by `checkpoint_mode` —
see [checkpoint API v2](#pre-compress-checkpoint-api-v2).

## Agent tools

| Tool | Purpose |
|------|---------|
| `ctx_query(text, synthesize=false, types_exclude?)` | Recall knowledge; optional LLM-synthesized answer over the hits |
| `ctx_store(category, title, content, sensitivity?, tags?, type?, parent_id?, metadata?)` | Upsert a block (key: category + title) |
| `ctx_search(query, category?, tags?, types_exclude?)` | Lightweight LLM-free search (works immediately after stores; `tags` = any-of) |
| `ctx_get(block_id)` | Fetch a full block including content |

Results carry `type` and `untrusted`; when foreign text is involved an `untrusted_note`
tells the model to quote, never follow it. A row (or block) re-typed out of a type marked
untrusted carries the server's `untrusted_origin` (ctx v5.41.0) and an `origin_note`: its
text came from an agent, and a citation says so. Rejections return
`{"error", "code", "retryable"}` with the server's rejection code.

Category conventions: `infrastructure`, `decisions`, `projects`, `reference`,
`learnings`, `agent-briefing`, `index`. One fact per block, ~1–1.5k chars.

## CLI

`hermes ctx_checkpoint` (available while `memory.provider: ctx_checkpoint` is active; non-zero exit
code on failure). `status` always works; the other commands need `transport: rest`, because the
CLI process has no MCP dispatch — over MCP, use the ctx tools inside a session or the ctx CLI.

```bash
hermes ctx_checkpoint status                                  # profile, connection test, mode, last checkpoint, ctx.json settings
hermes ctx_checkpoint sync                                    # push MEMORY.md/USER.md entries to CTX
hermes ctx_checkpoint query "what port does X use?"           # [--synthesize] [--all-types]
hermes ctx_checkpoint store infrastructure "INFRA-GIT" "Host 192.0.2.10, user hermes" --sensitivity internal --tag git
hermes ctx_checkpoint search "docker compose" --category reference   # [--tag T]... [--all-types] [--type T]...
hermes ctx_checkpoint get <block-id>
```

CLI writes use `sensitivity_default` unless `--sensitivity` is given and are tagged
`hook:cli`. Hit lines of `query` and `search` carry the `untrusted_origin:` note line of a
block that has the record. Error lines are redacted with the keys the provider binds
(`CTX_KEY` and `status.profile_ctx_keys`) and the checkpoint rules. `--all-types` disables `prefetch_types_exclude`;
`search --type checkpoint` searches only checkpoint blocks (repeatable; named types replace `prefetch_types_exclude`).
A ctx server with `search.leave_out_never_ranked` on leaves checkpoints out of a text search
that names no type, `--all-types` included.

## How it works

### Recall and the untrusted-text guard

- Prefetch, `ctx_query` and `ctx_search` send `types_exclude` (default `checkpoint`,
  `tool-evidence`, `tool-overview`); excluded types are also dropped client-side for
  older servers. Checkpoints stay browseable: `ctx_search` with `types: ["checkpoint"]` or
  `hermes ctx_checkpoint search --type checkpoint`. `types_exclude: []` finds them only while
  the ctx server has `search.leave_out_never_ranked` off (its default): with it on, a text
  search that names no type leaves checkpoints out.
- `search` sends `types` (0.16.0, GottZ/ctx#342), over MCP too: a ctx server with #342 takes
  every registered type there. An older one refuses a type that retrieval never ranks
  (`block type "checkpoint" is not retrieval-visible`, or before ctx v5.8.0 the argument
  itself); on exactly that refusal the MCP client searches again without `types` and keeps
  the rows of the asked types and the rows that carry no `type` (a server older than that
  field names none, so the fallback is not a strict type filter). It remembers the refusal
  for the binding, and what it refused decides the later searches: after a refused type, a
  search that names only other types still sends them and the server filters it, while a
  search that names the refused type goes without `types` first; after a refused argument
  (before ctx v5.8.0), every search with `types` goes without them first and is narrowed in
  the client, whatever types it names. A search the server later answers with `types`
  drops the memory of those types and of the argument. The fallback is best effort: the search without `types` fills its limit with every type, so rows of the
  asked type can fall behind it. It asks with `types` again under two conditions: the
  refusal was remembered from an earlier search (the search that meets the refusal first
  answers from the fallback without a second typed request), and every row of the answer
  without `types` carries a `type` (a row without one means a server older than the type
  filter, which cannot take it). Then, when an asked type has no row in that answer, or the
  narrowing dropped rows, it asks with `types` again (one more refused call on an older
  server), so a server updated meanwhile is not read through the fallback. That ask takes at
  most half of what is left of the call's timeout and is not sent below 0.5 s, so the caller
  keeps time for its next call; when it fails, the answer stays. A type the server does not
  know (`unknown block type`, whatever name it quotes) is raised, also from that ask.
  Any other error of that extra ask keeps the fallback answer; any other error of the
  search itself is raised.
- Hits with `untrusted: true` (foreign text — server V-11: `checkpoint`, `tool-evidence`,
  `tool-overview`, `insight`) are labelled in the injected context (or dropped with
  `prefetch_untrusted: drop`), and the context tells the model to treat them as data.
- A hit with the server's `untrusted_origin` record (a block re-typed out of a type marked
  untrusted, ctx v5.41.0) gets the line `untrusted_origin:<type> (agent-produced text:
  re-typed out of a type marked untrusted)` under its label — the wording of the server's
  text surfaces — and the context asks the model to say an agent produced it when citing
  it. It is kept under `prefetch_untrusted: drop`: the block was lifted into a trusted type
  on purpose. A hit without the record renders byte for byte as before.
- `queue_prefetch` pre-warms the next turn's recall in the background; a matching
  result younger than 60 s is consumed by the next prefetch.
- The host's recall indicator (`🧠 CTX — recalled N memories`) reports the number of
  injected blocks.

### Write paths and provenance

- Every write carries `source:hermes`, `profile:<name>` (`…/profiles/<name>` → `<name>`,
  otherwise `default`) and `hook:<origin>`: `memory_write`, `pre_compress`, `checkpoint`,
  `session_end`, `digest_rolling`, `digest_idle`, `tool`, `cli`. Agent tags using the
  reserved `source:`/`profile:`/`hook:` prefixes are dropped.
- A CTX upsert without `tags` resets a block's tags to `[]`, so tags are sent on every
  store. A `noop` (identical content) leaves existing tags unchanged.
- Memory mirror: `add`/`replace` upsert one block — `agent-briefing` with title
  `memory: …` (MEMORY.md) or `reference` with `user: …` (USER.md). `remove` is not
  mirrored (no delete-by-key API); a `replace` that changes the first 60 characters
  creates a new block and leaves the old one (Dream mode handles supersession).
- No mirror, fact or digest writes in `cron`, `flush` and `subagent` agent contexts. The
  transcript checkpoint chain is the exception (1.8.0): a fork's transcript is archived
  and labelled via `metadata.agent_context`, because under `compression.checkpoint_required`
  a gated chain would block every fork compaction with no path to success.
- Rejections carry the server's `code` (`reserved_category`, `reserved_type`,
  `reserved_metadata`, `unknown_type`, `parent_required`, `unknown_parent`,
  `provenance_protected`, `rate_limit`, …). Only `rate_limit`/429 is retried (once by the
  client after 1 s). `metadata.provenance` is server-owned and rejected client-side. Over
  MCP, Hermes passes a tool error's text without ctx's code: `rate_limit` is read from ctx's
  whole answer `Rate limit exceeded: max <n> … per 60 seconds`, `confirm_writes` from an
  answer starting `STAGED — NOT` — never from words an error repeats from the call (0.16.1,
  GottZ/ctx#354).
- Every automatic store is logged at INFO (hook, profile, category/title, action
  `created`/`updated`/`noop`, block id), failures at WARNING. Only the shortened title is
  logged, never content.

### Pre-compress: checkpoint API v2

- The provider declares `pre_compress_checkpoint_api_version = 2` (negotiated as
  `max(2, min(host, 2))`, port of GottZ/ctx `ctx_checkpoint`); `checkpoint_mode: off`
  declares 1 and gets the legacy raw-transcript hook.
- With v2 the host hands over its normalized evidence messages (user/assistant only, no
  compaction carriers, no tool rows); unknown keyword arguments are tolerated.
- `require_checkpoint=False` (default): the chain is written synchronously within
  `checkpoint_budget_s`, a chain failure is a WARNING and compaction continues; facts are
  stored in the background. Never raises.
- `require_checkpoint=True` (host `compression.checkpoint_required`): the chain must
  succeed within the budget, otherwise `CtxCheckpointError` — the host keeps the
  uncompressed transcript. Success returns a non-empty receipt. With `checkpoint_chain:
  false` every required checkpoint raises (facts are not a transcript checkpoint). Facts
  after a stored chain are stored within the same deadline (each store bounded by the time
  left); one that is refused or late is a WARNING, its dedupe keys stay pending in the fact
  ledger so a later compaction of the root on this profile stores it, and the receipt says how
  many were stored — the checkpoint stands (0.12.0;
  before, it failed the compaction although the chain was durable).
- The receipt (< 6000 chars) lists the chain head/manifest IDs and the transcript
  SHA-256, then the extracted facts (with block IDs in required mode). Since 0.9.0 the
  chain part ends with a read path (`_receipt_read_path`, empty in the bare engine): the
  umbrella names the session's tools there and puts the same text, with the head id
  recorded in `ctx_state/checkpoint-status.state`, into its system prompt block
  (`checkpoint.read_path()` / `checkpoint.pointer_block()`).

### Transcript checkpoint chain

Module [`checkpoint.py`](checkpoint.py) is a port of the plugin's original chain writer
(`ctx_checkpoint` 0.4.3, commit `38fdb74`) to a client interface, see
[License & attribution](#license--attribution); since 0.6.0 it is the one chain writer of
the plugin and runs over either transport.

- Per compaction: search the head by title (`types: ["checkpoint"]`) → read
  `latest_manifest_id` → write the parts (`### Message N — role`, split at message
  boundaries) → the manifest (`source_block_ids`, `parent_manifest_id`) → the head
  (upsert, `latest_manifest_id`). The head is published **last**: a failing part or
  manifest never moves it. If the server search misses the head, the manifest this
  process wrote last for the root links the chain.
- Manifest and head bodies are bit-identical to upstream; titles keep upstream's prefixes
  (`Compaction checkpoint head <root>`, `Compaction source <root> <sha16> <cpid> manifest` /
  `… after <parent id> manifest`; up to 0.9.0 without the parent): the server classifies them
  as `checkpoint` (migrations M107/M120) and the distiller parses the part format.
- A part is stored once per identical write (0.15.0, GottZ/ctx#344): title
  `Compaction source part <32 hex>`, the first 32 hex characters of the SHA-256 of its whole
  write (category, content after redaction, metadata, sensitivity, tags, type;
  `checkpoint.part_title_for`), and a header and metadata that name no root, session,
  transcript or position. The same text from another compaction, a branch or another
  conversation of the profile, with the same platform, agent context, host contract,
  redaction passes and sensitivity, meets the stored block (any of those different makes
  another block): ctx answers "No change needed" with its
  id, books no write, and the manifest lists it; a store under a part title can only meet the
  payload that title names, so a part another manifest lists is never rewritten. Scope and
  category are part of ctx's upsert identity: the same text in another scope (another tenant)
  or category is another block. The part's position is its place in the manifest's
  `source_block_ids`. Parts up to 0.14.0 (`Compaction source <root> <sha16> <cpid> <part sha16>
  part NNN of NNN`, header with root, session, digest and `Part: i/n`) are not rewritten; a
  retry of the checkpoint the head names rewrites that manifest in place (0.10.0), so a 0.15.0
  retry of a 0.14.0 checkpoint lists new-format parts in it, all stored before the manifest. Category `compaction-checkpoints`, sensitivity `checkpoint_sensitivity`.
- Only direct user/assistant text; tool rows, system prompts, `_compressed_summary`
  carriers and synthetic hand-offs are dropped. Redaction (`checkpoint.redact_text`, the same
  for every automatic write) runs three passes: the bound ctx keys (`_bound_secrets()`: the REST
  key, and in the umbrella provider the key of the `mcp_servers` entry and `CTX_API_KEY`, plus the
  keys of earlier bindings of the same instance, at most 16) are
  replaced wherever they stand; host `redact_sensitive_text(force=True,
  redact_url_credentials=True)`; the plugin's rules — the value behind a credential name,
  judged by its shape (code, types, references, placeholders and prose words stay; digits,
  phrases, auth-scheme values and host-mask remnants go), passwords in prose with a digit,
  webhook path tokens, bearer tokens. A redactor failure writes nothing. Not recognized:
  unnamed values other than the bound keys, plain-word passwords, values the name does not
  directly precede, mixed-case `Pin`, table cells, letters-only values with word shape (12+ letters
  without it are masked), card numbers and IBANs (list in `../README.md`).
- Chain linkage lives in **metadata** (`parent_manifest_id`, `source_block_ids`,
  `latest_manifest_id`), not in `parent_id` (that foreign key cascades on hard delete).
  Further metadata: upstream keys (`root_session_id`, `active_session_id`, `sha256`,
  `platform`, `agent_context`, `host_checkpoint_api`, `checkpoint_api_version`,
  `tool_index_status`, …), `fact_hashes` and `ctx_plugin` (writer, version, profile, port
  reference, redaction rules, checkpoint id) on manifest and head. A part keeps only how and
  where its text was captured (`source_kind`, `platform`, `agent_context`, the host contract,
  `tool_index_status`, `warnings`, `invalidated_by`, `ctx_plugin` with writer, profile and
  redaction passes — the version stays on manifest and head)
  plus `part_sha256`; it has no `root_session_id`, `active_session_id`, `sha256` or
  `part`/`parts` any more (0.15.0). A part's position is its place in the manifest's
  `source_block_ids`, the count is that list's length (and the head's `source_block_count`).
- Pacing (0.10.0): at most `checkpoint_max_parts` + 2 writes inside one 60 s window; a
  longer chain waits for the next window (the user is told before), and its deadline
  grows by 61 s per window after the first. A refused write (other writers on the key)
  waits until a slot is free — when the oldest write in the window ages out, else a whole
  window — at the latest until just before the deadline, and then tries once more; the
  second refusal in a row ends the chain with `CtxCheckpointError` (Hermes opens its
  per-server circuit breaker at the third error answer in a row). When no try fits before the
  deadline, the first refusal ends it (`no time left to wait`); refusals with an answer of the
  entry between them are not in a row, so such a write keeps trying until its deadline. A
  required fact (`store_paced`) is not retried: its first refusal ends the facts.
- A notice (the user's callback) can take its time: the wait after it counts from when it
  returned, and a notice that ends past the deadline stops the write there (0.18.0).
- One state per server entry (0.17.0, GottZ/ctx#299): the window's writes and the error
  answers in a row are counted per entry for every chain of the process
  (`checkpoint.EntryState`: the MCP entry by name and a digest of its connection identity — route
  and credentials, as Hermes compares them before profiles share one connection; an OAuth entry by
  its profile as well — REST by base URL and key digest). A call goes out only while the entry's
  error answers in a row plus the calls in flight stay below `MAX_RATE_REFUSALS`, a write only
  while fewer than `checkpoint_max_parts` + 2 writes are booked or in flight in the window; an
  answer ends the errors released before its call was sent. Two chains side by side share `checkpoint_max_parts` + 2 writes per window and their writes never show
  the entry more error answers in a row than one chain alone. Several Hermes processes of one
  profile share nothing.
- Served in start order (0.17.0, round 1 of #299): every run (a chain, or required facts without
  one) registers the writes it plans (`EntryState.register`) and ends the registration on every
  way out (`_ChainRun.close`: the writer for a failed chain or one without facts, the provider
  for the chain its required facts continue). A run's plan and deadline hold the windows the
  writes booked or in flight and the planned writes of the runs that started before it need
  together — of each such run the writes whose slots come before its deadline — never past the
  host's wait; held behind them, a run's deadline follows them (its last planned slot plus its
  window budget, never past the host's wait); a write goes out only while the window has a slot
  beyond those planned writes, and at the limit of calls in flight a later run's call gives way
  to a waiting call of an earlier one. A run whose turn comes after its deadline, or whose last
  planned write would although it would fit from an empty window, stops before its next write
  (`other checkpoints of this Hermes process started first …`) — judged without the required
  facts after a chain (`Reservation.optional`, 0.18.0: a fact that does not fit stops alone, the
  chain stands); one that needs more than its
  deadline even alone saves what fits and stops at its deadline as before. A run's planned
  writes hold their places until it ends, at the latest until its deadline. A planned write ctx
  confirms books nothing; a run held behind others looks again at every answer of the entry, and
  every `ENTRY_HOLD_RECHECK_S` (5 s) while none is in flight.
- Error answers: no write goes out after `MAX_RATE_REFUSALS` (2) in a row, so writes never draw a
  third. A read still goes out, alone: the head search of a chain that starts then, and the read
  a chain that wants to write sends first (`ChainWriter._probe`: a listing of its category, one
  row — `PROBE_QUERY` is empty since 0.18.0, GottZ/ctx#289: ctx answers it from the category's
  listing index in constant time, where the search for "Compaction checkpoint head" ranked every
  head). The admission that finds the limit hands out the read's ticket (`Admission.probe`), so
  one writer sends it and the others wait for its answer. Its answer ends the run; a read that
  fails there ends its chain and is the entry's third error answer in a row from this process.
  A chain gives up on a write ctx refused twice with no answer of the entry in between (any call
  of the process; the count of answers is the one `EntryState.release` returns under the lock),
  so refusals of other chains do not stop it. A failure envelope the REST client returns instead
  of raising (`client.is_failure_envelope`: `success: false`, an `error` without `success: true`,
  or a body that is no JSON) counts as an error answer; a write answered that way keeps its slot
  unless its code is `rate_limit`.
- A write the chain gives up on (abandoned, cut off) keeps its slot from that moment; ctx may book
  it until the host ends the call (through the dispatch: the entry's tool timeout), so a window
  can then hold one write more than `checkpoint_max_parts` + 2 — inside ctx's 100 per key while
  `checkpoint_max_parts` + 2 ≤ 97; a later refusal is handled as any refusal.
- Idempotent (0.10.0): the checkpoint id is `sha256(root \0 transcript digest)[:16]`, so a
  retry of the same transcript writes the same titles; parts carry no time of their own
  (`evidence_date` is on manifest and head only), so ctx answers a part that is already
  stored "identical content" and books no write — the server's confirmation of title and
  content (SHA-256). A part title names the SHA-256 of its exact content (header and text),
  so a retry that renders a part differently (another `checkpoint_chunk_chars` or byte
  budget, another active session in the header) writes new blocks and never rewrites a part
  a published manifest references. A manifest title names its parent (`… after <parent id> manifest`), so
  a manifest is never re-written under another parent; when the head already names this
  transcript's checkpoint, the manifest is re-written in place under its own parent.
- A batch without direct user/assistant text returns a no-op receipt
  (`ctx checkpoint: empty …`) instead of raising — fail-closed mode must not block a
  compaction that has nothing to archive (`ctx_checkpoint` 0.5.1 does the same).
- Switch off with `checkpoint_chain: false` (facts unaffected) or `checkpoint_mode: off`.
  Under `compression.checkpoint_required` both make Hermes refuse every compaction; the
  session start and `hermes memory status` say so.
- A head that keeps the tag `checkpoint-head` but carries no valid `latest_manifest_id`
  (its metadata was replaced outside the plugin; an update replaces the whole object) is
  logged and reported to the user, and the new chain starts without a parent (or the
  process' cached last manifest) with its head published over the broken one (`lookup=
  head-invalid`); the earlier chain's blocks stay. Before 0.12.0 it blocked every later
  checkpoint of that root.
- A head whose tags were replaced outside the plugin (no `checkpoint-head` tag) is found by
  a second search under the head title without the tag, made only when the tagged search
  finds nothing (the first checkpoint of every root costs one more search). With a valid
  `latest_manifest_id` the chain continues from it and the new head carries the tag again
  (`lookup=untagged`, a WARNING line); without one it is handled as above. Before 0.16.2 it
  was missed: the chain started without a parent and the new head replaced it silently.
- The chain never runs past Hermes' wait for the compaction (`resolve_context_compression_
  timeouts` of the loading Hermes, less 10 s): budget, paced windows, the refusal window and
  the required facts end there, and the error names the wait. The receipt's `windows` counts the
  windows the booked writes entered (a retry whose parts ctx confirms reports `windows=1`); the
  deadline holds every window of the plan, because a confirmed part still costs one call.
- Log: `CTX checkpoint chain stored (… head= manifest= parent= parts= writes= sha= lookup= …)`.

### Fact selection and dedupe

- Candidates are scanned from the **end** of the batch (closing answers carry results; the
  transcript head is carried over between compactions), max 5, stored chronologically as
  `learnings` blocks titled `fact: …`. Assistant turns count only when they close a turn
  or are longer than 600 chars; user turns follow `facts_from_user`.
- Noise filters drop compaction/harness wrappers (`[CONTEXT COMPACTION`,
  `[Message from agent`, `[IMPORTANT:`, `[ASYNC DELEGATION`, …), stream/harness warnings,
  bot greetings, quoted mails, slash commands, attachment-only turns, leaked reasoning
  and plan narration, step-progress notices, mostly-CJK text (>20 %; any CJK inside
  Latin text only with `facts_reject_cjk_codeswitch`) and short acknowledgements,
  announcements and transient status (≤ 600 chars).
- **The filters are language-specific.** They were built from German/English replays
  (narration verbs, greetings, request markers, quoted-mail attribution lines) and know
  nothing about other languages — facts in a third language pass the filters unexamined,
  German/English narration is what gets caught. Treat the precision figure below as a
  German/English number.
- Dedupe per root session: keys are SHA-256 prefixes of the normalized text **and** of
  the block title (CTX upserts by category + title). They live in
  `$HERMES_HOME/ctx_state/<root>.json` (max 500) and cumulatively (max 200) in the chain
  head/manifest `fact_hashes`, so a restarted process or another host skips facts the
  chain already lists. Failed stores release their keys for the next compaction.
- Replay of 191 real compactions (Sep/Oct 2026): duplicate titles per chain 227 → 0;
  precision (blind-labelled, held-out n = 60) 33 % → 72 %. This is a heuristic ceiling —
  the server-side distiller (which reads the checkpoint chain) is the long-term path.

### The block type of digests and facts (0.19.0)

- Facts and session digests are stored with `type: session-digest` (GottZ/ctx#289): a
  builtin type of ctx from migration 196 on, damped in recall, out of guard, dream,
  digest and overview, and marked untrusted — every read surface frames such a block
  (`"untrusted": true`), and the recall labels it as untrusted text. Before 0.19.0 they
  were stored without a type: ctx filed a digest as `audit-trail` and a fact mostly as
  `knowledge`, both unframed. Blocks stored before keep their type; the curated memory
  mirror (`MEMORY.md`/`USER.md`) is stored without a type, as before.
- **A server without the type** (before migration 196) refuses such a write before it
  stores or books anything: REST 422 `unknown_type`, over MCP the tool error
  `type: unknown block type "session-digest" (see manage type-list)` (the whole text is
  read, `mcp_answers.json` section `session_digest`). The write is sent once more without
  the type, and no write of the process names it to that server entry for an hour
  (`TYPE_REFUSAL_TTL_S`); then one write asks again: at most one refused write per process,
  entry and hour (`checkpoint.store_typed`, the entry's state of
  `EntryState.type_to_send`). Only that refusal is a verdict: another error, a listing
  that fails before the ask and a write that waited out the ask leave the question open
  for the next write. Each of these ways to a write without the type logs one WARNING per
  process and entry. While the first write that names it is out, the others of
  the entry wait for its answer (up to `TYPE_ASK_WAIT_S`, a required fact until its
  deadline) and then name the type or not. A required fact refused so is sent again in the
  same checkpoint run, admitted as a call of its own: the refusal is one error answer of
  the entry (Hermes counts it toward its breaker), ctx books no write for it and the entry
  holds no slot for it; at two error answers in a row the write after it waits for a read
  alone, as every write does. Digests and the facts stored outside a required checkpoint
  are not admitted at the entry (as before): over MCP the write of theirs that asks is
  preceded by a listing of its category (one row), whose answer ends Hermes' run of error
  answers — the agent's tools and recall can leave two the plugin does not see, and the
  refusal must not be the third — and their refusal is followed at once by the write
  without the type; a listing that fails leaves the question open and the write goes
  without the type. After the server is updated, a running process names the type again
  at the latest an hour after the refusal.
- `prefetch_untrusted: drop` drops untrusted sources from the recall — with 0.19.0 that
  includes digests and facts stored as `session-digest`.
- **The recall's second query.** The type is damped in recall (0.8), so a digest or a fact
  that alone answers a question can rank below the five blocks the recall renders. After
  the recall's query a second, small one that sees only `session-digest` blocks (over MCP
  the `query` tool's `types`; over REST `/api/query` with the other type names of the
  server's `type-list`, read once per client, as `types_exclude`) adds up to
  `prefetch_session_digest_results` (default 3) of its blocks that the first did not
  answer, behind the others, each labelled as untrusted text; the first five are never
  changed. It is sent only to a server entry that took a write naming the type in this
  process (a server without the type refuses it — over MCP an error answer Hermes counts),
  within its own bound (2 s) and only while what Hermes' wait for the prefetch (8 s)
  leaves after the first query covers its worst case (0.75 s over REST; 2.5 s over MCP,
  where a call through Hermes' `call_mcp` takes at least 1 s and is waited for 1 s past
  its bound); a failure or timeout keeps the five, logs at DEBUG and counts toward nothing
  of the plugin's entry state (over MCP Hermes counts it toward its breaker, as a failed
  recall). A block the first answer holds but the recall does not show
  (`max_prefetch_results` below five) can come from the second query.
  Measured on a test rig (a hashing embedder, three corpora of 432–656 blocks): no block
  of the first five changed, every added block untrusted, the question's embedding
  cached for the second query, 22 ms median and 33 ms p95 for it.

### Session digests

- The host calls `on_session_end` at real session end **and** after every compaction,
  on websocket orphan reap and gateway soft-evict. Each digest covers only user/assistant
  text messages not covered by an earlier digest of the same root session ⇒ at most one
  digest per segment; repeated hooks with the same history write nothing.
- Triggers: `compaction` / `end` (`hook:session_end`), `rolling` (`hook:digest_rolling`,
  `digest_every_exchanges` / `digest_every_hours`, checked per turn) and `idle`
  (`hook:digest_idle`, timer re-armed by every turn, cancelled by session end, switch and
  shutdown). All need `auto_capture` + `auto_session_digest` and at least
  `digest_min_exchanges` new exchanges.
- Title `Session digest <root8> seg NNN <YYYY-MM-DD>` in `agent-briefing`, unique per
  root and segment, type `session-digest` (see above). Content: intent (first new user turn), last request, outcome (last
  assistant turn); compaction digests reference the chain manifest. Metadata:
  `root_session_id`, `session_id`, `segment`, `trigger`, `exchanges`, `manifest_id`.
- A failed store releases the exchanges; the next trigger retries under a new segment
  number. Digests are heuristic: `/api/query` synthesizes over stored blocks, it does not
  summarize provided text.

### Root session and local state

- The chain, the fact ledger and digest segments belong to the **compression-lineage
  root** of a session: `initialize` and resume/branch switches resolve it read-only from
  `$HERMES_HOME/state.db` (same walk as the host's
  `SessionDB.get_compression_lineage()[0]`; branch/delegate/tool forks are their own
  root). Compaction switches keep the root, `/new` starts a new one, `/undo` keeps
  everything; without a DB row the current root is kept.
- Local state lives in `$HERMES_HOME/ctx_state/` (`<root>.json` fact ledger,
  `<root>.digest.json` digest segments; atomic writes, mode 0600). Files untouched for
  `state_retention_days` are pruned once per process. Deleting them is safe — only
  dedupe gets weaker.

### Profiles and multiplexed gateways

Every binding (client, key, `HERMES_HOME`, profile) is made in `initialize()` and kept on
the instance. `CTX_KEY` / `CTX_BASE_URL` are read through the host's profile secret scope
(`agent.secret_scope.get_secret`), and every background thread or timer runs under the
spawner's context (`spawn_context_thread` / `ctx_bound`), so one gateway process can
serve several profiles without mixing their stores.

## Notes & pitfalls

- **Startup validation (`transport: rest`):** `initialize` calls `/api/whoami`. If CTX is
  unreachable at that moment the provider stays disabled for the session (restart Hermes once
  CTX is back). Over MCP the provider makes no call at startup (Hermes connects the server
  entry itself); setup and the status run a connection test instead.
  Every other failure degrades gracefully (log + no-op) — a down CTX never crashes the agent.
- **Embedding backfill:** right after stores, `/api/query` may time out until embeddings
  are ready — `/api/search` and `/api/store` work immediately; recall falls back to
  search automatically.
- **API field name:** `/api/query` expects the body field `query`, not `text`.
- **Sensitivity is fail-closed** on the server (`credentials` by default). Automatic writes
  use `sensitivity_default`; downgrades require server-side confirmation.
- **Rate limit:** the server allows 100 writes per 60 s per key; a large checkpoint plus
  facts and mirror writes share that budget. The chain paces itself to `checkpoint_max_parts`
  + 2 writes per window, and since 0.17.0 the required facts after it are stored in the chain's
  run: planned with it, paced against the same window and its count of error answers (a refused
  fact is reported and left for a later compaction; the checkpoint stands). Best-effort facts,
  the mirror and digests are not paced.

## Development

The tests use a fake client (no network) but import the host contract (`agent.*`,
`tools.*`, `hermes_cli.*`, `hermes_constants`, `hermes_state`, `utils`) from a Hermes Agent
checkout — `HERMES_REPO` (default `~/.hermes/hermes-agent`). Run them with a Python
environment that has pytest and the host's dependencies, from the ctx checkout. The MCP
answers they parse come from `tests/mcp_answers.json`, the fixture ctx's own handler test
(`go/internal/handler/mcp_checkpoint_answers_integration_test.go`) pins its answers against:

```bash
HERMES_REPO=/path/to/hermes-agent python -m pytest -p no:cacheprovider \
    integrations/hermes/plugin/ctx_checkpoint/memory/tests
```

The ctx repository's CI runs these tests, the umbrella suite, ruff and `hermes plugins
validate` in `.github/workflows/hermes-ctx-checkpoint.yml` against a pinned hermes-agent
checkout; run them locally before a change lands. Lint and format (configuration in `ruff.toml` inside the
plugin directory — deliberately no `pyproject.toml`, see CHANGELOG 1.7.1):

```bash
cd integrations/hermes/plugin/ctx_checkpoint && ruff check . && ruff format --check .
```

Layout (`integrations/hermes/plugin/ctx_checkpoint/memory/`):

```
__init__.py           engine package (the umbrella in ../__init__.py registers with Hermes)
provider.py           CtxMemoryProvider: lifecycle, recall, hooks, digests, seams for the umbrella
tools.py              ctx_query / ctx_store / ctx_search / ctx_get
client.py             stdlib HTTP client for the CTX REST API (transport: rest)
mcp_client.py         the same surface over Hermes' MCP dispatch (default transport)
config.py             settings, umbrella mapping, secrets, tags, fact heuristics, fact ledger
checkpoint.py         transcript checkpoint chain (ChainWriter)
digest.py             digest state, lineage root, state pruning
cli.py                hermes ctx_checkpoint … commands (wrapped by ../cli.py)
CHANGELOG.md          engine history (1.x by W4-NERF; 0.6.0+ with the plugin)
tests/                pytest suite (conftest loads the engine as package ctx_plugin)
```

## License & attribution

[Mozilla Public License 2.0](../../../../../LICENSE), like the rest of ctx.

This engine was written by **derLonius ([W4-NERF](https://github.com/W4-NERF))** as
[hermes-ctx-plugin](https://github.com/W4-NERF/hermes-ctx-plugin) (releases 1.0–1.7.1,
2026-09/10, from long-term production use) and offered to the ctx project for porting.
It was imported verbatim at 1.7.1 (upstream commit `fbd210a`) in GottZ/ctx commit
`e9ae6010`, which carries his authorship, and folded into the `ctx_checkpoint` plugin
as its memory engine in 0.6.0; everything since is maintained here, with changes
recorded in [CHANGELOG.md](CHANGELOG.md). His review notes and the
reciprocal review are tracked in
[W4-NERF/hermes-ctx-plugin#3](https://github.com/W4-NERF/hermes-ctx-plugin/issues/3).

[`checkpoint.py`](checkpoint.py) and
[`tests/test_p3_checkpoint_chain.py`](tests/test_p3_checkpoint_chain.py) are modified
versions of the plugin's original chain writer (`ctx_checkpoint` 0.4.3, commit `38fdb74`,
© Jan-Stefan Janetzky, MPL-2.0); their file headers keep that notice. Changes vs. that
origin: REST `CtxClient` instead of MCP dispatch, explicit `type=checkpoint`, time
budget, rate-limit backoff, part cap, configurable sensitivity, own metadata key
`ctx_plugin`, auth-header redaction, no-op receipt on empty evidence. The 1.7.1 write
gate on cron/flush/subagent chain writes was lifted in 1.8.0 (see CHANGELOG).
