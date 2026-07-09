// Hand-maintained wire types (design 04-§2.5) — no OpenAPI spec exists, the
// Go-side JSON golden tests are the drift anchor. One source comment per type.

// Source: go/internal/handler/whoami.go (whoamiResponse),
// pinned by TestWhoamiGoldenShape.
export interface WhoamiResponse {
  success: true
  label: string
  home_scope: string
  read_scopes: string[]
  admin: boolean
  // Modell-C tenant identity (060): owning tenant UUID + per-tenant role
  // (owner|admin|member). Orthogonal to the server-global `admin` flag — the
  // gate uses both to tell server-admin from tenant-admin.
  tenant_id: string
  role: string
  // The caller's OWN key id (not a secret — neither hash nor plaintext). Used
  // for the Self-Revoke-Schutz (design/05 §7.5): the row of the current key is
  // marked so its revoke control is disabled. Additive read, no migration.
  api_key_id: string
  // The UUID of the person the calling key belongs to (OAuth F2, migration 095).
  // Not a secret and NOT an authorisation input (INV-B) — used only to group a
  // person's keys in the identity UI. OPTIONAL + additive like `capabilities`:
  // absent on a pre-F2 server (an old ctx_auth without the column). Additive read.
  principal_id?: string
  // The owning tenant's human-readable identity (059 context_tenants), resolved
  // by a LEFT JOIN on the key's tenant_id (design/06 N9 / D1) so the header can
  // show a name instead of the bare UUID. Both degrade to "" when the key has
  // no tenant row. Additive read, no migration.
  tenant_slug: string
  tenant_display_name: string
  // Achse-04 feature gates (design 04 §3.2 B9 / §4.1.3, wave U04). Server-sent
  // (handler/whoami.go whoamiCapabilities — since v4.3.0 statically true; a
  // later per-tenant gate only changes the VALUE) and folded into the derived
  // caps (capabilitiesFor → viewWorkflow) so nav visibility needs no async
  // fetch. OPTIONAL + additive: absent on a pre-v4.3.0 server ⇒ every gate
  // false ⇒ dark-launch (routes reachable, section/tile hidden). The flat
  // `capabilities` bag IS the final wire shape (B9's `workflow:{enabled}`
  // sketch was superseded by the shipped Go struct; single read site:
  // capabilitiesFor).
  capabilities?: {
    /** The issue/board workflow surface is enabled for this key's tenant. */
    workflow?: boolean
  }
}

/** Effective-value provenance (handler/settings.go apiSource). */
export type SettingSource = 'default' | 'env' | 'db'

// Source: go/internal/handler/settings.go (settingView). `type` is the
// registry typeName (string|int|float|bool|protocol|think|seconds|hours|
// timezone|scopes — config/describe.go); `mutability` is hot|restart|
// coupled|coupled:embed-cache (config/registry.go). Unknown values of either
// must degrade to a read-only rendering (forward compatibility).
export interface SettingView {
  key: string
  env_var?: string
  type: string
  mutability: string
  value: unknown
  source: SettingSource
  default: unknown
  sensitive?: boolean
}

// Source: go/internal/handler/settings.go (HandleList).
export interface SettingsListResponse {
  success: true
  settings: SettingView[]
}

// Source: go/internal/handler/settings.go (HandlePut).
export interface SettingPutResponse {
  success: true
  key: string
  value: unknown
  source: SettingSource
  previous: { value: unknown; source: SettingSource }
  warnings: string[]
}

// Source: go/internal/handler/settings.go (HandleDelete).
export interface SettingDeleteResponse {
  success: true
  key: string
  value: unknown
  source: SettingSource
}

// Source: go/internal/handler/health.go (healthResponse) — the /api/status
// health block reuses the public /health shape. Service values are ok|error;
// overall status is ok|degraded|unhealthy.
export interface HealthStatus {
  status: string
  services: Record<string, string>
}

// effective_state is a CLOSED set (source: pool.go BackendStatus). profile-disabled
// (092, U01-W2) = an active disable-profile holds the backend out of every chain;
// precedence disabled > profile-disabled > cooldown > active.
export type BackendEffectiveState = 'active' | 'disabled' | 'profile-disabled' | 'cooldown'

// Source: go/internal/backends/pool.go (BackendStatus), surfaced via
// /api/status. trust is one of full-trust|no-credentials|non-personal|public.
// last_error_class is the sanitized error class (never a raw URL/body).
// disabled_by_profiles names the active profiles disabling this backend.
export interface BackendStatus {
  id: string
  name: string
  trust: string
  locality: string
  roles: string[]
  priority: number
  enabled: boolean
  effective_state: BackendEffectiveState
  disabled_by_profiles?: string[]
  cooldown_remaining_s: number
  consecutive_fails: number
  last_error_class?: string
  last_ok?: string
}

// Source: go/internal/backends/backend.go (ModelSpec) — a model_map value is
// either a bare model-id string (short form) or this object. backend-list
// always reads it back as the object form.
export interface ModelSpec {
  model: string
  params?: Record<string, unknown>
}

// Source: go/internal/handler/backends_manage.go (backendView) — the full
// editable backend as read back from backend-create/update/list. The resolved
// api key is NEVER serialized (json:"-"); api_key_ref is the harmless secret
// name. model_map keys are role names plus the special "default".
export interface BackendView {
  id: string
  name: string
  base_url: string
  protocol: string
  provider_class: string
  api_key_ref: string
  trust: string
  locality: string
  roles: string[]
  model_map: Record<string, ModelSpec>
  timeouts: Record<string, number>
  num_ctx: number
  priority: number
  enabled: boolean
  extra_headers: Record<string, string>
  extra_body: Record<string, unknown>
  limits: Record<string, unknown>
  metadata: Record<string, unknown>
  // disable_profiles is the config-time disable-profile MEMBERSHIP (092, U01-W4):
  // the full set of profile names this backend belongs to, incl. inactive ones.
  // Distinct from disabled_by_profiles (the ACTIVE subset holding it out of the
  // chain right now). The W6 BackendDialog checkbox section reflects this field;
  // omitted by the server when empty (attachDisableProfiles).
  disable_profiles?: string[]
}

