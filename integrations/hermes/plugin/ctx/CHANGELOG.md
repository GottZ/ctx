# Changelog

All notable changes to the CTX memory provider. Versions follow `plugin/plugin.yaml`.
Internal phase names (P1–P6) refer to the ctx-plugin-v2 work packages.

## [1.8.1] — 2026-10-31

Checkpoint-chain fixes shared with `ctx_checkpoint` 0.5.2. The two providers write
identical chain titles, so the title function changes in both at once.

### Fixed
- **Parts respect ctx's byte cap.** `split_chunks` cut at `checkpoint_chunk_chars`
  characters, but `/api/store` caps content at 50 KiB of UTF-8 bytes. A full part of
  Cyrillic text is ~72 KB, of CJK text ~108 KB, so every checkpoint of such a
  transcript was rejected — under `compression.checkpoint_required` the session halted
  for good. Parts are now cut at whichever limit comes first, with the largest possible
  part header reserved. The part format is unchanged (the distiller golden test holds).
- **Head lookup is narrowed to heads.** The head search filtered on
  `types=["checkpoint"]` only; parts and manifests share that type, so source parts
  talking about checkpoints or headings could push the head out of the result window and
  silently start a new chain (the in-process cache only rescues this within one process).
  The search now also filters on the `checkpoint-head` tag, which every head carries.
- **No more shared heads for different sessions.** A root session id that sanitizing or
  the 48-character cut changes (`team/alice:1` vs `team:alice/1`, or two long ids with
  the same prefix) mapped to the same head title. Such ids now get an 8-character hash of
  the full id as suffix. Ordinary Hermes ids (`20261031_120000_abcdef`, UUIDs) keep
  their titles, so existing heads are still found. Golden vectors pinned in both
  providers' tests keep the two title functions in lockstep.

## [1.8.0] — 2026-10-31

Maintained in [GottZ/ctx](https://github.com/GottZ/ctx) as
`integrations/hermes/plugin/ctx/` from this version on (imported verbatim at 1.7.1,
commit `fbd210a`, with the author's attribution; see README "License & attribution").

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
- Tests live inside the plugin directory (`tests/`), as for `ctx_checkpoint`; the
  bootstrap resolves the plugin as its parent directory.

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
