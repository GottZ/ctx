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

Answer **y** when the installer asks to use it as the memory provider. Then
set it up — no YAML editing needed:

- **Hermes Desktop / Dashboard:** open the memory settings, pick
  `ctx_checkpoint` and fill in the form.
- **Terminal:** `hermes memory setup ctx_checkpoint`

Setup asks for your ctx server URL and API key. Everything else has a working
default. The key goes into Hermes' `.env` as `CTX_API_KEY`; `config.yaml` only
references it. Setting up a ctx server is covered in the
[ctx README](https://github.com/GottZ/ctx#readme).

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
    category: compaction-checkpoints # ctx category for checkpoint blocks
    chunk_chars: 36000               # source part size, clamped to 1k..40k; a part also stays under ctx's 50 KB byte cap
    sensitivity: internal            # public | internal | personal | credentials
    budget_s: 60                     # wall-clock budget per checkpoint, 5..600 s
    max_parts: 80                    # part cap per checkpoint, 1..95 (ctx writes 100/60 s per key)
```

Every key under `memory.ctx_checkpoint` is optional; the values shown are the
defaults. `budget_s` and `max_parts` are not asked during setup; set them in
`config.yaml` when needed (setup keeps them). An existing `mcp_servers.ctx` entry is kept: setup updates its URL
and re-enables it, but asks for a key only when it manages that entry's
authentication itself (no auth yet, or `Bearer ${CTX_API_KEY}` from an earlier
setup). An entry with its own `headers` or `auth`, or a stdio `command` entry,
keeps them untouched. Setup always targets the entry named by `mcp_server`;
change that name in `config.yaml` if your ctx entry is called differently.

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
| **best-effort** (default) | any Hermes ≥ 0.20.4 | checkpoint is attempted before compaction; a failure is logged and compaction proceeds |
| **fail-closed** | Hermes ≥ 0.21.1 (carries the checkpoint contract, upstream PR #94639) | with `compression.checkpoint_required: true`, compaction is **blocked** unless the checkpoint is confirmed durable; the uncompressed transcript is preserved |

Fail-closed means accepting a session halt (`BLOCKED_MISSING_PREREQUISITE`) while
ctx is unreachable, instead of an unarchived compaction. The halt also triggers
when a checkpoint would need more than `max_parts` source parts or does not
finish inside `budget_s` (a rate-limited store is retried after 2/4/8 s while the
budget allows; since 0.5.2 the budget also bounds every single call, the head
search included, so a hanging ctx no longer holds the compaction for Hermes' MCP
timeout). A ctx key with `confirm_writes` is refused with that reason: ctx stages
its writes instead of storing them, and checkpoint writes run unattended, so the
plugin never confirms them on your behalf. Every failure is logged as a warning
(since 0.5.2), also in best-effort mode. A batch with no direct user/assistant text — only tool rows or
compaction envelopes — has nothing to archive and never blocks: the provider
returns an empty receipt and the compaction proceeds (0.5.1; before, it raised).
The plugin negotiates the contract version with the loading host at runtime;
details, host probes and the operator warnings are in the
[full integration guide](../../README.md).

## Recall

```
ctx search compaction-checkpoints query:"Compaction checkpoint head <root-session>"
ctx get <head-id>        # → latest_manifest_id
ctx get <manifest-id>    # → source part IDs, parent manifest chain
```

## What it does on your machine

- **Network:** only the MCP calls Hermes makes to the ctx server you configured
  (`search`/`get` to find the session's checkpoint head, `store` for the blocks).
  No third-party services, no telemetry.
- **Data sent:** direct user/assistant text of the turns about to be compacted.
  Tool results, system prompts and synthetic compaction envelopes are excluded.
  Secrets are redacted twice before anything leaves the process: Hermes'
  `redact_sensitive_text` plus a conservative key/value and bearer-token pass.
- **Reads:** its own section of the Hermes config (`memory.ctx_checkpoint`), the
  named `mcp_servers` entry (to check it exists and is enabled, and to prefill
  setup), `compression.threshold` (for the warning above) and the transcript
  Hermes passes to `on_pre_compress`. Nothing outside that.
- **Writes, only when you run setup:** `memory.ctx_checkpoint`, the named
  `mcp_servers` entry and — in terminal setup — `memory.provider` in
  `config.yaml`, merged into the file so other sections stay as they are; the
  API key goes to `.env` as `CTX_API_KEY`. It never writes compaction settings.
- **No** shell commands, background processes, stored credentials or
  self-updating code.
- **Core modules used:** `agent.memory_provider`, `agent.redact` (redaction),
  `tools.registry` (MCP dispatch) and `hermes_cli.config` (provider config) —
  imported because the memory-provider surface has no public seam for these yet.

## Tests

90 tests (mocked MCP dispatch, no live ctx needed) against the real host on
`PYTHONPATH`: the checkpoint contract, its runtime negotiation and the setup
surfaces (each setup test writes into its own temporary `HERMES_HOME`):

```bash
PYTHONPATH=/path/to/hermes-agent:. python -m pytest ctx_checkpoint/tests
```

## License

MPL-2.0, part of [ctx](https://github.com/GottZ/ctx).