// Source: go/internal/handler/backends_manage.go (backend-list) — backendView
// merged with the live pool status by id; the status keys are renamed off
// BackendStatus (cooldown_remaining_s, last_error vs last_error_class).
export interface BackendListItem extends BackendView {
  effective_state: BackendEffectiveState
  disabled_by_profiles?: string[]
  cooldown_remaining_s: number
  consecutive_fails: number
  last_error?: string
  last_ok?: string
}

// Source: go/internal/handler/backends_manage.go (backendSpec) — the create/
// update patch. UPDATE semantics: an omitted scalar key keeps its value; an
// explicit `roles:[]` / `model_map:{}` CLEARS it (server reads raw-key
// presence). confirm_trust_elevation is added by the client wrapper, not here.
export interface BackendSpec {
  name?: string
  base_url?: string
  protocol?: string
  provider_class?: string
  api_key_ref?: string
  trust?: string
  locality?: string
  roles?: string[]
  model_map?: Record<string, ModelSpec | string>
  num_ctx?: number
  priority?: number
  enabled?: boolean
  // disable_profiles (092, U01-W4) rides on backend-update: profile NAMES the
  // server resolves to ids and syncs into the join. Raw-key presence semantics
  // like the rest of the patch — an absent key leaves membership untouched, an
  // explicit [] clears ALL memberships of this backend.
  disable_profiles?: string[]
}

export interface BackendListResponse {
  success: true
  backends: BackendListItem[]
}

// create/update share this shape; warnings carries the F3 validateRoles
// advisories (non-core role, dream-on-local), present only when non-empty.
export interface BackendMutateResponse {
  success: true
  backend: BackendView
  warnings?: string[]
}

export interface BackendDeleteResponse {
  success: true
  deleted: string // the deleted backend's name, not its id
}

// Source: go/internal/handler/backends_manage.go (backend-test). Always HTTP
// 200 even when unreachable — the verdict is in the body. checks is a free map
// of probe-name → status string; openrouter is present only for that class.
export interface BackendTestResult {
  success: true
  reachable: boolean
  latency_ms: number
  checks: Record<string, string>
  openrouter?: {
    credits_remaining?: number
    usage_usd?: number
    zdr_endpoints?: number
  }
}

// Source: go/internal/handler/status.go (dreamStatus). mode is on|throttled|
// off; the queue fields are dream.QueueStats verbatim; last_cycle_at is the
// last dream-cycle LLM call.
export interface DreamStatus {
  mode: string
  throttle_interval_s: number
  pickable_now: number
  in_cooldown: number
  never_dreamed: number
  awaiting_embed: number
  incoming_1h: number
  incoming_6h: number
  next_pending_at: string | null
  last_cycle_at: string | null
}

// Source: go/internal/handler/status.go (llm24hRow) — telemetry aggregate, NO
// prompt/response bodies.
export interface LLM24hRow {
  backend: string
  pipeline: string
  calls: number
  avg_ms: number
  errors: number
  prompt_tokens: number
  completion_tokens: number
}

// Source: go/internal/handler/status.go (activityStatus) — Wave-G host idle
// signal; the whole field is null until the agent pushes.
export interface ActivityStatus {
  host: string
  idle_ms: number
  updated_at: string
}

// =============================================================================
// Dispatch status surfaces (inference-scheduler MW12/MW12b, design/05 §4.5).
// TWO deliberately distinct wire shapes (K13 exposure doctrine), drift-anchored
// to go/internal/handler/status_dispatch.go (dispatchStatus / dispatchTenant-
// Status), pinned by TestStatusGoldenKeys + status_dispatch_test.go:
//   - DispatchStatus (server-admin): the FULL registry snapshot incl. per-fair-
//     key bucket detail, wait aggregates, preempt/aged counters, embed-token
//     rollup and the P6 last-run timestamps.
//   - DispatchTenantStatus (tenant-admin): the coarsened occupancy view. It
//     carries NO fair_key field at all — a foreign principal is structurally
//     unreachable through it (F-B3). `depth` is the coarsened waitQ bucket, NOT
//     a live count; there is deliberately no exact foreign-load number.
// =============================================================================

// Source: status_dispatch.go (dispatchWait) — one class's wait aggregate. All
// *_ms are integer milliseconds; oldest/p95/max come off the MW7 K=512 ring.
export interface DispatchWait {
  waiting: number
  oldest_wait_ms: number
  p95_wait_ms: number
  max_wait_ms: number
  samples: number
}

// Source: status_dispatch.go (dispatchPreempt) — per-target preemption counters
// (since boot) + the last/max release latency. SERVER-ADMIN only.
export interface DispatchPreempt {
  preempts_total: number
  wasted_ms_total: number
  release_ms_last: number
  release_ms_max: number
  forced_releases_total: number
  aged_admits_total: number
  aged_preempts_total: number
}

// Source: status_dispatch.go (dispatchBucket) — per-fair-key detail. SERVER-
// ADMIN ONLY: fair_key is principal identity (HomeScope) and NEVER appears in
// the tenant shape.
export interface DispatchBucket {
  fair_key: string
  waiting: number
  oldest_wait_ms: number
  inflight: number
  tokens: number
  charges: number
}

// Source: status_dispatch.go (dispatchEmbedTokens) — the D1(a) embed-token
// metric: summed prompt_tokens of the embed pipelines per target over 24h.
export interface DispatchEmbedTokens {
  target: string
  prompt_tokens: number
}

