// Server-admin tenant model (design 04 §3 Wave A2 read + design 05 §3 Wave FE-M1
// lifecycle). Holds the tenant register (the /admin list) and drives the
// register-level mutations: compound create (A3a), suspend/activate (status
// patch) and delete (A3b). Mirrors the KeysModel shape — a plain $state class
// with an injectable api so vitest covers the flows without a DOM, busyId guards
// the in-flight row, actionError surfaces a mutation failure above the table. The
// client gate is UX, not security — the server answers 403 for a non-admin key,
// and a load 403 lands here as loadError (apiFetch raises the envelope,
// api.ts:103). HYGIENE: create() returns the reveal-once TenantCreateResult (with
// the owner-key PLAINTEXT) for the dialog to reveal-and-discard; the plaintext is
// NEVER held on the model (same rule as KeysModel.create, design 05 §6).
//
// SCOPE NOTE (design 05 §3): the design scoped TenantsModel to create-only,
// because TenantDetail (/admin/tenants/:id) holds NO TenantsModel — its lifecycle
// mutations run as bare api fns + a local reload. The suspend/activate/remove
// here reload the LIST (listTenants) and drive the AdminPage register view; the
// FE-3 brief widens the model to carry them. TenantDetail keeps using the bare
// fns (its reload is local), so the two surfaces do not collide.

import { toApiError, type ApiError } from '../../lib/api'
import { createTenant, deleteTenant, listTenants, updateTenant, type TenantSpec } from '../../lib/api/tenants'
import type { Tenant, TenantCreateResult } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

/** Injectable seam: the read action plus the register-level lifecycle writes. */
export interface TenantsApi {
  list: typeof listTenants
  create: typeof createTenant
  update: typeof updateTenant
  del: typeof deleteTenant
}

export class TenantsModel {
  tenants = $state<Tenant[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** Row id with a lifecycle action (suspend/activate/delete) in flight. */
  busyId = $state<string | null>(null)
  /** Last lifecycle-action failure, shown above the register. */
  actionError = $state<string | null>(null)

  #api: TenantsApi

  constructor(
    api: TenantsApi = {
      list: listTenants,
      create: createTenant,
      update: updateTenant,
      del: deleteTenant,
    },
  ) {
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

  /**
   * Compound create (atomic, K10): tenant row + initial auto-prefixed scope +
   * minted owner-key. Returns the reveal-once TenantCreateResult — its `owner_key`
   * PLAINTEXT is for the dialog to reveal-and-discard and is NEVER retained on the
   * model (design 05 §6). Reloads the register on success so the new tenant shows;
   * throws ApiError (409 slug-exists / 400 reserved) for the dialog to render.
   */
  async create(spec: TenantSpec): Promise<TenantCreateResult> {
    const res = await this.#api.create(spec)
    await this.load()
    return res
  }

  /** Suspend a tenant (status→suspended); the server status-gate (060) takes
   * effect at the tenant's next auth. busyId guards the row, reload on success. */
  async suspend(id: string): Promise<void> {
    await this.#patchStatus(id, 'suspended')
  }

  /** Reactivate a suspended/offboarding tenant (status→active). */
  async activate(id: string): Promise<void> {
    await this.#patchStatus(id, 'active')
  }

  async #patchStatus(id: string, status: string): Promise<void> {
    this.busyId = id
    this.actionError = null
    try {
      await this.#api.update(id, { status })
      await this.load()
    } catch (err) {
      this.actionError = toApiError(err).message
      throw err
    } finally {
      this.busyId = null
    }
  }

  /**
   * Full-prune (irreversible) a tenant — all scope-carried data + keys + the row.
   * The default tenant is server-guarded (400, surfaced as actionError + rethrow);
   * the Slug-typing confirm is a UI concern. Reloads the register on success.
   */
  async remove(id: string): Promise<void> {
    this.busyId = id
    this.actionError = null
    try {
      await this.#api.del(id)
      await this.load()
    } catch (err) {
      this.actionError = toApiError(err).message
      throw err
    } finally {
      this.busyId = null
    }
  }
}
