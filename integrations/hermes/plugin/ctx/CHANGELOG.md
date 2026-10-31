# Changelog

All notable changes to the CTX memory provider. Versions follow `plugin/plugin.yaml`.
Internal phase names (P1–P6) refer to the ctx-plugin-v2 work packages.

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