// Source: status_dispatch.go (dispatchTarget) — one backend target's live
// occupancy + wait/preempt aggregates + per-fair-key buckets. SERVER-ADMIN.
export interface DispatchTarget {
  origin: string
  slots: number
  preempt_background: boolean
  herald_scope: string
  held: number
  inflight: number
  interactive: DispatchWait
  background: DispatchWait
  preempt: DispatchPreempt
  buckets: DispatchBucket[]
}

// Source: status_dispatch.go (dispatchStatus) — the FULL server-admin registry
// view. Present ONLY on a server-admin response (null for a tenant-admin).
export interface DispatchStatus {
  enabled: boolean
  enforcing: boolean
  demand: number
  reaps_total: number
  class_downgrades: number
  uncharged_calls: number
  ops_total: number
  max_op_ms: number
  last_guard_at: string | null
  last_digest_at: string | null
  last_overview_at: string | null
  embed_tokens: DispatchEmbedTokens[]
  targets: DispatchTarget[]
}

// Source: status_dispatch.go (dispatchTenantTarget) — the coarsened per-target
// occupancy a tenant may see. `depth` is 'leer' | 'niedrig' | 'hoch' (a
// coarsened waitQ bucket, NEVER an exact foreign count); own_* is the caller's
// OWN fairness bucket. There is deliberately NO fair_key / foreign-count field.
export type DispatchDepth = 'leer' | 'niedrig' | 'hoch'
export interface DispatchTenantTarget {
  origin: string
  busy: boolean
  depth: DispatchDepth
  own_waiting: number
  own_inflight: number
  own_oldest_wait_ms: number
}

// Source: status_dispatch.go (dispatchTenantStatus) — the coarsened tenant view.
// Present ONLY on a tenant-admin response (null for a server-admin).
export interface DispatchTenantStatus {
  targets: DispatchTenantTarget[]
}

// Source: go/internal/handler/status.go (statusResponse),
// pinned by TestStatusGoldenKeys. Exactly ONE of dispatch / dispatch_tenant is
// non-null per response: a server-admin gets the full `dispatch`, a tenant-admin
// gets the coarsened `dispatch_tenant` (so a tenant response can never carry a
// foreign fair_key, F-B3). Both null when no dispatch source is wired.
export interface StatusResponse {
  success: true
  as_of: string
  health: HealthStatus
  backends: BackendStatus[]
  dream: DreamStatus
  llm_24h: LLM24hRow[]
  llm_24h_complete: boolean
  // Disable-profile registry line (U01-W7), replacing the retired
  // gaming:{active} field. PRESENT on the server-admin path, ABSENT on the
  // per-tenant path (the Go field is *[]statusProfile,omitempty — the tenant
  // snapshot stays nulled, N8), hence optional. The status page guards `?? []`.
  profiles?: StatusProfile[]
  activity: ActivityStatus | null
  dispatch?: DispatchStatus | null
  dispatch_tenant?: DispatchTenantStatus | null
}

// Source: go/internal/handler/status.go (statusProfile). The slim per-tick shape
// of one disable-profile: NO member names / description (those load on-demand
// via disable-profile-list, §4.5-5). scope is the splice key alongside name
// (name is unique only per scope, AM-5); member_count is the total membership.
export interface StatusProfile {
  name: string
  scope: string
  label: string
  active: boolean
  member_count: number
}

// Source: go/internal/handler/events.go (statusEvent) — the SSE `status` event
// payload: the StatusResponse minus `backends` (its own `backends` event) and
// `success`. The client merges it onto the held StatusResponse (one render
// path shared with the GET /api/status poll fallback).
export interface StatusEvent {
  as_of: string
  health: HealthStatus
  dream: DreamStatus
  llm_24h: LLM24hRow[]
  llm_24h_complete: boolean
  // The SSE stream is server-admin-only, so profiles is always carried (a plain
  // array, never omitted, unlike the poll shape's optional field).
  profiles: StatusProfile[]
  activity: ActivityStatus | null
  // The SSE stream is SERVER-admin only (events.go RequireAdmin), so it carries
  // the FULL `dispatch` section — never the coarsened tenant shape. Absent on a
  // pre-MW12 server (⇒ the tile simply hides).
  dispatch?: DispatchStatus | null
}

// Source: go/internal/handler/llmlog.go (llmlogError) — class + length-capped
// detail; NEVER a full prompt body.
export interface LLMLogError {
  class: string
  detail: string
}

// Source: go/internal/handler/llmlog.go (llmlogEntry),
// pinned by TestLLMLogGoldenKeys. NO prompt/response bodies — the list is
// body-free by invariant (TestLLMLogNoPrompts); bodies live only behind the
// per-id LLMLogDetail fetch. The three dispatch-telemetry columns (MW10/091)
// are pure Lease measurands: queue wait in ms, the admission class the row ran
// under, and the abort cause (only background rows ever carry one). All three
// are null on a pre-scheduler / lease-free row.
export interface LLMLogEntry {
  id: string
  created_at: string
  pipeline: string
  model: string
  backend: string
  duration_ms: number | null
  error: LLMLogError | null
  prompt_tokens: number | null
  completion_tokens: number | null
  cost_usd: number | null
  queue_wait_ms: number | null
  dispatch_class: string | null
  dispatch_abort: string | null
}

// Source: go/internal/handler/llmlog.go (HandleLLMLog).
export interface LLMLogResponse {
  success: true
  entries: LLMLogEntry[]
}

// The reason a detail body is absent (llmlog.go bodyPresent/bodySealed/
// bodyEvicted): 'present' = bodies returned; 'sealed' = credentials-class row,
// bodies never stored (E4 no-shadow-corpus); 'evicted' = retention NULLed them
// (or a bodyless rejection line). The UI shows WHY rather than a blank card.
export type LLMLogBodyState = 'present' | 'sealed' | 'evicted'

