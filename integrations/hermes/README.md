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
head → manifest → parts to recover exact wording and provenance. Secrets are
redacted twice (Hermes' own `redact_sensitive_text` plus a conservative
key/value + bearer-token pass) before anything leaves the process.

## Two operating modes

| Mode | Host | Guarantee |
|------|------|-----------|
| **best-effort** (start here) | any Hermes | checkpoint is attempted before compaction; a failure is logged and compaction proceeds (historical hook semantics, contract version 1) |
| fail-closed | Hermes carrying the checkpoint contract (version 2) | compaction is **blocked** unless a checkpoint-capable provider confirms the durable checkpoint; the uncompressed transcript is preserved |

The plugin is a plain Hermes **user plugin** — install it into
`$HERMES_HOME/plugins/` on a completely stock Hermes and it works today in
best-effort mode (since upstream v2026.8.18, provider text is forwarded into
the compaction summary).

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
  (class default stays `1`)
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
  `feat/context-engine-compaction-authority`, PR pending) closes it with
  `ContextEngine.compacts_outside_compress` plus the
  `engine_compacts_outside_compress()` resolver in `agent/context_engine.py`,
  an init refuse (`BLOCKED_MISSING_PREREQUISITE`) in `agent/agent_init.py`,
  per-turn withholding of `on_turn_complete` in `agent/conversation_loop.py`,
  and suppression of the proactive tool-result prune there — each suppression
  counted on `agent._checkpoint_gate_suppression_count`

Everything is default-off: an untouched config behaves exactly like stock
Hermes, and the contract without this plugin behaves like stock Hermes.

## Install (plugin, both modes)

```bash
scripts/install.sh                      # auto-detects the Hermes home
scripts/install.sh --hermes-home /opt/data --hermes-src /opt/hermes
```

The installer copies `plugin/ctx_checkpoint/` to
`$HERMES_HOME/plugins/ctx_checkpoint/` (Hermes' user-plugin discovery path,
which survives image updates), probes the host for the fail-closed contract
and the context-engine binding, and prints the config steps. It never edits `config.yaml` and never restarts
anything.

Configuration reference (all under `memory.ctx_checkpoint`):

| Key | Default | Meaning |
|-----|---------|---------|
| `mcp_server` | `ctx` | name of the ctx MCP server entry in `mcp_servers` |
| `category` | `compaction-checkpoints` | ctx category for checkpoint blocks |
| `chunk_chars` | `36000` | source part size (clamped to 1k..40k) |
| `sensitivity` | `internal` | ctx sensitivity for stored blocks |

Activate with `memory.provider: ctx_checkpoint`. The provider talks to ctx
through Hermes' own MCP dispatch, so any ctx reachable as an MCP server works
— no extra credentials inside the plugin.

## Fail-closed host (stock Hermes, no patches)

Any Hermes carrying the merge already has the contract: any release cut after
2026-08-25, or `main` at/after `1ee524f77d`. Since the last tag before the
merge is `v2026.8.19`, that currently means running from `main` until the next
release is cut.

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
reports the resulting mode and engine binding. Then enable the gate:

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

Contract tests ship on both sides: host-side upstream in
`tests/agent/test_pre_compress_checkpoint_contract.py` (17 tests on `main`)
and provider-side here in
`plugin/ctx_checkpoint/tests/test_provider_contract.py` (9 tests, mocked MCP
dispatch — no live ctx needed).

The pre-merge patch series that used to supply the contract is retired and
kept for history under `patches/archive/` — see its README before touching
anything in there.

## Recall path

After a compaction, ask ctx for the session's checkpoint head (its title is
stable per root session):

```
ctx search compaction-checkpoints query:"Compaction checkpoint head <root-session>"
ctx get <head-id>        # → latest_manifest_id
ctx get <manifest-id>    # → source part IDs, parent manifest chain
```

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

- Plugin: see `plugin/ctx_checkpoint/VERSION`.
- Host contract: `PRE_COMPRESS_CHECKPOINT_API_VERSION` in the Hermes host —
  v2 is fail-closed, v1 is historical best-effort.
- Context-engine binding: follow-up **pending upstream** (branch
  `feat/context-engine-compaction-authority`, on top of `main`; PR not yet
  opened). Probe a host with
  `grep -c compacts_outside_compress agent/context_engine.py` — `0` means the
  host predates it.
- Retired patch series: `patches/archive/`, last entry `v2026.8.19`.

## Licensing

The plugin and scripts are part of ctx (MPL-2.0). The archived patch series
under `patches/archive/` modifies
[NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent)
(MIT); those patches carry unmodified context lines from that codebase and
have been upstreamed.
