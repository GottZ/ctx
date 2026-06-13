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