// Source: go/internal/handler/llmlog.go (llmlogDetail),
// pinned by TestLLMLogDetailGoldenKeys. The ONLY llmlog shape that MAY carry the
// M025 prompt/reply bodies — and ONLY per-id, behind the gated GET
// /api/llmlog/{id} (server-admin any row; tenant-admin only its own keys' rows,
// uniform 404 otherwise). The three bodies are null unless body_state==='present'.
export interface LLMLogDetail {
  id: string
  created_at: string
  pipeline: string
  model: string
  backend: string
  required_sensitivity: string
  body_state: LLMLogBodyState
  request_system: string | null
  request_user: string | null
  response_content: string | null
}

// Source: go/internal/handler/llmlog.go (HandleLLMLogDetail happy path). An
// unknown or foreign id answers 404 {success:false,error:'not found'} (raised as
// ApiError, no existence oracle) — never this shape.
export interface LLMLogDetailResponse {
  success: true
  detail: LLMLogDetail
}

// Source: go/internal/store/sealbox.go (SecretMeta) + handler/sealbox.go
// (secretView). Write-only by design: NO ciphertext, nonce, value or
// fingerprint is ever returned (pinned by the server response-scan test).
// rotated_at is omitempty (absent until first rotation); referenced_by is
// always present (the settings keys whose secret_ref points at this secret).
export interface SecretMeta {
  name: string
  key_version: number
  created_at: string
  rotated_at?: string
  referenced_by: string[]
}

// Source: go/internal/handler/sealbox.go (HandleList).
export interface SecretListResponse {
  success: true
  secrets: SecretMeta[]
}

// Source: go/internal/handler/sealbox.go (HandlePut). action distinguishes a
// first insert from a re-encrypt of an existing name.
export interface SecretPutResponse {
  success: true
  name: string
  action: 'create' | 'rotate'
}

// Source: go/internal/handler/sealbox.go (HandleDelete). A 409 (still
// referenced) instead carries {success:false, error, referenced_by} and is
// surfaced as ApiError — the referenced_by list is read off the error path.
export interface SecretDeleteResponse {
  success: true
  name: string
  deleted: true
}

// =============================================================================
// Server-Admin / Multi-Tenant manage types (design 04 §2/§3, Wave A1).
// Drift anchor = the Go structs + manage handlers cited per type. ADDITIVE —
// nothing above this line is touched. NOTE for TK1 (lib/api/keys.ts): A1 owns
// exactly the symbols below; keys.ts must not redeclare any of them.
// =============================================================================

// Source: go/internal/store/tenant.go:104 (Tenant) — a row in context_tenants,
// the owner/management register (059). NO live counts and NO scope LIST are
// carried here (tenant-get returns this verbatim, store/tenant.go:190); a tenant
// owns 0..N scopes, read separately. status is the 059 CHECK domain.
// max_scopes/max_keys are the BE5 structural Tenant-Limits (design 02): stored in
// context_tenants.metadata JSONB (migration-free, 059) and echoed on tenant-get/
// list/create. null OR absent = unlimited (063 NULL-unlimited contract).
export type TenantStatus = 'active' | 'suspended' | 'offboarding'
export interface Tenant {
  id: string
  slug: string
  display_name: string
  status: TenantStatus
  created_at: string
  updated_at: string
  max_scopes?: number | null
  max_keys?: number | null
}

// Source: go/internal/store/tenant_grants.go:21 (TenantGrant) — one cross-tenant
// READ grant (061): grantee_tenant gains read access to granted_scope, a scope
// owned by ANOTHER tenant. created_by is the api_key_id of the admin who created
// it (nullable — FK ON DELETE SET NULL).
export interface TenantGrant {
  id: string
  grantee_tenant: string
  granted_scope: string
  created_at: string
  created_by: string | null
}

// Source: go/internal/store/block_grants.go:61 (BlockGrant) — one row-level READ
// grant (067): grantee_tenant gains read access to a SINGLE block_id living in
// another scope/tenant. granted_by is the api_key_id of the granting admin
// (nullable — FK ON DELETE SET NULL).
export interface BlockGrant {
  id: string
  block_id: string
  grantee_tenant: string
  granted_by: string | null
  created_at: string
}

// Source: go/internal/backends/quota.go:34 (TenantQuota.OnExceed) + the 063
// CHECK. external_off (the default) degrades a budget-exhausted tenant to local
// backends; block hard-errors.
export type QuotaOnExceed = 'block' | 'external_off'

// Source: go/internal/handler/quota_manage.go:46 (quotaView). SCOPE-keyed, never
// tenant-id-keyed — a tenant has 0..N scopes, each with its own quota row. Two
// branches: a nil policy (no row) renders {scope, enabled:false, unlimited:true};
// a real policy renders the budget fields, where a null cost/calls field is the
// "unlimited for that dimension" state (the *float64/*int pointer was nil).
export interface TenantQuotaView {
  scope: string
  enabled: boolean
  unlimited?: boolean
  daily_cost_usd?: number | null
  monthly_cost_usd?: number | null
  daily_calls?: number | null
  on_exceed?: QuotaOnExceed
}

// Source: go/internal/handler/quota_manage.go:35 (quotaSpec) — the tenant-quota-
// set payload. scope is required; every budget field is optional and null = that
// dimension is unlimited (the server stores a nil *float64/*int). on_exceed
// defaults to external_off, enabled defaults to true (server-side).
export interface QuotaSpec {
  scope: string
  daily_cost_usd?: number | null
  monthly_cost_usd?: number | null
  daily_calls?: number | null
  on_exceed?: QuotaOnExceed
  enabled?: boolean
}

// Source: go/internal/events/audit.go:60 (AuditSample) — one dry-run verdict for
// the N=30 sample gate. credentials/personal are *bool (null = question not
// reached / no verdict).
export interface AuditSample {
  id: string
  title: string
  credentials: boolean | null
  personal: boolean | null
  verdict: string
}

