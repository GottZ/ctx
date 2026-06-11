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
