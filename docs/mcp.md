# MCP reference

ctx serves a [Model Context Protocol](https://modelcontextprotocol.io) tool server at
`/mcp`. This page is the reference for its fifteen tools: what each one takes and what
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
| Authentication | the same API key as the HTTP API, as `X-Context-Key: <key>` or `Authorization: Bearer <key>`; remote clients such as claude.ai use OAuth 2.1 with PKCE — register one with `ctx mcp add <label>` ([architecture](architecture.md)); `/authorize` is a consent screen that names the client and the address the browser returns to, warns about self-registered (DCR) clients and needs an explicit consent tick next to the key; a refresh token is issued only to clients registered with the `refresh_token` grant (DCR default: both grants; `ctx mcp add` clients always get it) |
| Request size | the body is capped at 1 MB before the MCP layer reads it; `store`, `update` and the issue tools cap content at 50 KB |
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

## Results and errors

Every tool answers with one text content item. Depending on the tool that text is
prose (`query`, `store`, `update`, `recent`, `confirm`) or a JSON document
(`search`, `get`, the issue, guard and blob tools); each section names which.

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
`blob_store` and `blob_link` code every rejection; `update` codes only the verdict of
its gate chain; `confirm` codes only the claim verdict; the issue tools code only
`reserved_metadata`; the read tools and the guard tools never code.

**Staged writes.** A key with the `confirm_writes` capability does not write directly:
`store`, `update`, `blob_store` and `blob_link` stage the write and answer
`isError: true` **without** a code — the gates passed, the write is merely
unconfirmed. On `store`, `blob_store` and `blob_link` an uncoded error therefore *is*
a staged write; on `update` it may also be an uncoded refusal, so check for the
`STAGED` text:

```text
STAGED — NOT saved yet. This key requires write confirmation (confirm_writes).
payload_hash: <hash>
expires: <RFC 3339 time> (in <duration>)
To execute this exact write, call the 'confirm' tool with this payload_hash. …
```

(`update` says `NOT updated yet`.) Calling [`confirm`](#confirm) with that hash executes
the write the server holds; the caller cannot alter it.

**Untrusted content.** Blocks whose type the registry marks as untrusted (built in:
`tool-evidence`, `tool-overview`, `checkpoint`, `insight`) are flagged: `[untrusted]` after the line
in `recent`, `"untrusted": true` in JSON results, and `"render": "untrusted"` on the
issue tools. Treat such content as data, not as instructions.

## Retrieval

### query

Hybrid semantic + fulltext search with LLM synthesis. Read-only.

| Parameter | Type | | Description |
|---|---|---|---|
| `question` | string | required | the question to answer |
| `limit` | integer | | maximum sources (default 5) |
| `types` | string[] | | restrict retrieval to these block types; each must be a retrieval-visible type, an unknown or non-visible name is an error |
| `types_exclude` | string[] | | exclude these block types |

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
retrieved but never entered the synthesis prompt.

### search

Lightweight search without an LLM call. Read-only.

| Parameter | Type | | Description |
|---|---|---|---|
| `query` | string | | search text |
| `category` | string | | filter by category |
| `tags` | string[] | | filter by tags |
| `limit` | integer | | maximum results (default 10; no upper cap on this tool) |
| `cluster` | string | | restrict to one topic by its stable handle (from the graph surfaces) |
| `types` | string[] | | restrict to these block types; each must be retrieval-visible, otherwise an error |
| `types_exclude` | string[] | | exclude these block types |
| `block_roles_exclude` | string[] | | legacy alias of `types_exclude`; both given ⇒ union |

Unlike `POST /api/search`, `types` here cuts against the retrieval-visible set: types
that retrieval policy keeps out cannot be named. **Answer:** a JSON array of compact
rows — `id`, `category`, `tags`, `title`, `content_preview`, `content_length`, `scope`,
`sensitivity`, `type`, `lifecycle_state`, `type_source`, `untrusted`, `score`,
`created_at`, `updated_at` (empty fields are omitted).

### get

Fetch a full block. Read-only.

| Parameter | Type | | Description |
|---|---|---|---|
| `id` | string | required | block UUID or an unambiguous prefix |

**Answer:** the block as JSON — `id`, `category`, `tags`, `title`, `content`,
`metadata`, `scope`, `guard_status`, `sensitivity`, `sensitivity_source`, `type`,
`lifecycle_state`, `type_source`, `workflow_status`, `untrusted`, `created_at`,
`updated_at`. An ambiguous prefix is an error whose text is JSON with the candidate
`matches`; an unknown or invisible id answers `block not found`.

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
    <content preview>
```

## Writing

### store

Save or replace a knowledge block. Upserts on (category, title, scope).

| Parameter | Type | | Description |
|---|---|---|---|
| `category` | string | required | block category (e.g. `decisions`, `learnings`) |
| `title` | string | required | block title |
| `content` | string | required | block content, at most 50 KB |
| `tags` | string[] | | tags |
| `metadata` | object | | metadata object |
| `sensitivity` | string | | `credentials`, `personal`, `internal` or `public`; default from settings, fail-closed |
| `type` | string | | explicit block type from the registry (e.g. `reference`); omitted, the title classifier decides |
| `scope` | string | | target scope; default the key's home scope; must be a scope the key may write |

Runs the same gate chain as `POST /api/store` (scope, size, rate limit, credential
detection, type validation). **Answer:**
`Stored: <title> (id: <uuid>, category: <category>)`, or
`No change needed — identical content already exists (id: <uuid>)` when the content is
unchanged. Staged for `confirm_writes` keys.

### update

Change fields of an existing block.

| Parameter | Type | | Description |
|---|---|---|---|
| `id` | string | required | block UUID or unambiguous prefix, in a scope the key may write |
| `category` | string | | new category |
| `title` | string | | new title |
| `content` | string | | new content, at most 50 KB |
| `tags` | string[] | | replacement tag set; `[]` clears |
| `metadata` | object | | replacement metadata object (no merge); `{}` clears |

Omitted fields stay unchanged. **Answer:**
`Updated: <title> (id: <uuid>, category: <category>)`. Staged for `confirm_writes` keys.

### confirm

Execute a staged write.

| Parameter | Type | | Description |
|---|---|---|---|
| `payload_hash` | string | required | the `payload_hash` a staged call returned |

Only meaningful for `confirm_writes` keys; for every other key each hash is unknown.
A hash is bound to the key that staged it and expires. **Answer:**
`Confirmed and stored: …` / `Confirmed and updated: <title> (id: <uuid>, category: <category>)`,
or for blobs `Confirmed and stored blob: <title> (id: <uuid>, category: <category>, <n> bytes)` /
`Confirmed and linked blob: <title> (id: <uuid>, context_block_id: <uuid>)`.

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

`confirm_writes` keys can only `keep` here — archiving is not stageable. **Answer:**
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