// Source: go/internal/events/audit.go:71 (AuditStatus) — in-memory state of the
// current/last G41 sensitivity-audit run. started_at/finished_at use omitzero;
// last_error/samples are omitempty → all optional on the wire.
export interface AuditStatus {
  running: boolean
  dry_run: boolean
  started_at?: string
  finished_at?: string
  processed: number
  kept_credentials: number
  to_personal: number
  to_internal: number
  no_verdict: number
  discarded: number
  aborted: boolean
  last_error?: string
  samples?: AuditSample[]
}

// Source: go/internal/events/classify.go:44 (ClassifySample) — one pattern hit
// for the dry-run gate; never carries the matched secret.
export interface ClassifySample {
  id: string
  title: string
  kind: string
}

// Source: go/internal/events/classify.go:51 (ClassifyStatus) — in-memory state of
// the current/last G40 credentials-classify run. Same omitzero/omitempty wire
// rules as AuditStatus.
export interface ClassifyStatus {
  running: boolean
  dry_run: boolean
  started_at?: string
  finished_at?: string
  scanned: number
  upgraded: number
  discarded: number
  aborted: boolean
  last_error?: string
  samples?: ClassifySample[]
}

// Source: go/internal/store/scope_overview.go:22 `ScopeOverview` (the A0
// `scope-overview` read-action, design 04 §3 A0) — BUILT and tierServerAdmin-gated
// in the Go backend (handler/scope_overview.go:18 handleScopeOverview). The JSON
// field names are a FROZEN contract pinned by the Go struct's json tags (the store
// doc comment forbids renaming scope/block_count/key_count/tenant_id). Shape:
// per-scope block count (SELECT scope, count(*) ... GROUP BY scope, unscoped) + the
// per-scope api-key count (home_scope ∪ allowed_scopes) + the owning tenant from
// context_tenant_scopes (null for a system scope like '_global' or an unmapped
// Altbestand scope).
export interface ScopeOverview {
  scope: string
  block_count: number
  key_count: number
  tenant_id: string | null
}

// --- Response envelopes (one per manage action; success:true on the happy path,
// every failure shape — incl. {success:false} inside an HTTP 200 — is raised as
// ApiError by apiFetch, api.ts:103). ---

// Source: go/internal/handler/tenant_manage.go:80 (tenant-list).
export interface TenantListResponse {
  success: true
  tenants: Tenant[]
}

// Source: tenant_manage.go:71/:97/:132 — tenant-create/get/update share this.
export interface TenantResponse {
  success: true
  tenant: Tenant
}

// Source: tenant_manage.go:172 (tenant-delete) — `deleted` is the pruned tenant's id.
export interface TenantDeleteResponse {
  success: true
  deleted: string
}

// Source: go/internal/handler/scope_overview.go:31 (scope-overview) — one
// ScopeOverview row per scope across the whole store, server-admin only.
export interface ScopeOverviewListResponse {
  success: true
  scopes: ScopeOverview[]
}

// Source: tenant_manage.go:231 (tenant-grant-list).
export interface TenantGrantListResponse {
  success: true
  grants: TenantGrant[]
}

// Source: tenant_manage.go:216 (tenant-grant-create).
export interface TenantGrantResponse {
  success: true
  grant: TenantGrant
}

// Source: tenant_manage.go:248 (tenant-grant-delete) — `deleted` is the grant id.
export interface TenantGrantDeleteResponse {
  success: true
  deleted: string
}

// Source: go/internal/handler/block_grant_manage.go:138 (block-grant-list) —
// owner-side only (filtered to the caller's own tenant; no global oracle, :124).
export interface BlockGrantListResponse {
  success: true
  grants: BlockGrant[]
}

// Source: block_grant_manage.go:120 (block-grant-create).
export interface BlockGrantResponse {
  success: true
  grant: BlockGrant
}

// Source: block_grant_manage.go:171 (block-grant-revoke) — echoes the revoked pair.
export interface BlockGrantRevokeResponse {
  success: true
  revoked: { block_id: string; grantee_tenant: string }
}

// Source: quota_manage.go:76/:136 — tenant-quota-get/set share this.
export interface TenantQuotaResponse {
  success: true
  quota: TenantQuotaView
}

// Source: go/internal/handler/blocks_audit.go:103 (writeBlocksAuditStatus) —
// blocks-audit-start/status share this. by_source is sensitivity_source → count
// (store.AuditProgress, sensitivity.go:166); scope is the tenant-LESS Scheduler
// home_scope snapshot (per-tenant audit is a later backend cut, §6.6).
export interface BlocksAuditStatusResponse {
  success: true
  scope: string
  pending: number
  by_source: Record<string, number>
  run: AuditStatus
}

// Source: go/internal/handler/blocks_classify.go:93 (writeBlocksClassifyStatus) —
// blocks-classify-start/status share this. NO `pending` (classify reports only
// by_source + run, unlike audit).
export interface BlocksClassifyStatusResponse {
  success: true
  scope: string
  by_source: Record<string, number>
  run: ClassifyStatus
}

// =============================================================================
// Tenant-Admin API-key manage types (design 05 §3, Wave TK1). ADDITIVE — owned
// by lib/api/keys.ts; the A1 section above (incl. TenantQuotaView /
// TenantQuotaResponse) is reused, never redeclared.
// =============================================================================

// Source: go/internal/store/api_keys.go:25 (ApiKey) — one row of context_api_keys
// as read back by api-key-list (store.ListApiKeys SELECT api_keys.go:118, scan
// :132). Metadata ONLY: the plaintext key is NEVER on this shape (it is returned
// once at creation, ApiKeyCreateResult). last_used_at is omitempty (null until
// first use → render as '—', never an epoch/Invalid-Date). tenant_role is the 059
// owner|admin|member column; it is NOT on the list SELECT yet (LÜCKE 1 / TK7a) —
// optional until that additive read-enrichment lands.
export interface ApiKeyView {
  id: string
  label: string
  home_scope: string
  allowed_scopes: string[]
  active: boolean
  last_used_at?: string
  created_at: string
  tenant_role?: string
}

