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
| **best-effort** (start here) | stock Hermes | checkpoint is attempted before compaction; a failure is logged and compaction proceeds (upstream hook semantics) |
| fail-closed | host with the checkpoint contract | compaction is **blocked** unless a checkpoint-capable provider confirms the durable checkpoint; the uncompressed transcript is preserved |

The plugin is a plain Hermes **user plugin** — install it into
`$HERMES_HOME/plugins/` on a completely stock Hermes and it works today in
best-effort mode (since upstream v2026.8.18, provider text is forwarded into
the compaction summary).

The fail-closed gate is what makes it a real guarantee: a memory system you
only *hope* ran is not a memory system. The host-side contract is **proposed
upstream** — issue
[NousResearch/hermes-agent#93986](https://github.com/NousResearch/hermes-agent/issues/93986),
PR [NousResearch/hermes-agent#93996](https://github.com/NousResearch/hermes-agent/pull/93996).
Once merged, fail-closed mode needs no patches at all. Until then, `patches/`
carries the same change as a reviewed patch series per pinned upstream
release. The contract is opt-in and provider-agnostic (API v1):

- `MemoryProvider.pre_compress_checkpoint_api_version` — provider opt-in
- `MemoryManager.supports_pre_compress_checkpoint()` + strict
  `on_pre_compress(require_checkpoint=True)` (failures propagate)
- `compression.checkpoint_required` config gate (default `false`) — fails
  closed with `BLOCKED_MISSING_PREREQUISITE` before any lossy rewrite,
  including the gateway hygiene/auto-compact paths
- a persistent `_compressed_summary` message marker so derivative summaries
  stay excluded from checkpoints across process restarts

Everything is default-off: an unpatched config behaves exactly like stock
Hermes, and the patched core without this plugin behaves like stock Hermes.

## Install (plugin, both modes)

```bash
scripts/install.sh                      # auto-detects the Hermes home
scripts/install.sh --hermes-home /opt/data --hermes-src /opt/hermes
```

The installer copies `plugin/ctx_checkpoint/` to
`$HERMES_HOME/plugins/ctx_checkpoint/` (Hermes' user-plugin discovery path,
which survives image updates), probes the host for the fail-closed contract,
and prints the config steps. It never edits `config.yaml` and never restarts
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

## Fail-closed host (until #93996 is merged: patch series + image build)

`patches/<upstream-tag>/` carries one reviewed patch per pinned upstream
release, plus SHA-256 manifests of the touched files **before**
(`baseline.sha256`) and **after** (`patched.sha256`) patching. The build
script refuses to patch a source state it does not recognize — never patch
blind over an unknown Hermes version.

```bash
scripts/build-image.sh --upstream-tag v2026.8.19            # clone, verify, patch, build
scripts/build-image.sh --upstream-tag v2026.8.19 --skip-build   # patched tree only
```

Then enable the gate:

```yaml
compression:
  checkpoint_required: true
```

> **Warning:** on a stock host this key is silently ignored — compaction
> keeps running without a guaranteed checkpoint while the config suggests
> otherwise. Enable it only on a host built from the patch series (probe:
> `grep PRE_COMPRESS_CHECKPOINT_API_VERSION agent/memory_provider.py`).

Contract tests ship on both sides: host-side in the patch
(`tests/agent/test_pre_compress_checkpoint_contract.py`, 8 tests) and
provider-side in `plugin/ctx_checkpoint/tests/test_provider_contract.py`
(6 tests, mocked MCP dispatch — no live ctx needed).

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
- The scripts never deploy: building an image, switching a container, and
  editing configs are deliberate operator steps.

## Versions

- Plugin: see `plugin/ctx_checkpoint/VERSION` (currently 0.4.0, live-proven).
- Patch series: one directory per supported upstream tag under `patches/`.

## Licensing

The plugin and scripts are part of ctx (MPL-2.0). The patch series modifies
[NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent)
(MIT); the patches carry unmodified context lines from that codebase and are
intended for upstreaming.
