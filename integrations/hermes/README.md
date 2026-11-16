# ctx_checkpoint — durable pre-compaction checkpoints for Hermes

[Hermes](https://github.com/NousResearch/hermes-agent) compacts long
conversations with a lossy LLM summary. `ctx_checkpoint` archives the direct
user/assistant transcript into [ctx](https://github.com/GottZ/ctx) **before**
each compaction, deterministically and without any LLM in the critical path —
so nothing the compaction discards is ever unrecoverable.

Every checkpoint is a chain of ctx blocks:

```
Compaction checkpoint head <root-session>     ← stable title, upserted per session
  └── manifest (per checkpoint, parent-linked to the previous manifest)
        └── source parts 001..NNN (redacted direct transcript, ≤40k chars each)
```

The provider returns the head/manifest IDs to Hermes, which carries them
into the compaction summary — a later session (or a human) can walk
head → manifest → parts to recover exact wording and provenance. Before anything
leaves the process the ctx keys of the profile are replaced wherever they stand (on
the default MCP transport the key the `mcp_servers` entry signs in with, since
0.11.0), Hermes' own `redact_sensitive_text` runs in its strict mode, and the
plugin's rules mask values behind credential names, passwords in prose and webhook
tokens while code and prose behind those names stay as written; the limits
(unnamed values, dictionary-word passwords, card numbers, IBANs) are listed in the
plugin README. A failing redactor aborts the checkpoint instead of storing
unredacted text.

**Listed in the Hermes plugin catalog** as
[`ctx_checkpoint`](https://hermes-agent.nousresearch.com/docs/plugins/ctx_checkpoint)
(community tier, category Memory, since 2026-10-25):

```bash
hermes plugins install ctx_checkpoint
```

The catalog page shows the plugin's own short README and links here as the
entry's documentation. This page is the full integration guide: operating
modes, the host contract, host probes, operator warnings and the recall path.

## One plugin, two layers

`ctx_checkpoint` is the one ctx memory provider for Hermes. Its checkpoint layer is
always on; its memory layer (`memory: true` in `memory.ctx_checkpoint`) adds recall
before each turn, the `ctx_*` agent tools, a mirror of Hermes' built-in
`MEMORY.md`/`USER.md`, fact extraction from compacted turns, per-segment session
digests, the `hermes ctx_checkpoint` CLI and the bundled `ctx-memory` skill.

| | checkpoint layer (always) | memory layer (`memory: true`) |
|---|---|---|
| Purpose | archive the direct transcript before every compaction; block the compaction under `compression.checkpoint_required` until the checkpoint is durable | let Hermes *use* the store: recall, explicit tools, mirrored built-in memory, facts, digests |
| Transport | Hermes' MCP dispatch to `mcp_servers.<mcp_server>` by default, no credentials in the plugin; with `transport: rest` the chain goes over REST too | the same transport; `transport: rest` (own `CTX_KEY`) adds semantic `/api/query` recall and the CLI's read/write commands |
| Writes per compaction | parts + manifest + head | plus up to 5 `learnings` facts and a segment digest |
| Agent contexts | chain written in every context, labelled via `metadata.agent_context` | mirror, facts, digests skipped in `cron`/`flush`/`subagent` forks |
| Setup | Desktop/Dashboard memory form or `hermes memory setup ctx_checkpoint` | the same form; one `memory: on/off` choice |

The memory layer's engine (`plugin/ctx_checkpoint/memory/`) was contributed by
derLonius (W4-NERF) as [hermes-ctx-plugin](https://github.com/W4-NERF/hermes-ctx-plugin)
and is maintained here since ctx v5.29.0 (plugin 0.6.0); v5.28.0 shipped it for a day
as a separate `ctx` plugin before it was folded in. Details, every setting and the
security notes: [plugin README](plugin/ctx_checkpoint/README.md),
[engine README](plugin/ctx_checkpoint/memory/README.md),
[engine CHANGELOG](plugin/ctx_checkpoint/memory/CHANGELOG.md). The rest of this page
is about the checkpoint layer.

## Two operating modes

| Mode | Host | Guarantee |
|------|------|-----------|
| **best-effort** (start here; Hermes' default) | any Hermes the plugin installs on | the resident agent's compaction attempts a checkpoint first; a failure is logged, recorded for `hermes memory status` and reported to the session where Hermes offers a warning channel, and compaction proceeds (historical hook semantics, contract version 1). Gateway session hygiene, `/compress` in a gateway channel, `delegate_task` sub-agents and the curator build their compacting agent without a memory provider and compact without a checkpoint |
| fail-closed | Hermes carrying the checkpoint contract (version 2; a version-3 host with the `tool_evidence` channel is detected at runtime, see [Runtime contract negotiation](#runtime-contract-negotiation)) | compaction is **blocked** unless a checkpoint-capable provider confirms the durable checkpoint; the uncompressed transcript is preserved |

The plugin is a plain Hermes **user plugin** — it lives in
`$HERMES_HOME/plugins/ctx_checkpoint/`, whether the catalog or the installer
put it there, and works in best-effort mode on a completely stock Hermes.
Plugin 0.12.0 declares `requires_hermes: ">=0.23.1"`, the first Hermes whose
`delegate_task` children inherit the provider under the fail-closed gate; the
catalog pin (0.6.2) still declares ≥ 0.20.4 (since upstream v2026.8.18,
provider text is forwarded into the compaction summary).

The fail-closed gate is what makes it a real guarantee: a memory system you
only *hope* ran is not a memory system. The host-side contract is **upstream**
— issue
[NousResearch/hermes-agent#93986](https://github.com/NousResearch/hermes-agent/issues/93986)
→ PR
[#93996](https://github.com/NousResearch/hermes-agent/pull/93996)
→ merged 2026-08-25 as
[#94639](https://github.com/NousResearch/hermes-agent/pull/94639)
(tip commit `1ee524f77d1c84a6ee1ae58341e633d8a9b6ef1d`). Fail-closed mode
needs no patches on a host that carries the merge.

Upstream numbered the contract while merging: **v1 is the implicit historical
best-effort behaviour every provider is already on, v2 is the opt-in
fail-closed checkpoint contract.** It stays opt-in and provider-agnostic:

- `PRE_COMPRESS_CHECKPOINT_API_VERSION = 2` in `agent/memory_provider.py`;
  providers opt in via `MemoryProvider.pre_compress_checkpoint_api_version`
  (class default stays `1`; this plugin serves it as a property negotiated
  against the host on every read — `max(2, min(host, plugin))`)
- `MemoryManager.supports_pre_compress_checkpoint()` + strict
  `on_pre_compress(require_checkpoint=True)` (failures propagate)
- `compression.checkpoint_required` config gate (default `false`) — fails
  closed with `BLOCKED_MISSING_PREREQUISITE` before any lossy rewrite,
  including the gateway hygiene/auto-compact paths and post-turn
  micro-compaction (`agent/turn_finalizer.py`)
- a persistent `_compressed_summary` message marker so derivative summaries
  stay excluded from checkpoints across process restarts
- **Plugin context engines — follow-up, not yet upstream.** On `main` at/after
  `1ee524f77d` the gate binds native compaction, micro-compaction and the
  codex app-server mode. An engine that compacts *outside* `compress()` — from
  `on_turn_complete()` or from a scheduler of its own — is **not yet** bound
  there: `compress()` is the one compaction verb the host can checkpoint, so
  such an engine never reaches the gate at all. The follow-up (branch
  `feat/context-engine-compaction-authority`, open upstream as
  [#94983](https://github.com/NousResearch/hermes-agent/pull/94983)) closes it with
  `ContextEngine.compacts_outside_compress` plus the
  `engine_compacts_outside_compress()` resolver in `agent/context_engine.py`,
  an init refuse (`BLOCKED_MISSING_PREREQUISITE`) in `agent/agent_init.py`,
  per-turn withholding of `on_turn_complete` in `agent/conversation_loop.py`,
  and suppression of the proactive tool-result prune there — each suppression
  counted on `agent._checkpoint_gate_suppression_count`

Everything is default-off: an untouched config behaves exactly like stock
Hermes, and the contract without this plugin behaves like stock Hermes.

## Install (plugin, both modes)

**From the Hermes plugin catalog** (recommended) — entry
[`ctx_checkpoint`](https://hermes-agent.nousresearch.com/docs/plugins/ctx_checkpoint),
added upstream in
[NousResearch/hermes-agent#144212](https://github.com/NousResearch/hermes-agent/pull/144212)
as `plugin-catalog/ctx_checkpoint.yaml`:

```bash
hermes plugins install ctx_checkpoint   # answer y: use it as the memory provider
hermes memory setup ctx_checkpoint      # asks for ctx URL and API key
hermes plugins update ctx_checkpoint    # later: follow the current catalog pin
```

Setup (plugin 0.5.0 and later) needs no YAML editing. Since 0.8.0 Hermes
Desktop shows the memory form under Settings → Memory (the declared surface:
`config_schema.py` with `storage=provider`, read and written through
`read_config_fields()` / `write_config_fields()`), and a second form under
Settings → Plugins → ctx_checkpoint (`config_schema` in `plugin.yaml`, stored by
Hermes in `plugins.entries.ctx_checkpoint.settings`) for the time limit, the part
limit and the single memory features; no key is in both forms. The web
dashboard's memory settings show the same memory form (`get_config_schema()` /
`save_config()`, built from the same field table); `hermes memory setup
ctx_checkpoint` asks for it on the terminal (`post_setup()`). Only the ctx server
URL and the API key need an answer. Saving tests the connection with Hermes' own
MCP probe (the one behind `hermes mcp test`) and refuses a failing test with its
reason; the terminal setup then offers the operating mode
(`compression.checkpoint_required`), and the Desktop form carries it as a switch
that needs a passing test to turn on. The web dashboard's form neither tests nor
carries the mode: it saves the key after the plugin's settings and sends every
field with the value it loaded. The key lands in Hermes' `.env` as `CTX_API_KEY`, and
the `mcp_servers` entry references it as `Authorization: Bearer
${CTX_API_KEY}`. Setup targets the entry named by `memory.ctx_checkpoint.mcp_server`
(default `ctx`, changed in `config.yaml` only) and keeps an existing one: it
re-enables it, updates the `url` of an HTTP entry, and asks for the key only
when it manages that entry's auth itself — no `headers`/`auth` yet, or headers
referencing `${CTX_API_KEY}`. Entries with their own `headers` or `auth`, and
stdio `command` entries, keep their transport and authentication. Setup writes
only `memory.ctx_checkpoint`, that server entry and (terminal setup)
`memory.provider`, merged into `config.yaml` as it is on disk at write time.
It reads the file without `${VAR}` expansion, so no secret behind a template
ends up in `config.yaml`. A package-managed install refuses setup. Of the
compaction settings setup writes only `compression.checkpoint_required`, and
only when you choose a mode that differs from the current one; never the
thresholds.

The entry pins an exact commit of this repository — currently `9f98ff6`
(release v5.29.6), plugin 0.6.2, re-pinned in
[#147170](https://github.com/NousResearch/hermes-agent/pull/147170) — and
Hermes enforces its `requires_hermes: ">=0.20.4"`. Changes
here reach catalog users only through a reviewed re-pin PR upstream that bumps
the entry's `sha` and `version`; `hermes plugins update` then moves to the new
pin. The catalog install does **not** probe the host: run the probes under
[Fail-closed host](#fail-closed-host-stock-hermes-no-patches), or
`scripts/install.sh --dry-run --hermes-src <hermes-checkout>`, which reports
mode and engine binding without writing anything.

**From a checkout of this repository** — for code newer than the catalog pin,
or a Hermes without catalog access — with the host probes below:

```bash
scripts/install.sh                      # auto-detects the Hermes home
scripts/install.sh --hermes-home /opt/data --hermes-src /opt/hermes
```

The installer copies `plugin/ctx_checkpoint/` to
`$HERMES_HOME/plugins/ctx_checkpoint/` (Hermes' user-plugin discovery path,
which survives image updates), probes the host for the fail-closed contract
and the context-engine binding, and prints the config steps. It never edits `config.yaml` and never restarts
anything.

Use one install path per Hermes home. `hermes plugins install` refuses an
existing `plugins/ctx_checkpoint/` ("already exists"), and `install.sh` moves
any existing install — a catalog one included — aside to
`.ctx_checkpoint.bak-<timestamp>` (dot-prefixed since 0.6.1: Hermes lists every
other directory under `plugins/` as a memory provider of its own, so the installer
also names any old `ctx_checkpoint.bak-*` it finds). To move a manual install onto the catalog,
remove the copied directory first, then run `hermes plugins install
ctx_checkpoint`.

Configuration reference (under `memory.ctx_checkpoint`; the keys marked
*Plugins form* are also written by Settings → Plugins → ctx_checkpoint to
`plugins.entries.ctx_checkpoint.settings`, which wins over
`memory.ctx_checkpoint` for those keys; `$HERMES_HOME/ctx.json` ranks below both):

| Key | Default | Meaning |
|-----|---------|---------|
| `mcp_server` | `ctx` | name of the ctx MCP server entry in `mcp_servers` |
| `category` | `compaction-checkpoints` | ctx category for checkpoint blocks |
| `chunk_chars` | `36000` | source part size (clamped to 1k..40k); a part also stays under ctx's 50 KB content cap in UTF-8 bytes |
| `sensitivity` | `internal` | ctx sensitivity for stored blocks (`public`, `internal`, `personal`, `credentials`) |
| `budget_s` | `60` | *Plugins form* — wall-clock budget of one checkpoint, clamped to 5..600 s; a paced checkpoint adds 61 s per extra rate window |
| `max_parts` | `80` | *Plugins form* — parts one checkpoint writes per 60 s rate window, clamped to 1..95; a longer checkpoint is paced over several windows (until 0.9.0 it was refused) |
| `auto_recall`, `auto_memory_mirror`, `auto_compress_extract`, `auto_session_digest` | `true` | *Plugins form* — the single memory features; they act only with `memory: true` |

`hermes memory status` (and `hermes ctx_checkpoint status`) runs the connection
test and shows the server entry, the operating mode and the outcome of the last
checkpoint (`$HERMES_HOME/ctx_state/checkpoint-status.state`).

Activate with `memory.provider: ctx_checkpoint` (terminal setup and the
installer prompt set it). The provider talks to ctx through Hermes' own MCP
dispatch, so any ctx reachable as an MCP server works — no extra credentials
inside the plugin. It counts as available only while that `mcp_servers` entry
exists and is not disabled.

## Long contexts (1M) and compaction thresholds

The plugin leaves every compaction setting at Hermes' defaults, and those work
for 1M-token models:

- **Compressor:** `compression.threshold` defaults to 0.50 of the window on
  models with 512K or more, and is floored at 0.75 below 512K.
- **Gateway session hygiene:** compacts between turns at a fixed 0.85 of the
  window (`gateway/run_turn_hygiene.py`, not configurable) and holds the next
  turn for at most `compression.hygiene_max_turn_hold_seconds` (10 s). After
  that the turn runs on the uncompressed transcript and the summary lands one
  turn late. The short hold is deliberate: a longer one can trip transport
  idle timeouts, and 30 s without summary progress cancels the hygiene
  compaction instead.

With a threshold at or above 0.85, hygiene compacts before the compressor
and that short hold becomes the normal path. On a host without the
compaction-lock follow-up, the compressor's own attempts during such a turn
find the lock held and send the request anyway, up to and past 100 % of the
window. The plugin logs a warning once per process when
`compression.threshold >= 0.85`.

A compaction does not get slower with the window. Hermes caps the summary at
`min(5 % of the window, 10,000)` tokens and the summarizer input at 160,000
characters, sampling across the window above that. Measured on a 272K window
with gpt-6-sol (2026-10-25/26, six compactions): 16k–49k summary input
tokens, 7.3k–9.9k output tokens, 54–78 s, output at a steady 115–136 tokens/s.
The time goes into writing the summary. At 1M the same caps make the summary
much lossier, and the checkpoint is what keeps the exact wording. Its part
count follows the direct user/assistant text only: at about 250k tokens of
context that was one part, stored in 1–5 s.

## Fail-closed host (stock Hermes, no patches)

Any Hermes carrying the merge already has the contract: any release cut after
2026-08-25 (`v0.21.1` / `v2026.9.7` onward — the catalog page's "Hermes
0.21.1+"; verified on `v0.22.4`, 2026-10-22), or `main` at/after
`1ee524f77d`. The last tag *without* it is `v2026.8.19`. Up to `v0.23.0`,
though, `delegate_task` builds its children without a memory provider
(`skip_memory=True`), so with the gate armed every sub-agent compaction fails
closed whether ctx is reachable or not; `v0.23.1` is the first release whose
children inherit the provider (`skip_memory=not child_checkpoint_required` in
`tools/delegate_tool.py`), and plugin 0.8.0 requires it.

Probe the host you are about to enable the gate on:

```bash
grep -n "PRE_COMPRESS_CHECKPOINT_API_VERSION" agent/memory_provider.py
# ...:PRE_COMPRESS_CHECKPOINT_API_VERSION = 2   ← fail-closed capable
# no match, or = 1                              ← best-effort only

grep -c compacts_outside_compress agent/context_engine.py
# 1 or more   ← engine binding present: this host carries the context-engine
#               follow-up, so a plugin engine that compacts outside compress()
#               is refused at init instead of slipping past the gate
# 0           ← engine binding absent: the gate covers the built-in compaction
#               paths only. With a plugin context engine installed, compaction
#               it performs from on_turn_complete() or its own scheduler never
#               reaches the checkpoint and reports nothing — the gate then
#               guarantees less than it appears to. Hosts running the built-in
#               compressor are unaffected.
```

`scripts/install.sh --hermes-src <hermes-checkout>` runs both probes and
reports the resulting mode and engine binding; with `--dry-run` it only
probes and copies nothing, which is the way to probe next to a catalog
install. Then enable the gate:

```yaml
compression:
  checkpoint_required: true
```

> **Warning:** on a host older than the merge — anything without that
> constant, or reporting version 1 — this key is silently ignored. Compaction
> keeps running without a guaranteed checkpoint while the config suggests
> otherwise. Enable it only when the probe reports v2 or higher.

> **Operator warning — availability.** Arming the gate means accepting a
> **session halt** when the checkpoint provider is unavailable (ctx down, DB
> unreachable, embed backend dead), instead of unarchived compaction. With the
> gate armed the checkpoint-bound compressor is the only lossy authority left;
> the paths that used to reclaim context without it — micro-compaction, and,
> with the follow-up, the proactive tool-result prune and a withheld
> `on_turn_complete` — are suppressed. The context window then grows until the
> model limit and requests start failing, with a named
> `BLOCKED_MISSING_PREREQUISITE` error. Nothing is lost, and no compaction runs
> unarchived: that stand-still *is* the promise of fail-closed, not a side
> effect of it.

## Runtime contract negotiation

The plugin does not hard-code the contract version it declares. On every read
of `pre_compress_checkpoint_api_version` (a property — the host reads it as
`int(getattr(provider, ...))` on the instance) it **probes the loading host**
and declares the **minimum of both sides**:

- **Host probe** (`_probe_host_checkpoint_api()`): reads
  `agent.memory_provider.PRE_COMPRESS_CHECKPOINT_API_VERSION` and checks
  whether `agent.conversation_compression` exposes the v3 helper
  `_tool_evidence_for_pre_compress_memory` as a callable. Result: 3 only when
  the constant is ≥ 3 **and** the helper exists; 2 when the constant is ≥ 2;
  otherwise 1. The probe never raises — a missing module, a non-int constant
  or an import error all yield 2, the conservative gate-compatible answer for
  a host that loads this plugin at all.
- **Plugin ceiling** (`PLUGIN_CHECKPOINT_API_MAX`, currently 2): the highest
  contract this plugin implements. It is raised to 3 once the tool-index
  renderer lands (W02-5); the negotiation then flips without further code
  changes.
- **Declared version** = `max(2, min(host, plugin))`. The plugin implements v2
  completely, so 2 is the floor even on a v1 host (whose gate checks `>= 1`).

Every checkpoint block (parts, manifest, head) records the negotiation in its
metadata, and the head body repeats the negotiated value as
`Checkpoint API version: <n>`:

| Field | Meaning |
|-------|---------|
| `host_checkpoint_api` | what the probe found on the host (1, 2 or 3) |
| `checkpoint_api_version` | what the plugin declared — the negotiated value |
| `tool_index_status` | `absent` when no `tool_evidence` reached the call; `received-unrendered` when the host sent it |

**v3 host before W02-5:** `on_pre_compress(messages, *, tool_evidence=None,
**kwargs)` tolerates the v3 keyword and any future ones, but this version
does **not** render tool evidence. A v3 host still gets a complete prose
checkpoint, declared and labelled as v2; if the host sends `tool_evidence`
anyway, the blocks carry `tool_index_status: received-unrendered` and the
plugin logs one warning per provider instance — no error, no blocked
compaction.

Contract tests ship on both sides: host-side upstream in
`tests/agent/test_pre_compress_checkpoint_contract.py` (26 tests on `v0.22.4`, 2026-10)
and provider-side here in `plugin/ctx_checkpoint/tests/` (mocked MCP dispatch —
no live ctx needed; the store, get and search answers it gives are the ones
ctx's handler test pins in `plugin/ctx_checkpoint/memory/tests/mcp_answers.json`):
`test_provider_contract.py` covers the checkpoint chain,
`test_contract_negotiation.py` the runtime negotiation against the real host
on `PYTHONPATH` plus a monkeypatched v3 host, `test_setup.py` the setup
surfaces, `test_settings_and_status.py` the Desktop memory form through the
host's declared-surface route, the
Settings → Plugins section through the host's settings writer, the connection
test, the checkpoint outcome and the status, each test against its own
temporary `HERMES_HOME`, including a round trip through the host's Dashboard
form writer. `.github/workflows/hermes-ctx-checkpoint.yml` runs both suites,
ruff and `hermes plugins validate` in CI.

The pre-merge patch series that used to supply the contract is retired and
kept for history under `patches/archive/` — see its README before touching
anything in there.

## Recall path

The agent does not have to find the head itself: since 0.9.0 the provider's system
prompt block names the session root's checkpoint head and the read path with the
session's tool names (`mcp__<server>__get` / `mcp__<server>__search` with memory
features off, `ctx_get` / `ctx_search` with them on) as soon as a checkpoint exists,
and the compaction receipt carries the same line. Hermes rebuilds the prompt at each
compaction, right after the checkpoint; gateway hygiene and `/compress` in a gateway
channel keep the stored prompt, so the pointer arrives there with the next build.
Details in the [plugin README](plugin/ctx_checkpoint/README.md#recall-of-a-checkpoint).

By hand, ask ctx for the session's checkpoint head (its title is stable per root
session):

```
ctx search compaction-checkpoints query:"Compaction checkpoint head <root-session>"
ctx get <head-id>        # → latest_manifest_id
ctx get <manifest-id>    # → source part IDs, parent manifest chain
```

A root id longer than 48 characters (Hermes' `local-<sha256>` ids among them) or
one outside `[A-Za-z0-9_.-]` appears in the title sanitized, cut to at most 39
characters and with an 8-character hash suffix (since 0.6.1); search by the id's
start, the full id is in the head's `metadata.root_session_id`.

Load source parts only when exact wording or provenance is needed — the head
and manifest are deliberately small so routine recall stays cheap.

## Scope and non-goals

- The checkpoint preserves **direct user/assistant text**. Tool results,
  system prompts, and synthetic compaction envelopes are intentionally
  excluded; the local Hermes `state.db` remains the authoritative full
  transcript.
- No LLM in the critical path — the checkpoint is deterministic and fast,
  and cannot be taken down by a slow or failing summarizer model.
- The installer never deploys: editing configs, switching a container, and
  restarting Hermes are deliberate operator steps.

## Versions

- Plugin: see `plugin/ctx_checkpoint/VERSION` (0.4.3: runtime contract
  negotiation, `tool_evidence` keyword tolerance, `host_checkpoint_api` /
  `checkpoint_api_version` / `tool_index_status` metadata; 0.4.4: catalog
  packaging — `requires_hermes: ">=0.20.4"` in `plugin.yaml` and a plugin
  README for the catalog page, no code change; 0.5.0: setup through Hermes'
  own surfaces — `get_config_schema()` / `save_config()` for the Desktop and
  Dashboard form, `post_setup()` for `hermes memory setup ctx_checkpoint` —,
  a warning when `compression.threshold >= 0.85`, and `is_available()` now
  requires the ctx `mcp_servers` entry to exist; before 0.5.0 a missing entry
  counted as available; 0.5.1: port-backs from the review of
  [W4-NERF/hermes-ctx-plugin](https://github.com/W4-NERF/hermes-ctx-plugin)
  1.7.1, whose `checkpoint.py` is an MPL-2.0 port of this plugin — the
  key/value redaction masks the credential behind `Authorization:
  Bearer|Basic|Digest` instead of only the scheme word, a redactor failure
  raises instead of rendering an empty transcript, every write claims
  `type: checkpoint` explicitly (ctx ≥ v5.5.0), a batch without direct
  user/assistant text returns an empty receipt instead of blocking a required
  compaction, and the chain write runs inside `budget_s` (default 60 s) with
  2/4/8 s backoff on rate-limited stores and a `max_parts` cap (default 80)
  that refuses before writing anything).
- Catalog pin: 0.6.2 at `9f98ff6` (`plugin-catalog/ctx_checkpoint.yaml`
  upstream; listed 2026-10-25 with 0.4.4 at `98749f3`, re-pinned 2026-10-29 in
  #145302 to 0.5.0 at `ee8cb2e` and 2026-11-02 in
  [#147170](https://github.com/NousResearch/hermes-agent/pull/147170) to
  0.6.2). `VERSION` in this repository can run ahead of the pin until the
  next re-pin PR lands.
- Plugin contract ceiling: `PLUGIN_CHECKPOINT_API_MAX = 2` in
  `plugin/ctx_checkpoint/__init__.py` — the declared version is negotiated
  at runtime as `max(2, min(host, plugin))`, see
  [Runtime contract negotiation](#runtime-contract-negotiation).
- Host contract: `PRE_COMPRESS_CHECKPOINT_API_VERSION` in the Hermes host —
  v2 is fail-closed, v1 is historical best-effort; v3 (upstream follow-up,
  `tool_evidence` channel) is detected by the probe but not yet rendered.
- Context-engine binding: follow-up **open upstream** as
  [#94983](https://github.com/NousResearch/hermes-agent/pull/94983) (branch
  `feat/context-engine-compaction-authority`); not on `main` as of
  2026-10-26 (`c714c42`). Probe a host with
  `grep -c compacts_outside_compress agent/context_engine.py` — `0` means the
  host predates it.
- Retired patch series: `patches/archive/`, last entry `v2026.8.19`.
- 0.6.0 (ctx v5.29.0): the memory layer. hermes-ctx-plugin (imported verbatim at
  1.7.1, commit `e9ae6010`, author W4-NERF) becomes the engine under
  `plugin/ctx_checkpoint/memory/`; one settings section `memory.ctx_checkpoint`
  with `memory: on/off` and `transport: mcp|rest`; MCP transport adapter
  (`memory/mcp_client.py`); bundled skill `ctx_checkpoint:ctx-memory`; CLI
  `hermes ctx_checkpoint`; security sweep (every automatic write redacted, recall
  frame escaping, http(s)-only REST base URL, quoted SQLite URI, standing
  `ruff --select S` gate). The chain is written in every agent context; CJK
  code-switch rejection is opt-in; the untrusted label is English. Engine
  history: `plugin/ctx_checkpoint/memory/CHANGELOG.md`.
- 0.6.1: checkpoint-chain fixes (GottZ/ctx#51) — parts stay under ctx's 50 KB
  content cap in UTF-8 bytes (Cyrillic/CJK transcripts failed every checkpoint),
  the head search filters on the `checkpoint-head` tag (the MCP transport drops
  `types`), root ids that sanitizing or the 48-character cut changes get a hash
  suffix in the head title (timestamp ids and UUIDs keep theirs, Hermes'
  `local-<sha256>` ids get the suffix) and a chain started under the 0.6.0 title
  continues under the new one, MCP store answers yield a block id only from a
  structured `id` or the `(id: …)` group, `confirm_writes` keys are refused by name
  instead of the never-reached auto-confirm, the chain writer's per-call budget
  bounds MCP dispatches too and `/stop` aimed at the caller still reaches them,
  `transport: rest` is available with `rest_base_url` in the section alone, and
  `install.sh` writes dot-prefixed backups.
- 0.6.2: `transport: rest` redacts the ctx key (`[REDACTED]`) from every error text
  the client produces — HTTP error bodies, codes outside the reject vocabulary,
  reasons, connection and `http.client` errors and HTTP 200 failure envelopes, before
  truncation and without the chained original exception — refuses redirects (urllib
  would carry `X-Context-Key` to the `Location`), redacts the bound key from checkpoint,
  mirror, fact and digest writes, and a re-initialized provider drops the previous
  client, key, whoami identity and checkpoint writer. On both transports a re-init or
  session switch drops the recall prefetch (including warm-ups in flight), warm-ups and
  `prefetch()` stay on the client they started with, and a binding change drops the
  manifest-lineage cache (under `transport: mcp` the binding is the profile home plus
  the MCP server name, so a backend switch under the same name is not detected).
- 0.7.0 (ctx v5.36.0 or newer for the new recall, GottZ/ctx#183): `transport: mcp`
  recalls through the `query` tool with `synthesize: false, include_content: true` —
  the semantic hybrid `transport: rest` already used, without the synthesis step —
  instead of the lexical `search` tool; as on REST, recalled rows carry no `type`. A
  server older than ctx v5.36.0 refuses the two arguments; the client then recalls
  through `search` as before, after one refused call per client (calls overlapping
  that first one use `search`).
- 0.8.0 (GottZ/ctx#285): settings and status for Hermes Desktop. Settings → Memory
  shows the memory form (declared surface `config_schema.py`, one field table with
  the web dashboard and terminal form), Settings → Plugins → ctx_checkpoint the time
  limit, the part limit and the single memory features (`plugins.entries.ctx_checkpoint.settings`,
  over `memory.ctx_checkpoint`). Saving tests the connection (Hermes' own MCP probe)
  and refuses a failing test with its reason; the operating mode
  (`compression.checkpoint_required`) is shown in every surface and written only on an
  explicit choice after a passing test. Every checkpoint leaves its outcome in
  `ctx_state/checkpoint-status.state`; `hermes memory status` and
  `hermes ctx_checkpoint status` show the server entry, a connection test, the mode
  and the last checkpoint on the MCP transport (no more "set CTX_BASE_URL and
  CTX_KEY"); a failed or skipped checkpoint names its cause through Hermes' warning channel.
  Warnings for `checkpoint_mode: off` and for plain `http://` to a remote host.
  `requires_hermes` is `>=0.23.1`. CI workflow `hermes-ctx-checkpoint.yml`.
- 0.9.0 (GottZ/ctx#286): after a compaction the agent learns that the checkpoint
  exists and how to read it. The system prompt block names the session root's
  checkpoint head (id and title) and the read path with the session's tool names —
  also with memory features off, where the block used to be empty; it stays empty
  while no head is known. The compaction receipt ends with the same read path, and the
  bundled skill describes it per mode. The head is kept per root session in
  `ctx_state/checkpoint-status.state` (512 most recent), so every process of the
  profile names it.
- 0.10.0 (GottZ/ctx#287): a checkpoint with more parts than `max_parts` is paced over
  ctx's 60 s write windows instead of refused, so a session under
  `compression.checkpoint_required` no longer stops compacting for good once its
  transcript outgrows one window; `budget_s` grows by 61 s per extra window and Hermes
  shows a notice before the wait. The checkpoint id derives from the root session and
  the transcript's SHA-256, so a retry meets its own titles and ctx confirms the parts an
  earlier attempt stored without writing them again. A write ctx refuses for its limit
  waits for a free slot instead of a fixed 2/4/8 s and stops at the second refusal in a
  row (Hermes' circuit breaker opens at the third).
- 0.11.0 (GottZ/ctx#288): on the MCP transport the ctx key is removed from checkpoint
  parts and every automatic write: the key the `mcp_servers` entry signs in with
  (`Authorization` or `X-Context-Key` header, `${VAR}` resolved from `.env`) and
  `CTX_API_KEY` are replaced wherever they stand. Hermes' redactor runs in its strict
  mode (signed URLs, OAuth codes, URL user info), the plugin's rules add German labels,
  passphrases, PINs, seed phrases, recovery codes, `creds`, `X-Context-Key`, passwords in
  prose with a digit and webhook URL tokens, and they judge a value by its shape: code and
  prose behind a credential name (`token = self.token`, `password: str`) are no longer
  overwritten. A transcript checkpointed again after the update redacts differently, so
  its digest and titles change and it starts a new chain next to the old one.
- 0.12.0 (GottZ/ctx#289): under `compression.checkpoint_required`, `checkpoint_chain:
  false` refuses the compaction instead of passing on facts alone (facts are not a
  transcript checkpoint); the session start and `hermes memory status` warn about it. Facts
  after a stored chain are bounded by the time the deadline leaves and no longer fail the
  checkpoint: a refused or late fact is logged and left for a later compaction. A head that
  keeps its tag but lost `latest_manifest_id` no longer blocks its root: the new chain's
  head replaces it and the user is told. An engine name such as `checkpoint_sensitivity`
  written next to the plugin key `sensitivity` wins in either order. The error text of a
  failed chain, fact, mirror or digest store is redacted with the bound keys before it is
  raised or logged (GottZ/ctx#301).

## Licensing

The plugin and the scripts are part of ctx (MPL-2.0). The memory engine under
`plugin/ctx_checkpoint/memory/` was contributed by derLonius (W4-NERF) under the
same license; its `checkpoint.py` in turn is a port of the plugin's chain writer
and keeps that notice (see the
[engine README](plugin/ctx_checkpoint/memory/README.md#license--attribution)).
The archived patch series under `patches/archive/` modifies
[NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent)
(MIT); those patches carry unmodified context lines from that codebase and
have been upstreamed.