// Source: go/internal/handler/context_manage.go:1346-1353 (handleApiKeyCreate
// happy path). The plaintext `api_key` is shown EXACTLY ONCE and is never re-
// derivable (only its SHA-256 hash is retained server-side) — the client must
// reveal-and-discard it, never persist it. tenant_role is the 059 owner|admin|
// member column: BE6 (design 03 §2/§7) adds it to the CreateApiKey RETURNING so
// the compound tenant-create owner-key carries role='owner'. OPTIONAL on the wire
// — absent on a pre-BE6 server and the reveal-once flows don't need it (design 05
// Offene #6); read-lenient bare string, like ApiKeyView.tenant_role. (Design 03
// §7 wrote it required; kept optional here so the pre-BE6 literal in
// keys.svelte.test.ts:44 still type-checks — a strict relaxation, not a drift.)
export interface ApiKeyCreateResult {
  success: true
  id: string
  label: string
  home_scope: string
  allowed_scopes: string[]
  api_key: string
  tenant_role?: string
}

// Source: go/internal/handler/context_manage.go:1370 (handleApiKeyList happy
// path). Tenant-filtered for a non-server-admin (resolveApiKeyListParams :1394-
// 1396) and active-only unless active_only=false is sent explicitly.
export interface ApiKeyListResponse {
  success: true
  keys: ApiKeyView[]
}

// =============================================================================
// Self-Service capabilities — frozen wire contract (FE-1 / FE-T1, design
// 05-frontend-a3-selfservice §1). ADDITIVE: only NEW interfaces live here. The
// in-place spec extensions ApiKeyCreateSpec.tenant_id (→ keys.ts, FE-T2) and
// TenantSpec.max_scopes/max_keys (→ tenants.ts, FE-T3) are deliberately NOT
// redeclared here — a duplicate would be a TS redeclare error (design 05 §1).
// Drift anchor = the Go structs + manage handlers cited per type.
// =============================================================================

// The 059 tenant_role CHECK domain (owner|admin|member), mirroring how
// TenantStatus names the 059 status domain. Used by the WRITE spec below, where
// the client MUST send a valid role; the READ shapes (ApiKeyView.tenant_role,
// WhoamiResponse.role, ApiKeyCreateResult.tenant_role) stay bare `string` for
// forward-compat (a future server role must not break the parse).
export type TenantRole = 'owner' | 'admin' | 'member'

// Source: design 03-be6-roles §5 (handleApiKeyUpdate). api-key-update mutates the
// 059 tenant_role and/or the active flag of ONE key, tenant-isolated. At least
// one of tenant_role/active must be set (server 400 otherwise). home_scope /
// allowed_scopes are deliberately NOT updatable from the FE (self-elevation risk,
// design 05 Offene #5). tierTenantAdmin; the per-resource delegation + last-owner
// guard is server-side (409/403).
export interface ApiKeyUpdateSpec {
  id: string
  tenant_role?: TenantRole
  active?: boolean
}

// Source: design 03-be6-roles §5 (handleApiKeyUpdate, 200 happy path) — the
// re-read key row after the update, carrying the new tenant_role + active.
export interface ApiKeyUpdateResult {
  success: true
  key: ApiKeyView
}

// Source: design 01-be5-scope-assign §3 (handleScopeCreate). The client sends
// ONLY the bare `name`; the server builds the full `<tenant_slug>:<name>` from
// ar.TenantID (S1 — server-authoritative prefixing, prefix injection is
// structurally impossible client-side). tenant_id is a server-admin-only override
// (A3c, provision for a foreign tenant); a non-server-admin's tenant_id is
// ignored/rejected server-side. tierTenantAdmin.
export interface ScopeCreateSpec {
  name: string
  tenant_id?: string
}

// Source: design 01-be5-scope-assign §3 (scope-create response, the handler
// owner) + design 05 §1. SLIM shape { success, scope, tenant_id } — `scope` is
// the FULL server-built name (e.g. "acme:research"). NOTE (unreconciled
// cross-doc): design 06 CD-7 ASSUMED a ScopeOverview-shaped nested
// `scope:{scope,block_count,key_count,tenant_id}`, but 06 itself defers to 01/02
// ("Falls Design 01/02 eine schlankere Shape friert … anpassen"). The handler
// doc (01) freezes the slim form → slim wins.
export interface ScopeCreateResult {
  success: true
  scope: string
  tenant_id: string
}

// Source: design 03-be6-roles §6 (handleTenantCreate compound JSON, the handler
// owner) + design 06 fixtures (§2.4 owner_key:'ctx_sk_…'). The compound
// tenant-create is atomic: tenant row + initial auto-prefixed scope + minted
// owner-key (role 'owner') — solving the K10 inert-tenant gap. owner_key is the
// reveal-once PLAINTEXT (shown exactly once, RevealOnceKey hygiene), NOT a nested
// object. UNRECONCILED cross-doc: design 05 §1 instead named the scope field
// `initial_scope` and typed owner_key as `ApiKeyCreateResult` (nested); the
// handler (03) + fixtures (06) concur on flat `scope` + flat `owner_key: string`,
// so that is encoded here. The master-synthesis must settle the field names
// before FE-A3a consumes the value (design 05 §4 wrote `result.owner_key.api_key`,
// which does NOT match this flat shape).
export interface TenantCreateResult {
  success: true
  tenant: Tenant
  scope: string
  owner_key_id: string
  owner_key: string
}

// Source: design 02-be5-tenant-quota §3 (tenant-limit-set / tenant-create
// seeding) — the structural Tenant-Limits patch. BOTH fields are mandatory in the
// spec (mirrors the server-side requirePresence on tenant-limit-set); null =
// unlimited for that dimension. Distinct axis from the per-scope cost/call
// TenantQuota (063): structure, not spend.
export interface TenantLimitSpec {
  max_scopes: number | null
  max_keys: number | null
}

