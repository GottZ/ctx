# ctx_checkpoint

Durable pre-compaction checkpoints for [Hermes](https://github.com/NousResearch/hermes-agent),
stored in [ctx](https://github.com/GottZ/ctx).

Hermes compacts long conversations with a lossy LLM summary. `ctx_checkpoint` is a
memory provider that archives the direct user/assistant transcript into ctx
**before** each compaction — deterministically, without any LLM in the critical
path — and hands the resulting block IDs to Hermes, which carries them into the
compaction summary. A later session (or a human) can walk them back to the exact
wording that the summary discarded.

```
Compaction checkpoint head <root-session>     ← stable title, upserted per session
  └── manifest (per checkpoint, parent-linked to the previous manifest)
        └── source parts 001..NNN (redacted direct transcript, ≤40k chars each)
```

## Install

```bash
hermes plugins install ctx_checkpoint
```

The plugin talks to ctx through Hermes' own MCP dispatch, so it needs a ctx MCP
server entry in `config.yaml` and holds no credentials of its own:

```yaml
mcp_servers:
  ctx:
    url: https://your-ctx-host.example/mcp
    headers:
      X-Context-Key: ${CTX_KEY}

memory:
  provider: ctx_checkpoint
  ctx_checkpoint:
    mcp_server: ctx                  # name of the entry above (default: ctx)
    category: compaction-checkpoints # ctx category for checkpoint blocks
    chunk_chars: 36000               # source part size, clamped to 1k..40k
    sensitivity: internal            # ctx sensitivity for stored blocks
```

Every key under `memory.ctx_checkpoint` is optional; the values shown are the
defaults. Setting up a ctx server is covered in the
[ctx README](https://github.com/GottZ/ctx#readme).

## Two operating modes

| Mode | Host | Guarantee |
|------|------|-----------|
| **best-effort** (default) | any Hermes ≥ 0.20.4 | checkpoint is attempted before compaction; a failure is logged and compaction proceeds |
| **fail-closed** | Hermes ≥ 0.21.1 (carries the checkpoint contract, upstream PR #94639) | with `compression.checkpoint_required: true`, compaction is **blocked** unless the checkpoint is confirmed durable; the uncompressed transcript is preserved |

Fail-closed means accepting a session halt (`BLOCKED_MISSING_PREREQUISITE`) while
ctx is unreachable, instead of an unarchived compaction. The plugin negotiates the
contract version with the loading host at runtime; details, host probes and the
operator warnings are in the
[full integration guide](../../README.md).

## Recall

```
ctx search compaction-checkpoints query:"Compaction checkpoint head <root-session>"
ctx get <head-id>        # → latest_manifest_id
ctx get <manifest-id>    # → source part IDs, parent manifest chain
```

## What it does on your machine

- **Network:** only the MCP calls Hermes makes to the ctx server you configured
  (`search`/`get` to find the session's checkpoint head, `store` for the blocks,
  `confirm` when the ctx guard stages a store). No third-party services, no telemetry.
- **Data sent:** direct user/assistant text of the turns about to be compacted.
  Tool results, system prompts and synthetic compaction envelopes are excluded.
  Secrets are redacted twice before anything leaves the process: Hermes'
  `redact_sensitive_text` plus a conservative key/value and bearer-token pass.
- **Reads:** its own section of the Hermes config (`memory.ctx_checkpoint`), the
  named `mcp_servers` entry (only to check it exists and is enabled) and the
  transcript Hermes passes to `on_pre_compress`. Nothing outside that.
- **No** shell commands, background processes, stored credentials or
  self-updating code.
- **Core modules used:** `agent.memory_provider`, `agent.redact` (redaction),
  `tools.registry` (MCP dispatch) and `hermes_cli.config` (provider config) —
  imported because the memory-provider surface has no public seam for these yet.

## Tests

32 contract tests (mocked MCP dispatch, no live ctx needed) against the real host
on `PYTHONPATH`:

```bash
PYTHONPATH=/path/to/hermes-agent:. python -m pytest ctx_checkpoint/tests
```

## License

MPL-2.0, part of [ctx](https://github.com/GottZ/ctx).
