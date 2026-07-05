# Multi-Tenancy

ctx is a shared knowledge layer across multiple LLM/tenant identities. The multi-tenant line (**v4.0.0**, migrations 058–068) is feature-complete across all six axes; the optional migration 066 (tenant-owned OAuth) is deferred. The full integration suite and race detector are green, and a pre-release isolation audit found no cross-tenant leak across the read/write, settings/secrets, admin-tier, MCP, chat and background paths.

**Pausability invariant.** Every wave is built behavior-neutral and pausable: the single-tenant deployment (one default tenant, no provisioned tenants) is byte-identical to the pre-multi-tenant state. Each mechanism activates only when its data arrives (a tenant is provisioned, a grant is minted, a per-tenant setting is written). Rolling out to a running deployment — migrating the production DB from 057 across the 058–068 chain — is a separate operational step; see [operations](operations.md).

## Model C: three levels

ctx uses **Model C** — three levels of isolation: **tenant → scope → block**.

- `scope` is the data discriminator: a `VARCHAR(50)` column on every data table (blocks, blobs, sources, dream-links, write-log). It is **not** a `tenant_id` on the data tables. Migration 058 dropped the legacy 3-value CHECK constraints, so scope strings are unconstrained at the schema level (`private` | `work` | `shared` | any tenant scope).
- The tenant register (migration 059) maps scopes to tenants: `context_tenants` (owner-register) + `context_tenant_scopes` (scope → tenant partition map), plus `tenant_id` and a `tenant_role` (`owner`/`admin`/`member`) on API keys.
- Block-level grants (migration 067) are the finest level — sharing a single block with a foreign tenant.

Each key sees: all blocks in its own scope, all blocks in `shared` (the default tenant's shared knowledge layer — **not** cross-tenant; foreign tenants only via grants), plus any scopes/blocks explicitly granted to it. Nothing from other tenants' private scopes.

> **`shared` is a scope OF the default tenant** — a shared layer *inside* that tenant, reversibly re-hangable by moving one `context_tenant_scopes` row (`TENANT-DECISION(shared-scope-owner)`).

## Authentication & read-scope resolution

`ctx_auth` (the per-request auth function, rebuilt in migration 060) consumes the tenant schema and returns the key's `tenant_id`/`tenant_role`. It applies:

- **Status gate (fail-closed).** A `suspended`/`offboarding` tenant — or a key whose `tenant_id` is NULL — authenticates to the `__UNAUTHORIZED__` sentinel. Setting a tenant's `status` to `suspended` (via `tenant-update`) mutes it at the next auth.
- **Positional read-scope resolution.** `read_scopes` = `[home_scope] ++ allowed_scopes ++ cross-tenant grants`, with order-preserving dedup so `read_scopes[0]` stays `home_scope` for wire stability. System `_`-prefixed scopes are filtered out.

The **`RequireScopes` guard** establishes the fail-closed read contract: an empty resolved scope set is an error, never a silent `scope = ANY('{}')` that matches nothing. It is wired into every scope-filtered store read (block + blob search/get/list/stats, the graph overview + ego reads), and the four MCP tool handlers (store/search/get/recent) no longer fall back to the default-tenant `private` scope when no caller identity resolves. Tenant lifecycle status (active/suspended/offboarding) supports muting a tenant (access + background paused, data preserved and reactivatable) versus an explicit super-admin full-prune. A key-bearing tenant can't be deleted naked (`ON DELETE RESTRICT`).

`store.TenantScopes(tenant)` exposes a tenant's owned scope set (the per-tenant data foundation, distinct from a key's `read_scopes`).

## Cross-tenant grants (scope level)

