-- =============================================================================
-- 196_session_digest_block_type.sql — block type 'session-digest': agent
-- session material a client files, recalled with the untrusted frame
-- (issue #289, point 4)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- WHAT THIS MIGRATION IS: registry DATA, like 136 and 143. One row in
-- context_block_types; no schema, no index, no trigger, no row of
-- context_blocks is read or written.
--
-- WHY A TYPE: the Hermes plugin ctx_checkpoint stores, with `memory: true`,
-- session digests (category agent-briefing, title "Session digest <root> seg
-- NNN <date>") and the facts it extracts at a compaction (category learnings,
-- title "fact: …"). Before this type the classifier filed a digest as
-- audit-trail (its pattern "session") and a fact as knowledge, or audit-trail
-- when the fact's words hit an audit-trail pattern. Both types are trusted, so
-- the plugin's recall and every read surface handed transcript text on
-- unframed. No type a client may claim was both recall-visible and marked
-- untrusted: tool-evidence/tool-overview describe captured tool output and the
-- plugin's recall leaves them out by default, insight is write.internal_only,
-- checkpoint is excluded from retrieval. From ctx_checkpoint 0.19.0 on the
-- plugin names this type on both writes.
--
-- WHY A MIGRATION AND NOT ONLY builtin.go: the compiled-in set alone resolves
-- the name (Registry.Reload merges builtins over the rows), so a store with
-- the type would be accepted without this row. Without it,
-- TestRegistryGolden_Integration goes red, every registry reload logs ERROR
-- "builtin row missing from table" for the name, and a re-type OUT of the type
-- records no untrusted_origin, because trg_block_retype (174) decides by the
-- registry ROWS, not by the compiled-in set.
--
-- WHY untrusted = true: a digest quotes the user's request and the agent's
-- last answer; a fact is a sentence the agent extracted from the transcript.
-- Both carry what the session read — tool output, fetched pages, foreign
-- prompts. 138's doctrine: summarising attacker-shapable output does not
-- launder it.
--
-- WHY NO write SECTION: the type is claimable — the plugin names it on its
-- writes, which is what separates it from insight. An absent
-- write.internal_only decodes to false; the builtin rows carrying a write
-- section stay exactly insight and catalog (the 148 sweep pins that).
--
-- WHY damped 0.8 WITHOUT intent patterns: measured (builtin.go
-- sessionDigestDamping has the sweep). Factor {0.3 … 1.0} x pattern list {none,
-- the ten audit-trail words, eight session phrases}, two pre-hashed classes of
-- questions read through the plugin's recall: 0.8 is the highest factor at
-- which no digest or fact took the place of a knowledge block in the first
-- five (1.0 did), and either pattern list let a digest take it at every factor
-- — a lifted type leaves the damping entirely. Questions only a digest or a
-- fact answers found it among the ten sources in 73 % of the cases at 0.8
-- (33 % at 0.6, 91 % at 1.0). A tuned factor is a registry data update.
--
-- WHY guard/dream/digest/overview all false (the tool-overview posture):
-- check=false — the plugin deduplicates its facts itself (fact ledger and the
-- chain head's fact hashes) and writes one digest per segment under a title of
-- its own; candidate=false — agent-produced text must not become the reason a
-- block a person wrote is archived; dream.linkable=false — dream links are the
-- only input the topic clustering reads; digest/overview=false — the topic map
-- is not derived from agent session summaries. guard.mode stays 'archive'
-- (dead text with check=false, pointing the safe way, as in 143).
--
-- WHY NO classify RULE: a block becomes session-digest only when its writer
-- names the type. A title rule ('session digest ' at a priority below
-- audit-trail's 20) would file the digests of plugin releases that name no
-- type as well, but title patterns are substring tests: it also moved titles a
-- person writes ("How the session digest works", the memory mirror's "memory:
-- Session digest settings …") into the type, framed untrusted and out of guard
-- and dream, and a re-type back would leave untrusted_origin on them for good
-- (174). Facts could get no rule at all: "fact: " is a substring of every
-- "Artifact: …" title. An empty classify section decodes to the default
-- priority with no rule.
--
-- WHAT THIS MIGRATION DOES NOT DO: re-type the digests and facts stored before
-- it; they keep their type (audit-trail, knowledge) until a writer names
-- session-digest for them (the plugin writes each digest and fact once; manage
-- update can re-type one). Only the plugin's origin tags and digest metadata
-- tell its rows apart from blocks a person wrote, and any client may write
-- those; a re-type in the migration would run trg_block_retype and the
-- statement triggers of context_blocks over every matched row inside the boot
-- transaction of each instance.
--
-- ENVELOPE 'v': 1 IS MANDATORY: DecodePolicy checks it before anything else; a
-- row without it fails every registry reload into the builtin fallback.
--
-- LOCKSTEP WITH internal/blocktype/builtin.go: the config below MUST decode
-- equal to the compiled-in policy; TestRegistryGolden_Integration diffs the
-- rows of the real chain against builtinPolicies().
--
-- LOCKS AND COST: one INSERT into context_block_types (11 -> 12 rows): ROW
-- EXCLUSIVE on that table and its three indexes, ROW EXCLUSIVE on
-- context_settings_audit (one audit row via=sql), the transaction advisory lock
-- of trg_block_types_embed_excluded (176) on the type's name, and one
-- ctx_settings_write notice, which every instance answers with a registry
-- reload. context_blocks is neither read nor locked. Measured on the production
-- and the CI image, with no block and with 100 000: 4-6 ms for the statement.
--
-- ON CONFLICT (name, scope) DO NOTHING = idempotent (M107 doctrine): a re-run
-- never overwrites an operator's tuning of the row.
-- =============================================================================

SET LOCAL lock_timeout = '2s';

INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('session-digest', '_global', 'Session-Digest', true, false, '{
  "v": 1,
  "retrieval": {"policy": "damped", "damping_factor": 0.8,
                "untrusted": true},
  "guard":     {"check": false, "candidate": false, "mode": "archive", "candidates": "all"},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "classify":  {}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;
