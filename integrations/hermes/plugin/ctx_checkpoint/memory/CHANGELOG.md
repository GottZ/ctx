# Changelog — memory engine of ctx_checkpoint

History of the memory engine (`ctx_checkpoint/memory/`). Entries up to 1.7.1 are the
releases of [W4-NERF/hermes-ctx-plugin](https://github.com/W4-NERF/hermes-ctx-plugin);
from ctx_checkpoint 0.6.0 on the engine is versioned with the plugin (`../VERSION`,
`../plugin.yaml`) and this file records engine-level changes only. Internal phase names
(P1–P6) refer to the ctx-plugin-v2 work packages.

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
  file's error text. An unresolvable reference is skipped; an `auth` (OAuth) or `command`
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
