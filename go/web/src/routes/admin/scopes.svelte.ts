// Server-admin scope-landscape read-model (design 04 §3 A0, Wave A0-FE).
// READ-ONLY: surfaces the `scope-overview` action only — there is no scope
// mutation in this wave (scope-assign is the deferred §6.8 write-action). Mirrors
// TenantsModel: a plain $state class with an injectable api so vitest covers the
// load flow without a DOM. The client gate is UX, not security — the server
// answers 403 for a non-admin key, and that 403 lands here as a load error
// (apiFetch raises the envelope, api.ts:103).

import { toApiError, type ApiError } from '../../lib/api'
import { scopeOverview } from '../../lib/api/tenants'
import type { ScopeOverview } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

/** Injectable seam: only the read action this wave needs. */
export interface ScopesApi {
  overview: typeof scopeOverview
}

export class ScopesModel {
  scopes = $state<ScopeOverview[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)

  #api: ScopesApi

  constructor(api: ScopesApi = { overview: scopeOverview }) {
    this.#api = api
  }

  /** Load (or reload) the scope landscape. A 403 (non-admin key) or any other
   * failure is normalized to ApiError and surfaced as the error state. */
  async load(): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    try {
      const res = await this.#api.overview()
      this.scopes = res.scopes
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  reload = (): Promise<void> => this.load()
}
