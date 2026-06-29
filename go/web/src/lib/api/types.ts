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

// Source: go/internal/backends/pool.go (BackendStatus), surfaced via
// /api/status. trust is one of full-trust|no-credentials|non-personal|public;
// effective_state is active|disabled|cooldown. last_error_class is the
// sanitized error class (never a raw URL/body).
export interface BackendStatus {
  id: string
  name: string
  trust: string
  locality: string
  roles: string[]
  priority: number
  enabled: boolean
  effective_state: string
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
}

// Source: go/internal/handler/backends_manage.go (backend-list) — backendView
// merged with the live pool status by id; the status keys are renamed off
// BackendStatus (cooldown_remaining_s, last_error vs last_error_class).
export interface BackendListItem extends BackendView {
  effective_state: string
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

// Source: go/internal/handler/status.go (statusResponse),
// pinned by TestStatusGoldenKeys.
export interface StatusResponse {
  success: true
  as_of: string
  health: HealthStatus
  backends: BackendStatus[]
  dream: DreamStatus
  llm_24h: LLM24hRow[]
  llm_24h_complete: boolean
  gaming: { active: boolean }
  activity: ActivityStatus | null
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
  gaming: { active: boolean }
  activity: ActivityStatus | null
}

// Source: go/internal/handler/llmlog.go (llmlogError) — class + length-capped
// detail; NEVER a full prompt body.
export interface LLMLogError {
  class: string
  detail: string
}

// Source: go/internal/handler/llmlog.go (llmlogEntry),
// pinned by TestLLMLogGoldenKeys.
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
}

// Source: go/internal/handler/llmlog.go (HandleLLMLog).
export interface LLMLogResponse {
  success: true
  entries: LLMLogEntry[]
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
// the owner/management register (059). NO counts and NO scopes are carried here
// (tenant-get returns this verbatim, store/tenant.go:190); a tenant owns 0..N
// scopes, read separately. status is the 059 CHECK domain.
export type TenantStatus = 'active' | 'suspended' | 'offboarding'
export interface Tenant {
  id: string
  slug: string
  display_name: string
  status: TenantStatus
  created_at: string
  updated_at: string
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

// Source: PLANNED — the optional additive A0 `scope-overview` read-action (design
// 04 §3 A0), NOT YET BUILT in the Go backend. Forward-declared so A2/A4 can type
// against it; field names are provisional until the A0 golden test pins them.
// Shape: per-scope block count (SELECT scope, count(*) ... GROUP BY scope) + the
// per-scope api-key count + the owning tenant from context_tenant_scopes (null
// for an unmapped Altbestand scope).
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
