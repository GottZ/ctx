# Changelog — memory engine of ctx_checkpoint

History of the memory engine (`ctx_checkpoint/memory/`). Entries up to 1.7.1 are the
releases of [W4-NERF/hermes-ctx-plugin](https://github.com/W4-NERF/hermes-ctx-plugin);
from ctx_checkpoint 0.6.0 on the engine is versioned with the plugin (`../VERSION`,
`../plugin.yaml`) and this file records engine-level changes only. Internal phase names
(P1–P6) refer to the ctx-plugin-v2 work packages.

## [ctx_checkpoint 0.16.1] — 2026-11-18

No output names the credentials of a configured URL (GottZ/ctx#311), and the code of an MCP
tool error comes from the form of ctx's answer (GottZ/ctx#354).

- **Status, CLI and system prompt on `transport: rest`.** `hermes memory status`
  (`base_url`, summary), `hermes ctx_checkpoint status` (`base URL:` and the `ctx_base_url`
  of `ctx.json`) and the memory block of the system prompt printed the base URL as written,
  user info, query and fragment included. They show it through `status.display_url` now, as
  the MCP status did.
- **`display_url`** never returns the text of a URL it cannot take apart: without
  `scheme://host` (`user:pass@host/mcp` came back as `user://pass@host/mcp`), with an `@` in
  the path (`https://user/x:pass@host`), an unreadable host or port — `(unreadable URL)`. A
  URL starting with a `${VAR}` template also loses user info after `://`.
- **Error texts.** `redact_error_text` (every error text the plugin raises, logs, records in
  the outcome file, shows in the status or hands to the model) replaces every URL with user
  info, a query or a fragment by its display form: a transport error naming the MCP entry's
  URL kept its fragment and query values that no credential rule names. The connection test
  also masks the entry URL's parts where the error repeats them without the URL, and
  withholds the error of a URL that cannot be taken apart and holds an `@`.
- **REST client.** A base URL with user info, a query or a fragment never reached the API
  (urllib read the password as the port and repeated it in its error, or looked the user
  info up as part of the host; a query or fragment came before every request path). It is
  refused before any connection now, with a text that names no part of the URL.
- **Error codes over MCP.** `rate_limit` was set for every tool error holding `rate limit` or
  `rate_limit` — a search refusing a type of that name, a transport error mentioning one —
  with status 429: the chain writer waited a whole window (61 s), then failed as
  "rate-limited", and the agent tool told the model to retry. Now only ctx's whole answer
  `Rate limit exceeded: max <n> … per 60 seconds` is a rate limit. `confirm_writes` likewise
  only for an answer starting `STAGED — NOT`, no longer for a store error naming the word.

## [ctx_checkpoint 0.16.0] — 2026-11-18

Checkpoint searches name the `checkpoint` type, over MCP too (GottZ/ctx#342).

- **Why.** ctx plans to leave the types that retrieval never ranks (`checkpoint`,
  `system-meta`) out of a full-text search that names no type: those searches cannot use the
  full-text indexes, which leave both types out since ctx migration 145, and read every
  block's text vectors. A search that names the type keeps finding checkpoint blocks then.
  Until now the MCP client dropped `types`, because the ctx MCP `search` tool refused a type
  that retrieval never ranks; a ctx server with #342 takes it.
- **MCP `search` sends `types`.** The head lookup of the chain writer
  (`ChainWriter.load_previous_head`: title, category, tag `checkpoint-head`,
  `types=["checkpoint"]`) and every other search that names types send them. A server before
  #342 answers `block type "checkpoint" is not retrieval-visible` (ctx v5.8.0 to v5.52.x) or,
  before v5.8.0, `unexpected additional properties ["types"]`; on exactly that refusal of the
  sent types `McpClient.search` searches again without `types`, within what is left of the
  call's timeout, and keeps the rows of the asked types and the rows without a `type` (a
  server older than that field names none, so this is no strict type filter). The fallback is
  best effort: the search without `types` fills its limit with every type, so rows of the
  asked type can fall behind it.
- **What is remembered.** The refused type per client (or that the argument itself was
  refused): a search that names other types still sends them, and an older server, which takes
  the types retrieval ranks, answers it typed. A search whose types were refused before goes
  without them first and asks with them again when its narrowed answer may differ from the
  typed one — an asked type has no row, or the narrowing dropped rows — so a server updated
  meanwhile is not read through the fallback; a typed answer clears the memory. Against an
  older server that costs one refused call per binding, plus one for such a search: the head
  lookup by title, category and tag costs one call when it finds the head and two when it finds
  none. A row without a `type` means a server that predates the filter: no ask again. The ask
  takes at most half of what is left of the call's timeout (`EXTRA_ASK_SHARE`) and is not sent
  below 0.5 s (`MIN_EXTRA_ASK_S`), so the chain writer keeps time to read the head; when it
  fails, the answer stays the one the older server gave, except `unknown block type`, which is
  raised as it is on a first search. Any other error (an unknown type, a rate limit, another type
  than the sent ones) is raised as before. The refusal's log line names the server, not the type
  values. The REST client sent `types` already.
- **An unknown type is raised on every way.** In a search with `types`, ctx answers
  `unknown block type "<name>"` only for a name of those types, so the client takes that text
  for the refusal whatever name it quotes, and raises it on the first search and on the ask
  again alike. The name is not read: Go quotes it with `%q`, and Hermes' error handling can
  mask it (a name holding `sk-`, such as `task-list`) or cut it. Before, only a name's first
  word was read and compared with the sent types, and the ask again answered such a refusal
  with the older server's rows. The check comes before the one for a never-ranked type, so a
  name holding that refusal's words is no fallback either.
- **A staged write is a store's answer only.** The staging refusal (`code="confirm_writes"`,
  `staged, not stored`, the hash, the advice to use another key) is read from a tool the
  server stages (`store` here). An error of `search`, `get` or `query` that echoes
  `confirm_writes` — a search for a type of that name — is raised as that tool's error,
  uncoded; before, it was reported as a staged write, and the ask again took it for a failing
  ask. The staged store's message is unchanged.
- **The agent's read path names the type.** The passage search of the system prompt block and
  the compaction receipt: `ctx_search … types=["checkpoint"]` with memory features on,
  `hermes ctx_checkpoint search … --type checkpoint` on REST without them, and
  `mcp__<server>__search … types=["checkpoint"]` with the instruction to search without
  `types` if the server refuses the type (the agent calls that tool itself, no client falls
  back for it). The skill `ctx-memory` says the same.
- **`ctx_search` takes `types`.** Named types replace the `prefetch_types_exclude` default
  (which lists `checkpoint`) unless `types_exclude` is given; the CLI's `search` takes
  `--type` (repeatable) with the same rule.
- **Unchanged:** every search without `types`, and every answer of a server with #342 to the
  head lookup (the head carries the type).

## [ctx_checkpoint 0.15.0] — 2026-11-18

A source part is stored once per identical write (GottZ/ctx#344).

- **The same write is one part.** Measured on the reference instance: 44 % of all checkpoint
  characters were exact copies of a part ctx already held — 34 % across conversations (one
  burst wrote 23 roots with the same 34-part transcript), 10 % repeats of one transcript by
  the writer before 0.10.0. Every part named its root, active session, transcript digest and
  position in its header and title, so no copy could meet the stored one. A part's title is
  now the first 32 hex characters (128 bits) of the SHA-256 of its whole write
  (`Compaction source part <32 hex>`,
  `checkpoint.part_title_for`: category, content after redaction, metadata, sensitivity, tags,
  type), and header and metadata name no conversation: the header is the same for every part,
  the metadata keeps how and where the text was captured (`source_kind`, `platform`,
  `agent_context`, the host contract, `tool_index_status`, `warnings`, `invalidated_by`,
  `ctx_plugin` with writer, profile and redaction passes) plus `part_sha256`. The writer's
  version stays on manifest and head: a part is shared only for byte-identical text, and the
  version in its write would end the sharing at every release. Reuse needs the identical
  complete write, not only the same text: another compaction of the conversation (Hermes
  hands every compaction the whole live transcript, so the protected first turns come
  again), a branch (a root of its own that carries its parent's turns) or another
  conversation of the profile that holds the same text with the same platform, agent
  context, host contract, redaction passes and sensitivity gets ctx's "No change needed"
  with the stored id — no write booked, so it also takes nothing from the write window — and
  its manifest lists that id. No server change: ctx's identical-content check already
  compares title, content, metadata, tags, sensitivity and type, and scope (one tenant per
  scope) and category are part of its upsert identity, so the same text in another scope or
  category stays another block, and so does the same text with another agent context or
  sensitivity. A store under a part title can only meet the payload the title names, so a
  part another manifest lists is never rewritten.
- **The manifest carries the conversation.** Manifest and head keep `root_session_id`,
  `active_session_id` and `sha256` as before; the manifest keeps the parts in order
  (`source_block_ids`, the position of a part is its place there), the head keeps
  `latest_manifest_id` and `source_block_count`. A part's `part`/`parts` keys and its
  `Root session`/`Active session`/`Transcript SHA-256`/`Part: i/n` header lines are gone. The distiller cuts the header at `## Direct transcript`
  and reads parts by id in the manifest's order, as before.
- **Finding a passage.** The read path in the system prompt, the receipt and the skill
  searches by the words alone (`query="<words>"`) and names the rule: a hit is from this
  conversation when one of its manifests lists the hit's id; without such a hit the agent
  reads the listed parts (and those of the parent manifests) before it concludes a passage
  is not saved. The root token narrowed the search to one conversation's parts; a shared
  part carries none, and a search with a query answers its top matches without a cursor, so
  other conversations' parts can fill the page.
- **Existing chains.** Nothing rewrites existing parts in bulk; they keep their titles and
  headers. A retry of the checkpoint a head names rewrites that manifest in place, as every
  such retry has since 0.10.0: after a 0.14.0 checkpoint, a 0.15.0 retry of the same
  transcript replaces the manifest's `source_block_ids` with new-format parts. Every listed
  part is stored before the manifest is written, so the head names a complete list of the
  same transcript even when the head write after it fails, and the parts listed before stay
  stored. An older plugin never reads parts, only head and manifest.
- `ChainWriter._store` sends the tags it is given; the writer resolves them through
  `tags_for` once, so a part is stored with exactly the tags its title digest covers, whatever
  callable the writer was given.
- Archiving or deleting a part now reaches every manifest that lists it.

## [ctx_checkpoint 0.14.0] — 2026-11-16

Redaction keys of status and CLI, installer tests, untrusted_origin in the recall
(GottZ/ctx#308, part of #295).

- **Status, connection test, setup and CLI redact with the keys the provider binds
  (GottZ/ctx#308).** `status.profile_ctx_keys(name)` is the one resolution of a profile's
  ctx keys: the `Authorization`/`X-Context-Key` value of `mcp_servers.<name>` as written
  (`${VAR}`/`${env:VAR}` resolved through Hermes' `get_env_value`, `.env` included) and as
  Hermes loads it, and `CTX_API_KEY`. It moved from the umbrella into the engine module
  that is stdlib-only at import time and uses the same three `hermes_cli.config` functions
  it already called (`load_config`, `read_user_config_raw`, `get_env_value`), so the CLI
  reads it without a provider and no Hermes import was added. The provider binds it on
  connect (seam `_profile_secrets`); `hermes memory status` and `hermes ctx_checkpoint
  status` on MCP (`mcp_report`) and on REST (`get_status_config`, `cmd_status`), the
  connection test of the Desktop form and of the terminal setup, `Setup failed: …` (now with
  a redacted DEBUG traceback instead of `exc_info`) and every error line of the CLI on
  `transport: rest` (`search`, `get`, `query`, `store`, `sync`, `status`, a raised
  `CtxApiError`) redact with it — plus `CTX_KEY` on REST, as the provider — through
  `checkpoint.redact_error_text`. The connection test adds the keys of the entry it probes
  (`status.entry_keys`) to whatever its caller passes. Before, status and test knew
  `CTX_API_KEY` or the key just typed, the CLI `CTX_KEY` only: a key the entry referenced
  through another variable, or a `password:` value, passed. A CLI error line is now one line of at most 300 characters after
  redaction (`status.redact_error`), the `whoami failed` cause of the REST status at most 140.
- **Installer tests outside a checkout.** The seven tests of
  `integrations/hermes/scripts/install.sh` skip with a reason when the script is not next to
  the plugin folder and the folder is not in a ctx checkout (`go/go.mod` four levels up), as in
  a catalog install, a flat copy such as `/plugins/ctx_checkpoint` included (it failed
  collection before); there five failed and two passed only because bash could not find the
  script. In a checkout they fail when the script is missing: the tests that run it assert it
  exists, so the staging and move tests no longer pass on bash's own non-zero exit.
- **untrusted_origin in the recall (part of GottZ/ctx#295).** ctx v5.41.0 records on a block
  re-typed out of a type marked untrusted the type it came from (`untrusted_origin`, #280).
  `config.untrusted_origin` reads the record (a value that is no type name keeps the mark
  as `?`), `config.untrusted_origin_note` words it as the server does, and
  `config.with_untrusted_origin_checked` puts the checked value into an answer passed on whole
  (`ctx_get`, `hermes ctx_checkpoint get`, the MCP client's note line and content reload).
  The prefetch block puts the note line under the hit's label and one sentence into its intro; `ctx_query`,
  `ctx_search` (`_trust_fields`) and `ctx_get` carry the field and an `origin_note`; the CLI
  hit lines carry the note line. It is a note, not untrusted framing: the owner's decision
  on #280 lets the corpus take such knowledge up, so `prefetch_untrusted: drop` keeps the hit.
  The MCP query text parser dropped every source line with text after the id, so a source
  with ` superseded_by:<uuid>` was lost; `key:value` tokens after the id are kept now,
  `superseded_by` is read, the indented note line under a source is its record, and the
  content reload copies `untrusted_origin` too. Without the record (a server before v5.41.0)
  prefetch block, tool results and CLI lines are byte for byte as in 0.13.0. The shared answer
  fixture (`tests/mcp_answers.json`) gains the section `origin`, which the ctx side renders
  through its real MCP handlers.

## [ctx_checkpoint 0.13.0] — 2026-11-16

Setup, MCP tool names and error texts (GottZ/ctx#294, #297, #304).

- **The required mode needs a passing test on every surface (GottZ/ctx#294).** The Desktop
  form ran the connection test before it wrote `compression.checkpoint_required: true` but
  refused the save only when the test failed; when the loading Hermes could not run it
  (`ConnectionTest.ok is None`: `_probe_single_server` missing or without `connect_timeout`)
  the mode was written anyway, while the terminal setup offers it only after a passing test.
  `_apply_form` now refuses that save with the reason and how to set the mode once
  `hermes mcp test <entry>` passes; nothing is written. Saves without the mode, and turning it
  off, still need no test.
- **MCP tool names as Hermes registers them (GottZ/ctx#297).** Hermes registers the tools of
  an `mcp_servers` entry with every character outside `[A-Za-z0-9_]` replaced by `_` and a name
  over 64 characters shortened with a hash suffix (`tools.mcp_tool_schema.
  mcp_prefixed_tool_name`); the plugin built `mcp__<server>__<tool>` from the raw entry name,
  so an entry such as `ctx-prod` answered `Unknown tool` to every call, and since 0.9.0 the
  system prompt block and the receipt named tools that do not exist. `mcp_client.mcp_tool_name`
  now follows Hermes' rule (that function is not a public plugin API, so the rule is rebuilt and
  the tests pin it against Hermes' function); dispatch, prompt block, receipt and the failure
  hint read it. An entry is refused when another enabled entry can register a tool under the
  name of one of its checkpoint tools (`mcp_client.colliding_servers`): both names come out
  alike, or `<own>__` starts with `<other>__` after the rewrite (`ctx--prod` next to `ctx`, whose
  tool `prod__store` would be `mcp__ctx__prod__store`). Hermes keeps the first registration of a
  name, so a call by name could reach the other server; config.yaml does not list the other
  server's tools, so the rule takes every tool name as possible — a test checks it against
  Hermes' function for every pair of 130 entry names — and the provider reports itself unavailable with
  the reason, and the connection test (setup forms, `hermes memory status`) fails before it
  connects. A passing test names the tools when the entry name is rewritten. The pointer
  binding stays on the entry name; for `ctx` and every name Hermes keeps, names and binding
  digest are byte-identical.
- **The rest of the unredacted error texts (GottZ/ctx#304).** `raise … from None` inside an
  `except` block leaves the original as the implicit `__context__` (only hidden from printing),
  so the chain writer's error still held the raw `CtxApiError`, and the REST client's error the
  raw `HTTPError`/`URLError`/`ValueError` (the last with the bytes repr of a rejected key).
  These errors — the chain writer's `_call`, the REST client's `_request` (HTTP failures now in
  `_http_failure`), the MCP client's dispatch failure, the crashed-chain error and the
  redaction failure of `render_direct_evidence` — are now raised after the handler, without
  cause or context. `McpClient` takes the provider's bound keys (`secrets`) and redacts the
  dispatch error, the error envelope and the bodies of `store`/`get` answers without a block
  where they enter the plugin. The agent tools (`ctx_store`, `ctx_search`, `ctx_get`,
  `ctx_query`) put the redacted text and code into the tool result and the log; their DEBUG
  lines, and those of the digest triggers, carry `checkpoint.redacted_traceback` instead of
  `exc_info`. The prefetch, warm-up, content-fetch and `whoami` lines are redacted with the
  bound keys. Decisions read the raw values, never the redacted ones: a failing redactor
  withholds every text, so `ctx_store` decides `retryable` on the raw code, rejection codes are
  shown through `checkpoint.redact_code` (an empty code stays empty, ctx's closed vocabulary and
  the clients' own `confirm_writes`/`timeout` stay verbatim, any other code is redacted), and the
  MCP client tells a server from before `synthesize: false` by the refusal as it arrived
  (`_RetrievalArgsRefused`), so the fallback to `search` survives a failing redactor.

## [ctx_checkpoint 0.12.0] — 2026-11-16

Gate and setting edge cases with the memory features on (GottZ/ctx#289).

- **`checkpoint_chain: false` under a required checkpoint.** The required path passed on facts
  alone: with no fact a receipt without any write, with facts at most five `learnings` blocks,
  and no word of the conversation in ctx. Facts are not a transcript checkpoint: with
  `require_checkpoint` and the chain switched off, `on_pre_compress` raises
  `CtxCheckpointError` (`CHAIN_OFF_REQUIRED`), so Hermes keeps the uncompressed transcript.
  Best effort is unchanged (facts are stored, the skip notice names `checkpoint_chain: false`).
  `status.checkpoint_chain_warning` (next to `checkpoint_mode_warning`, both through
  `setting_warnings`) names the setting and its source at session start and in
  `hermes memory status`.
- **Facts after a stored chain.** A refused fact store failed the required checkpoint although
  the chain was durable, so a deterministic refusal blocked every later compaction; the fact
  stores ran without a timeout (over MCP Hermes' tool timeout, 300 s), the deadline was checked
  only before each fact. Each required fact store now carries `max(0.5, min(api_timeout, time
  left))`; after a stored chain a refused or late fact is a WARNING, the claims of the facts
  not stored are released and the receipt says `k of n stored`. Without a stored chain a fact
  failure still raises. Since 0.10.0 a retry meets the chain's own titles, so a repeated
  attempt does not write the parts again; the required facts are still not paced (#299).
- **A head without a valid `latest_manifest_id`.** A head that keeps the tag `checkpoint-head`
  but whose metadata lost `latest_manifest_id` (an update replaces the whole object) raised
  before any write and blocked every later checkpoint of its root until it was repaired by
  hand. `load_previous_head` now logs it and records it on the run (`invalid_head`): the new
  chain starts without a parent (or with the process' cached last manifest), the head is
  published over the broken one, `lookup=head-invalid`, and the user gets a notice naming
  both heads. Its dedupe keys are not taken over; the earlier chain's blocks stay. An invalid
  head under the 0.6.0 title is only logged (the new head goes under the current title). A block
  with the head title but without the tag is still not recognized as a head: the new head
  replaces it and the chain starts without a parent (unchanged; the MCP store answer does not
  say whether it replaced a block).
- **Engine names next to plugin keys.** `overlay_from_umbrella` mapped the section in file
  order, so `checkpoint_sensitivity: personal` next to the `sensitivity: internal` that setup
  writes counted only when it came later. An engine name now wins over its plugin key in either
  order, and the memory forms write their value to an engine name present next to the key, so
  a save takes effect.
- **A fact that was not stored is stored by a later compaction.** The head names the dedupe keys
  of the facts selected for it (`fact_hashes`) before they are stored, so after a failed store a
  later compaction read those keys from the head and dropped the fact for good (in best effort
  since P4). The fact ledger now keeps the keys of a failed store as `pending`
  (`ctx_state/<root>.json`); the head does not count them as stored, and the next compaction of
  that root on this profile stores the fact. Another host reading the head still skips it.
- **On REST a call timeout bounds the 429 retry.** `CtxClient` retried a 429 once after 1 s with
  the full timeout again, so a required fact store or a chain write could start its retry after
  the checkpoint deadline. The timeout now bounds the whole request; the retry is made only while
  the wait fits and gets the time that is left. A rejection code is redacted like the error text.
- **Error texts are redacted before they are raised or logged (GottZ/ctx#301).** A failed chain
  write put the server's or the MCP dispatch's error text into the `CtxCheckpointError` it raised
  (and into its `__cause__`) and into the WARNING line without redaction; the outcome file and the
  user notice were already masked. `checkpoint.redact_error_text` (bound keys, a piece of a bound
  key that a client cut off, then the checkpoint redaction; withheld on a redaction failure) is
  the one function for it: the chain writer builds its errors from it and raises them
  `from None`, the provider logs chain, fact, mirror and digest store failures through it
  (`_error_text`), and `status.redact_error` uses it as well. The cause hints of the user notice
  read the redacted text and still apply.

## [ctx_checkpoint 0.11.0] — 2026-11-15

The redaction removes the ctx key on the MCP transport and more secret forms, and keeps code
and prose it used to overwrite (GottZ/ctx#288).

- **The ctx key on MCP.** Without a REST key the bound secret was empty, so a ctx key written
  bare, as `X-Context-Key: <key>` or as that header in JSON reached the checkpoint parts while
  every part header said credentials were excluded (0.10.0 recorded it as a known limit). The
  umbrella provider now resolves the key the `mcp_servers` entry signs in with on every
  connect — its `Authorization` / `X-Context-Key` headers, `${VAR}` and `${env:VAR}` through
  Hermes' `get_env_value` (`.env` included), the auth scheme dropped — plus `CTX_API_KEY`,
  and replaces them (`_bound_secrets()`) in parts, mirror, facts, digests and the outcome
  file's error text. A re-initialization of the same instance keeps the keys of its earlier
  bindings (at most 16, `MAX_REMEMBERED_SECRETS`): a write scheduled through the old client
  redacts when its thread runs, and a later text may still carry the old key. A transcript
  that contains such an earlier key therefore redacts it in this process and not in a fresh
  one (its digest differs there); for the same bound keys the output stays byte-identical.
  An unresolvable reference is skipped; an `auth` (OAuth) or `command`
  entry has no key to resolve. Bound values are applied longest first; values under 8
  characters are not bound (a stray short header value would erase a common word).
- **Host strict mode.** `redact_sensitive_text` runs with `redact_url_credentials=True`, as
  Hermes does for its own compaction summary: signed URLs (S3, Azure SAS, GCS), OAuth
  `?code=` and URL user info. The parameter is older than the floor
  `requires_hermes >=0.23.1` (it is present from Hermes 0.22.0 on).
- **Plugin rules judge the value.** The generic key/value rule replaced every value behind
  `token`, `secret`, `password`, `api_key`, … and changed 15 of 15 harmless code and prose
  lines of the audit. The value is now judged by its shape: attribute paths, calls,
  subscripts, bit-width types, references (`${VAR}`), placeholders (`<…>`), comparisons and
  prose words stay; a value with letters and a digit, a value behind an auth scheme, and the
  remnants of a Hermes mask (`8f3a9c...9c2e`, `***&3`) go, digits alone from 4 digits behind a
  password name or PIN and from 6 behind a token name (`PIN 4711`, a one-time code; not `the
  password is 12 characters long`, not `token: 4096`). Password names (also `Passwort`,
  `Kennwort`, upper-case `PIN`, `PIN-Code`) mask any value with a digit or a non-word
  character after `:`, `=` or `:=`, and a value with a digit in prose (`the password is …`,
  `mit Passwort …`); after a password word Hermes masked the rest of the line goes too
  (`password: 8 characters minimum` keeps only `password:`); phrase names (`passphrase`, seed phrase,
  mnemonic, recovery codes or key, backup codes) mask the rest of the line; token names add
  `creds`, `credentials`, `client_secret` and `X-Context-Key`, and `_` / `-` separate words
  (`MY_SERVICE_CRED=`). A label may sit in markdown emphasis (`**Password:** …`,
  `**API key**: …`), and every name takes the verb form (`the token is …` with letters and a
  digit, `Passwort lautet: …`). Calls, subscripts and lower-case attribute paths may carry
  digits (`base64.b64encode(raw)`, `self.token2`), and typed integers (`Int64`, `Uint8Array`)
  stay. The MCP key is also read from the entry as Hermes loads it (expanded, managed overlay),
  and a reference may carry spaces (`${ VAR }`), as Hermes allows. Discord and Slack webhook URLs lose their path token; a
  standalone `Bearer` value is masked only when it is shaped like a credential (`Bearer
  authentication` stays). A value is judged on at most 1024 characters; a longer unbroken run
  behind a name is masked whole, so the work per name stays bounded and the rules stay
  linear in the text length. Measured on the corpora of `tests/test_redaction_coverage.py`:
  0 of 20 audit secret forms leave a fragment (0.10.0: 18), 0 of 8 further forms (0.10.0: 7); of the
  audit's 22 forms 2 pass unchanged (0.10.0: 19); the plugin rules change 0 of 66 harmless
  lines (0.10.0: 38; Hermes' own pass changes 8). A 2-million-character transcript renders
  about 8 % slower (1.78 s → 1.92 s).
- **Letters-only values.** 0.10.0 masked every value behind `token`, `secret`, `password`,
  `api_key`, `authorization`, …; Hermes masks a letters-only run only at the start of a line, or
  from 20 characters or two letter cases on, so behind a name in mid-line such a value would
  have passed. A value of at least 12 letters (`-_./` between them allowed) is masked unless it
  has the shape of words or an identifier: camelCase humps of three or more letters, each part
  with at least 15 % vowels and at most 4 consonants in a row. Measured on seeded samples:
  about 10 % of random letter runs (12–64 letters) have that shape and pass, about 2 % of the
  stdlib's long identifiers do not and are masked (`StrFormatStyle`). A value in prose (`the
  token is …`) still needs a digit.
- **Known limits, named in the READMEs.** A credential without a name (a bare hex or base64
  value other than the bound keys), a plain-word password, a letters-only value with word shape, a value the name does not directly
  precede, a mixed-case `Pin`, a table cell, card numbers and IBANs pass;
  Hermes' own pass still rewrites some code lines and masks URL parameters named `key`,
  `code`, `session` or `auth`.
- **Consequence for chains.** The transcript digest is taken over the redacted transcript, so
  a conversation checkpointed again after the update gets another digest, checkpoint id and
  titles and starts a new chain next to the old one. Parts stored before keep their 0.10.0
  redaction; nothing rewrites them.
- The part header says what is masked ("credentials in a known shape, and this profile's ctx
  key where its server entry names it, are masked") instead of "credentials are intentionally excluded"; `ctx_plugin.redaction`
  names the passes (`host:redact_sensitive_text(force=True, redact_url_credentials=True)`,
  `named-value`, `webhook-path`, `bearer`).

## [ctx_checkpoint 0.10.0] — 2026-11-15

A long transcript no longer stops a required checkpoint for good, and a retry continues
where the failed attempt stopped (GottZ/ctx#287).

- **Paced instead of refused.** `checkpoint_max_parts` (plugin key `max_parts`) now means
  parts per 60 s rate window: the chain writes at most that many parts plus manifest and
  head inside one window (what 0.9.0 wrote in one burst) and waits for the next window
  with the rest. Until 0.9.0 a transcript with more parts was refused before its first
  write, and under `compression.checkpoint_required` every later compaction of the session
  failed the same way, since the transcript only grows. The budget (`checkpoint_budget_s`)
  covers one window; each further window the part count needs adds 61 s. Before the first
  wait the user gets a notice through Hermes' warning callback (the compaction waits).
  The waits run between calls, never inside one: Hermes' per-server RPC lock stays free.
- **Idempotent retries.** The checkpoint id is `sha256(root \0 transcript digest)[:16]`
  (it hashed the time before), so a retry of the same transcript writes the same titles.
  Parts no longer carry `evidence_date` (manifest and head keep it): the retry sends the
  identical write, and ctx answers "identical content" without booking a write — the
  server's confirmation that title and content (SHA-256) match. A retry of the same
  transcript (Hermes' retry of a stalled compaction, `/compress` without new messages)
  reuses the parts a failed attempt stored (`reused` in the receipt and the log line); a
  transcript that changed in between has another digest and other titles, and the parts of
  the failed attempt stay unreferenced as before. A part title names the SHA-256 of the
  part's exact content (`Compaction source <root> <sha16> <cpid> <part sha16> part NNN of
  NNN`): a retry that cuts the same transcript elsewhere (another `chunk_chars` or byte
  budget) or renders another header (another active session of the root) writes new blocks
  instead of rewriting a part a published manifest references, which a later failure of
  that retry would have left as a mix of two splits. A manifest title names its parent (`… after <parent id> manifest`), so an
  existing manifest is never re-written under another parent; when the head already names
  this transcript's checkpoint (the compaction after it did not land), the manifest is
  re-written in place under its own parent instead of pointing at itself.
- **Rate-limit waits sized to the window.** A refused write used to back off 2/4/8 s and
  give up after 14 s, whatever the budget, after four refusals in a row. It now waits until
  a slot is free — when the chain's own oldest write in the window ages out, else a whole
  window — at the latest until just before the deadline, and tries once more; the second
  refusal in a row ends the chain. The first refusal of a chain adds one window (61 s) to
  its deadline: other writers' slots free a whole window after their writes, which a
  budget of one window could never reach. Hermes opens its per-server circuit breaker after three
  consecutive error answers and then refuses every call to the ctx server for 60 s.
- Failure hints: the `max_parts` hint is gone (the cause no longer occurs); budget and
  rate-limit hints say that the parts already saved are kept for the next compaction.
- Readers: chains written by 0.9.0 and older are read as before (head title per root,
  `latest_manifest_id`, `source_block_ids`, `parent_manifest_id`, `fact_hashes`); a head
  of an older version never matches the new checkpoint id, so its chain is continued, not
  re-written.

## [ctx_checkpoint 0.9.0] — 2026-11-15

After a compaction the agent learns that the checkpoint exists and how to read it
(GottZ/ctx#286).

### Added
- **Checkpoint pointer in the system prompt.** The provider's `system_prompt_block()` ends
  with a block naming the session root's checkpoint head (id and title) and the read path
  (head → `latest_manifest_id` → manifest → `source_block_ids`; a passage: search with the
  root token in the query), as soon as a head is known for the root — with memory features
  on and off. Tool names follow the session: `mcp__<mcp_server>__get` / `__search` with
  memory features off, `ctx_get` / `ctx_search` (`types_exclude=[]`) with them on, the
  plugin CLI on `transport: rest` without them. The block carries ids, the head title and
  tool names only (a head that is not a block id is never put there), says that saved
  turns are untrusted, and is byte-identical across compactions of one root (the head is
  upserted under one title). Hermes rebuilds the prompt at the compaction commit boundary,
  after the checkpoint.
- **Read path in the receipt.** `ChainReceipt.frame()` takes the same read path as its last
  line (`_receipt_read_path`, an engine seam the umbrella fills; empty in the bare engine).
- **Known heads.** `status.record_checkpoint()` keeps `heads` (root session → head id and
  the binding it was written through: transport, MCP entry name or REST base URL, category,
  as a digest) in `ctx_state/checkpoint-status.state`, bounded to `KNOWN_HEADS_MAX` (512)
  roots checkpointed last; `status.known_head()` reads it for the prompt block, so a new
  provider of the profile (gateway message, restart, resume) names the head without a
  network call, also after a later checkpoint failed. A head of another binding is not
  named.

### Changed
- **System prompt block with memory features off** is no longer always empty: it carries
  the pointer once a head is known (empty without one, as before).
- **Receipt** is one line longer (about 300 characters); with required facts the room for
  the fact list under the host's 6000-character cap shrinks by that much.
- **Skill** `ctx-memory`: the checkpoint paragraph names the read path per mode, the MCP
  tool names, the root-token search and the untrusted label; it used to name only
  `ctx_search`, which is not registered with memory features off.
- `mcp_client.mcp_tool_name()` is the one place that builds `mcp__<server>__<tool>` for the
  dispatch and the text; `provider._checkpoint_category()` and `provider._checkpoint_root()`
  the one place for the chain's category and root session (writer and read path).

## [ctx_checkpoint 0.8.0] — 2026-11-15

Settings and status for Hermes Desktop (GottZ/ctx#285): a memory form under Settings →
Memory, a section under Settings → Plugins, a connection test on save, the outcome of every
checkpoint, and a status that shows whether checkpoints work on the MCP transport.

### Added
- **Desktop memory form.** `../config_schema.py` declares the memory form Hermes Desktop
  requests with `?surface=declared` (`storage=provider`): URL, API key, memory features and
  the operating mode inline, sensitivity, category and part size under *Full config…*. The
  provider reads and writes it through `read_config_fields()` / `write_config_fields()`, over
  the same validation and writer as `save_config()`. `get_config_schema()` (web dashboard,
  terminal) is derived from the same field table; a unit test keeps labels, descriptions and
  defaults equal to the plugin's `_DEFAULT_*` values.
- **Settings → Plugins section.** `../plugin.yaml` declares `budget_s`, `max_parts`,
  `auto_recall`, `auto_memory_mirror`, `auto_compress_extract` and `auto_session_digest`;
  Hermes stores them in `plugins.entries.ctx_checkpoint.settings`. `config.umbrella_section()`
  reads them over `memory.ctx_checkpoint` (precedence: defaults ← `ctx.json` ←
  `memory.ctx_checkpoint` ← the Plugins form, for those six keys only; a form value also
  replaces the engine name of its key there, e.g. `checkpoint_budget_s`), and the existing
  clamps apply unchanged. The provider and `hermes ctx_checkpoint` share that function.
- **Connection test.** `status.run_connection_test()` uses Hermes' own MCP probe
  (`hermes_cli.mcp_config._probe_single_server`, behind `hermes mcp test`) with a connect
  timeout of 15 s (or the entry's own longer `connect_timeout`), and needs the ctx tools
  `search`, `get` and `store`. The Desktop form runs it when a save changes the URL or the
  key (once both are known) and before turning the operating mode on; the terminal setup
  before writing anything. A failure refuses the save with a readable, redacted reason. A
  Hermes without the function, or with another signature, counts as "cannot test"
  (`ok: None`) and blocks nothing. The web dashboard's form does not test: it saves the key
  only after `save_config()` returns.
- **Operating mode.** The Desktop memory form carries `compression.checkpoint_required`
  as *Wait for the checkpoint before compacting*; the terminal setup explains both modes
  after a passing test and asks for one; the status shows it. The plugin writes the key
  only when the choice differs from the current value (merged into `compression`,
  thresholds untouched). The web dashboard's form leaves it out (`desktop_only` in the
  field table) and `save_config()` ignores it: that form sends every field with the value
  it loaded, so it would switch back a mode changed elsewhere.
- **Outcome of every checkpoint.** `on_pre_compress` records time, mode, head id, part count
  and the redacted error (or why no chain was written) and the session kind
  (`<platform>/<agent context>`) in `$HERMES_HOME/ctx_state/checkpoint-status.state`,
  atomically, mode 0600, as `last_checkpoint`, and keeps the latest failure as
  `last_failure`; the name is no `*.json`, so `prune_state_dir` keeps it. Writers of one
  profile hold an exclusive `flock` on `checkpoint-status.lock` (at most 2 s, then they write
  unserialized; on Windows a process-local lock) around reading `last_failure` and replacing
  the file, so a success cannot put back an older failure. Recording never raises.
- **Status on the MCP transport.** `get_status_config` (`hermes memory status`) and
  `hermes ctx_checkpoint status` print the same report: server entry, connection test,
  operating mode, last checkpoint, warnings. `hermes ctx_checkpoint status` exits with 1 when
  the test fails. The server URL is shown without user info, query and fragment, also when
  it starts with a `${VAR}` template.
- **Reason for the user.** A failed checkpoint names its cause (part limit, time limit, MCP
  server not connected, rate limit, `confirm_writes`) through the `warning_callback` Hermes
  passes on CLI-platform sessions, once per distinct cause; a best-effort compaction that ran
  without a checkpoint and without an error (provider not connected, `checkpoint_chain: false`,
  `checkpoint_mode: off`) says so the same way.
- **Warnings.** `checkpoint_mode: off` (from `memory.ctx_checkpoint` or `ctx.json`) is
  logged at session start and shown in the status, with the source and — under
  `compression.checkpoint_required` — that every compaction is refused (logged once per
  process and profile). Plain `http://` to a host outside loopback and the private networks
  is warned about in setup and status.
- **CI.** `.github/workflows/hermes-ctx-checkpoint.yml` runs ruff, `hermes plugins validate`
  and both suites against hermes-agent at a pinned commit.

### Changed
- `requires_hermes` is `>=0.23.1` (was `>=0.20.4`): from v0.23.1 on `delegate_task` children
  inherit the provider under `compression.checkpoint_required`; on 0.21.1 to 0.23.0 every
  sub-agent compaction failed closed. The catalog checks the floor at install time only.
- The engine resets `_last_chain_receipt` at the start of every `on_pre_compress` (an early
  return kept the previous receipt) and keeps the cause of a failed chain in
  `_last_chain_error`.
- A package-managed install refuses setup with `ConfigReadOnlyError`, a `RuntimeError` and a
  `ValueError`, so the Desktop route answers 400 with the reason instead of 500.
- The Desktop form refuses a URL for a stdio server entry (it has none) instead of dropping it.
- **`ctx.json` checkpoint settings apply again.** Up to 0.7.0 the umbrella put its defaults
  for `category`, `sensitivity`, `chunk_chars`, `budget_s` and `max_parts` into its section
  whenever `memory.ctx_checkpoint` did not set them, and that overlay replaced
  `checkpoint_category`, `checkpoint_sensitivity`, `checkpoint_chunk_chars`,
  `checkpoint_budget_s` and `checkpoint_max_parts` from `ctx.json` (a `ctx.json` budget of
  120 s ran with 60 s). The plugin's defaults now sit under `ctx.json` (`load_ctx_config(...,
  base=...)`, the provider's `_config_base()` seam); the section carries only what is set
  there. The memory forms show the value in effect and fall back to it, so a save keeps a
  `ctx.json` value (copied into `memory.ctx_checkpoint`). A profile whose `ctx.json` sets one
  of these keys now runs with that value.

## [ctx_checkpoint 0.7.0] — 2026-11-09

`transport: mcp` recalls with the semantic hybrid of `/api/query`, as `transport: rest`
does, when the ctx server's MCP `query` tool takes `synthesize: false` (ctx v5.36.0 or
newer, GottZ/ctx#183). Older servers keep the lexical `search` recall.

### Changed
- **Retrieval-only recall over MCP uses `query` without synthesis.**
  `McpClient.query(synthesize=False)` (prefetch recall, `ctx_query` without synthesis)
  called the lexical `search` tool, because the MCP `query` tool always ran the LLM
  synthesis. It now calls `query` with `synthesize: false` and, when content is asked
  for, `include_content: true`, passing `limit` and `types_exclude` as before. The tool's
  JSON answer (`confidence`, `sources[]` with `id`, `title`, `category`, `score`,
  `superseded_by`, `untrusted`, `content`) maps to the dictionary `CtxClient.query`
  returns for `/api/query` (`success`, `answer: ""`, `confidence`, `sources`); the
  provider and the agent tools read it unchanged. Rows that are not objects are dropped;
  an empty or `null` list is `[]`.
- **What MCP users see differently.** As on REST, the recalled rows carry no `type`:
  the client-side `prefetch_types_exclude` check has nothing to match on this path (the
  server applies `types_exclude`), and `ctx_query` without synthesis no longer lists a
  `type` per source. Query errors (rate limit, quota, an answer that is not the sources
  document) now reach `ctx_query` as errors, as on REST; the prefetch falls back to
  `search` for that call, as it does on REST.
  - At most five recalled rows: the `query` tool answers 5 sources when no `limit` is
    given (`search` answered 10), and the recall gives none, as on REST (`/api/query`
    has the same default and `CtxClient.query` sends no `limit`). A
    `max_prefetch_results` of 6–10 takes effect only on the `search` path (the
    fallback and `prefetch_mode: search`); on the `query` path 5 is the cap on both
    transports.
  - Block text reaches the prompt over MCP for the first time: the compact `search`
    rows carry `content_preview`, which the recall does not read, so it rendered
    titles only; the `query` rows carry `content` (up to 1500 characters from the
    server), and the recall renders up to 500 characters per hit, rows marked
    `untrusted` included under their label (dropped with `prefetch_untrusted: drop`).
  - Without the synthesis step is not without a model call: the server embeds the
    question, counts the call against the tenant quota, runs the reranker where one is
    configured, translates a question its heuristic takes for non-English (German,
    Swedish, Danish, Finnish) with the LLM, and may run the temporal LLM fallback;
    `search` did none of this. Measured on one deployment, reranker off: English
    questions about 200 ms, German about 730 ms, a lexical `search` about 220 ms.
- **Fallback for servers older than ctx v5.36.0 (before #183).** Their `query` tool
  refuses the two arguments at schema validation (`unexpected additional properties
  ["synthesize" "include_content"]`), which Hermes hands back as `{"error": …}`. The
  client serves that call by `search` and keeps to `search` for later retrieval-only
  calls. Only one call asks: retrieval-only calls made while it is in flight use
  `search` too, so an older server sees one refused call per client, not one per
  overlapping prefetch, warm-up or `ctx_query` (each refusal would count against
  Hermes' per-server error breaker). Any other error leaves the question open for the
  next call. A new client (the provider connecting again) asks once more.
  `query(synthesize=True)` is unchanged.

### Known limits
- A standing error that is not the refusal (tenant quota, rate limit, no eligible
  embedder) costs one failing `query` and one `search` on every turn for as long as it
  holds; there is no cooldown. Each failing call is a strike on Hermes' error breaker,
  and the `search` that succeeds right after it resets the count.
- The retrieval-only `query` call has no client-side bound (REST bounds it at 10 s).
  Hermes gives a prefetch 8 s and lets the call run on; MCP calls to one server are
  serialized by Hermes' per-server RPC lock, so a long recall holds checkpoint stores
  and `mcp__ctx__*` tools behind it. A bound through `_run_bounded` would abandon the
  call without releasing that lock, so none is set.

## [ctx_checkpoint 0.6.2] — 2026-11-01

`transport: rest`: the ctx key no longer leaves the client in error texts, follows no
redirect and is redacted from automatic writes, and a re-initialized provider keeps
nothing of its previous client. The recall-prefetch invalidation, the warm-up pin and the
manifest-cache binding are transport-independent and apply to `transport: mcp` as well.

### Security
- **The key is redacted from every REST error text.** A ctx server or a proxy in front of
  it that repeats the request's `X-Context-Key` in an answer put the key verbatim into
  `CtxApiError` texts and codes, into the provider's log lines, into `exc_info` tracebacks
  (the original `HTTPError`/`URLError` was chained via `raise … from exc`) and, through
  tool errors, into the model context. `CtxClient` now replaces the key with `[REDACTED]`
  in the HTTP error body (`error`, `message`, nested error objects, plain text), in the
  `code` (codes from the closed `REJECT_CODES` vocabulary stay verbatim), in `exc.reason`,
  in `URLError`/`TimeoutError`/`OSError` texts and, recursively in every string value, in
  HTTP 200 failure envelopes (`success: false`, or a non-empty `error` without
  `success: true`). Texts are redacted
  before the 300-character cut, so no key prefix survives it. Errors are raised
  `from None`; their content is in the message. The debug line for an unreadable error
  body names only the exception type (its `exc_info` traceback chained the unredacted
  `HTTPError`). Successful answers are returned
  unchanged, even for a one-character key; when a `success: true` answer also carries an
  `error` key, only that value is redacted. New public helpers `redact_secret` /
  `redact_deep` in `memory/client.py`; the whoami warning in the provider redacts as
  well, for clients that are not a `CtxClient`.
- **Two more exception classes are redacted.** `http.client.HTTPException` is no
  `OSError`: a proxy's non-HTTP status line (`BadStatusLine`, whose text is that line and
  can reflect a header value) left `_request` raw, into tool errors and DEBUG tracebacks;
  it is now a redacted connection error. A key with an inner CR/LF made `http.client`
  raise a `ValueError` carrying the key's bytes repr (its escapes defeat exact matching)
  into the whoami warning, `hermes memory status` and `hermes ctx_checkpoint status`; it
  is now a `CtxApiError` with a fixed message that names no value.
- **Redirects are refused.** urllib's default opener follows 301/302/303/307/308 (a POST
  answered with 301/302/303 is repeated as a GET) and copies every request header,
  `X-Context-Key` included, into the request to the `Location` without comparing hosts,
  and the provider became active against the redirect target. The ctx API never
  redirects; `CtxClient` now opens requests through an opener that follows no redirect,
  and a 30x becomes `HTTP 30x: redirect not followed …` without the `Location`.
  Pre-existing.
- **Automatic writes redact the bound key.** Neither the host redactor nor the generic
  key/value and bearer rules know a minted ctx key (64 hex characters) or the
  `X-Context-Key` header form, so a key pasted into the conversation was stored verbatim
  in compaction checkpoint parts and in mirror writes (content and title). `redact_text`
  and the chain writer take the bound key, and mirror, fact and digest writes apply it;
  without a key (`transport: mcp`) the redaction is unchanged. Pre-existing.
- **The verified binding keeps a digest.** The binding identity kept for the manifest
  cache held the key as a second plain-text copy, also after a re-init that ended
  inactive; it now holds the key's SHA-256.

### Fixed
- **Re-initialization drops the previous client.** `_connect()` (REST) reset nothing
  before binding: a re-init that ended inactive (no `CTX_KEY`/`CTX_BASE_URL`) kept the
  old client with the old key, the whoami identity and the checkpoint writer on the
  instance, and a failed whoami kept the previous identity. It now clears `_active`,
  `_client`, `_chain_writer`, `_api_key`, `_base_url`, `_home_scope` and `_whoami_label`
  first. The MCP `_connect()` of the umbrella binds a new client and every identity field
  on each call, holds no key and never ends inactive; `_chain()` replaces a writer whose
  client is not the current one, so it is unchanged.
- **Re-initialization and session switches drop caches of the previous binding.**
  `initialize()` did not clear the recall prefetch: a re-initialized instance could serve
  the previous profile's recall for up to 60 s, and a warm-up still in flight could
  publish into the new binding — defense for hosts outside the contract (Hermes v0.22
  builds a fresh provider per agent and initializes it once). `on_session_switch` cleared
  the cache, but a warm-up in flight across the switch still published afterwards; that
  is the path Hermes v0.22 reaches. The
  cache is now invalidated together with a generation counter, at the start of a re-init
  and again after its connect (a warm-up queued in between still used the old client),
  and on session switches; a warm-up publishes only into the generation it started under,
  and it queries the client it was queued with (query and search fallback), not the one
  bound when it runs (the generation is read before the client, so a re-init in between
  never pairs the previous client with the new generation); `prefetch()` reads the client
  once, so its gate and its query see the same binding. The
  manifest-lineage cache (`_last_manifest_by_root`, the parent fallback when the server
  has no head) is dropped when the profile home, backend, key or whoami identity of a
  successful connect differs from the last verified binding, so a manifest id from another
  binding never becomes a parent; a re-init with the same binding keeps it, and so does a
  failed re-init (inactive, no writes) followed by a successful retry on that binding.

### Compatibility
No configuration or wire change. Error messages and failure envelopes differ only where
they contained the key; callers that matched on `CtxApiError.__cause__` get `None` now.
Credentials are still read through `env_secret` only (no `.env`/`os.environ` fallback of
its own — the host's fail-closed secret scope stays the contract). A 30x answer from the
base URL is an error now instead of being followed: point `rest_base_url`/`CTX_BASE_URL`
at the ctx server itself.

### Known limits
- `redact_deep` leaves dict keys untouched; an error object rendered through `str()` can
  escape a key containing backslashes or quotes before matching (ctx mints hex keys);
  `__context__` of a raised `CtxApiError` still references the original exception
  (`from None` only suppresses it — `traceback` and `logger.exception` output is clean,
  explicit traversal reaches it).
- Under `transport: mcp` the binding identity is the profile home plus the MCP server
  name. A backend switch under the same server name — also through Hermes' MCP
  auto-reload in the CLI or `/reload-mcp` in the gateway, where no `initialize()` runs —
  is not detected; the first manifest on the new backend then points once to a parent
  that does not exist there.

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
