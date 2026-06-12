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
