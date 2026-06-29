// Server-admin tenant read-model (design 04 §3, Wave A2). READ-ONLY: this wave
// surfaces `tenant-list` only — create/edit/suspend/delete is A3. Mirrors the
// KeysModel shape (a plain $state class with an injectable api so vitest covers
// the load flow without a DOM). The client gate is UX, not security — the server
// answers 403 for a non-admin key, and that 403 lands here as a load error
// (apiFetch raises the envelope, api.ts:103). A later wave (A3) extends this.

import { toApiError, type ApiError } from '../../lib/api'
import { listTenants } from '../../lib/api/tenants'
import type { Tenant } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

/** Injectable seam: only the read action this wave needs. */
export interface TenantsApi {
  list: typeof listTenants
}

export class TenantsModel {
  tenants = $state<Tenant[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)

  #api: TenantsApi

  constructor(api: TenantsApi = { list: listTenants }) {
    this.#api = api
  }

  /** Load (or reload) the tenant register. A 403 (non-admin key) or any other
   * failure is normalized to ApiError and surfaced as the error state. */
  async load(): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    try {
      const res = await this.#api.list()
      this.tenants = res.tenants
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  reload = (): Promise<void> => this.load()
}
