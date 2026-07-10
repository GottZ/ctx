// Session state (design 05 §4.3/§4.4, OAuth R4/05-W5): der eingegebene Key
// wird bei POST /auth/login gegen httpOnly-Cookies (ctx_session + ctx_refresh)
// eingetauscht — der rohe Key berührt keinen Client-Storage mehr. Der CSRF-
// Synchronizer lebt NUR in-memory (Modul-State; eine JS-lesbare Kopie würde
// die httpOnly-Härtung untergraben) und erreicht den api-Layer via
// configureApi. Reload-Restore = whoami-Probe auf dem Cookie mit EINEM
// stillen /auth/refresh-Fallback; der 401-Interceptor räumt ab, wenn auch
// der Refresh tot ist.

import { apiFetch, configureApi, refreshSession } from './api'
import type { WhoamiResponse } from './api/types'
import { capabilitiesFor } from './auth/capabilities'

/** Wire-Shape von POST /auth/login (handler/auth_session.go HandleLogin). */
interface LoginResponse {
  success: true
  csrf_token: string
}

export class Session {
  /** Per-Session CSRF-Synchronizer (design 05 §4.4) — nur in-memory. */
  csrfToken = $state<string | null>(null)
  whoami = $state<WhoamiResponse | null>(null)
  /** True while the boot-time cookie probe is in flight. */
  restoring = $state(false)
  /** Why the previous session ended — rendered on the login screen. */
  notice = $state<string | null>(null)

  readonly active = $derived(this.whoami !== null)
  readonly admin = $derived(this.whoami?.admin ?? false)
  readonly label = $derived(this.whoami?.label ?? null)

  // Role-adaptive capability deriveds (design 06-role-nav.md §3/§4, Welle N2).
  // `caps` is the single source of truth (capabilitiesFor, Welle N1); the rest
  // expose the raw whoami identity fields for downstream consumers (Nav-Rail,
  // landing-redirect, route-guards, IdentityBadge/N7, key-mgmt/TK5). While
  // `restoring` or otherwise pre-login, whoami is null → capabilitiesFor(null)
  // yields tier='loading' with every flag false (restore race, §6/R6).
  readonly caps = $derived(capabilitiesFor(this.whoami))
  readonly tier = $derived(this.caps.tier)
  readonly role = $derived(this.whoami?.role ?? null)
  readonly tenantId = $derived(this.whoami?.tenant_id ?? null)
  readonly homeScope = $derived(this.whoami?.home_scope ?? null)
  readonly readScopes = $derived(this.whoami?.read_scopes ?? [])
  readonly tenantSlug = $derived(this.whoami?.tenant_slug ?? null)
  readonly tenantDisplayName = $derived(this.whoami?.tenant_display_name ?? null)
  readonly apiKeyId = $derived(this.whoami?.api_key_id ?? null)

  /**
   * Exchange the entered key for a cookie session (POST /auth/login), then
   * hydrate the identity over the fresh cookies. The raw key is used for the
   * exchange only — it is never kept client-side.
   */
  async login(rawKey: string): Promise<void> {
    const body = JSON.stringify({ api_key: rawKey.trim() })
    const login = await apiFetch<LoginResponse>('/auth/login', { method: 'POST', body }, { skipRefresh: true })
    const whoami = await apiFetch<WhoamiResponse>('/api/whoami', {}, { skipRefresh: true })
    this.csrfToken = login.csrf_token
    this.whoami = whoami
    this.notice = null
  }

  /**
   * Boot-time restore (Reload): whoami-Probe auf dem Session-Cookie. Ein
   * toter Access-Token bekommt EINEN /auth/refresh-Versuch (Refresh-Cookie
   * lebt evtl. noch), dann whoami erneut; sonst Login-Maske. Transiente
   * Fehler (Netz, 5xx) landen ebenfalls dort — die Cookies überleben, ein
   * späterer Reload probiert erneut.
   */
  async restore(): Promise<void> {
    if (this.active || this.restoring) return
    this.restoring = true
    try {
      let whoami = await this.probe()
      if (whoami === null && (await refreshSession())) whoami = await this.probe()
      if (whoami !== null) {
        this.csrfToken = whoami.csrf_token ?? null
        this.whoami = whoami
      }
    } finally {
      this.restoring = false
    }
  }

  /** whoami über das Cookie; null wenn die Session tot/unerreichbar ist. */
  private async probe(): Promise<WhoamiResponse | null> {
    try {
      return await apiFetch<WhoamiResponse>('/api/whoami', {}, { skipRefresh: true })
    } catch {
      return null
    }
  }

  /** User-initiated logout: revoke the server session, drop local state. */
  logout(): void {
    void apiFetch('/auth/logout', { method: 'POST' }, { skipRefresh: true }).catch(() => {
      // Best-effort — die Cookies clearen server-seitig; lokal wird immer abgeräumt.
    })
    this.clear()
  }

  /** Interceptor path: the cookie session died mid-flight (failed refresh). */
  invalidate(reason: string): void {
    this.clear()
    this.notice = reason
  }

  private clear(): void {
    this.csrfToken = null
    this.whoami = null
  }
}

export const session = new Session()

configureApi({
  getCsrfToken: () => session.csrfToken,
  onUnauthorized: () => session.invalidate('Session expired — sign in again.'),
})