`context_tenant_grants` (migration 061) is the opt-in, least-privilege cross-tenant read channel: one tenant grants another read access to one of its scopes (FK-guarded so system scopes can't be granted). A grant only ever **widens the grantee's READ scope set** — never its write side (the `context_store.go:99` home-scope write gate is unchanged). It takes effect at the grantee's next auth.

Managed by server-admin manage-actions `tenant-grant-create` / `tenant-grant-list` / `tenant-grant-delete`: create rejects a `_`-system scope (400, with the `granted_scope` FK as fail-closed backstop), an unregistered grantee/scope (400) and a duplicate pair (409), and records the creating admin key for provenance; delete is a 404-no-oracle by id.

## Per-key write scopes (E4b, migration 078)

By default a key may WRITE blocks only to its `home_scope` (plus `shared` when that is in its `allowed_scopes`); every other `allowed_scope` stays read-only (write-in, never-out). Migration 078 adds an explicit `context_api_keys.write_scopes TEXT[] NOT NULL DEFAULT '{}'` so a key can be granted extra write targets **beyond** its home scope without widening its read set. The effective write gate — the single eval point `writableBlockScopes`, used by `/api/store` create and `manage` update/delete/guard-resolve — is:

```
writableBlockScopes = [home_scope] ∪ (write_scopes ∩ (allowed_scopes ∪ {home_scope})) ∪ {shared if allowed}
```

The invariant `write_scopes ⊆ allowed_scopes ∪ {home_scope}` (a write right implies a read right — no blind-writers) is enforced **twice**, fail-closed:

- **(a) at mint/update** — `api-key-create` and `api-key-update` reject a `write_scope` outside `allowed_scopes ∪ {home_scope}` (or a `_`-reserved name) with **400** (Go-side, no DB CHECK — the v2.0.0 open-set line).
- **(b) at the eval point** — the formula intersects `write_scopes` with the readable set, so a `write_scope` left **stale** by a later `allowed_scopes` shrink is neutralised for free; no per-write-site re-check is needed.

`ctx_auth` returns the RAW `write_scopes` column as its 9th return value (appended after `tenant_role` — old binaries selecting the original 8 columns keep working); the intersection is applied in Go, so the DB stays the record of intent and the gate stays the single fail-closed decision. **Backfill: none** — the empty default reproduces v4.2.x behaviour byte-for-byte (pausability/rollback invariant). Set at creation via `ctx keys create … --write <scopes>` (or the `write_scopes` field on `api-key-create`); mutate an existing key via `api-key-update {write_scopes:[…]}` (`[]` clears the set, absent = leave unchanged).

## Per-key confirm-writes capability (F6-C6, migration 090)

Migration 090 adds `context_api_keys.confirm_writes BOOLEAN NOT NULL DEFAULT false` and rebuilds `ctx_auth` with it as the **10th** return column (appended after `write_scopes` — the same DROP+CREATE/append-at-end convention as 078, so old binaries selecting the original 9 columns keep working). It flows into `auth.AuthResult.ConfirmWrites` and is consumed **only at the store handlers** (`/api/store`, MCP `store`) — internal writers (digest, dream) go through `store.UpsertBlock` and never see it.

This is **not a security boundary**: gating LLM-initiated writes is the calling harness's responsibility (tool approval in Claude Code/claude.ai; the ctx web chat's own confirm card). The flag is a per-principal **distrust tool** — the option to force server-side stage-then-confirm onto a harness that has no trusted gating layer of its own. Default `false` = fail-open for every existing key: nothing changes on deploy, opt-in is a per-id `UPDATE context_api_keys SET confirm_writes = true` (the `is_admin`/052 bootstrap convention).

The staging building blocks (D-W2): `store.CanonicalWrite` defines the canonical JSON form of a staged write — fixed field order, sorted tags, sorted metadata keys — and `PayloadHash` yields the sha256 the confirm call will select by; the hash binds the resolved write **including its post-gate sensitivity**. `handler.runStageWriteGates` runs every direct-path write gate (size, sensitivity, explicit type, G40 credentials detector, scope, write rate limit) **before** anything is staged, so a confirm card can only ever promise a write the execute step will accept.

The staged path is live on MCP (D-W5): a flagged key's `store` call runs that full gate set, holds the canonical payload server-side (`context_pending_writes`, 089) and answers **`IsError=true`** with a `payload_hash` — a legacy client must never read "staged" as success. The new `confirm` tool executes exactly that server-held write: per-key, replay-safe (atomic single consume), with the write scope re-validated against the key's *current* rights at confirm time (a shrunk right rejects on a lookup, without burning the stage token). REST `/api/store` and the CLI stay direct by decision D-E1 — they serve human-driven clients, not LLM harnesses.

