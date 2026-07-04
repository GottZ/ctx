# HTTP API

All endpoints under `/api/*`. Auth via `X-Context-Key` header or `Authorization: Bearer` token.

## Endpoints

| Endpoint | Description |
|----------|-------------|
| `POST /api/query` | 4-Way RRF + LLM synthesis (auto-backfills pending embeddings). Optional `categories_exclude` / `types_exclude` arrays filter slot-stealers (`block_roles_exclude` = documented legacy alias for `types_exclude`, both ⇒ union). Optional `sensitivity` classifies the query text for trust gating (default settings key `pool.default_query_sensitivity`). Optional `include_content` attaches a ≤1500-char snippet per source on the retrieval-only path (`synthesize:false`, default off so eval/sweep responses stay byte-identical — the chat harness's ctx_query tool sets it). Whenever synthesis is on (`synthesize != false`), the response commits `200` up front and streams a whitespace keepalive every 25s so buffering proxies don't hit their read timeout; the body stays valid JSON (leading whitespace, RFC 8259) and a late synthesis failure reports `success:false` inside the 200 body. |
| `POST /api/store` | Upsert (embedding async via scheduler). Optional `sensitivity` (`credentials`\|`personal`\|`internal`\|`public`) classifies the block manually (`sensitivity_source='manual'`); absent ⇒ settings key `pool.default_block_sensitivity` (fail-closed `credentials`). On an upsert conflict an explicit value applies upgrade-only — downgrades go through `manage update` with `confirm_sensitivity_downgrade`. A credentials pattern in the content forces `credentials` upgrade-only regardless of the requested level (G40 detector, `sensitivity_source='pattern'`). Optional `type` validates against the registry and sets `type_source='manual'`. |
| `POST /api/search` | Lightweight search (no LLM). Results carry the type axes (`type`/`lifecycle_state`/`type_source`). Optional `types` / `types_exclude` arrays are server-side opt-in type filters (bind parameters; `block_roles_exclude` = legacy alias, both ⇒ union). |
| `GET /api/graph/ego` | Scope-filtered k-hop ego subgraph over dream links (read-only, no LLM — see [Graph API](#graph-api)). |
| `GET /api/graph/overview` | Scope-pure Louvain cluster supergraph ("landkarte"); reads precomputed scope-partitioned aggregates, gated on `graph_overview.enabled` (off → 404). Read-only, no LLM. |
| `GET /api/whoami` | Calling key's identity: `label`, `home_scope`, `read_scopes`, the server-global `admin` tier flag, plus the Model-C tenant identity `tenant_id` + per-tenant `role` (`owner`/`admin`/`member`). The SPA login gate probes it, derives its read-only degradation from `admin`, and tells server-admin from tenant-admin. |
| `POST /api/manage` | CRUD, Guard API, stats, API-key management, block-type registry (see [manage actions](#manage-actions)). |
| `GET /api/types[/{name}]` | Block-type registry reads: effective type list (`_global` ∪ your tenant) and single-type policy config, **member-gated** (any valid key) (see [Block-type registry](#block-type-registry)). |
| `PUT\|DELETE /api/types/{name}` | Block-type registry writes (upsert/delete), **admin-or-tenant-admin-gated**: a tenant-admin writes its own tenant namespace, `_global` types are operator-only; DELETE 409s while referenced or on a builtin (see [Block-type registry](#block-type-registry)). |
| `GET\|POST /api/project` + `GET\|PATCH\|DELETE /api/project/{id}` | Project register: one project = one repo corpus bound to one tenant scope. Reads **member-gated** (scope-read), create/patch/delete **tenant-admin** (see [Project register](#project-register)). |
| `GET\|PUT\|DELETE /api/settings[/{key}]` | Runtime config overrides, **admin-gated incl. reads** (see [Settings API](#settings-api)). |
| `GET\|PUT\|DELETE /api/secrets[/{name}]` | Write-only sealed credentials, **admin-gated**: PUT creates/rotates (value never returned), GET lists metadata + `referenced_by`, DELETE 409s while referenced (see [security](security.md#sealed-secrets--break-glass)). |
| `GET /api/status` | **Admin dashboard** aggregate from the process-wide status collector: health, backend pool (`pool.Status()` shape), dream queue + mode, 24h LLM telemetry (with an `llm_24h_complete` attribution flag), gaming toggle. Served from a cache (N pollers cost one collection). Carries hostnames, so it is admin-gated where `/health` stays anonymous. Opens to a tenant-admin with a reduced own-tenant view (see [multi-tenancy](multi-tenancy.md#telemetry-per-tenant-views)). |
| `GET /api/llmlog` | **Admin** LLM telemetry table (`?limit=`/`pipeline=`/`errors_only=`). NEVER returns the `request_system`/`request_user`/`response_content` body columns (the prompt shadow corpus); the `error` is normalized to a class + 256-char-capped detail. Tenant-admin sees only its own tenant's rows. |
| `GET /api/events` | **Admin-only** SSE live stream (`text/event-stream`) for the dashboard. The collector diffs its snapshot ONCE per tick and fans `status`/`backends`/`llmcall` events to every connection; a new connection gets the full state first, then diffs. `: ping` keepalive (`CTX_EVENTS_PING_INTERVAL`), a rolling 90s write deadline that outlives the absolute server `WriteTimeout`, the `CTX_EVENTS_MAX_CONNECTIONS` cap → 429 (client degrades to polling), and an in-stream re-auth every 12th tick that ends the stream on key revocation. |
| `POST /api/digest` | Topic map generation. |
| `POST /api/ingest` | Obsidian vault ingestion. |
| `POST /api/blob/*` | Binary storage (store/fetch/search/manage). |
| `POST /api/chat/stream` + `/api/chat/sessions[/{id}]` | Web-chat harness (see [Web chat sessions](#web-chat-sessions)). |
| `GET /health` | DB + pool role reachability, aggregated to anonymous service classes (no backend names, no states — topology is admin-only via `backend-list`). |
| `POST\|GET\|DELETE /mcp` | MCP Streamable HTTP (remote tool server). |
| `GET /authorize` + `POST /token` | OAuth 2.1 authorization (PKCE) + token exchange. |
| `GET /` (unregistered paths) | Embedded admin SPA (Svelte 5 + Vite, served from the binary). History-API fallback answers HTML navigations (`Accept: text/html`) only — mistyped API URLs stay 404 for JSON clients. Hashed `/assets/*` are immutable-cached and pre-compressed (`.br`/`.gz`); binaries built without the frontend serve a 503 placeholder while all APIs stay functional. See [development](development.md#web-ui-svelte-5--typescript--vite-bun). |

## Manage actions

`POST /api/manage` covers CRUD, Guard API, stats, key/MCP-client management, the block-type registry and the issue corpus.

- **Keys/clients:** `api-key-create` requires `home_scope`; key + MCP-client management and mutating `dream-mode` require an **admin key** since migration 052 (see [security](security.md#admin-tier)). `api-key-create`/`api-key-update` accept optional `write_scopes` (078, E4b) — explicit per-key write scopes that must be ⊆ `allowed_scopes ∪ {home_scope}` (else 400); the effective block-write gate and its double enforcement are in [multi-tenancy](multi-tenancy.md#per-key-write-scopes-e4b-migration-078). Per-tenant scoping of the `api-key-*` actions and the tenant lifecycle (`tenant-*`, `tenant-grant-*`, `scope-*`, `tenant-limit-set`, `tenant-usage-get`, `tenant-quota-*`, `block-grant-*`) are in [multi-tenancy](multi-tenancy.md).
- **Block-type registry:** `type-list`/`type-get` are open reads; `type-create`/`type-update`/`type-delete` are the **operator transport** (server-admin, `_global` namespace) of the same store logic the REST `PUT/DELETE /api/types/{name}` surface uses — kept functional for MCP/CLI consumers, not superseded (see [Block-type registry](#block-type-registry)). `manage update` accepts a registry-validated `type` field; `list-meta` accepts the `types`/`types_exclude` filters.
- **Issue corpus:** `issue-create`/`issue-update`/`issue-get`/`issue-list`/`issue-comment-create`/`issue-link-create`/`issue-link-delete` are the **operator transport** (all tierOpen; isolation is store-layer scope filtering, not an admin tier) over the same store issue logic the REST `/api/project` issue surface (W6/W7) uses — kept reachable for MCP/CLI operators, not the primary UI surface (see [Issue corpus](#issue-corpus)).
- **Backend pool:** `backend-list`/`create`/`update`/`delete`/`test` — see [Backend pool](#backend-pool).

**Type axes on responses.** `/api/search`, `manage get`, the MCP/chat `recent` surface and `manage list-meta` carry `type` (policy type), `lifecycle_state` and `type_source` per block; `/api/query` results carry `type`. Server-side type filters (`types` / `types_exclude` arrays) are pure opt-in bind parameters — no hard exclude (retrieval-excluded types stay browseable by design), with `block_roles_exclude` kept as the documented legacy alias everywhere including `/api/query` (both names ⇒ union).

## Block-type registry

The block-type registry has its own REST namespace, the canonical surface going forward: reads (workflow W1) are **member-gated** — any valid key reads — and writes (workflow W2) are **admin-or-tenant-admin-gated**. Both gates live inside the mount (`RequireMember` for GET, `RequireAdminOrTenantAdmin` for PUT/DELETE), so a missing/invalid key is `401` and an under-privileged one is `403` before the handler runs — the gate cannot be forgotten without the route vanishing.

| Endpoint | Description |
|----------|-------------|
| `GET /api/types` | Effective type list: the shipped `_global` set **∪** your own tenant's overlay, with a tenant-scoped type shadowing the `_global` row of the same name. Each entry carries a `source` badge (`builtin` = shipped `_global` namespace, `tenant` = your tenant's own). |
| `GET /api/types/{name}` | One type incl. its policy `config` envelope + `source`. A name that is unknown **or** belongs to another tenant reads as `404 {success:false,error:"Type not found"}` — the same body either way (no existence oracle: type names can leak project internals). |
| `PUT /api/types/{name}` | Upsert (workflow W2). Body: `display_name?`, `description?`, `is_default?` (create only), `config?` (raw policy envelope, validated against the same authority the reload decoder uses). The scope is **pinned by role** — a tenant-admin writes its own tenant namespace, an operator writes `_global` — never taken from the body. A tenant-admin targeting a `_global` type gets `403` (global types are operator-only; they are member-visible, so this is not an oracle). Returns the frozen wire row + `source`. |
| `DELETE /api/types/{name}` | Delete (workflow W2). Same role gate as PUT. A still-referenced type is refused `409 {success:false,error,blocks:{active,archived}}` (the count spans archived rows too); a builtin is refused `409`. Unknown/foreign name ⇒ `404` (same body as the read). |

The response row is the frozen wire shape (K5): `id`, `name`, `scope`, `display_name`, `description`, `builtin`, `is_default`, `config` (the design 01-§3.3 policy envelope, carried verbatim), `created_at`, `updated_at`, `updated_by?`, `source`. The shape is pinned by a Go golden test (`TestTypesGoldenShape`) and its TypeScript mirror (`web/src/lib/api/types.ts`); the two must move in lockstep. The write responses reuse this exact row shape, so the freeze is unchanged.

**One write logic, two transports.** The REST `PUT/DELETE` handlers and the `manage type-create/update/delete` family call the **identical** store functions (`store.Create/Update/DeleteBlockType`), validation authority (`blocktype.DecodePolicy`) and error mapper — no mutation logic is duplicated. The `_global` write path carries the same authority (server-admin) on both transports, so there is no divergent gate; the tenant-admin write path exists only on REST. The manage family stays functional for its MCP/CLI consumers (not removed).

CLI: `ctx types` / `ctx types list` (table on a TTY, JSON when piped), `ctx types get <name>`, `ctx types set <name>` (upsert) and `ctx types rm <name>` (delete).

## Issue corpus

Issues and comments are ordinary `context_blocks` (`type_name` `issue`/`comment`, migration 084 seeds) — they inherit scope isolation, RRF, Guard and Dream for free (Modell C, one project scope = one repo corpus). The **primary** wire surface is the REST `/api/project` issue family (workflow W6/W7); the `manage issue-*` actions below are the **operator transport** over the identical store functions (`store.InsertIssueBlock`/`UpdateIssueBlock`/`InsertCommentBlock`/`GetIssue`/`ListIssues`, plus the structural-link store) — one logic, two transports (masterplan K2). All are `tierOpen`: isolation is enforced in the store layer via the caller's write scopes (writes) and read scopes + block grants (reads), never an admin tier.

| Action | Data | Semantics |
|--------|------|-----------|
| `issue-create` | `title` (required), `content?`, `scope?` (default `home_scope`), `tags?`, `metadata?`, `status?` | Insert-once issue. Title gets a per-scope `#L<seq>` prefix (so two issues may share a human title without a `23505`). `workflow_status` = the type's `initial` (a supplied `status` must be a valid entry, else `422`). |
| `issue-update` | `id`; `title?`/`content?`/`tags?`/`metadata?`/`status?` | By-id patch (never upsert). A `status` change is validated against the type's workflow **policy data** (invalid ⇒ `422`); the `#L<seq>` prefix is preserved; `metadata` is JSONB-merged. A foreign/absent id ⇒ `404` (no oracle). |
| `issue-get` | `id` | The issue + its comment thread. |
| `issue-list` | `status?`, `labels?`, `limit?` (≤100), `cursor?` | Keyset board page. `status` set ⇒ one board column; absent ⇒ per-status merge. Scoped to the caller's read scopes (a foreign key sees an empty list). |
| `issue-comment-create` | `parent_id` (required), `content?`, `author?`, `metadata?` | Comment under an issue. The scope is **always** the parent's (never the request); a parent the caller cannot write ⇒ `404`. |
| `issue-link-create` | `source_id`, `target_id`, `link_class` | Structural edge (`context_structural_links`). `link_class` must be in the source type's `structural_link_classes` allowlist (else `422`); source and target must share one writable scope — a foreign/absent target ⇒ the **same** `404` as a nonexistent one (no existence oracle). |
| `issue-link-delete` | `source_id`, `target_id`, `link_class` | Remove a structural edge. Foreign/absent source ⇒ `404`. |

**render:'untrusted'.** Every body-bearing response (`issue-create`/`issue-update`/`issue-get`/`issue-list`/`issue-comment-create`) carries `render: "untrusted"` — issue/comment bodies are attacker-controlled markdown; the UI MUST take the sanitising render path (the field cannot be silently overlooked).

## Project register

The project register (workflow W4, migration 079) binds a project identity to exactly **one** tenant scope — that scope is the project's repo corpus (issues/comments live there, so isolation/RRF/Guard/Dream come for free, Modell C). Reads are **member-gated** (scope-read); create/patch/delete are **tenant-admin**. Both gate groups live inside one `MountProject`, so a missing gate is a missing route (404), never fail-open.

| Endpoint | Gate | Description |
|----------|------|-------------|
| `GET /api/project` | member | Projects whose scope is in your `read_scopes`, newest first. `?identity=<id>` narrows to the single project of that identity (the `ctx project init` existence probe). |
| `POST /api/project` | tenant-admin | Compound create: assign the project scope **and** insert the register row in ONE transaction (a failure after the scope assign rolls the scope back too). Body `{identity, scope, display_name?, forge?}`; `scope` is a NAME the server prefixes from the tenant slug (`<slug>:<name>`, ≤ 50 chars). A **server-admin** targets a foreign tenant via a `tenant_id` field (T22-analog; a tenant-admin passing `tenant_id` ⇒ 403). Scope quota is loaded **fail-closed** (a limits-lookup error is a 500, never silent unlimited) and enforced (over `max_scopes` ⇒ 429). Re-init of an identical `identity` ⇒ idempotent 200, no duplicate, no orphan scope. |
| `GET /api/project/{id}` | member | One project, scope-read. Unknown, malformed, or foreign-scope id all read as `404 {success:false,error:"Project not found"}` — same body (no existence oracle). |
| `PATCH /api/project/{id}` | tenant-admin | `display_name` and/or `forge` only. `scope`/`tenant_id` (the tenant_id = tenant_of(scope) invariant) and `webhook_secret_ref` (server-managed, W13) ⇒ 422. `forge.api_base` is validated (SSRF): non-`https`, or a private/loopback/link-local IP literal (RFC1918, 127/8, 169.254/16, ::1, fd00::/8, fe80::/10) or `localhost` ⇒ 422. A foreign/absent project ⇒ 404 uniform. |
| `DELETE /api/project/{id}` | tenant-admin | Delete the register row (its `context_project_sync_runs` cascade). The project's **blocks and its tenant scope survive** — scope teardown is a tenant-lifecycle concern. A foreign/absent project ⇒ 404 uniform. |

`identity` is one of `github:owner/repo` | `git-root:<sha>` | `manual:<slug>` (validated in Go, no DB CHECK on the open set). The register row is `{id, tenant_id, scope, identity, display_name, forge, webhook_secret_ref, sync_status, last_sync_at, sync_cursor, created_at, created_by?, metadata}`. The `webhook_secret_ref` is server-managed and always NULL until the W13 webhook surface lands; `forge`/`sync_cursor` are the Achse-02 sync contract (extended additively by migration 080). A tenant prune drains `context_projects` (+ sync runs) as a mandatory K14 gate.

## Graph API

`GET /api/graph/ego?block=<uuid>` returns the k-hop ego subgraph of a focus block over the dream-link graph. Designed for 1M+ blocks: the server only ever ships budgeted subgraphs, never the full graph.

```
GET /api/graph/ego?block=<uuid>&hops=2&per_node_cap=25&limit=500
                  &min_confidence=0.5&link_class=topical,causal
                  &category=learnings&created_after=2026-01-01T00:00:00Z&edge_limit=4000
```

| Param | Default | Range | Meaning |
|-------|---------|-------|---------|
| `block` | — (required) | full UUID | focus node (hop 0) |
| `hops` | 1 | 1–3 | BFS depth |
| `per_node_cap` | 25 | 1–100 | top-N edges per frontier node by `raw_confidence` — slots count only visible, filter-passing edges |
| `limit` | 500 | 1–1500 | total node budget (truncation: closer hop wins, then higher confidence, then id) — ceiling set by the G39 1M benchmark (p95 < 500ms) |
| `min_confidence` | 0 | 0–1 | gate on weighted confidence (traversal + displayed edges) |
| `link_class` | all 5 | topical,factual,causal,recurrent,supersedes | `supersedes` is display-only, never traversed |
| `category` | all | CSV | filter on neighbor blocks (focus always included) |
| `created_after` / `created_before` | open | RFC3339 | window on neighbor `created_at` |
| `edge_limit` | 4000 | 1–20000 | budget for edges within the node set, strongest first |

Out-of-range values are a `400`, never silently clamped. Response: `nodes` (id, title capped at 120 chars, category, scope, visible `degree` — capped at 201, rendered "200+" — and `hop`), `edges` as compact index tuples `[srcIdx, dstIdx, relIdx, confidence]`, and `stats`. The payload never contains block `content` (load it lazily via `manage get`).

**Security semantics.** The visibility triple (not archived, block type on the registry visibility allowlist, scope readable by the key) is applied inside every hop AND inside the per-node cap legs — a node reachable only through a foreign private bridge is never delivered, and invisible edges never consume cap slots. `degree` counts only visible neighbors (scan budget 1000 raw edges/direction). "Does not exist" and "not visible" answer with an identical `404` (no existence oracle); only successful calls write an access-log row (`action='graph'`, `block_id=NULL`).

### Overview (cluster "landkarte")

`GET /api/graph/overview` returns the cluster supergraph: a few hundred meta-nodes (precomputed Louvain communities over the dream-link graph) with `size`, `top_categories`, a representative block, and aggregated inter-cluster meta-edges. The Louvain rebuild runs offline in the scheduler (`internal/overview`, gonum); the endpoint only reads precomputed tables. Since WF T6 the node set is policy-cut: a block becomes a Louvain node only if its type is on the registry visibility allowlist AND carries `overview.include=true`.

```
GET /api/graph/overview?min_cluster_size=1&min_inter_cluster_weight=0&node_limit=500&edge_limit=2000
```

| Param | Default | Range | Meaning |
|-------|---------|-------|---------|
| `min_cluster_size` | 1 | ≥1 | hide meta-nodes whose visible size is below this |
| `min_inter_cluster_weight` | 0 | ≥0 | hide meta-edges below this aggregated weight |
| `node_limit` | 500 | 1–2000 | max meta-nodes (largest first) |
| `edge_limit` | 2000 | 1–20000 | max meta-edges (strongest first) |

Response: `nodes` (`cluster` ordinal, `size`, `top_categories`, `repr_id`/`repr_title`, `scope_mix`), `edges` as compact tuples `[srcOrdinal, dstOrdinal, link_count, weight]`, and `stats` (`computed_at`, `null` if never built). Gated on the hot setting `graph_overview.enabled` (default off → `404`).

**Security semantics** (the solved scope-count-leak). Aggregates are **scope-partitioned** — each precomputed row belongs to exactly one scope (nodes) or scope-pair (edges), and a request sums only rows whose scope(s) lie entirely within the caller's `read_scopes` (edges need **both** endpoint scopes visible). No global total is ever exposed, so a private member count cannot be recovered by difference. The internal `cluster_id` (smallest member UUID) is **never** emitted — clients see a per-request ordinal, so the identifier is not an existence oracle. Only successful calls write an access-log row (`action='graph-overview'`, `block_id=NULL`).

## Settings API

Runtime config editing over the `context_settings` override layer. **Admin-gated including reads** — the effective config (hosts, models, thresholds) is operational intelligence, and a non-admin key that can read it can also enumerate what to attack. (Tenant-admins reach a two-scope view — see [multi-tenancy](multi-tenancy.md#per-tenant-settings--secrets).)

```
GET    /api/settings           # every registry key: value, source, type, mutability, default
GET    /api/settings/{key}     # single key + last 10 audit rows (action, actor, via)
PUT    /api/settings/{key}     # body {"value": <scalar>} — validated BEFORE persist
DELETE /api/settings/{key}     # drop the override, revert to env/default
```

- **Validation before persist.** A PUT builds the candidate config through the same path the reload uses; a value the build would reject or ignore is a `422` and never reaches the table (no row, no audit entry). Unknown keys are `404`; `restart`/`coupled` keys are `409` with the env var to set instead. String inputs are normalized to their registry type before persist (`"0.7"` → the number `0.7`).
- **Hot effect.** After commit the handler swaps the snapshot — the next request/cycle runs with the new value, no restart. Direct `psql` edits arrive through the NOTIFY listener with the same effect (audited as `via='sql'`).
- **Masking rule.** Any response position carrying the effective value of a sensitive key renders `"(set via env)"` when the value comes from env (incl. `previous.value` on PUT and the post-revert `value` on DELETE). DB-sourced sensitive values render the secret **name** (`secret_ref`), never resolved material.
- **secret_ref gate.** Sensitive keys (`*.api_key`, `server.db_password`) accept only the *name* of an existing sealed secret — a provider-key-shaped value is rejected with `422`.
- **Embed-cache coupling.** Writes (and reverts) that change the effective embed/dream-embed host or protocol flush `context_embed_cache` automatically. The response `warnings` array flags a `.host` change whose sibling `.protocol` still comes from env: **change host + protocol + api_key together**.

```bash
curl -s -X PUT "$CTX/api/settings/rerank.blend_weight" \
  -H "X-Context-Key: $ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"value":0.6}'
# → {"success":true,"key":"rerank.blend_weight","value":0.6,"source":"db",
#    "previous":{"value":0.5,"source":"env"},"warnings":[]}
```

## Backend pool

Manage actions (all **admin-gated, reads included** — the list discloses egress topology; tenant-admin scoping in [multi-tenancy](multi-tenancy.md#backend-pool-isolation-egress)):

```
POST /api/manage {"action":"backend-list"}                 # rows + live status (effective_state, cooldown, sanitized last_error)
POST /api/manage {"action":"backend-create","data":{…}}    # full validation, see below
POST /api/manage {"action":"backend-update","id":…,"data":{…}}   # single-field patch
POST /api/manage {"action":"backend-delete","id":…}        # hard delete (llmlog history stays readable)
POST /api/manage {"action":"backend-test","id":…,"data":{"probe":"chat"}}  # reachability dry-run
```

**Validation guards** (create AND update, `422` with field errors): credential-carrier headers in `extra_headers` (`Authorization`, `Cookie`, `*-key`, `*-token`, …) and credential-semantic `extra_body` fields are rejected — provider keys go through `api_key_ref` (the *name* of a sealed secret, resolved in-memory only); `locality` is cross-validated against `base_url` (a publicly routable host must be `external`); embed roles on external backends are blocked without `metadata.embed_equivalence_verified=true` (foreign quantization corrupts the shared vector space). Raising `trust` requires `confirm_trust_elevation:true`. Every mutation reloads the pool snapshot synchronously — `backend-update {"enabled":false}` is an instant brake, no restart; psql edits converge via the 053 NOTIFY trigger.

**OpenRouter (first external backend, G29).** `provider_class: "openrouter"` refines the openai wire: the request **always** carries `provider.zdr:true` + `provider.data_collection:"deny"`, independent of trust level — trust decides WHICH content may flow, the provider class decides whether the provider may store it. `extra_body.provider` entries merge but can only tighten (the force runs after the merge). The single escape is `metadata.allow_data_collection:true`, requiring `confirm_data_collection:true`. Responses feed telemetry: `usage.cost` → llmlog `cost_usd` (local NULL), the top-level `model` (that actually answered) overwrites the row's model, the response `id` lands in `metadata.provider_request_id`. A "no providers" rejection (zdr/deny filter leaves no provider) classifies as configuration-permanent (1h cooldown, no retry storm). `backend-test` additionally reports `credits_remaining`/`usage_usd` (`GET /v1/key`) and `zdr_endpoints` (`GET /v1/endpoints/zdr`). **`base_url` is the API root WITHOUT the version segment** — the wire paths append `/v1/...` themselves; a `base_url` ending in `/v1` double-segments to a 404.

## Web chat sessions

(F6, migration 056) The persistence layer, server-side tool harness and streaming HTTP endpoint for web chat. `context_chat_sessions` is **scope-owned**: list and delete key on the creating tenant's home scope, so a key never sees a foreign tenant's chats. It snapshots the creating key's `read_scopes` and carries a **monotone `max_sensitivity` high-water-mark**. Reading or continuing a session requires `session.read_scopes ⊆ caller.ReadScopes` (else 404, no oracle), closing the shadow-corpus channel against future least-privilege keys. The HWM rises with **every** appended message (in the same short transaction that assigns the message `seq`), so a credentials-touched session stays full-trust-only for its whole life. `context_chat_messages` records per-message `sensitivity` (fail-closed `credentials` default), tool-call metadata, telemetry and a gapless `seq` (`UNIQUE(session_id, seq)`). A turn claims its session via a short `busy_until` CAS — a second concurrent turn gets 409 without blocking, a crashed turn self-heals on expiry. Retention is off by default (`CTX_WEBCHAT_SESSION_RETENTION`).

The **harness** (`internal/chat`) drives the model loop (model call → tool execution → next call), re-resolving the F3 chat chain each iteration on `max(request, session HWM)` sensitivity (an empty chain ends the turn — never a silent escalation). Four read-only tools run under the session's `read_scopes` snapshot: **ctx_query** (hybrid retrieval, delegated to `/api/query` with `synthesize:false`+`include_content:true`, attributed to the real key), **ctx_search**, **ctx_get** (full block, paged past the window via a resumable `offset`), **ctx_recent**. Each result is annotated with `max(sensitivity)` of the blocks it carried; tools are offered only to a full-trust backend, and the closing call after the tool-budget cap carries no tools array. Tool errors return to the model as `{"error":…}` and never abort the turn. Events flow through a narrow `Sink` interface, so a future headless agent runner can drive the same loop without HTTP.

**Endpoints** (auth required; `CTX_WEBCHAT_ENABLED=false` ⇒ 404):

| Route | What |
|---|---|
| `POST /api/chat/stream` | Run one turn, response `text/event-stream`. Body `{session_id?, message, sensitivity?, tools_enabled?, max_tokens?}` — empty `session_id` creates a session. Pre-stream failures are JSON (`404` unknown/foreign session or feature off, `409` busy, `429` scope semaphore); once the first event flows, later failures are `error` events. SSE events: `session, backend, delta, tool_call_start, tool_call, tool_result, usage, done, error` + a `: hb` keepalive every 15s. Errors are **laundered** to class code + backend NAME (the raw backend URL never reaches the client). Wrapped in the scheduler signal so dream yields the single llama.cpp slot during a turn. |
| `GET /api/chat/sessions?limit=50` | List the caller's home-scope sessions (metadata + `message_count`, newest first; no content). |
| `GET /api/chat/sessions/{id}?after_seq=0&limit=0` | One session + its messages (full tool-result contents); gated by `read_scopes ⊆ caller` → 404 on miss. Pagination additive. |
| `DELETE /api/chat/sessions/{id}` | Hard-delete (messages cascade); home-scope-owned → 404 on miss. llmlog logs `web-chat` **metadata-only** (no conversation bodies in the un-scoped `context_llm_log`). |

A per-`home_scope` semaphore (`CTX_WEBCHAT_CONCURRENT_TURNS`, default 1) bounds concurrent turns — multi-tenant fairness on the single slot (429 before stream start).