// Source: design 05 §1 (tenant-usage-get) — tenant-admin-readable usage + limits
// for the /tenant self-service quota visibility (S3). null = unlimited. FLAG
// (design 05 Offene #3): this READ action is so far modeled ONLY in 05 — NOT yet
// in 02-be5-tenant-quota or 04-security-model. It must be frozen as
// tierTenantAdmin with the handler PINNING a non-server-admin onto ar.TenantID
// (req.ID read only when IsServerAdmin), else it is a cross-tenant counts leak.
export interface TenantUsageView {
  tenant_id: string
  max_scopes: number | null
  max_keys: number | null
  scope_count: number
  key_count: number
}
export interface TenantUsageResponse {
  success: true
  usage: TenantUsageView
}

// =============================================================================
// Block-type registry — frozen wire contract (workflow W1, K5). ADDITIVE. Drift
// anchor = go/internal/handler/types.go (typeView) + go/internal/store/
// blocktypes.go (BlockTypeRow), pinned by TestTypesGoldenShape; the config
// envelope vocabulary is design 01-§3.3 (the field truth per masterplan K5).
// A change to either side must update BOTH in lockstep (Go golden ↔ this file).
// =============================================================================

// The origin badge on the effective type (handler/types.go typeSourceForScope):
// 'builtin' = a row in the shipped '_global' namespace, 'tenant' = the caller's
// own tenant-scoped overlay. Derived from scope, not the `builtin` column.
export type TypeSource = 'builtin' | 'tenant'

// The config JSONB envelope (design 01-§3.3, "v":1). ALL fields optional on the
// wire: a stored config may be partial and the server fills the documented
// defaults at resolve time. The UI must render an unknown/missing field as its
// default, never crash — new config keys are forward-compatible.
export interface BlockTypeConfig {
  v: 1
  retrieval?: {
    policy?: 'full-pass' | 'excluded' | 'damped' | 'aggregate-to-parent'
    damping_factor?: number
    intent_patterns?: string[]
  }
  guard?: {
    check?: boolean
    candidate?: boolean
    threshold_duplicate?: number | null
    threshold_review?: number | null
  }
  dream?: { linkable?: boolean; link_classes?: string[] | null }
  digest?: { include?: boolean }
  overview?: { include?: boolean }
  parent?: { mode?: 'none' | 'optional' | 'required'; relationship?: string | null }
  classify?: {
    priority?: number
    metadata_flags?: string[]
    source_prefixes?: string[]
    title_patterns?: string[]
  }
  // Workflow status set (design 01-§3.3 cfgWorkflow, policy.go:281). Carried
  // VERBATIM in the config RawMessage (blocktypes.go:36), so it rides the wire
  // for every workflow-bearing type even though the earlier waves did not read
  // it. The board (U07) consumes `states` (the ordered column set / known-status
  // vocabulary) and `terminal` (the closed subset → collapsed columns) — the
  // board wire itself carries NO category, so this is the only policy source for
  // the open/closed/unmapped verdict (board-columns.ts).
  workflow?: {
    states?: string[]
    initial?: string
    terminal?: string[]
    forge_state_map?: Record<string, string>
  }
}

// Source: go/internal/handler/types.go (typeView) — one row of the effective
// type registry as read back by GET /api/types[/{name}]. config is carried
// verbatim (the API shows what the row stores); updated_by is omitempty (absent
// on a never-touched builtin seed row → optional on the wire).
export interface BlockTypeView {
  id: string
  name: string
  scope: string
  display_name: string
  description: string
  builtin: boolean
  is_default: boolean
  config: BlockTypeConfig
  created_at: string
  updated_at: string
  updated_by?: string
  source: TypeSource
}

// Source: go/internal/handler/types.go (HandleList) — effective list, '_global'
// ∪ the caller's own tenant namespace, tenant rows shadowing '_global' by name.
export interface TypesListResponse {
  success: true
  types: BlockTypeView[]
}

// Source: go/internal/handler/types.go (HandleGet) — one type; a foreign or
// unknown name is 404 {success:false,error} (raised as ApiError, no oracle).
export interface TypeResponse {
  success: true
  type: BlockTypeView
}

// Source: go/internal/handler/types_write.go (typeWritePayload) — the PUT
// /api/types/{name} body (workflow W2 write surface). `name` comes from the URL
// and `scope` is role-pinned server-side — NEITHER is a body field (the strict
// decoder 400s on an unknown/extra key, §5.2 scope-injection class). Every field
// is optional: an absent field keeps its value on update / takes the documented
// default on create, so a bare `{}` PUT never silently clears a field. `config`
// is the full BlockTypeConfig envelope (validated by DecodePolicy — a violation
// is a 422, surfaced as ApiError). is_default cannot change on an existing type
// (loud 422, not a silent no-op).
export interface BlockTypeWriteSpec {
  display_name?: string
  description?: string
  is_default?: boolean
  config?: BlockTypeConfig
}

// Source: types_write.go HandleDelete happy path — `deleted` is the removed
// type's name. A builtin (409 ErrBlockTypeBuiltin) or a still-referenced type
// (409 + active/archived count in the {success:false} envelope) is raised as
// ApiError, never this shape.
export interface TypeDeleteResponse {
  success: true
  deleted: string
}

// ============================================================================
// Workflow-UI wire types (design/04 §3.1 / U03). The fixtures in
// src/lib/api/__fixtures__/*.json are the shared contract source; the Go golden
// TestContractFreezeGolden (internal/handler/contract_freeze_golden_test.go)
// re-serializes the SAME files from the live handler structs, so a drift on
// either side turns red before deploy.
//
// K04-A is decided: the endpoint family is the /api/project/{id}/issues-style
// (built by W6/W7/W11), NOT the §3.1 POST /api/issues/list proposal. The shapes
// below follow the IST serialization.
// §3.1 → Ist DEVIATIONS (Ist wins): the list row is IssueRow (WorkflowBlockRow) —
// it has NO sync_state/external_ref/comment_count/labels/status field and NO
// top-level total/writable; the cursor is an OPAQUE base64 string, not the
// {after_updated,after_id} pair. Detail/comment bodies are the full block.
// ============================================================================

