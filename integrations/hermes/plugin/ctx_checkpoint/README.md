# ctx_checkpoint — the ctx memory provider for Hermes

[Hermes](https://github.com/NousResearch/hermes-agent) compacts long conversations
with a lossy LLM summary. `ctx_checkpoint` keeps what the summary discards: before
each compaction it archives the direct user/assistant transcript into
[ctx](https://github.com/GottZ/ctx) — deterministically, with no LLM in the critical
path — and hands the block IDs to Hermes for the summary. With `memory: true` the
same plugin is also Hermes' long-term memory over ctx: recall before each turn,
explicit `ctx_*` tools, a mirror of the built-in `MEMORY.md`/`USER.md`, fact
extraction from compacted turns and per-segment session digests. One plugin, one
provider name, one settings section.

```
Compaction checkpoint head <root-session>     ← stable title, upserted per session
  └── manifest (per checkpoint, parent-linked to the previous manifest)
        └── source parts 001..NNN (redacted direct transcript, ≤40k chars each)
```

Everything talks to ctx through Hermes' own MCP dispatch to the `mcp_servers.ctx`
entry — the plugin holds no credentials. `transport: rest` switches to a REST client
with its own key (see below for what that buys).

## Install

```bash
hermes plugins install ctx_checkpoint
```

Answer **y** when the installer asks to use it as the memory provider. Then set it
up — no YAML editing needed:

- **Hermes Desktop / Dashboard:** open the memory settings, pick
  `ctx_checkpoint` and fill in the form.
- **Terminal:** `hermes memory setup ctx_checkpoint`

Setup asks for your ctx server URL and API key, and whether to turn the memory
features on. Everything else has a working default. The key goes into Hermes'
`.env` as `CTX_API_KEY`; `config.yaml` only references it. Setting up a ctx server
is covered in the [ctx README](https://github.com/GottZ/ctx#readme). From a ctx
checkout the directory can also be installed with
`integrations/hermes/scripts/install.sh`.

The plugin talks to ctx through Hermes' own MCP dispatch and holds no
credentials of its own. Setup writes the same configuration you could also
write by hand:

```yaml
mcp_servers:
  ctx:
    url: https://your-ctx-host.example/mcp
    headers:
      Authorization: Bearer ${CTX_API_KEY}

memory:
  provider: ctx_checkpoint
  ctx_checkpoint:
    mcp_server: ctx                  # name of the entry above (default: ctx)
    memory: false                    # true = recall, tools, mirror, facts, digests
    category: compaction-checkpoints # ctx category for checkpoint blocks
    chunk_chars: 36000               # source part size, clamped to 1k..40k; a part also stays under ctx's 50 KB byte cap
    sensitivity: internal            # public | internal | personal | credentials
    budget_s: 60                     # wall-clock budget per checkpoint, 5..600 s
    max_parts: 80                    # part cap per checkpoint, 1..95 (ctx writes 100/60 s per key)
```

Every key under `memory.ctx_checkpoint` is optional; the values shown are the
defaults. `budget_s` and `max_parts` are not asked during setup; set them in
`config.yaml` when needed (setup keeps them). An existing `mcp_servers.ctx` entry is
kept: setup updates its URL and re-enables it, but asks for a key only when it
manages that entry's authentication itself (no auth yet, or `Bearer ${CTX_API_KEY}`
from an earlier setup). An entry with its own `headers` or `auth`, or a stdio
`command` entry, keeps them untouched. Setup always targets the entry named by
`mcp_server`; change that name in `config.yaml` if your ctx entry is called
differently.

## Memory features (`memory: true`)

| Feature | What it does | Switch |
|---|---|---|
| Recall | before each turn the most relevant blocks are injected as `<ctx-context>` (untrusted text labelled); the host's indicator shows `🧠 CTX — recalled N memories` | `auto_recall`, `max_prefetch_results`, `prefetch_types_exclude`, `prefetch_untrusted` |
| Agent tools | `ctx_query(text, synthesize?, types_exclude?)`, `ctx_store(category, title, content, …)`, `ctx_search(query, category?, tags?, …)`, `ctx_get(block_id)` | — |
| Memory mirror | Hermes' curated `MEMORY.md`/`USER.md` writes become `agent-briefing` / `reference` blocks | `auto_memory_mirror` |
| Fact extraction | up to 5 salient closing statements of the compacted turns become `learnings` blocks, deduplicated across compactions | `auto_compress_extract`, `facts_from_user`, `facts_reject_cjk_codeswitch` |
| Session digests | one intent → outcome digest per segment (after a compaction, rolling, after idle, at session end) | `auto_session_digest`, `digest_*`, `idle_digest_minutes` |
| Provenance | every write is tagged `source:hermes`, `profile:<name>`, `hook:<origin>` | — |
| Skill | `ctx_checkpoint:ctx-memory` tells the model how to use the store well (query as a question, verify before acting, one durable fact per block) | registered with the provider |
| CLI | `hermes ctx_checkpoint status\|sync\|query\|store\|search\|get` | needs `transport: rest` for everything but `status` |

All of these keys go under `memory.ctx_checkpoint` by their engine names; the full
table with defaults and ranges is in the [engine README](memory/README.md). With
`memory: false` (the default) none of this runs and the plugin behaves exactly like
the checkpoint-only versions before 0.6.0. Mirror, facts and digests are skipped in
`cron`, `flush` and `subagent` agent contexts; the checkpoint chain is written in
every context (labelled via `metadata.agent_context`).

**Transport.** Over the default MCP dispatch, retrieval-only recall uses the ctx
`search` tool (lexical hybrid: fulltext + trigram) because the MCP `query` tool
always runs the LLM synthesis. `transport: rest` (plus `CTX_BASE_URL`/`CTX_KEY` in
the profile's `.env`, or `rest_base_url` in the section) uses `/api/query` with
`synthesize: false` for recall — the semantic hybrid without the LLM step — and
enables the CLI's read/write commands. A server-side `synthesize` flag on the MCP
query tool is the planned way to close that gap.

**Two tool surfaces.** With `memory: true` the model sees this plugin's `ctx_*`
tools next to the `mcp__ctx__*` tools of the same server entry. Nothing breaks;
the plugin's tools add provenance tags and the untrusted labelling, so prefer them
for explicit stores, or disable the MCP entry's tools if you want a single surface.

## Long contexts (1M)

The defaults work unchanged for 1M-token models. Compaction thresholds stay
Hermes' own: compression at 50 % of the window on models with 512K or more,
75 % below. A compaction takes about as long at 1M as at 272K (around a
minute with gpt-6-sol), because Hermes caps the summary at 10,000 tokens and
writing it is what takes the time. At 1M that summary is far lossier than at
272K — the checkpoint keeps the exact wording of what it dropped.

Keep `compression.threshold` below 0.85. At 0.85 the gateway's session
hygiene, which waits only 10 seconds before a turn continues, compacts before
the compressor does. The plugin logs a warning when the threshold is that high.

## Two operating modes

| Mode | Host | Guarantee |
|------|------|-----------|
| **best-effort** (default) | any Hermes ≥ 0.20.4 | checkpoint is attempted before compaction; a failure is logged (WARNING) and compaction proceeds |
| **fail-closed** | Hermes ≥ 0.21.1 (carries the checkpoint contract, upstream PR #94639) | with `compression.checkpoint_required: true`, compaction is **blocked** unless the checkpoint is confirmed durable; the uncompressed transcript is preserved |

Fail-closed means accepting a session halt (`BLOCKED_MISSING_PREREQUISITE`) while
ctx is unreachable, instead of an unarchived compaction. The halt also triggers
when a checkpoint would need more than `max_parts` source parts or does not
finish inside `budget_s` (a rate-limited store is retried after 2/4/8 s while the
budget allows; since 0.6.1 every single call over MCP is bounded by the remaining
budget too, so a hanging ctx no longer holds the compaction for Hermes' 300 s MCP
timeout). A ctx key with `confirm_writes` is refused with that reason: ctx stages
its writes instead of storing them, and the plugin never confirms them on your
behalf. A batch with no direct user/assistant text — only tool rows or
compaction envelopes — has nothing to archive and never blocks: the provider
returns an empty receipt and the compaction proceeds. The plugin negotiates the
contract version with the loading host at runtime; details, host probes and the
operator warnings are in the [full integration guide](../../README.md).

## Recall of a checkpoint

```
ctx search compaction-checkpoints query:"Compaction checkpoint head <root-session>"
ctx get <head-id>        # → latest_manifest_id
ctx get <manifest-id>    # → source part IDs, parent manifest chain
```

## What it does on your machine

- **Network:** only the MCP calls Hermes makes to the ctx server you configured
  (`search`/`get` to find the session's checkpoint head and for recall, `store` for
  the blocks, `query` for
  `ctx_query(synthesize=true)`). With `transport: rest`, HTTPS calls of the
  plugin's own stdlib client to `CTX_BASE_URL` instead. No third-party services,
  no telemetry.
- **Data sent:** direct user/assistant text of the turns about to be compacted;
  with memory features on also the mirrored `MEMORY.md`/`USER.md` entries, the
  extracted facts, the digests and what the model stores explicitly. Tool
  results, system prompts and synthetic compaction envelopes are excluded.
  Every automatic write is redacted twice before anything leaves the process:
  Hermes' `redact_sensitive_text` plus a conservative key/value, auth-header and
  bearer-token pass; a failing redactor skips the write.
- **Reads:** its own section of the Hermes config (`memory.ctx_checkpoint`), the
  named `mcp_servers` entry, `compression.threshold` (for the warning above), the
  transcript Hermes passes to `on_pre_compress`, and with memory features on the
  profile's `state.db` (read-only, to find a resumed session's root) and
  `$HERMES_HOME/ctx.json` (legacy engine settings, lower priority than the
  section). Nothing outside that.
- **Writes, only when you run setup:** `memory.ctx_checkpoint`, the named
  `mcp_servers` entry and — in terminal setup — `memory.provider` in
  `config.yaml`, merged into the file so other sections stay as they are; the
  API key goes to `.env` as `CTX_API_KEY`. It never writes compaction settings.
  With memory features on, small state files under `$HERMES_HOME/ctx_state/`
  (fact dedupe keys and digest segments per root session, mode 0600, pruned
  after `state_retention_days`).
- **No** shell commands, background network beyond the above, stored credentials
  or self-updating code.
- **Core modules used:** `agent.memory_provider`, `agent.redact` (redaction),
  `agent.secret_scope` (profile-scoped `.env` reads, REST transport),
  `tools.registry` (MCP dispatch and the tool error envelope) and
  `hermes_cli.config` (provider config) — imported because the memory-provider
  surface has no public seam for these yet.

## Tests

Two suites, run separately (both use fakes, no live ctx; the host contract comes
from a Hermes checkout):

```bash
# umbrella: checkpoint contract, negotiation, setup surfaces, MCP transport (84)
PYTHONPATH=/path/to/hermes-agent:integrations/hermes/plugin python -m pytest \
    integrations/hermes/plugin/ctx_checkpoint/tests
# engine: recall, tools, mirror, facts, digests, chain over REST, CLI, security sweep (288)
HERMES_REPO=/path/to/hermes-agent python -m pytest \
    integrations/hermes/plugin/ctx_checkpoint/memory/tests
# lint + format + bandit rules (ruff.toml in this directory)
cd integrations/hermes/plugin/ctx_checkpoint && ruff check . && ruff format --check .
```

The ctx repository's CI does not run these suites (it has no Hermes checkout); run
them locally before a change lands.

## Layout

```
__init__.py          umbrella provider: name, settings section, transport, setup surfaces
cli.py               hermes ctx_checkpoint … (delegates to memory/cli.py)
plugin.yaml, VERSION manifest and version (one for the whole plugin)
skills/ctx-memory/   the bundled skill (registered as ctx_checkpoint:ctx-memory)
memory/              the engine: provider, tools, client (REST), mcp_client, config,
                     checkpoint (chain writer), digest, cli — README + CHANGELOG inside
tests/               umbrella suite
memory/tests/        engine suite (loads the engine as package ctx_plugin)
```

## License and attribution

MPL-2.0, part of [ctx](https://github.com/GottZ/ctx). The memory engine
(`memory/`) was written by derLonius ([W4-NERF](https://github.com/W4-NERF)) as
[hermes-ctx-plugin](https://github.com/W4-NERF/hermes-ctx-plugin) from long-term
production use, imported verbatim at 1.7.1 under his authorship (commit
`e9ae6010`) and maintained here since; its `checkpoint.py` is in turn a port of
this plugin's chain writer and keeps that notice. Details in the
[engine README](memory/README.md#license--attribution).
