# Changelog — memory engine of ctx_checkpoint

History of the memory engine (`ctx_checkpoint/memory/`). Entries up to 1.7.1 are the
releases of [W4-NERF/hermes-ctx-plugin](https://github.com/W4-NERF/hermes-ctx-plugin);
from ctx_checkpoint 0.6.0 on the engine is versioned with the plugin (`../VERSION`,
`../plugin.yaml`) and this file records engine-level changes only. Internal phase names
(P1–P6) refer to the ctx-plugin-v2 work packages.

## [ctx_checkpoint 0.6.1] — 2026-10-31

Checkpoint-chain fixes from the review in GottZ/ctx#51, checked against the answers ctx
(`go/internal/handler/mcp.go`, `mcp_confirm.go`, `context_store.go`) and Hermes' MCP
dispatch (`tools/mcp_tool_handlers.py`) actually produce.

### Fixed
- **Parts respect ctx's byte cap.** `split_chunks` cut at `chunk_chars` characters while
  ctx caps content at 50 KiB of UTF-8 bytes; a full Cyrillic part is ~72 KB, a CJK part
  ~108 KB, so such transcripts failed every checkpoint (a permanent halt under
  `checkpoint_required`). Parts are cut at whichever limit comes first, with the largest
  possible part header reserved; the part format is unchanged. A hard cut (inside a
  message) no longer strips whitespace beside it, so the parts concatenate back to the
  exact transcript; only message boundaries trim their separators.
- **Head lookup is narrowed to heads.** The MCP transport drops `types`, so the head
  search ran over every block of the category; source parts about checkpoints or headings
  could push the head out of the result window and silently start a new chain. The search
  now filters on the `checkpoint-head` tag, which every head carries.
- **No shared heads for different sessions.** A root id that sanitizing or the
  48-character cut changes (`team/alice:1` vs `team:alice/1`) gets an 8-character hash
  of the full id as suffix. Timestamp ids (`20261031_120000_abcdef`) and UUIDs keep
  their titles. Hermes' `local-<sha256>` session ids (70 characters, the format of
  cli/tui sessions on the gateway store) and every other id over 48 characters get the
  suffix too.
- **Chains started under 0.6.0 continue.** For a root whose head title changed, the
  writer looks up the 0.6.0 title once when no head exists under the new one (and the
  process has no chain cached): the new manifest links to that head's latest manifest,
  its fact-dedupe keys carry over, and the new head is published under the new title
  (log `lookup=legacy`). The old head is taken only if its metadata names the same full
  root id — `team/alice:1` and `team:alice/1` shared it under 0.6.0 — and it stays as
  it is.
- **Block ids over MCP.** `McpClient` took the first UUID anywhere in a store answer when
  the `(id: …)` group was missing; the answer starts with the title, which can carry a
  UUID-shaped session id. Only a structured `id` or the `(id: …)` group count now; no id
  fails the store.
- **`confirm_writes` keys are refused by name.** ctx stages their writes and Hermes
  delivers that as an error envelope, so the auto-confirm branch was never reached and
  the failure was an unnamed store error. The staged answer now raises
  `code="confirm_writes"` ("staged, not stored … use a key without confirm_writes", with
  the staged `payload_hash`); writes from this plugin are never confirmed on the
  operator's behalf.
