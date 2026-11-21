# MCP reference

ctx serves a [Model Context Protocol](https://modelcontextprotocol.io) tool server at
`/mcp`. This page is the reference for its seventeen tools: what each one takes and what
it answers. The tool definitions live in `go/internal/handler/mcp*.go`; the JSON
schemas a client receives from `tools/list` are generated from the same Go structs,
so the parameter descriptions below match what an agent sees.

The same operations exist over HTTP ([HTTP API](api.md)) and the CLI
([CLI reference](cli.md)); where a tool deliberately differs from its REST twin, the
tool section says so.

## Transport and authentication

| | |
|---|---|
| Endpoint | `POST\|GET\|DELETE /mcp` — Streamable HTTP, **stateless** (no session; every call stands alone), JSON responses instead of SSE |
| Server name | `ctx` |
| Instructions | sent with `initialize` (and `server/discover`), the one text every connecting agent gets before it has read any documentation. By default the text ctx ships: ctx is the agent's memory across sessions; before it answers from memory or starts work it looks things up (`search` for a lightweight lookup, `query` for a synthesized answer); what should outlast the session it saves with `store`, its source in `metadata.source`. An operator can switch it off or replace it (`mcp.instructions_enabled`, `mcp.instructions`, see [operations](operations.md#mcp-instructions-mcpinstructions_enabled-and-mcpinstructions)); a change reaches the next connecting agent without a restart. Clients decide whether they show it to the model — Claude Code does (it cuts server instructions after 2,048 characters, 4,096 since 2.1.296), Hermes does not |
| Authentication | the same API key as the HTTP API, as `X-Context-Key: <key>` or `Authorization: Bearer <key>`; remote clients such as claude.ai use OAuth 2.1 with PKCE — register one with `ctx mcp add <label>` ([architecture](architecture.md)); `/authorize` is a consent screen that names the client and the address the browser returns to, warns about self-registered (DCR) clients unless they return to a known connector callback (static allowlist or `CTX_OAUTH_REDIRECT_EXTRA`) and needs an explicit consent tick next to the key; a refresh token is issued only to clients registered with the `refresh_token` grant (DCR default: both grants; `ctx mcp add` clients always get it) |
| Request size | the body is capped at 1 MB before the MCP layer reads it — for `store_batch` that is the bound on all its items together; `store`, `store_batch` (per item), `update` and the issue tools cap content at 50 KB |
| Scope | every tool acts within the scopes the key may read or write ([multi-tenancy](multi-tenancy.md)); a key without a resolved tenant identity is refused |

A minimal client configuration (Claude Code):

```json
{
  "mcpServers": {
    "ctx": {
      "type": "http",
      "url": "https://your-ctx-host.example/mcp",
      "headers": { "X-Context-Key": "your-api-key-here" }
    }
  }
}
```

## Local stdio server: `ctx mcp serve`

`ctx mcp serve` runs the CLI as a local MCP server on stdin/stdout. It forwards to
`<CTX_BASE_URL>/mcp` with the key of the CLI configuration (environment or config
file, [CLI reference](cli.md)), so a local client needs neither a header nor an OAuth
client:

```sh
claude mcp add ctx -- ctx mcp serve
claude mcp add ctx -- ctx mcp serve --allow-files /home/me/notes
```

| | |
|---|---|
| Tools | the server's, mirrored: `tools/list` is answered from the server on every request, with its names, descriptions and schemas, so a tool added on the server appears without a CLI release. The proxy keeps no copy of the list |
| Instructions | the server's, taken into the answer to `initialize` (and `server/discover`) as they were when the proxy opened its session to the server; a change of `mcp.instructions` reaches the next `ctx mcp serve` process. A server that does not complete the connect within 10 seconds leaves that one answer without instructions (the reason goes to stderr); the tools still work once it answers |
| Answers and refusals | a call through the proxy has the effect and the answer of the same call against `/mcp`: the server's results, its tool errors and its JSON-RPC errors pass through with their values unchanged (decoded and encoded again by the go-sdk, so key order may differ and a field that SDK version does not know is not carried). Staged writes (`confirm_writes`) stage and confirm as they do on `/mcp`. The client's request `_meta` is not forwarded |
| Failures | a server that cannot be reached, a key the server refuses (`401`) and a missing configuration are answered, not fatal: `tools/list` answers a JSON-RPC error and a tool call an error result, each naming the cause (`ctx mcp serve: cannot use the server at <url>/mcp: …`, `… refused the API key of the ctx configuration (HTTP 401)`, `… no usable ctx configuration: …`). The process keeps serving; after a server restart the next request reaches it again. The key is never part of an answer, a log line or the command line, and it goes only to the origin of `CTX_BASE_URL` (scheme, host and port): a redirect within that origin is followed, a redirect anywhere else is refused before it is sent and answered with its cause (`… refused a redirect to <url>: ctx mcp serve follows redirects only within <origin> …`) |
| Session | one session to the server, opened on the first request; opening it (one `server/discover` or `initialize` exchange) waits at most 10 seconds, so a server that takes the connection and never answers ends in an error naming the wait instead of holding the requests behind it. A failure that leaves the connection intact (unreachable, `401`, a `429` or `5xx` the SDK treats as transient) keeps it; a failure that breaks it (an answer that is not JSON-RPC, a `403` or `404` from something in front of the server) drops it, and the next request opens a new one. A request waiting while another one connects gives up with its own deadline. Nothing in the proxy grows with the number of tools or the time it runs |
| stdout | carries protocol frames only; diagnostics go to stderr |

**Local files (`--allow-files DIR`, repeatable).** Without the flag the proxy offers no
file access and forwards every argument unchanged. Each `DIR` must be an existing
directory; an empty value is refused at start. With the flag, two parameters join the
mirrored schemas (a parameter sent as `null` counts as absent):

- `content_file` on every tool whose schema has a `content` string (today `store`,
  `update`, `issue_create`, `issue_comment`): the proxy reads the file and sends its
  text as `content`. Give `content` or `content_file`, not both; where `content` is
  required, the schema the proxy lists no longer requires it, since `content_file` can
  stand in. The file must be a regular, non-empty UTF-8 file of at most 50 KB (the
  server's content cap). Otherwise the call is refused and nothing is sent.
- `output_file` on `get`: the proxy writes the block's content to a temporary file
  beside the target and renames it over the target, so a reader or a second `get` to
  the same file sees a whole file, never a truncated or mixed one. A new file gets mode
  `0600`; a replaced one keeps its permission bits (a hard link to the old file keeps
  the old content). The answer is the block's JSON with
  `"output_file": {"path": …, "bytes": …}` where `content` was. The target must be
  absent or a regular file in an existing directory; this is checked before the `get`
  is sent. A `get` the server refuses passes through unchanged and nothing is written.

Both paths are confined to the given directories after symlink resolution: a relative
path resolves against the directory the server was started in, symlinks are resolved
as far as the path exists (a file that does not exist yet resolves through its
directory), and the result must lie under one of the directories, which are resolved
when the server starts. The file is then opened through Go's `os.Root` for that
directory, which refuses any component that leads outside it, so a link that leaves
the directory is refused also when it appears between the check and the open. Each
directory is opened once at start and held, so a file is always opened in the
directory that was named. Files are opened without blocking and checked to be regular
files after the open, so a FIFO put in a file's place cannot hold a call. The same
rules apply on Windows, where reserved device names such as `NUL` are refused. A stdio server that read any path a model asks for would hand the file's
content to whatever the model stores it in; the directories bound that.

## Results and errors

Every tool answers with one text content item. Depending on the tool that text is
prose (`query`, `store`, `update`, `archive`, `recent`, `confirm`) or a JSON document
(`search`, `get`, the issue, guard and blob tools, and `query` with
`synthesize: false`); each section names which.

A failed call sets `isError: true` and puts the reason in the text. Rejections from
the write gate chain — the same chain `POST /api/store` and `POST /api/blob/store`
run — additionally carry a machine code in `structuredContent`:

```json
{ "code": "scope_denied" }
```

The codes are the ones the HTTP API uses for the same class:
`unauthorized`, `invalid_body`, `missing_fields`, `invalid_sensitivity`,
`invalid_encoding`, `scope_denied`, `size_cap`, `unknown_type`, `reserved_type`,
`parent_required`, `unknown_parent`, `reserved_category`, `provenance_protected`,
`reserved_metadata`, `unknown_action`, `constraint`, `rate_limit`, `internal`.
Other errors (an unknown id, an invalid type filter, a failed lookup) carry text only,
so a client must not rely on a code being present.

Which errors carry a code is a property of each tool's write path: `store`,
`blob_store` and `blob_link` code every rejection; `update` codes the verdict of
its gate chain and the identity collision (`constraint`, see [`update`](#update)),
and, when it stages, `provenance_protected` for a target that carries the derived
layer's provenance (no card is issued for an update the confirm cannot execute);
`archive` codes `provenance_protected` and, when it stages,
`rate_limit`; `confirm` codes the claim verdict and the identity collision of a
confirmed update; the issue tools code
`reserved_metadata` and the identity collision (`constraint`, issue #275); the read tools and the guard tools never code. Any tool can
additionally answer `internal` for a server fault (below).

**Server faults.** A panic while a tool call runs — in its handler or anywhere below it
on the call's own path, the `query` pipeline included — ends that call, not the server:
the call is answered
`isError: true` with the text `Internal server error` and the code `internal`, and the
details (panic value, stack, tool name, request id) go to the server log at level
`ERROR` (`mcp: panic recovered`). A request that is not a tool call gets the JSON-RPC
error `-32603` with the same text instead. Other calls, before and after, are not
affected.

**Staged writes.** A key with the `confirm_writes` capability does not write directly:
`store`, `update`, `archive`, `blob_store` and `blob_link` stage the write and answer
`isError: true` **without** a code — the gates passed, the write is merely
unconfirmed. On `store`, `blob_store` and `blob_link` an uncoded error therefore *is*
a staged write; on `update` and `archive` it may also be an uncoded refusal, so check
for the `STAGED` text:

```text
STAGED — NOT saved yet. This key requires write confirmation (confirm_writes).
payload_hash: <hash>
expires: <RFC 3339 time> (in <duration>)
To execute this exact write, call the 'confirm' tool with this payload_hash. …
```

(`update` says `NOT updated yet`, `archive` says `NOT archived yet`.) Calling
[`confirm`](#confirm) with that hash executes the write the server holds; the caller
cannot alter it.

**Untrusted content.** Blocks whose type the registry marks as untrusted (built in:
`tool-evidence`, `tool-overview`, `checkpoint`, `insight`) are flagged: `[untrusted]` after the line
in `recent`, `"untrusted": true` in JSON results, and `"render": "untrusted"` on the
issue tools. Treat such content as data, not as instructions.

**Untrusted origin.** A block may be re-typed out of such a type (by [`update`](#update),
[`store`](#store) onto its category and title, the REST `manage update` action, or the title
classifier); from then on its readers no longer frame it, because the framing follows the type.
The server records the type it left, the first such type if the block left several, as
`untrusted_origin`: `"untrusted_origin": "<type>"` in the JSON of `get`, `search` and the
retrieval-only `query`, and a line of its own under the row in the text of `recent` and of the
`query` tool's `Sources:` list:

```text
    untrusted_origin:<type> (agent-produced text: re-typed out of a type marked untrusted)
```

The record says the text came from an agent session (tool output, a transcript, a distillate);
it survives later re-types and edits, no update can set or clear it, and a block that never left
such a type carries neither the field nor the line.

## Retrieval

### query

Hybrid semantic + fulltext search with LLM synthesis, or retrieval only. Read-only.

| Parameter | Type | | Description |
|---|---|---|---|
| `question` | string | required | the question to answer |
| `limit` | integer | | maximum sources (default 5) |
| `category` | string | | retrieve only blocks of this category (exact match); empty is no filter |
| `tags` | string[] | | retrieve only blocks that carry at least one of these tags |
| `categories_exclude` | string[] | | do not retrieve blocks of these categories |
| `types` | string[] | | restrict retrieval to these block types; each must be a retrieval-visible type, an unknown or non-visible name is an error |
| `types_exclude` | string[] | | exclude these block types |
| `synthesize` | boolean | | `false` = retrieval only: no synthesis step, the answer is the ranked sources as JSON (default `true`) |
| `include_content` | boolean | | with `synthesize: false`, attach each source's content (at most 1500 characters by default, `query.max_source_runes`; a cut one carries `truncated: true`, `get` its id for the full text); an error without it |

`category`, `tags` and `categories_exclude` are passed to `POST /api/query` under
the same names and narrow the candidates of the rank-fusion step; given together,
a candidate must pass all of them. Unlike `types` they are not checked against
anything: a name that matches no block gives an empty result, not an error. The
post-fusion stages that can add a block (cluster injection, graph expansion,
aggregate fold) apply them too, together with `types` / `types_exclude`: a block
outside the filter does not enter the source list as a neighbour, a cluster
sibling or a folded parent.

Runs the full `POST /api/query` pipeline (temporal parsing, rank fusion, temporal
gravity, rerank, synthesis). **Answer:** the synthesized answer, then a source list:

```text
<answer text with [1], [2] citations>

Sources:
[1] <title> (<category>) id:<uuid>
[2] <title> (<category>) id:<uuid>
[n/a] <title> (<category>) id:<uuid>
```

`[N]` is the citation number the answer uses; `[n/a]` marks a source that was
retrieved but never entered the synthesis prompt. A source another block supersedes
ends its line with ` superseded_by:<uuid>` (only a question with a date returns a block
whose supersedes is declared or confirmed; see `store`).

The call ends with its client: a client that closes the connection while the
pipeline runs cancels it, as on `POST /api/query`, so no further backend call or
quota is spent on an answer that can no longer be delivered. This holds on every
protocol version. Only `query` is bound to the connection this way; the other
tools are not.

With `synthesize: false` the tool passes `synthesize` and `include_content` to
`POST /api/query` under the same names and skips the synthesis step, as that route
does. The answer is then a JSON document instead of prose: `confidence` and
`sources`, the ranked sources in retrieval order, each with `id`, `title`,
`category` and `score`, plus `superseded_by`, `untrusted`, `untrusted_origin` and (with
`include_content`) `content` when set. Use it when the caller only needs the block
ids, for example to `get` them. The two fields need no privilege beyond the key's
read access, as on the HTTP route; the HTTP route's admin measurement fields
(`arm_ranks`, `pinned_*`, `shadow_types`) are not arguments of this tool.
`include_content` without `synthesize: false` is refused, because the synthesis
path never attaches content.

### search

Lightweight search without an LLM call. Read-only.

| Parameter | Type | | Description |
|---|---|---|---|
| `query` | string | | search text |
| `category` | string | | filter by category |
| `tags` | string[] | | blocks that carry at least one of these tags |
| `tags_all` | string[] | | blocks that carry all of these tags; with `tags` both apply (all of these, and at least one of those) |
| `limit` | integer | | maximum results (default 10, max 50) |
| `after` | object | | page a listing with an empty `query`: `{}` for the first page, then the `next_after` of the previous answer |
| `include_content` | boolean | | answer each block's full `content` and `metadata` instead of the preview |
| `cluster` | string | | restrict to one topic by its stable handle (from the graph surfaces) |
| `types` | string[] | | restrict to these block types: any registered type, `checkpoint` and `system-meta` included, which a full-text search without `types` may leave out; an unknown name is an error |
| `types_exclude` | string[] | | exclude these block types |
| `block_roles_exclude` | string[] | | legacy alias of `types_exclude`; both given ⇒ union |

`types` takes every type of the key's registry, including the types that `query` never
ranks (`checkpoint`, `system-meta`): the tool returns their blocks without a type filter,
so naming one narrows the answer to them. With the server's settings key
`search.leave_out_never_ranked` on (default off), a search with a `query` and without
`types` leaves out every type `query` never ranks (`checkpoint`, `system-meta` and the
derived layers `insight` and `catalog` on a stock registry), and the answer does not say
so; naming the type is then the only way to find such blocks by text. A listing (empty
`query`) keeps every type. A client that looks for its checkpoint blocks
(a checkpoint head, a passage of a saved transcript) names `checkpoint`. Unlike
`POST /api/search`, where an unknown name matches nothing, an unknown name is an error
here; a type that another tenant registered for itself is unknown. Older ctx servers
refuse a retrieval-excluded name in one of two ways: from v5.8.0 with the error
`block type "<name>" is not retrieval-visible` (the `query` tool still answers that), and
before v5.8.0, which has no `types` on this tool, with a schema error that names `types`
as an unexpected additional property. A client that has to work with both sends `types`
and, on exactly one of these two errors, the same search without it, narrowed to the rows
of the asked types — as the Hermes plugins `ctx_checkpoint` (0.16.0) and `ctx_handover`
(0.1.2) do for their checkpoint searches. `unknown block type` is not such an error: the
name is wrong, and a search without `types` would not answer the question. The retry is
best effort. The search without `types` ranks every type, so its limit can be filled with
rows of other types while matching rows of the asked type fall behind it, and a full-text
search has no next page: the narrowed answer can miss rows, or be empty, where the typed
search would find them. Narrow the query, or page an empty-query listing (`after`), which
can go on until it reaches them. The narrowing keeps a row that carries no `type`, as a
server older than that field answers, so it is not a strict type filter either;
`ctx_handover` takes such a row as the checkpoint head when its title is the head's
title. **Answer:** a JSON array of compact
rows — `id`, `category`, `tags`, `title`, `content_preview`, `content_length`, `scope`,
`sensitivity`, `type`, `lifecycle_state`, `type_source`, `untrusted`, `score`,
`created_at`, `updated_at` (empty fields are omitted).

**Limit.** One call answers at most 50 rows, as on `POST /api/search`. A `limit` above
that is lowered to 50, and the answer says so (below).

**Paging (`after`).** With an empty `query` the tool lists the blocks that match the
filters, newest `updated_at` first. Send `after: {}` for the first page; each answer
then carries `next_after` exactly when another row follows, and passing it back as
`after` (with the same filters) answers the next page. The cursor is the
`{after_updated, after_id}` position of the last row, the same cursor `POST /api/search`
takes and answers: it carries no filter and no scope, and each page applies the
filters and the read access of the call that sends it, so a cursor from another key
or from other filters only names a position. `{}` is the first page; a cursor with
only one of its two fields, or whose `after_id` is not a block id, is an error (the
REST route reads a one-field cursor as the first page). Paging orders by
`updated_at`: a block that is updated while a listing is paged moves ahead of the
cursor, so a listing that has not reached it yet does not see it; blocks added
meanwhile are newer than the cursor and are not listed, archived ones drop out. `after` next to a
non-empty `query` is an error: the full-text path answers its top matches and has no
next page.

**Content (`include_content`).** The rows are those of `POST /api/search` with
`compact: false`: `content` and `metadata` (with the guard's `guard_matched_id` dropped
when the block it names lies outside the key's read scopes, as on `get`) instead of `content_preview`
and `content_length`, through the same scope, grant and type rules, with `untrusted`
on foreign-text rows. The rows of one answer are bounded at 64 KiB of JSON, content,
metadata and escaping included: the page ends before the first row that would cross
it, and `truncated: true` says so. A block is
never cut, and the first row of a page is always answered whole, even when it alone
is larger. With an empty `query`, `next_after` continues at the first row left out;
with a `query` there is no next page: narrow the query, or list by filters with an
empty `query` and page through them.

**The page object.** A call with `after` or `include_content` answers one JSON object
instead of the array: `next_after` (absent on the last page and always with a
`query`), `limit` (the effective page size), `requested_limit` (the `limit` sent, only
when it was lowered), `truncated` (only when the 64 KiB bound ended the page), and
`results`, the rows. A call with neither keeps the array byte for byte; when its
`limit` was lowered or more rows follow, the same `next_after`, `limit` and
`requested_limit` ride beside the array in the result's `_meta`, under the key
`ctx/search_page` — not in `structuredContent`, which a client may take for the result
itself. A client that reads only the text never sees them.

### get

Fetch a block: the whole block, or only the keys and the part of the content a reader
needs. Read-only.

| Parameter | Type | | Description |
|---|---|---|---|
| `id` | string | required | block UUID or an unambiguous prefix |
| `include_archived` | boolean | | also read an archived block; full block UUID only |
| `fields` | string[] | | answer only these keys of the block (see below) |
| `content_offset` | integer | | first character of the content to answer, counted from 0 |
| `content_limit` | integer | | at most this many characters of the content |

**Answer:** the block as JSON — `id`, `category`, `tags`, `title`, `content`,
`metadata`, `scope`, `sensitivity`, `sensitivity_source`, `type`,
`lifecycle_state`, `type_source`, `untrusted`, `untrusted_origin`, `archived`,
`created_at`, `updated_at`; `untrusted`, `untrusted_origin` and `archived` appear only
where they apply. An ambiguous prefix is an error whose text is JSON with
the candidate `matches`; an unknown or invisible id answers `block not found`. An
archived block is invisible: it answers `block not found` like an id that never
existed.

`include_archived: true` makes an archived block readable again, so a block that
cites another by id stays checkable after the cited block was archived. It reads only
blocks in the key's own scopes (its home and allowed scopes) — a grant, of one block or
of a whole scope by another tenant, shares a live block and never an archived one — and
the answer carries `"archived": true`. A live block answers the
same with or without the field. With an id prefix the call is refused
(`include_archived requires the full block id …`); a prefix never resolves to an
archived block. The REST `manage get` action takes the same field, and the CLI sends
it with `ctx get --include-archived <id>`.

**Part of a block (issue #407).** `fields` names keys of the answer above; the answer
then carries `id` and exactly the named keys, plus `untrusted`, `untrusted_origin` and
`archived` whenever they apply, named or not — they say how to read the block, so a
part of it keeps them. A name outside that list (also `guard_status` or
`workflow_status`, which a by-id read never fills) is refused with the list of valid
names; an empty list is the whole block. The keys keep the order of the whole answer.
The server reads the `content` and `metadata`
columns only when the answer carries them, so `fields: ["title", "updated_at"]` never
loads a long content.

`content_offset` and `content_limit` cut a window out of the content, counted in
characters (Unicode code points), never inside one: from `content_offset` (default 0)
at most `content_limit` characters (default: to the end). With either one the answer
adds, right after `content`, `content_length`, the length of the whole content in the same unit, and
`content_truncated`, true when the content goes on after the window — the window was
then full, and the next one starts at `content_offset + content_limit`. An offset at or
past the end answers an empty `content` that is not truncated; a limit of 0 answers
an empty `content` with its `content_length`. Negative values are refused, and so is a
window beside a `fields` list without `content`. Without `fields` the window answers the
whole block around the cut content.

The options are checked before the block is looked up, so a refusal says nothing
about the block. They do not change what the key may read: an unknown, foreign or
archived id answers `block not found` and an ambiguous prefix its `matches`, exactly as
without them, and an answered block books the read record of a full read (see below).
With `include_archived` they apply to the archived block as to a live one. A call
without them answers byte for byte as before. The REST `manage get` action takes the
same three keys and answers through the same function.

### recent

Newest blocks first — for questions like "what was saved this week". Read-only.

| Parameter | Type | | Description |
|---|---|---|---|
| `limit` | integer | | maximum blocks (default 10, max 50) |
| `category` | string | | filter by category |
| `types` | string[] | | only these block types |
| `types_exclude` | string[] | | exclude these block types |
| `block_roles_exclude` | string[] | | legacy alias of `types_exclude`; both given ⇒ union |

**Answer:** one entry per block, or `No blocks found.`:

```text
[1] <title> (<category>, <YYYY-MM-DD>) id:<uuid> [untrusted]
    untrusted_origin:<type> (agent-produced text: re-typed out of a type marked untrusted)
    <content preview>
```

The `untrusted_origin` line appears only for a block that carries the record (see
**Untrusted origin** above).

### The read record of `get`, `search` and `recent`

Each block these tools hand to the caller can leave one row in `context_access_log`,
the table REST `manage get` writes as well: the reading key and its principal, the
block id, the tool as the action (`mcp-get`, `mcp-search`, `mcp-recent`; REST stays
`manage-get`), and in `metadata` the block's `type` and `block_created_at`. `get`
books every block it answers; `search` and `recent` book only the answered rows whose
type is listed in `mcp.read_log_types` (default `checkpoint,insight`). A refusal
(`block not found`, an ambiguous prefix, a type rejection) books nothing, and a page
that `include_content` ended early books only the rows it carries. No content, title
or query text is recorded. The row is written in the background: a slow or failing
access log never slows or fails the read. Details, cost and an example analysis:
[operations](operations.md#the-read-record-in-context_access_log).

## Writing

### store

Save or replace a knowledge block. Upserts on (category, title, scope).

| Parameter | Type | | Description |
|---|---|---|---|
| `category` | string | required | block category (e.g. `decisions`, `learnings`) |
| `title` | string | required | block title |
| `content` | string | required | block content, at most 50 KB |
| `tags` | string[] | | tags |
| `metadata` | object | | metadata object; the source of the knowledge (URL, file, conversation, person) goes into `metadata.source` |
| `sensitivity` | string | | `credentials`, `personal`, `internal` or `public`; default from settings, fail-closed |
| `type` | string | | explicit block type from the registry (e.g. `reference`); omitted, the title classifier decides |
| `scope` | string | | target scope; default the key's home scope; must be a scope the key may write |
| `supersedes` | string | | id of a block in the same scope that this block replaces; see below |

Runs the same gate chain as `POST /api/store` (scope, size, rate limit, credential
detection, type validation). **Answer:**
`Stored: <title> (id: <uuid>, category: <category>)`, or
`No change needed — identical content already exists (id: <uuid>)` when the write would
change nothing. Staged for `confirm_writes` keys.

With `supersedes` the written block replaces the named one, in the same transaction:
the old block stays as history (a `query` with a date still finds it, with
`superseded_by`), a `query` without a date no longer returns it, and the guard does not
archive the new block as a duplicate of the old one. The answer then ends with
`, supersedes: <uuid>)`, and a write with identical content is not answered as a no-op.
Refusals carry their class in `structuredContent.code`: `unknown_supersedes` (unknown,
archived, other-scope, malformed or self id — one answer for all), `supersedes_conflict`
(already superseded by another block — the message names the block to supersede instead
— or a cycle), `supersedes_not_allowed` (a chunk row, or a type outside the dream loop).
A staged store is checked at stage time and again at `confirm`. Details and the rules in
[api.md](api.md#endpoints), *A writer-declared supersedes*.

### store_batch

Save or update several blocks, **all or none**: every item is written in one
transaction, or none is.

| Parameter | Type | | Description |
|---|---|---|---|
| `items` | object[] | required | the blocks to write; each takes the [`store`](#store) arguments (`category`, `title`, `content` required) |

```json
{"items": [
  {"category": "decisions", "title": "Cache layer", "content": "We cache …", "metadata": {"source": "design review"}},
  {"category": "decisions", "title": "Decision index", "content": "- Cache layer"}
]}
```

Each item runs the gate chain of `store` and upserts on (category, title, scope); an
item may name its own `scope` and `supersedes`. Any refusal — of one item or of the
batch — writes nothing and answers `isError: true` with the refusal's code. A refusal
of one item names it, its text behind the prefix `items[<i>]:` (the item's position):
a gate of the item (as `store` answers it), `constraint` for two items with the same
category, title and scope, `supersedes_conflict` for two items that declare to
supersede the same block, and, decided in the transaction on the block as it is then,
`provenance_protected`, `constraint` (an explicit type onto a comment or an issue) and
the supersedes codes — the items written before it in the transaction are rolled back.
The transaction writes the items in (scope, category, title) order, and an item that
declares a `supersedes` is checked against its target as the items written before it
left that target (an item may retype it). A refusal of the batch as a whole names no
item: a batch without items answers `missing_fields` and more items than
`writes.batch_max_items` (default 100, `0` switches batches off) `size_cap`, each with
a text that opens with `items:`; a batch over the write budget (below) answers
`rate_limit` with `Rate limit exceeded: …`. An item whose write would change nothing (the `store` tool's
identical-content check) is not written and costs nothing; the batch decides that in its
transaction on the block while it holds the block's lock, and keeps the lock to its
commit, so no other write lands between the decision and the commit. Every other item
counts as one write of the key's write budget (`query.rate_limit_write`), and a batch
that does not fit into what is left of the current 60-second window is refused whole
with `rate_limit`; a batch of only unchanged items is neither counted nor refused. The
count and the booking happen inside the batch's transaction under a lock of the key, so
two batches of one key — on any ctxd instance — cannot both spend the same rest of the
window; the count measures the window back from the moment it runs, after the batch
waited for its locks. Single `store` calls do not take that lock; how far they and a batch can
exceed the limit together is in [api.md](api.md#endpoints) (`POST /api/store/batch`,
*Budget*).
**Answer:**

```
Batch written in one transaction: 2 stored, 1 unchanged.
items[0]: Stored: <title> (id: <uuid>, category: <category>)
items[1]: No change needed — identical content already exists (id: <uuid>)
items[2]: Stored: <title> (id: <uuid>, category: <category>, supersedes: <uuid>)
```

For a `confirm_writes` key the batch is **staged as one card**: one `payload_hash`
over every item — an item unchanged at stage time included, named in the stage answer
as `items[<i>]: identical content exists now (id: <uuid>) — the confirm leaves it as it
is if it still is` — and one write intent per item of the card charged in the
transaction that stages it. A batch of which every item is unchanged at stage time —
every item read on one snapshot, so all of them were unchanged at the same moment —
stages nothing, costs nothing and is answered at once like a direct batch. A staged
batch of more than one item that declares a `supersedes` is run at stage time as the
confirm would run it, then rolled back: whatever its transaction would refuse on
today's blocks — two blocks declaring each other, a target another item of the batch
retypes out of the dream loop, a derivative in the way — refuses the stage, and no
card is issued; a batch without a declaration meets such refusals at the confirm.
[`confirm`](#confirm) then runs every item of the
card in one transaction, deciding on each locked block whether it is still unchanged
(then it is left as it is, its intent spent). Before it spends the hash, the confirm re-checks each
item's scope and claims against the key and the registry of that moment — a refusal
there names the item (`items[<k>]:` in front of the confirm's wording) and keeps the
card; a refusal of the transaction itself (a supersedes target that
changed, a derivative in the way) spends it, writes nothing, and answers behind
`confirmed store_batch not executed, nothing of it is written, the stage token is consumed: `.
REST `POST /api/store/batch` runs the same core ([api.md](api.md#endpoints)).

### update

Change fields of an existing block.

| Parameter | Type | | Description |
|---|---|---|---|
| `id` | string | required | block UUID or unambiguous prefix, in a scope the key may write |
| `category` | string | | new category |
| `title` | string | | new title |
| `content` | string | | new content, at most 50 KB |
| `tags` | string[] | | replacement tag set; `[]` clears |
| `metadata` | object | | replacement metadata object (no merge); `{}` clears; keep the source of the knowledge in `metadata.source` |
| `type` | string | | new block type from the registry (e.g. `reference`) |

Omitted fields stay unchanged. **Answer:**
`Updated: <title> (id: <uuid>, category: <category>)`. Staged for `confirm_writes` keys.

A `type` is checked exactly as the REST `manage update` action checks it, with the
same codes and texts: `unknown_type` for a name the key's registry view does not
hold (a type another tenant owns answers the same), `reserved_type` for a type the
server writes itself (`insight`, `catalog`, any `write.internal_only` type), and
`parent_required` for a type that needs a parent (`comment`; this tool carries
none), and `reserved_type` for `issue` (an issue is created through the issue
tools, which write its workflow status and number). The block keeps the type as a manual choice (`type_source` `manual`): the
title classifier no longer changes it. A re-type changes what reads the block from
then on — its retrieval policy, whether the guard and dream pick it — and leaves
its content, embedding and existing links as they are.

A re-type out of a type marked untrusted is taken and recorded as `untrusted_origin`
(see **Untrusted origin** above); until issue #280 this tool refused it with
`reserved_type`. An issue or a comment keeps its type: the update answers `constraint` (409 on REST) and writes
nothing, on this tool, on [`store`](#store) onto its category and title and on the
REST `manage update` action alike. A re-type of a block the server derived answers
`provenance_protected`, as every write to it does. Both are decided by the write
itself, on the row as it is then; for a staged re-type that is the confirmed write,
so such a confirm consumes the stage token. The claim gates run at stage time and
again at confirm, against the registry of that moment: a type that left the
registry in between refuses the confirm and keeps the stage token.

A changed `title` or `content` marks the block's vector stale in the same
transaction as the update, and the backfill writes the new one. Until then the
block keeps its previous vector, so the semantic arms of a query still find it,
ranked by the text it held before the change. A `title` or `content` sent with
the value the block already holds leaves the vector as it is. The REST `manage
update` action behaves the same ([api](api.md), "Embedding on update").

An update that would give the block the `category` and `title` of another active
block in its scope is refused, and nothing is written: `isError: true`, code
`constraint`, text
`another active block in this scope already has this category and title (block <uuid>) — choose another category or title`.
Sending the same update again cannot succeed; pick another category or title, or
archive or rename the block the text names first (it lies in a scope this key reads,
so `get` returns it; the ` (block <uuid>)` part is missing when that block or the updated one
changed in the moment between the refusal and the lookup, or the lookup failed). A staged update that collides
when it is confirmed answers the same code and text behind the prefix
`confirmed update not executed, the stage token is consumed: ` — stage the corrected
update anew. The REST `manage update` action answers the same collision with HTTP 409.

An update whose `title` (or `category`) would give the block the identity of an
archived block the server derived is refused as well, with `provenance_protected`
and nothing written: the identity stays reserved for the derived layer after its block
is archived. A confirmed update refused this way has spent its stage token. A `title`
or `category` sent with the value the block already holds is not refused.

### archive

Archive a block. It leaves `query`, `search`, `recent` and `get`; the row stays, and
[`get`](#get) with `include_archived` still reads it by full id. There is no tool that
unarchives.

| Parameter | Type | | Description |
|---|---|---|---|
| `id` | string | required | block UUID or unambiguous prefix, in a scope the key may write |

It behaves like the REST `manage delete` action, which also archives: the id resolves
within the scopes the key may write (a block grant gives no write access), and a block
that carries a derived `provenance` is refused with `provenance_protected` and stays
live.
**Answer:** `Archived: <title> (id: <uuid>, category: <category>)`. An unknown id, a
block outside the key's writable scopes and an already archived block all answer
`Block not found (or not in a writable scope of this key)`. Staged for
`confirm_writes` keys: the card names the target block, and the confirm is rejected
(the stage stays open until it expires) if the block changed after staging.

### confirm

Execute a staged write.

| Parameter | Type | | Description |
|---|---|---|---|
| `payload_hash` | string | required | the `payload_hash` a staged call returned |

Only meaningful for `confirm_writes` keys; for every other key each hash is unknown.
A hash is bound to the key that staged it and expires. **Answer:**
`Confirmed and stored: …` / `Confirmed and updated: <title> (id: <uuid>, category: <category>)` /
`Confirmed and archived: <title> (id: <uuid>, category: <category>)`,
for a staged [`store_batch`](#store_batch)
`Confirmed and stored <n> blocks in one transaction:` (`, <m> unchanged` before the
colon when the confirm left items as they were) and one line per item of the card,
`items[<k>]: <title> (id: <uuid>, category: <category>)` or
`items[<k>]: No change needed — identical content already exists (id: <uuid>)` (on
`POST /api/confirm`: `{"success": true, "op": "store_batch", "blocks": [{id, title, category, scope}, …]}`,
an unchanged item as `{"action": "noop", "reason": "identical_content", "existing_id"}`),
or for blobs `Confirmed and stored blob: <title> (id: <uuid>, category: <category>, <n> bytes)` /
`Confirmed and linked blob: <title> (id: <uuid>, context_block_id: <uuid>)`.
A confirmed update that would give the block another active block's category and
title is refused after the hash was spent: code `constraint`, the [`update`](#update)
text behind `confirmed update not executed, the stage token is consumed: `.

## Issues

The issue tools write the per-project issue corpus ([issue corpus](api.md#issue-corpus)).
There is no delete. All three answer JSON:

```json
{ "success": true, "render": "untrusted", "issue": { … full block … } }
```

(`"comment"` instead of `"issue"` for `issue_comment`; the block has the fields listed
under [`get`](#get)).

### issue_create

| Parameter | Type | | Description |
|---|---|---|---|
| `title` | string | required | issue title; the `#L<seq>` prefix is added automatically |
| `content` | string | | body in Markdown, at most 50 KB |
| `tags` | string[] | | tags |
| `metadata` | object | | metadata (e.g. labels) |
| `status` | string | | initial workflow status; must be a valid entry status for the issue type (default from type policy) |

Writes the key's home scope.

### issue_comment

| Parameter | Type | | Description |
|---|---|---|---|
| `issue_id` | string | required | UUID of the parent issue, in a scope the key may write |
| `content` | string | | comment in Markdown, at most 50 KB |
| `author` | string | | author label (default `anon`) |
| `metadata` | object | | metadata |

The comment inherits the issue's scope.

### issue_state

| Parameter | Type | | Description |
|---|---|---|---|
| `issue_id` | string | required | UUID of the issue, in a scope the key may write |
| `status` | string | required | target workflow status |

The transition is validated against the type policy; an invalid target is an error,
not a no-op.

## Duplicate review

The write guard flags blocks that look like duplicates of existing ones. These two
tools work through that queue; they mirror the `guard-list` / `guard-resolve` manage
actions.

### guard_list

Read-only.

| Parameter | Type | | Description |
|---|---|---|---|
| `status` | string | | `needs_review`, `near_duplicate` or `possible_duplicate`; empty = all flagged |
| `category` | string | | filter by category |
| `types` | string[] | | only these block types |
| `limit` | integer | | maximum results (default 50, server cap 200) |

**Answer:** `{"success": true, "count": <n>, "blocks": [ … ]}`; each entry carries the
matched partner block and the similarity score, most similar first.

### guard_resolve

| Parameter | Type | | Description |
|---|---|---|---|
| `ids` | string[] | required | block ids, 1 to 500 |
| `resolution` | string | required | `keep` (clear the flag) or `archive` (archive as duplicate) |

`confirm_writes` keys can only `keep` here — a batch archive is not stageable; the
[`archive`](#archive) tool stages the archive of one block. **Answer:**
`{"success": true, "resolution": …, "resolved_count": <n>, "skipped_count": <n>, "resolved": [ … ], "skipped": [ … ]}`;
every id appears as resolved or skipped with a reason.

## Blobs

Binary or text payloads, stored next to the knowledge blocks
(`POST /api/blob/*` on the HTTP side).

### blob_store

Upserts on (category, title, scope); runs the same gates as `POST /api/blob/store`.

| Parameter | Type | | Description |
|---|---|---|---|
| `category` | string | required | blob category |
| `title` | string | required | blob title |
| `filename` | string | required | file name of the payload |
| `mime_type` | string | required | media type (e.g. `application/x-ndjson`) |
| `file` | string | one of | payload as base64 (binary data) |
| `text` | string | one of | payload as UTF-8 text |
| `scope` | string | | target scope; default the key's home scope |
| `tags` | string[] | | tags |
| `metadata` | object | | metadata |
| `context_block_id` | string | | UUID of the context block the payload belongs to |

Exactly one of `file` and `text`. **Answer:** JSON with `id`, `category`, `title`,
`filename`, `mime_type`, `file_size`, `checksum`, `scope` and, when set,
`context_block_id`. Staged for `confirm_writes` keys.

### blob_link

Attach an existing blob to a context block without rewriting the payload (phase 2 of a
two-phase write).

| Parameter | Type | | Description |
|---|---|---|---|
| `id` | string | required | blob UUID |
| `context_block_id` | string | required | UUID of the context block |

**Answer:** the same JSON as `blob_store`. Staged for `confirm_writes` keys.

### blob_fetch

Read a blob, by default a byte range of it. Read-only.

| Parameter | Type | | Description |
|---|---|---|---|
| `id` | string | | blob UUID — or give `category` and `title` |
| `category` | string | | with `title`, instead of `id` |
| `title` | string | | with `category`, instead of `id` |
| `meta_only` | boolean | | metadata only, no payload |
| `offset` | integer | | byte offset into the payload (default 0) |
| `length` | integer | | bytes to return (default 262144, max 1048576; a larger value is refused, not clamped) |

**Answer:** JSON with `id`, `category`, `title`, `filename`, `mime_type`, `file_size`,
`checksum`, `tags`, `metadata`, `scope`, `created_at`, `updated_at`, `context_block_id`
(when set) and, unless `meta_only`, the range: `offset`, `length` and either
`"encoding": "text"` with `content` or `"encoding": "base64"` with `file`.