/** Server-set trust tag: issue/comment bodies are attacker-controlled markdown,
 * so every body-bearing response carries render:'untrusted' and the client must
 * route the body through the sanitising markdown pipeline. */
export type RenderMode = 'untrusted'

/** Opaque base64url keyset cursor — echoed straight back as ?after=. `null` =
 * end-of-data; on the FTS (q!=='') path it is ALWAYS null (rank order is not
 * keyset-paginable, context_search.go:110-114). Never parsed client-side. */
export type IssueCursor = string | null

// Source: go/internal/store/workflow.go (WorkflowBlockRow) — the minimal list +
// board row. Carries NO content (10k+ rows × N columns). type_name +
// workflow_status are the registry vocabulary the badges + board columns consume.
export interface IssueRow {
  id: string
  scope: string
  type_name: string
  title: string
  workflow_status: string
  updated_at: string
}

// Source: go/internal/store/blocks.go (Block, issueScanCols) — the full issue OR
// comment body (W6 detail/comment endpoints). content is the markdown SOURCE
// (HTML is client-only). Fields the issue SELECT leaves empty are omitempty on
// the wire (optional here); a comment has no workflow_status (absent), and `type`
// carries the registry key ('issue' | 'comment').
export interface IssueBlock {
  id: string
  category: string
  tags: string[]
  title: string
  content: string
  metadata: Record<string, unknown>
  scope: string
  content_hash?: string
  guard_status?: string
  sensitivity?: string
  sensitivity_source?: string
  type?: string
  lifecycle_state?: string
  type_source?: string
  workflow_status?: string
  created_at: string
  updated_at: string
}

// Source: project_issues.go writeIssueListPage — GET /api/project/{id}/issues.
export interface IssueListResponse {
  success: true
  render: RenderMode
  issues: IssueRow[]
  cursor: IssueCursor
}

// Source: project_issues.go HandleDetail — GET /api/project/{id}/issues/{block_id};
// full issue + the first inline page of the comment thread.
export interface IssueDetailResponse {
  success: true
  render: RenderMode
  issue: IssueBlock
  comments: IssueBlock[]
  comments_cursor: IssueCursor
}

// Source: project_issues.go HandleComments — GET …/issues/{block_id}/comments (ASC).
export interface IssueCommentsResponse {
  success: true
  render: RenderMode
  comments: IssueBlock[]
  cursor: IssueCursor
}

// Source: project_issues.go HandleBoard — GET /api/project/{id}/board. Each column
// has an index-only count + a first page + a resume cursor (fed back as
// ?status=<col>&after=…). The column SET is the type-config status set (never
// hardcoded); a data status absent from the config is unmapped and not shown.
export interface BoardColumn {
  status: string
  count: number
  issues: IssueRow[]
  cursor: IssueCursor
}
export interface BoardResponse {
  success: true
  render: RenderMode
  columns: BoardColumn[]
}

// Source: project_issues_write.go HandleCreate/HandlePatch — POST/PATCH issue.
export interface IssueMutateResponse {
  success: true
  render: RenderMode
  issue: IssueBlock
}
// Source: project_issues_write.go HandleCommentCreate — POST …/comments.
export interface CommentCreateResponse {
  success: true
  render: RenderMode
  comment: IssueBlock
}

// Source: go/internal/store/project.go (ProjectRow) — the /api/project register
// row (W4). token_secret is NEVER on the wire (json:"-"); forge/sync_cursor/
// metadata are opaque JSON envelopes; last_error/backoff_until/created_by are
// omitempty (optional here).
export interface ProjectRow {
  id: string
  tenant_id: string
  scope: string
  identity: string
  display_name: string
  forge: Record<string, unknown> | null
  webhook_secret_ref: string | null
  sync_status: string
  sync_enabled: boolean
  push_enabled: boolean
  last_sync_at: string | null
  last_error?: string
  backoff_until?: string
  sync_cursor: unknown
  created_at: string
  created_by?: string
  metadata: unknown
}
export interface ProjectListResponse {
  success: true
  projects: ProjectRow[]
}
export interface ProjectResponse {
  success: true
  project: ProjectRow
}

// Source: go/internal/forge/sync.go (SyncStatus) — the in-memory run state.
// started_at/finished_at are omitzero (absent while zero); run_id/last_error are
// omitempty.
export interface SyncRunStatus {
  project_id: string
  running: boolean
  dry_run: boolean
  started_at?: string
  finished_at?: string
  fetched: number
  prs_skipped: number
  pages: number
  applied: number
  pushed: number
  conflicts: number
  aborted: boolean
  backoff_set: boolean
  run_id?: string
  last_error?: string
}
// Source: go/internal/store/project_sync.go (SyncRunRow) — one DB history row.
export interface SyncRunRow {
  id: string
  project_id: string
  started_at: string
  finished_at: string | null
  status: string
  error?: string
  stats: unknown
}
// Source: project_sync.go HandleStatus — GET /api/project/{id}/sync (in-memory run
// merged with the DB history + the register's display columns).
export interface SyncStatusResponse {
  success: true
  project_id: string
  sync_status: string
  sync_enabled: boolean
  push_enabled: boolean
  token_set: boolean
  last_sync_at: string | null
  backoff_until: string | null
  last_error: string | null
  conflicts: number
  run: SyncRunStatus
  last_run: SyncRunRow | null
  recent_runs: SyncRunRow[]
}
// Source: project_sync.go HandleStart — POST /api/project/{id}/sync.
export interface SyncStartResponse {
  success: true
  run: SyncRunStatus
}
