// Session state (design 04-§3.4): API-key login via a GET /api/whoami probe,
// key held in sessionStorage (tab lifetime — reload keeps it, a new tab does
// not), read-only degradation derived from whoami.admin. The api client's
// 401 interceptor tears the session down when the stored key gets revoked.

import { ApiError, apiFetch, configureApi } from './api'
import type { WhoamiResponse } from './api/types'
import { capabilitiesFor } from './auth/capabilities'

const STORAGE_KEY = 'ctx.api-key'

export class Session {
  key = $state<string | null>(null)
  whoami = $state<WhoamiResponse | null>(null)
  /** True while the boot-time sessionStorage probe is in flight. */
  restoring = $state(false)
  /** Why the previous session ended — rendered on the login screen. */
  notice = $state<string | null>(null)

  readonly active = $derived(this.key !== null && this.whoami !== null)
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

  /** Probe the key against /api/whoami; persist it only on success. */
  async login(rawKey: string): Promise<void> {
    const key = rawKey.trim()
    const whoami = await apiFetch<WhoamiResponse>('/api/whoami', {}, { key })
    this.key = key
    this.whoami = whoami
    this.notice = null
    sessionStorage.setItem(STORAGE_KEY, key)
  }

  /**
   * Boot-time restore: re-probe a key surviving in sessionStorage (tab
   * reload). A rejected key (401/403) is dropped; transient failures
   * (network, 5xx) keep it so a later reload can retry.
   */
  async restore(): Promise<void> {
    if (this.active || this.restoring) return
    const stored = sessionStorage.getItem(STORAGE_KEY)
    if (stored === null || stored === '') return
    this.restoring = true
    try {
      const whoami = await apiFetch<WhoamiResponse>('/api/whoami', {}, { key: stored })
      this.key = stored
      this.whoami = whoami
    } catch (err) {
      if (err instanceof ApiError && (err.status === 401 || err.status === 403)) {
        sessionStorage.removeItem(STORAGE_KEY)
        this.notice = 'Stored key was rejected — sign in again.'
      }
    } finally {
      this.restoring = false
    }
  }

  /** User-initiated logout. */
  logout(): void {
    this.clear()
  }

  /** Interceptor path: the stored key stopped working mid-session. */
  invalidate(reason: string): void {
    this.clear()
    this.notice = reason
  }

  private clear(): void {
    this.key = null
    this.whoami = null
    sessionStorage.removeItem(STORAGE_KEY)
  }
}

export const session = new Session()

configureApi({
  getKey: () => session.key,
  onUnauthorized: () => session.invalidate('Session ended: the API key is no longer valid.'),
})