The MCP `update` tool (D-W6a) extends the dance to field-level block updates — REST manage-update parity (resolve within `writableBlockScopes`, size limits, re-classify/temporal/re-embed afterwork), no scope/type/sensitivity field (those guard flows stay REST-only, D4-analog). For a flagged key the update stages as op `update` with two extra hash-bound fields: `update_fields` (the authoritative field list — "clear tags" and "leave tags unchanged" can never collide) and `base_updated_at` (the block's `updated_at` at stage time, the **TOCTOU pin**, D1-M3). A confirm re-reads the block and rejects — without consuming the token — when the block changed since staging (lost-update protection); the client re-reads and re-stages.

The web chat goes further (D-W6b): it is itself a harness **without** an own gating layer and runs the smallest model, so its new `ctx_store` tool stages **every** write — default-confirm by birth, independent of the per-key flag (there is no legacy chat writer to break). The staged card travels on the `tool_result` SSE event and inside the persisted tool message, and the SPA ConfirmCard executes it via `POST /api/confirm` — mounted behind the header-auth middleware only (`X-Context-Key` / `Bearer`; no cookie path exists server-wide, so a cross-site POST carries no usable credential). Dismissing a card is client-side: the stage expires (`writes.confirm_ttl`) and the retention ticker evicts it.

Since D-W6c ONE confirm sequence serves every surface: `confirm_core.go` holds the shared lookup → validate → D1-M1 scope re-check → D1-M3 TOCTOU re-check → atomic consume → execute chain, and the MCP `confirm` tool and `POST /api/confirm` only map its typed outcome onto their wordings (MCP surfaces raw errors, HTTP launders them; TOCTOU rejects answer HTTP 409). That makes the confirm surfaces interchangeable — a write staged over MCP confirms over HTTP and vice versa — and the chat gains `ctx_update` (staged field-level updates with the same `update_fields`/`base_updated_at` hash-bound form as the MCP update tool; the ConfirmCard shows the field list and the lost-update reject).

## Block-level grants (row level)

`context_block_grants` (migration 067) shares **one** `block_id` with a `grantee_tenant` — a granularity finer than the scope-level grants. `block_id` and `grantee_tenant` are both FK `ON DELETE CASCADE` (a deleted block or offboarded grantee drops its grants — contrast `context_dream_links`, which blocks the delete); `granted_by` is an `ON DELETE SET NULL` audit pointer; `uq_block_grant (block_id, grantee_tenant)` is the idempotency guard and `idx_block_grants_grantee (grantee_tenant, block_id)` the hot-read index. Deliberately **no `permission` column** (a single-value `'read'` enum gates nothing today — additive later). New table on an empty relation, so no `context_blocks` lock and no 1M index build.

**Read paths.** `store.GrantedBlockIDs(tenant)` resolves a tenant's granted block ids. The canonical visibility triple becomes `NOT archived AND type_name <> 'system-meta' AND ( scope = ANY OR id = ANY(grants) )` — the grant OR is strictly **inside** a mandatory parenthesised group after the archived/system-meta conjuncts (SQL binds AND tighter than OR; without the parens a granted archived or system-meta block would leak). The same parenthesised OR rides `GetBlock`, `ResolveBlockID`, `SearchBlocks`/browse, `mcp recent`, the `EgoGraph` legs and the `rrf` GraphExpand hop.

**Semantic-retrieval arm** (migration 068). `ctx_rrf` gained a 13th `p_granted_block_ids UUID[] DEFAULT NULL` param; all six CTE WHERE clauses replace the flat `AND cb.scope = ANY(p_scopes)` with the parenthesised scope-OR-grant form. `rrf.Search` threads a `grantedBlockIDs` param; `query.go` resolves the grant set once (fail-closed `resolveGrants` — a resolver error yields an empty set, never a widen). The hard empty-scope reject stays — a non-empty grant set never relaxes it.

**Graph bridge (leaf contract, T41).** A block visible ONLY via a grant is a **leaf** — it appears in the node set and induced edges but is never a hop seed, so expansion cannot traverse the grant bridge into the grantee's own in-scope blocks behind it.

**Grant CRUD (T43)** closes the write side: admin-gated `block-grant-create`/`block-grant-list`/`block-grant-revoke` (CLI `ctx block-grant`) behind a hard per-block **ownership** gate — a caller may only share a block whose scope its tenant OWNS via `context_tenant_scopes`; a foreign or unmapped scope fails closed to 403. A `tenant.allow_cross_tenant_block_grant` opt-in (global-only, default off) gates cross-tenant shares (intra-tenant department→department stays allowed). The egress floor is raised for grant-mediated results to `max(ownerFloor, granteeFloor, GRANT_FLOOR_DEFAULT=personal, block.sensitivity)`, so a shared block never reaches an external backend below the grantee's strictest floor or the config-independent `personal` backstop.

## Admin tiers

Two typed authorization predicates on the auth result (`auth.Role` = `owner`/`admin`/`member`, pinned to the 059 `tenant_role` CHECK by a live-schema test):

- `IsServerAdmin()` — the server-global M052 tier.
- `IsTenantAdminOf(tenant)` — server-admins administer every non-empty tenant; `owner`/`admin` only their own. An empty target tenant is denied for every tier.

The `whoami` endpoint carries this on the wire: alongside the server-global `admin` flag it returns the resolved `tenant_id` and per-tenant `role`, so the SPA gate can tell a server-admin from a tenant-admin.

**Action-tier gate (T25).** `actionRequiresAdmin` became `actionTier` (server-admin / tenant-admin / open) via a `requireTenantAdmin` predicate. Only the tenant-isolated `api-key-*` actions were lowered to tenant-admin — `mcp-client-*`, `backend-*` (until T37a), `blocks-audit`/`classify-*`, and the `tenant-*`/`tenant-grant-*` lifecycle actions STAY server-admin because their handlers carry no tenant filter (lowering them would be fail-OPEN). The rule is **isolate first, then promote**.

## Per-tenant key management

- **`DeleteApiKey`** takes the caller's tenant + server-admin flag and enforces the constraint in one atomic UPDATE (`WHERE id AND active AND (is-server-admin OR tenant_id = caller)`, TOCTOU-free). A miss collapses to one uniform `key not found` — no existence oracle over another tenant's keys (leak-path L2).
- **`ListApiKeys(tenantFilter, activeOnly)`** scopes a non-server-admin to its own tenant (leak-path L1) and — a **named behavior change** — defaults to returning only ACTIVE keys; soft-deleted keys reappear only with explicit `active_only=false`.
- **`CreateApiKey`** is tenant-bound (leak-path L3/L5): the `{shared}` allowed-scopes default is tenant-aware (only the default tenant inherits `shared` — a fresh foreign-tenant key gets an empty set, no implicit cross-tenant read); a non-server-admin's every requested scope must be one its own tenant owns (resolved against `store.TenantScopes` — Model C, not a `home_scope == tenant_id` string compare), else 403 on the first foreign scope.

## Per-tenant settings & secrets

Every config registry leaf carries a mandatory **`tenancy`** axis (T28) — `tenant-overridable` or `global-only` — enforced by a boot panic on any untagged leaf. **52 keys are tenant-overridable** (the per-tenant query/retrieval/dream-tuning surface: thresholds, rate limits, rerank/graph knobs, dream back-off, scope + sensitivity policy, web-chat budgets, the six provider `api_key` secret_refs). **43 keys are global-only** (process-shared resources: DSN/listener, backend HOST/MODEL topology, scheduler/collector cadences, egress-audit retention, the `gaming.active` switch, and — the R-SCALE6 invariant — the four embed-cache-coupled keys whose change flushes the process-wide, scope-less `context_embed_cache` for ALL tenants).

`config.IsGlobalOnly` is fail-closed (unknown key = global-only). A tenant-scope override on a global-only key is dropped with a value-free WARN before it reaches `config.Build`, so a tenant can never flip a server-wide switch or trigger the shared-cache flush — even via a hand-inserted psql row.

**Resolution chain.** `settings.TenantOverlay(pool)` (T09) resolves a tenant's effective config as `_global` base plus the tenant's `context_settings` rows (two-scope load `{_global, tenant}` via `LoadSettingOverridesMulti`, precedence tenant > `_global` > env > default). Migration 064 adds the scope-leading indexes `idx_settings_scope_key` / `idx_secrets_scope_name`. The overlay is wired into `config.Store` at boot (`SetOverlay`); a tenant with no own rows inherits the base pointer verbatim (§10.2 footprint guard at N tenants), a load failure fails safe to base without caching (self-healing). Source attribution distinguishes a tenant-won key (`Source "tenant"`) from a `_global` one (`"settings"`).

**Request vs background.** `SnapshotForRequest(ctx)` derives the tenant from the request context via a cycle-free hook keyed on `ar.HomeScope` — a request can't be pointed at a foreign tenant (there is no scope argument a body could spoof). `SnapshotForTenant(ctx, scope)` takes the scope explicitly and is background-only (sourced exclusively from the register). A `forbidigo` lint gate forbids a tenant-less `Snapshot()` on a request path (a forgotten call-site serving the `_global` generation would be a fail-open config leak); every legitimate tenant-less site carries an inline `//nolint:forbidigo` naming its class.

**Hot-reload channel (T32).** Migration 065 adds the changed row's `scope` to the `ctx_settings_write` NOTIFY payload (additively — old listeners ignore it; the 063 quota and 053 backend triggers inherit it). A tenant-scope write drops ONLY that tenant's cached generation (`config.Store.InvalidateTenant`, O(1) map delete, lazy rebuild); a `_global`/reserved/absent scope falls through to the full `settings.Reload` (base rebuild + O(1) `Replace`-wipe of every derived tenant generation). So a `_global` toggle no longer eager-rebuilds N tenant snapshots.

**Write API (T31).** The `/api/settings` + `/api/secrets` mounts move from `RequireAdmin` to `RequireAdminOrTenantAdmin` (the gate only admits; the handler scopes the target). A server-admin writes/reads `_global`; a tenant-admin writes ONLY its own scope (`writeScope = ar.HomeScope`, the body never carries a scope) and reads the effective tenant > `_global` view. The secret_ref resolver is tenant-scoped with a fail-closed `_global` fallback: `tenantSecretResolver` resolves a tenant's own secret first and falls back to the operator's `_global` provider key ONLY when the tenant carries the `tenant.allow_shared_secrets` opt-in — a global-only key, **default false = strict isolation**, unsettable by the tenant itself. The AAD (name+scope) makes a wrong scope a crypto auth error, not a leak. A `_global`-secret DELETE reference scan goes cross-scope (`referencedBy` over `_global` + every opt-in tenant scope via `store.OptInTenantScopes`) → 409 rather than a silent fail-open.

## Backend pool isolation (egress)

Migration 062 gives `context_backends` a `scope` dimension (`_global` = shared server backend, `<tenant>` = tenant-private), swaps `UNIQUE(name)` to `UNIQUE(scope, name)` (two tenants can each own a same-named backend), and records the row's own scope in the audit trigger.

**`Chain()` is tenant-filtered (T34, R-LEAK7).** A `visibleTo` first-class filter — the OUTERMOST gate, before role/trust/gaming — bounds every backend chain to the caller's scope (`ar.HomeScope` on request paths; `sess.Scope`, the session OWNER, on web-chat). A `_global`/unscoped backend stays shared; a tenant-private backend is **non-existent** to a foreign caller (no `ExclusionReason`, so no topology disclosure), and an empty/`_`-reserved caller (`__UNAUTHORIZED__`) sees ONLY shared backends. So Tenant A can never route a prompt to Tenant B's external backend on B's provider key. `backend-list` filters by the same `VisibleTo` predicate (the read counterpart).

**Per-tenant backend admin (T37a, R-LEAK8).** `backend-create` pins a tenant-admin's new backend to its own scope (`ar.HomeScope`, payload scope ignored — like the `/api/store` write guard) while a server-admin chooses freely (default `_global`). `store.UpdateBackend`/`DeleteBackend` take a `scopes []string` and add `WHERE id = $N AND ($scopes::text[] IS NULL OR scope = ANY($scopes))` — a tenant-admin mutating a foreign or `_global` row matches zero rows → 404-no-oracle (TOCTOU-free in the statement). `backend-test` stays server-admin (it reaches an arbitrary backend by id — isolate-first-then-promote).

## Cost/call quotas

Migration 063 adds `context_tenant_quota` (one row per tenant scope — `daily_cost_usd`/`monthly_cost_usd`/`daily_calls` budgets, an `on_exceed` policy of `external_off` or `block`, NULL budget = unlimited so a missing row is fail-open) plus the accounting indices (`idx_llm_log_apikey`, cost-covering `idx_llm_log_cost`, `idx_access_log_ratelimit`) and a NOTIFY trigger for hot-reload. Distinct from the 069 structural count-limits — quotas are money.

**Cost attribution.** The query-synthesis `context_llm_log` row now carries the calling key's `api_key_id` (threaded through `Synthesize`), as do the chain/embed wire-calls (`ChainCall`, `LogEmbedWire`). Background paths stay NULL by construction (no caller); the query-triggered embed-backfill is also left NULL by decision (it's maintenance, not the caller's request).

**Enforcement (T36a).** `QuotaAccountant` (`internal/backends/quota.go`) serves each tenant's rolling external-cost SUM + attributed call COUNT from a **lock-free** generation snapshot (`atomic.Pointer` map, CAS-guarded TTL refresh ~30s, single-flight off the read path — mirrors StatusCollector, not a mutex, never per-request over the 1M+ `context_llm_log` hypertable). The synthesis path consults it after resolving the chain: the **cost budget** gates only external backends (over budget → `external_off` drops external while local stays reachable, or `block` returns `*ErrQuotaExceeded` → 429); the **call budget** gates every backend including local (a cap skipping local would be toothless), counting only attributed calls. Fail-OPEN throughout (no quota row / disabled policy / empty scope / cold cache passes unchanged) — the fail-CLOSED axis is egress visibility, not cost. `block` is a SOFT cap with a documented TTL overshoot window (worst case ≈ parallelism × cost × TTL).

**Management (T36b).** `tenant-quota-set` / `tenant-quota-get` manage-actions + CLI `ctx quota` write/read the rows (`store.UpsertTenantQuota`/`GetTenantQuota`); a set refreshes the accountant synchronously. `tenant-quota-set` is SERVER-admin only (an operator cost ceiling — a tenant-admin raising its own budget would void it); `tenant-quota-get` admits a tenant-admin for its OWN scope. The `pool.default_tenant_quota` global fallback (T36c) is backlog.

## Telemetry (per-tenant views)

- **`GET /api/llmlog` (T37b).** Moves to `RequireAdminOrTenantAdmin`; a tenant-admin sees ONLY rows attributed to its own tenant's keys (resolved to a literal `uuid[]` via `store.TenantAPIKeyIDs` first, then `api_key_id = ANY($keys)` — index-friendly). A keyless tenant → empty filter → zero rows (fail-closed); NULL-`api_key_id` background rows are server-admin-only. Gate + filter ship together.
- **`GET /api/status` (T37c).** Opens to a tenant-admin with a REDUCED view — only its own backends (`VisibleTo`) + its own 24h rollup, from a separate lock-free per-tenant rollup generation. Server-global fields (health/dream/gaming/activity) stay zero for a tenant-admin.
- **`GET /api/events` (SSE) stays server-admin-only (T37d).** The broadcast fans one global diff to all subscribers; a per-tenant live stream is an architecture change (per-subscriber filtering + tenant-scoped SSE re-auth), deferred. The interim tenant-admin path is polling (`/api/status` + `/api/llmlog`), so there is no push leak. `events.go` carries a six-touch-point migration map for the eventual SSE wave.

## Background pipeline isolation

The background pipeline (dream, digest, daily synthesis, sensitivity audit, credentials classify) iterates the **authoritative tenant register** (T13), taking one `SnapshotForTenant(ctx, tenantScope)` per tenant (dream round-robins one tenant per cycle); the scope string comes exclusively from the register, never request input. The register (not a `DISTINCT home_scope` query) is the source: one tenant = one row, so the iteration collapses to a 1-element `_global` loop at single-tenant. Suspended/offboarding tenants are dropped; a `ListTenants` failure or no active tenant falls back to a single `_global` pass (the background never aborts).

**Entitlement clamp (T38, R-LEAK).** Each iterated tenant's read window is clamped to `read_scopes ∩ TenantScopes(tenant)` — the tenant-overridable `scheduler.read_scopes` is NOT grant-gated on its own, so an unintersected consumer would be a cross-tenant background read. The dream/synthesis backend chain is filtered to `_global ∪ tenant`, the per-tenant `ScopeSensitivityFloor` rides the per-tenant snapshot, and digest/synthesis/audit write scopes are clamped to the tenant's entitlements.

## Per-tenant graph overview (B line, migration 087)

Migration 087 adds a denormalized `scope` (TEXT, the 057 family convention) to `graph_cluster_member`, backfilled from `context_blocks` and `NOT NULL` afterwards. The rebuild writes it from the **Louvain input** (never a re-read at persist time), so a member row always records the partition the clustering ran in — the prerequisite for the scope-scoped teardown/aggregation (B-W3) and the per-tenant rebuild loop (B-W6). `block_id` stays the sole PK: the overview input is strictly owned-disjoint (no grants in the input) — a load-bearing invariant the B-W6 input-purity assert will enforce. Rollback note: a pre-087 binary's scope-less member INSERT fails the NOT NULL constraint loudly; the advisory-locked replace keeps the previous tables readable.

B-W3 makes the persist path partition-capable: `overview.Options.ScopeFilter` (nil = global run, behaviour-identical full replace) switches teardown AND aggregation to one partition **atomically** — scoped DELETEs (member/node per scope, edges with AND on both endpoint scopes) paired with scope-filtered aggregation SQL in the same advisory-locked transaction. The pairing is load-bearing (B1-C1): a scoped DELETE with the unscoped aggregation resurrects the surviving foreign partition into the `(cluster_id, scope)` PK ⇒ `23505` from tenant #2 on. persist fails loudly on input outside the filter (input scoping itself lands with the B-W6 loop); the meta singleton is only written by global runs until the 088 scope-PK migration (B-W5). The scheduler still passes a nil filter — nothing changes operationally until B-W6.

B-W4 partitions the advisory lock itself: `lockKeyForScopes` hashes the sorted, deduplicated scope set with seedless FNV-64a (process-stable — `hash/maphash` would give every ctxd instance its own seed and break cross-instance exclusion) and XORs it onto the base key. Two tenants persist in parallel; the same tenant keeps serializing. A nil/empty filter returns the pre-B-W4 base key **byte-identically**, so old and new binaries keep excluding each other through a mixed-version deploy window. A global run and a partition run do NOT exclude each other by design — the scheduler runs either globally or per-tenant, the transition logic ships with B-W5/B-W6.

B-W5 (migration 088) retires the `graph_overview_meta` singleton: the PK becomes `scope`, the legacy row is **data-migrated** onto every real node scope with its `computed_at` preserved (no spurious boot rebuild, B3-M2), and every run writes the rows it owns in the same advisory-locked tx — the global run replaces all rows (scope set = `DISTINCT scope FROM graph_cluster_node`, no sentinel scope, B3-M1), a scoped run only its filter scopes (an empty partition still records its fresh `computed_at`). Per-scope stats: `cluster_n`/`node_n` from the node rows, `edge_n` counts intra-partition edge rows. The read path answers freshness as `max(computed_at) WHERE scope = ANY(readScopes)` — never an unscoped row pick, which would leak a foreign partition's timestamp (B1-m1); the boot probe stays `count(*)` (server-global, no caller scopes). 088 also adds `idx_gcm_scope` on `graph_cluster_member(scope)` — the scoped teardown DELETE at 1M+ members (B-W3 finding).

B-W6 arms the loop: each scheduler tick rebuilds ONE tenant with `ScopeFilter = owned` (round-robin cursor; `owned` may legitimately be nil — migration seed, register fallback — and then runs the byte-identical global pass under the base lock key). The INPUT is now scoped too: `loadNodes` filters blocks, `loadEdges` keeps only edges with **both** endpoints inside the partition via `context_blocks` joins (never via the member table — that is the previous run's state; B2-M4: an unscoped edge load would pull the full ~3.24M-edge set into RAM once per tenant). Boot (never built) rebuilds every tenant once sequentially instead of one global run — no cross-tenant co-clustering window, per-scope meta consistent from the first build. Mixed cross-partition edge rows left by a pre-B global run are swept by the OR edge teardown of the first partition run touching either endpoint and are never re-created (the AND aggregation). The whole line rests on owned-disjointness: `owned` comes from `context_tenant_scopes` (ownership, one owner per scope); grants are read-widenings and never enter a Louvain input.

## Self-service onboarding (v4.1.1)

Migration 069 adds structural per-tenant count-limits on `context_tenants`: `max_scopes`/`max_keys` (typed `INTEGER`, `CHECK >= 0`, `DEFAULT 25`/`50` so every existing tenant is capped without a backfill, `NULL` = operator-set unlimited — the system/default tenant seeded `NULL`), plus `idx_tenant_scopes_tenant`/`idx_api_keys_tenant`. These are structural counts (how many scopes/keys a tenant may self-provision), distinct from the 063 cost quota.

| Action | Tier | What |
|---|---|---|
| `scope-create` | tenant-admin | Registers a scope in the caller's OWN namespace — the server builds `<tenant-slug>:<name>` (slug from the register, never the payload, so the prefix is unforgeable and two tenants can never collide — S1), charset-gates the name (lowercase alnum + hyphen, no `:`/`_`/whitespace), caps at the VARCHAR(50) budget, enforces `max_scopes` transactionally (429) under a `context_tenants`-row lock. A server-admin may target another tenant via `tenant_id`. |
| `scope-list` | tenant-admin | Tenant-filtered scope list. |
| `tenant-limit-set` | server-admin | Sets a tenant's structural ceiling (both `max_scopes`/`max_keys` required, range `0..1_000_000`, explicit null = unlimited); optionally seeded at `tenant-create` over the 069 defaults. A tenant-admin raising its own limit would void it. |
| `tenant-usage-get` | tenant-admin (own) | Scope/key counts vs limits; a server-admin may target any. |
| `api-key-create` | tenant-admin | Enforces `max_keys` transactionally (429, active-only count); a server-admin may mint into another tenant via `tenant_id` (foreign-target T22 — the new key's scopes must belong to the target; a non-server-admin's `tenant_id` → 403, AM2). New keys are always `member` (a smuggled `tenant_role` is ignored, AM3). Optional `write_scopes` (078, E4b) must be ⊆ `allowed_scopes ∪ {home_scope}` else 400 (see [Per-key write scopes](#per-key-write-scopes-e4b-migration-078)). |
| `api-key-update` | tenant-admin | Role delegation (`{tenant_role?, active?, write_scopes?}`): only an owner (or server-admin) may change a key's role — a non-owner tenant-admin → 403, so admin→owner self-elevation is structurally impossible (no `is_admin` field means the server-global tier can never be set here). Demoting/deactivating the last active owner → 409 (last-owner invariant); an admin cannot neutralize an owner key → 403 (enforced at both update and the `api-key-delete` revoke path). `write_scopes` (078, E4b) is validated ⊆ the key's existing `allowed_scopes ∪ {home_scope}` → 400 on a blind-writer; `[]` clears, absent leaves unchanged. |

**`tenant-create` as real onboarding.** Provisions atomically in ONE transaction: the tenant row, an auto-prefixed initial scope `<slug>:main` (cap-free so a zero limit can't wedge the bootstrap), and an owner key (role `owner`, plaintext returned once) — scope before key so the key's home scope is registered (T22). Any step's failure rolls the whole transaction back (no half-provisioned tenant; a scope-name collision → 409 with zero orphans). The freshly minted owner key authenticates immediately, closing the inert-tenant gap (K10) that blocked self-service onboarding.

These flows are surfaced in the web admin SPA — see the [admin SPA](development.md#web-ui-svelte-5--typescript--vite-bun) section.

## Deferred seams

Three deliberately deferred seams carry no leak (documented in the pre-release audit): tenant-owned OAuth (migration 066 / L6), the dream round-robin's scope-blind `PickBlock`, and the global quota default (`pool.default_tenant_quota` / T36c). The scope-selectivity scaling bench at 1M×N is a named follow-up.

The full build chronicle, security audit and roadmap live in the Context Store (`ctx query "ctx multi-tenant Bau-Stand"`).
