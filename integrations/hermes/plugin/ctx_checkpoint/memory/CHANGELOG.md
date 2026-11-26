# Changelog — memory engine of ctx_checkpoint

History of the memory engine (`ctx_checkpoint/memory/`). Entries up to 1.7.1 are the
releases of [W4-NERF/hermes-ctx-plugin](https://github.com/W4-NERF/hermes-ctx-plugin);
from ctx_checkpoint 0.6.0 on the engine is versioned with the plugin (`../VERSION`,
`../plugin.yaml`) and this file records engine-level changes only. Internal phase names
(P1–P6) refer to the ctx-plugin-v2 work packages.

## [ctx_checkpoint 0.19.1] — 2026-11-26

Every REST call ends within its bound — status line, headers and body (#566).

- **The bound of a call.** `transport: rest` calls ctx through urllib, which gives the timeout to
  each socket operation on its own (the connect, every read), not to the call: a ctx server or a
  proxy that sent its answer a few bytes at a time, each within the timeout, held the call for as
  long as it kept sending. Measured against a double that sends one byte per 0.5 s for 30 s: the
  prefetch, its second query and its `type-list`, the chain's head search and writes, a required
  fact and the agent tools each ran 30 s, past their bounds — the prefetch past Hermes' 8 s wait,
  which then dropped it whole. Now every read of the answer waits at most for what is left of the
  call's timeout, each address of the connect gets an equal share of what is left among the
  addresses not tried yet (at least 0.1 s; the last one gets all of it, so an address that never
  answers — IPv6 without a route — leaves time for the next), and the request goes out within what
  was left when the connection stood (`client._DeadlineConnection`, `client._DeadlineReader`): the
  same calls end at their bounds. A host whose first address never answers and whose second does
  is reached in half the bound (the stdlib gave each address the whole timeout: once the bound). The error at the bound is the
  one a server that stops sending gives (`connection error: timed out`, over HTTPS `connection
  error: The read operation timed out`; no status, no code), so every caller reads it as before; a
  call given up on closes its connection. An answer that ends before its `Content-Length` still
  raises `IncompleteRead`.
- **The prefetch** over REST bounds its query, and a search after a failed query, by what Hermes'
  8 s wait leaves less 0.5 s, each at most `api_timeout` (default 10 s, longer than the wait): a
  slow or trickling server ends the recall at 7.5 s with nothing recalled, as a failed query does,
  instead of running past the wait, where Hermes dropped the recall and counted the call as stuck.
  Warm-ups (`queue_prefetch`) keep `api_timeout`; over MCP nothing changes (the transport's own
  timeout applies there, as before).
- **The answer's size.** A body over 64 MiB ends the call as an error (`connection error: answer
  larger than 67108864 bytes, …`), a `Content-Length` over it as soon as the head arrives; before,
  a server or a proxy that kept sending grew the process until the call's end. The largest answers
  measured against ctx's handlers for the plugin's calls: a search at the plugin's largest limit
  (10) whose rows carry the tags of blocks at the store's 1 MiB request cap 59.8 MB, a get or store
  answer of such a block 6.3 MB (its metadata grows by 6 MB with every write merged into it), a
  retrieval query at its 20-source clamp 0.26 MB, `type-list` 8 KB.
- **`get_block`** sends its form for older servers with what the first left of the call's bound
  (before: a second whole timeout).
- **Declared changes.** A call whose answer arrives after its bound fails where it used to arrive
  late, as a silent server's always did: a `synthesize: true` query (the `ctx_query` tool, `hermes
  ctx_checkpoint query`) that runs longer than `synthesis_timeout` ends there — ctx's keepalive
  space every 25 s kept it going until the answer before (raise `synthesis_timeout`, up to 300 s, for
  a slower synthesis backend); a checkpoint whose head search or write answers after `api_timeout`
  fails closed at `api_timeout`; an error answer (429, 5xx) whose body does not arrive within the
  bound keeps its status but not the server's code and text. A prefetch answer in the last 0.5 s of
  Hermes' wait is given up. The host name lookup before the connect is not bounded (the
  resolver's own timeouts apply).

## [ctx_checkpoint 0.19.0] — 2026-11-26

Digests and facts are stored as ctx's block type `session-digest` (GottZ/ctx#289, point 4).

- **The type.** Session digests and the facts of a compaction (required and best effort) are
  stored with `type: session-digest`, the builtin type of ctx migration 196: damped in recall, out
  of guard, dream, digest and overview, and marked untrusted, so every read surface frames their
  transcript text. Before, they went without a type; ctx filed a digest as `audit-trail` (its
  title holds "session") and a fact mostly as `knowledge`, both unframed in recall. The memory
  mirror keeps storing without a type.
- **A server without the type.** It refuses the write before anything is stored or booked (REST
  422 `unknown_type`; over MCP the whole text `type: unknown block type "<name>" (see manage
  type-list)`, read into the code `unknown_type` for a store that named a type —
  `mcp_client._code_for_error`, shared fixture `mcp_answers.json` section `session_digest`). The
  write is sent once more without the type, and the server entry's process-wide state
  (`EntryState.type_to_send`/`type_answered`, `checkpoint.store_typed`) keeps the writes of the
  process from naming it for an hour (`TYPE_REFUSAL_TTL_S`); then one write asks again, so a server
  updated meanwhile gets the type and one without it sees at most one refused write per process,
  entry and hour. While the first write that names it is out, other writes of the entry wait for
  its answer (`TYPE_ASK_WAIT_S`, a required fact until its deadline); only ctx's `unknown_type`
  answer is a verdict — any other error, a write that waited out the ask and a failed listing
  before it leave the question open for the next write. Each way a write goes without the type
  logs one WARNING per process and entry (a refusal, a failed listing, a wait run out).
- **The recall's second query** (`prefetch_session_digest_results`, default 3, `0` = off). The
  type is damped in recall, so a digest or a fact that alone answers a question can rank below the
  five recalled blocks. After the recall's query a second, small query that sees only the type
  adds up to that many of its blocks the first did not answer, behind the five (which stay as they
  are), each labelled untrusted. Sent only to a server entry that took a write naming the type in
  this process, never with `prefetch_untrusted: drop` or the type in `prefetch_types_exclude`,
  with its own bound (2 s) and only while Hermes' 8 s wait for the prefetch leaves room; a failure
  keeps the five. Over MCP without the grant for `call_mcp` a given-up call holds the entry until
  its tool timeout, so there the second query follows only a first query of at most 1 s
  (`SESSION_DIGEST_RECALL_FAST_MAIN_S`) and stays off for the entry for 10 minutes after one was
  given up (`SESSION_DIGEST_RECALL_PAUSE_S`, one WARNING); measured through Hermes' MCP client
  with every query at 5 s, every second recall had gone past Hermes' 8 s wait without it. Over REST the other type names (`type-list`, once per client) go into
  `types_exclude`.
- **A slow first write.** The writes of an entry wait for the one that asks. Over REST a digest
  that asks and hangs holds the required facts of a compaction that starts meanwhile for up to its
  `api_timeout` (measured 0.4 s → 20.2 s for the hook); facts its deadline then leaves unstored
  fall to the next compaction (the ledger releases them, the chain holds their wording). The ask
  keeps the client's bound: a shorter one would give up on a write that 0.18.0 completes within
  `api_timeout` and send a second one while the first may still land.
- **The required facts.** A required fact refused for its type is sent again in the same run by
  `ChainWriter.store_paced`, admitted as a call of its own: the refusal is released as an error
  answer of the entry (Hermes counts it), without a slot of the window (ctx checks the type before
  its write limit), and the write is planned again. With one error answer in a row before it, the
  write after it waits for a read alone, as every write at that limit. A checkpoint that completed
  against such a server completes (`tests/test_session_digest_type_mcp.py`, and the e2e driver
  through Hermes' MCP client against the ctx MCP handler of v5.87.0, `CTX_E2E_SERVER=before-196`).
- **Digests and best-effort facts.** They are not admitted at the entry (as before), and Hermes
  counts error answers the plugin does not see (the agent's tools, recall). Over MCP the write of
  theirs that asks is therefore preceded by a listing of its category (one row): its answer ends
  Hermes' run, so the refusal is not the third error answer in a row (a fresh reviewer reproduced
  that against v5.87.0 without the listing: two failed agent reads, the digest's refusal opened the
  breaker, the digest was lost and the next checkpoint failed). A failed listing leaves the question
  open and that write goes without the type. One listing for each write that asks (after a
  successful first listing: one per process and entry and hour on a server without the type, none
  once the answer is known); REST has no breaker and sends none.

## [ctx_checkpoint 0.18.0] — 2026-11-26

The follow-ups of 0.17.0 (GottZ/ctx#289, the list of 2026-11-24): counts read under the entry's
lock, one probe at a time, a cheap probe, the early stop judged by the transcript, a fresh deadline
check after a notice, the grant asked again per checkpoint, one reading of failure envelopes.

- **Answers between two refusals.** A refused write read the entry's count of answers after
  `EntryState.release`, outside its lock; an answer of another chain released in between was
  taken for one before the refusal, so the next refusal counted as the second in a row and the
  chain stopped one refusal early. `release` returns the count under the lock now.
- **One probe at a time.** At `MAX_RATE_REFUSALS` every writer was told to probe; a second
  writer's read then went out after the first probe had ended the run, and its failure stopped a
  chain the entry no longer held back. The admission that finds the limit hands out the read's
  ticket (`Admission.probe` with a ticket); writers that want to write meanwhile wait for its
  answer.
- **A cheap probe.** The probe searched its category for `Compaction checkpoint head`, which every
  head matches: on a test ctx ctx read and ranked all of them — at 100 000 heads 0.7–1.1 s median,
  3.7 s at p95, about 770 000 buffer hits a call; at 1 000 000 heads 15–18 s median (7.7 million
  buffer hits), longer than the plugin's 10 s bound of a call, so the probe failed there; with
  `search.leave_out_never_ranked` on, 5 ms and no row. It lists the category now (`PROBE_QUERY`
  empty, one row): 1.0–1.6 ms and 8–10 buffer hits at 100 000 and at 1 000 000 heads as at none.
  Any answer ends the run.
- **The early stop is judged by the transcript.** A run held behind earlier runs stopped before
  its first write when its last planned write would come after its deadline — the required facts
  after the chain included. A chain whose transcript fit failed for a fact that did not; the
  facts are optional writes of its reservation now (`Reservation.optional`), and a fact that does
  not fit is left for a later compaction, as after any saved chain.
- **A slow notice.** The wait after a notice counted from before the user's callback ran: a
  callback of 150 s moved a paced write 150 s past its slot, here 50 s past its deadline. The
  wait counts from the callback's return, and a notice that ends past the deadline stops the
  write there.
- **Hermes' refusal of the grant** (`PermissionError` from `call_mcp`) held until the provider
  connected again; it holds for the checkpoint that received it now (`McpClient.forget_host_refusal`
  before each checkpoint), and a restored grant is used by the next one.
- **Failure envelopes.** An `error` without `success` was a failure envelope for the REST client and
  an answer for the chain writer (it ended the entry's run of error answers and booked a slot).
  Both read it through `client.is_failure_envelope` now: an error answer. A checkpoint behind a
  proxy that answers so fails now (`… rejected: …`); before, a head search answered so counted as
  "no head" and the chain started again without a parent, and a store failed with `store returned
  no block id`.
- Pinned without a change: a registration past its run's deadline holds no place for later runs
  (also while its call is still marked waiting); a failed read before a write is the entry's third
  error answer in a row (kept, see the README); an entry that needs approval completes when its
  stores are approved in time, and a store approved after the checkpoint gave up still reaches
  ctx (the README says so).
- Documented from measurements: the cancel of `call_mcp` frees the entry within milliseconds over
  the MCP SDK Hermes ships, while ctx completes the store; two Hermes processes of one profile
  share nothing; under `plugins.isolation: host` the MCP transport does not reach ctx.

## [ctx_checkpoint 0.17.1] — 2026-11-25

- ctx's `409 write_conflict` (GottZ/ctx#547: the write met concurrent changes of the rows it
  needed and wrote nothing) is retried once after one second, like a rate limit; a tool answers
  it to the model as `retryable`. Shipped with GottZ/ctx#554; this entry was added with 0.18.0.

## [ctx_checkpoint 0.17.0] — 2026-11-24

The chains of one process share one state per ctx server entry, the required facts pace against
it, and with the operator's grant a call the checkpoint gives up on ends in Hermes as well
(GottZ/ctx#299, points 2, 3 and 5).

- **One state per server entry.** Each chain counted its own refusals in a row and its own
  writes in the rate window. Two chains of one process at the write limit (two sessions of a
  gateway, a compaction and its retry) therefore showed the entry up to four error answers in a
  row — Hermes opens its circuit breaker at the third (`_CIRCUIT_BREAKER_THRESHOLD`) and refuses
  every call to the entry for 60 s — and booked `2 × (max_parts + 2)` writes in one window.
  `checkpoint.EntryState`, one per entry and process (`entry_state`; MCP: the entry's name and a
  digest of what makes it one Hermes connection — route and credentials, the fields Hermes
  compares before one profile of a multiplexed gateway adopts another profile's connection
  (`_connection_identity`), so two profiles on one connection share one state, a timeout change
  keeps it, and an OAuth entry, never adopted, is keyed by its profile too; REST: base URL and key
  digest), admits a call only while the entry's error answers in a row plus the calls in flight
  stay below `MAX_RATE_REFUSALS` (2), and a write only while fewer than `max_parts + 2` writes are
  booked or in flight in the window. An answer ends the errors released before its call was sent:
  answers reach their threads in another order than the entry gave them, and a read answered before
  a refusal but released after it must not end it (a first version did, and two chains showed three
  refusals in a row in 2 of 15 runs). At the limit no write goes out; a read still does, alone
  (every chain starts with the head search, and a chain that wants to write then sends one read
  first; its answer ends the run). A write that failed other than by a refusal keeps its slot (ctx
  may still book an abandoned write). A chain alone behaves as before (one wait, one retry, then it
  stops). A chain plans the windows the entry's booked writes take together with its own (its
  deadline grows by them, still never past Hermes' wait), and a chain that waits for writes of
  another checkpoint before its own fill a window tells the user why. Not covered: several Hermes
  processes of one profile share nothing, each keeps its own state; other callers of the entry
  (recall, the agent's tools, ctx_handover) are not counted; an entry whose route or key changes
  gets a fresh state at once, while Hermes keeps the old connection (and its count) until it
  reconnects (`/reload-mcp`); against a ctx server before GottZ/ctx#342 a typed search is one
  refused answer and its repeat without types one call of the plugin.
- **Required facts.** They were stored after the chain unpaced and outside its count of error
  answers. They are stored in the chain's run now (`ChainWriter.store_paced`, `ChainReceipt.run`):
  the chain's plan holds their windows, each fact waits for a slot of the entry's window and counts
  in its error answers, and none runs past the chain's deadline (so none past Hermes' wait). A
  refused fact still ends the facts at once, logged, its claim released for a later compaction.
- **A call the checkpoint gives up on.** Through the tool dispatch the plugin could only stop
  waiting: Hermes' MCP handler kept running the call under the entry's RPC lock until the entry's
  own tool timeout (300 s by default), and the next compaction's head search failed behind it
  (`head search failed [timeout]`) although the server would answer at once. When the operator
  lists the entry in `plugins.entries.ctx_checkpoint.mcp_allowlist`, the checkpoint's bounded calls
  go through `PluginContext.call_mcp(server, tool, args, timeout)` with their own timeout; Hermes
  cancels such a call on its MCP loop when it runs out (`tools/mcp_tool_loop.py`
  `_run_on_mcp_loop`), which releases the lock. A memory provider's `register()` receives Hermes'
  provider collector, which forwards only `register_*` calls: `call_mcp` is taken from the context
  where it has one, else from the `PluginContext` the collector holds for the plugin
  (`_plugin_context()`); only the method of a real `hermes_cli.plugins.PluginContext` counts (under
  `plugins.isolation: host` the plugin's context forwards `call_mcp` to a collector that has none).
  `call_mcp` reaches every tool of a granted server, so a call goes through it only for a tool
  Hermes registered for the entry (`tools.registry`: what its `tools.include`/`tools.exclude`
  filter left); any other goes through the dispatch as before. An answer Hermes cut at its 64 KB
  plugin cap fails the call (the largest head and manifest reads stay far below it). With the grant
  a call Hermes gives up on reads `MCP call failed: TimeoutError: …` instead of the plugin's
  `no answer within …; the call was abandoned`. Without the grant, on a Hermes without `call_mcp`
  and after Hermes refused the grant (`PermissionError`, before any transport work; remembered
  until the provider connects again) the calls go through the dispatch as before. Recall and the
  agent tools are unbounded calls and keep the dispatch.
- **Setup** offers the grant on every surface ("Let Hermes end a ctx call the checkpoint gives up
  on": the Desktop memory form, the web dashboard form, `hermes memory setup`), writes or removes
  only this entry in the list, and the terminal setup's summary names it — and says when the
  loading Hermes hands the plugin no `call_mcp`, so the grant has no effect there.
- **Chains side by side are served in the order they started** (review of the pull request).
  Each chain planned its windows from the writes booked when it started, not from the writes the
  chains beside it would make; the free slots went to whichever thread came first, and every chain
  whose next slot lay past its deadline stopped. In virtual time with Hermes' 300 s wait: two
  chains of ten writes at `max_parts` 3 both failed, four of twelve chains of eight writes at the
  defaults, fifty chains of eight writes at the defaults all fifty. Every run now registers the
  writes it plans at the entry (`EntryState.register`; the writer ends the registration of a
  failed chain and of one without required facts, the provider the one its facts continue, on
  every way out, and a registration holds no place past its run's deadline). A run plans its
  deadline behind the booked, in-flight and planned writes of the runs that started before it
  (never past Hermes' wait); a write goes out only while the window has a slot beyond those
  planned writes; at the limit of calls in flight a later run's call gives way to a waiting call
  of an earlier one. When the chains together do not fit Hermes' wait, the first ones complete; a
  chain that would fit from an empty window but not behind them stops before its next write with
  `other checkpoints of this Hermes process started first …`, and a chain too long for the wait
  even alone saves what fits after them. A chain alone and chains one after another behave as
  before.
- **Errors** (same review). A call that raised in the client (RuntimeError, MemoryError,
  KeyboardInterrupt, SystemExit) released its place already; tests over the provider pin it now
  on both transports. A failure envelope the REST client returns instead of raising (`success:
  false`, a body that is no JSON) counted as an answer and ended the run of error answers; it is
  an error answer now and a write keeps its slot (ctx may have stored it) unless its code is
  `rate_limit`. The shared count stopped every chain once two of them were refused one after the
  other (other writers holding the key's window: two chains of twenty writes at the defaults with
  90 foreign writes in ctx's window both failed, the base completed both): a chain now gives up on
  a write only when ctx refused it twice with no answer of the entry in between, and at the limit
  of two error answers in a row a chain that wants to write first sends one read alone
  (`ChainWriter._probe`), whose answer ends the run in the plugin's count and in Hermes' breaker.
  So a failed manifest read directly before a refusal no longer costs a chain its retry. Writes
  never draw a third error answer in a row; a read sent at the limit (a head search, the read
  before a write) goes out alone, and when it fails its chain ends — the wording before said no
  more than two error answers in a row for every call. A chain held behind others follows their
  delay (a refusal that cost one of them a window) up to Hermes' wait instead of stopping at the
  deadline it planned at its start, and of a run that cannot finish within its own deadline only
  the writes it can still send count ahead of the chains after it.
- **Setup** (same review). The entry's key follows Hermes' own `_connection_identity` where the
  loading Hermes has it (two entries Hermes treats as one connection — tool filters in another
  order, `ssl_verify: true` named, an empty `env` or `args`, `strict_redirect_headers: false`,
  another `tools` key — shared two states before); on a Hermes without it the raw fields as
  before. The web dashboard offers the grant as keep (the default), on and off: it sends back
  every field it loaded, and an unrelated save restored a grant taken back elsewhere meanwhile.
  `hermes memory setup` asks for the grant after the operating mode and names its current state.
  With `transport: rest` the grant has no effect: setup no longer offers or writes it, and the
  summary says so (it said "yes"). An `mcp_allowlist`, or a section above it, that is not of the
  shape Hermes reads is no longer replaced with `["ctx"]`: setup refuses the change with a message
  naming the key.

## [ctx_checkpoint 0.16.2] — 2026-11-19

A checkpoint ends inside Hermes' wait for the compaction, a retry adds no window it does not
use (part of GottZ/ctx#299), and a head whose tags were replaced still continues the chain
(part of GottZ/ctx#289).

- **Hermes' wait.** Hermes calls `on_pre_compress` in the compaction worker before the summary
  call and waits for that worker from its last progress, which only the summary stream reports
  (`compression.context_timeout_seconds`, raised to the summary call's own timeout: 300 s by
  default). A paced checkpoint past that window kept writing while Hermes abandoned the worker
  and ran the compaction again, so two chains of one root wrote at the same time — five windows
  with the defaults (327 parts), fewer with a smaller `max_parts`, and any chain with a
  `budget_s` above the window. `checkpoint.host_compaction_wait_s` reads the window from the
  loading Hermes (`resolve_context_compression_timeouts` of `agent.conversation_compression`,
  the function the host itself calls; `None` on a Hermes without it, with the wait switched
  off or on any failure, and then nothing changes). The provider reads it before its clock
  starts and gives the chain a limit 10 s before the window (`HOST_WAIT_MARGIN_S`): the
  deadline, every paced window and the window a refusal grants stop there, and so do the
  required facts after the chain. A chain that needs more stops before the next write with
  `ctx checkpoint stopped before <step>: Hermes waits <n>s for a compaction to make progress
  …`; the parts it stored are kept, and the next attempt continues from them (ctx confirms
  them without a write). The notice before the second window says so, and the failure notice
  names `compression.context_timeout_seconds` instead of `budget_s`.
- **Windows a retry does not use.** A retry whose parts ctx confirms books no write for them,
  yet its receipt said `windows=3` and the user was told again that the compaction waits
  minutes: both came from the part count before the first write. The receipt's `windows` now
  counts the windows the booked writes entered (every `max_parts + 2` of them), and the notice
  comes before the first write that enters the second window — a retry that is all confirmed
  parts reports `windows=1` and sends none. The deadline still holds every window of the plan
  (up to Hermes' wait): a confirmed part costs one call, and a retry walks all of them before
  it reaches the new ones — with only the one-window budget, a retry over 230 confirmed parts
  at 260 ms per call would never reach its new parts.
- **A head without its tag.** The head search names the tag `checkpoint-head`; a head whose
  tags an update or a store outside the plugin replaced was not found, and the next checkpoint
  started a new chain without a parent and replaced the head silently. When the tagged search
  finds nothing, the writer searches the head title once more without the tag (the first
  checkpoint of every root costs one search more): a block there with a valid
  `latest_manifest_id` continues the chain, logged as a WARNING, and the new head carries the
  tag again (`lookup=untagged`); one without is an invalid head as since 0.12.0 (notice, the
  new head published over it, `lookup=head-invalid`).

## [ctx_checkpoint 0.16.1] — 2026-11-18

No output names the credentials of a configured URL (GottZ/ctx#311), and the code of an MCP
tool error comes from the form of ctx's answer (GottZ/ctx#354).

- **Status, CLI and system prompt on `transport: rest`.** `hermes memory status`
  (`base_url`, summary), `hermes ctx_checkpoint status` (`base URL:` and the `ctx_base_url`
  of `ctx.json`) and the memory block of the system prompt printed the base URL as written,
  user info, query and fragment included. They show it through `status.display_url` now, as
  the MCP status did.
- **`display_url`** never returns the text of a URL it cannot take apart: without
  `scheme://host` (`user:pass@host/mcp` came back as `user://pass@host/mcp`), with an `@`
  after the host (a `/`, `?` or `#` in the user info ends the host early:
  `https://user:12#pass@host` came back as `https://user:12`), an unreadable host or port —
  `(unreadable URL)`. A URL starting with a `${VAR}` template also loses user info after `://`.
- **Error texts.** `redact_error_text` (every error text the plugin raises, logs, records in
  the outcome file, shows in the status or hands to the model) replaces every URL with user
  info, a query or a fragment by its display form: a transport error naming the MCP entry's
  URL kept its fragment and query values that no credential rule names. The connection test
  also masks the entry URL's parts where the error repeats them without the URL, and
  withholds the error of a URL that cannot be taken apart and holds an `@`.
- **REST client.** A base URL with user info, a query or a fragment never reached the API
  (urllib read the password as the port and repeated it in its error, or looked the user
  info up as part of the host; a query or fragment came before every request path). It is
  refused before any connection now — also with an `@` after a `/` of the user info — with a
  text that names no part of the URL.
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
