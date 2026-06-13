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