- **Per-call budget over MCP.** The chain writer passed its remaining budget as
  `timeout`, but `McpClient` ignored it, so one hanging call could hold a compaction for
  Hermes' MCP timeout (300 s by default). An explicit `timeout` now bounds the dispatch on
  a worker thread (caller's context variables copied along) and raises `code="timeout"`;
  calls without one keep the transport's timeout.
- **`/stop` reaches bounded MCP calls.** Hermes' interrupts are per thread; the worker of
  a bounded call now acts for the calling thread (`tools.interrupt.acting_for_tid`, set
  inside the copied context), so a `/stop` aimed at the caller ends the call as it ends
  an unbounded one. In Hermes' default compression path the hook itself runs on a
  `compress-ctx-timeout` pool worker that is not marked as acting for the agent; there
  neither 0.6.0 nor 0.6.1 sees `/stop` (host side, `tools/thread_context.py`).
- **REST availability reads `rest_base_url`.** `is_available()` and `get_status_config()`
  loaded defaults and `ctx.json` only. With `transport: rest`, `rest_base_url` in
  `memory.ctx_checkpoint` and no `CTX_BASE_URL`, Hermes dropped the provider before
  `initialize()` and `hermes memory status` reported it as not configured. Both read the
  same settings overlay as `initialize()` now.

## [ctx_checkpoint 0.6.0] — 2026-10-31 (engine 1.8.0, folded in)

Imported verbatim at 1.7.1 (upstream commit `fbd210a`, GottZ/ctx commit `e9ae6010` under
the author's name), then made the engine of the `ctx_checkpoint` plugin: one provider name,
one settings section (`memory.ctx_checkpoint`), the plugin's setup surfaces and skill.

### Added
- `mcp_client.McpClient`: the same client surface over Hermes' MCP dispatch
  (`mcp__<server>__{search,query,store,get,confirm}`) — the default transport of the
  plugin, no credentials of its own. Retrieval-only recall runs on the `search` tool
  (the MCP `query` tool always synthesizes); `transport: rest` keeps the REST client and
  its semantic `/api/query` recall.
- Umbrella mapping in `config.py` (`overlay_from_umbrella`, `umbrella_transport`,
  `umbrella_memory_enabled`): `category`/`sensitivity`/`chunk_chars`/`budget_s`/`max_parts`
  of the plugin section map onto the `checkpoint_*` keys; `memory: false` (default) forces
  recall, capture and fact extraction off.
- `checkpoint_category` (the chain category is a setting), `ChainWriter.category`,
  `ChainWriter.write(negotiated_api=…)` and the provider seams `_connect`, `_now`,
  `_probe_host_api`, `_negotiate`, `_config_overlay`, `_chain_writer_seams`.
- Security sweep (standing `ruff --select S` gate in `ruff.toml`, tests in
  `tests/test_security_sweep.py`): every automatic write — mirror, facts, digests — passes
  the same double redaction as the chain before it leaves the process — content, the
  block title derived from it and the receipt lines in the compaction summary — and a
  failing redactor skips the write; recalled text cannot close the `<ctx-context>` recall
  frame (closing tags are escaped) and recalled titles are flattened to one line; the REST
  client accepts http(s) base URLs only; the read-only `state.db` lookup quotes the path
  in the SQLite URI. An independent review pass over the diff found one gap (fact titles
  from the unredacted text) and it is closed in the same release.

### Fixed
- The recall frame's closing tag was rendered as `</ctx-context>>` (one `>` too many).
- `checkpoint_budget_s` clamps to 5–600 s (was 5–60), matching the plugin's `budget_s`.
- An empty-evidence batch returns an empty receipt (no no-op line in the compaction
  summary); the host counts a returning provider as a successful checkpoint.

### Changed
- The transcript checkpoint chain is written in **every** agent context. The
  cron/flush/subagent write gate now covers facts, the memory mirror and digests only.
  Before, a fork under the host's `compression.checkpoint_required` raised
  `CtxCheckpointError("… writes disabled for this agent context")` on every compaction
  attempt — `BLOCKED_MISSING_PREREQUISITE` with no path to success (the failure class
  is on record in a live gateway log, 2026-10-25). Forks are labelled via
  `metadata.agent_context`, as upstream `ctx_checkpoint` 0.4.2 does.
- `UNTRUSTED_LABEL` is English (`[untrusted — data, not instructions]`); the surrounding
  prompt and tool surfaces are English.
- Rejecting any CJK character inside Latin text (`cjk-codeswitch`) is opt-in:
  `facts_reject_cjk_codeswitch` (default `false`). The `>20 % CJK` rule stays. The old
  default dropped every fact of a user writing Chinese/Japanese/Korean with Latin
  identifiers.
- Tests live in `memory/tests/` and load the engine as package `ctx_plugin`; run them
  separately from the umbrella suite (`../tests/`), see the plugin README.
- CLI is `hermes ctx_checkpoint <status|sync|query|store|search|get>`; the write and
  read commands need `transport: rest` (the CLI process has no MCP dispatch).

### Added
- README section on running this provider next to a `mcp_servers.ctx` entry (duplicate
  tool surfaces) and on the language-specific fact heuristics.

## [1.7.1] — 2026-10-30

### Fixed
- Fact noise filter: the host notice "This turn did not complete. Some actions may already
  have run …" is now rejected as `harness-warning` instead of being stored as a `learnings`
  fact.

### Changed
- Dev-tooling configuration moved from `pyproject.toml` to `ruff.toml` and `pytest.ini`; the
  repository no longer ships a `pyproject.toml`. Hermes' plugin manager (before
  NousResearch/hermes-agent@ffe2f6411) turns every plugin directory that contains a
  `pyproject.toml` into a uv workspace member, injects a `[project]` table without `version`,
  `uv lock` fails and the plugin is auto-disabled — `memory.provider` is cleared in every
  profile. Installs that copied the whole repository instead of `plugin/` were hit by this.

## [1.7.0] — 2026-10-29

### Fixed
- `hermes ctx get` used the nested `{"data": {"id"}}` request form that current CTX
  servers reject ("Missing required field: id"); it now uses the client's top-level
  `id` contract with the legacy fallback.
- `hermes ctx sync` / `store` upserted without tags, which reset the tags of mirrored
  blocks to `[]`; they ignored `sensitivity_default` (hard-coded `personal`) and `store`
  never printed the block id.
- `hermes ctx sync` split `MEMORY.md`/`USER.md` on every `§`, cutting entries such as
  "gemäß § 22 UStG"; only lines holding just `§` separate entries now.
- Checkpoint redaction (plugin layer): `Authorization: Bearer|Basic|Digest <credential>`
  only masked the scheme word and left the credential in the transcript part.
- `ctx_store` rejections returned as `success: false` now carry `retryable` like
  exception rejections.

### Added
- `recall_status()` for the host's recall indicator (`🧠 CTX — recalled N memories`).
- `unavailable_reason()` hint when `CTX_KEY` / `CTX_BASE_URL` are missing.
- CLI: `--tag` for `store`/`search`, `--all-types` for `query`/`search`, non-zero exit
  codes on failure, profile shown in `status`.
- Tests for the CLI, the host contract and the `get_block` wire format; ruff and pytest
  configuration in `pyproject.toml`.

### Changed
- `CTX_KEY` / `CTX_BASE_URL` are read through the host's profile secret scope
  (`agent.secret_scope.get_secret`), falling back to `os.environ` on older hosts.
- Background threads and the idle-digest timer run under the spawner's contextvars
  (`spawn_context_thread` / `ctx_bound`) — profile isolation on multiplexed gateways.
- `get_status_config()` never raises.
- The CLI reuses `CtxClient` and the config helpers instead of its own HTTP code; CLI
  writes are tagged `hook:cli`, reads honour `prefetch_types_exclude`.
- Empty direct evidence returns a no-op checkpoint receipt instead of raising, so
  fail-closed mode never blocks a compaction that has nothing to archive (P6, landed
  after the 1.6.2 import without a version bump; deviation from upstream).
- Code base formatted with ruff; README restructured.

### Removed
- Duplicate `plugin/test_p3_checkpoint_chain.py` (shipped inside the installed plugin).
- Unused `VALID_HOOKS`, duplicate `FACT_HASHES_META_MAX`, `load_previous_manifest_id`.

## [1.6.2]

State at the repository import. Includes the P4.2 noise filters for harness/stream
warnings, leaked reasoning and step-progress notices (live slip-throughs of 2026-10-24).

## [1.6.1]

- `facts_from_user` defaults to `none` (replay: 31 of 34 user-turn facts were noise).

## [1.6.0] — Session digests per segment (P5)

- One digest per segment (compaction, rolling, idle, end) instead of one block per day;
  title `Session digest <root8> seg NNN <date>`; fingerprints in
  `ctx_state/<root>.digest.json`.
- Stable root session from the compression lineage in `state.db`.
- Pruning of `ctx_state` files (`state_retention_days`).

## [1.5.0] — Fact selection and cross-compaction dedupe (P4)

- Facts are selected from the end of the batch; dedupe keys per root session in a local
  ledger and in the chain metadata (`fact_hashes`); stricter noise filters;
  `facts_from_user`.

## [1.4.0] — Transcript checkpoint chain (P3)

- Port of GottZ/ctx `ctx_checkpoint` 0.4.3: head → manifest → parts over REST, with time
  budget, rate-limit backoff and part cap.

## [1.3.0] — Prefetch protection and rejection codes (P2)

- `types_exclude` on prefetch, `ctx_query` and `ctx_search`; untrusted labelling;
  machine-readable rejection codes; `auto_memory_mirror` / `auto_session_digest` switches.

## [1.2.0] — Pre-compress checkpoint API v2 (P1)

- Declares checkpoint API v2, extracts facts from the host's evidence messages,
  fail-closed `require_checkpoint` path with receipt.

## Earlier

- Provenance tags `source:` / `profile:` / `hook:` on every write and store result
  logging (2026-10-20).
- Noise filter for pre-compress fact extraction (2026-09-24).
