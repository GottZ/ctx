// Tenant API-key editing state (design 05 §3, Wave TK2). The keys ARE the
// tenant's member-proxys; this holds the list, its load status and table-level
// action errors. Mutations call the tier-gated manage actions and reload the
// list so the live fields (active, last_used_at) stay fresh. The `show revoked`
// toggle flips the list's active_only (active_only = !showRevoked) so revoked
// (soft-deleted) keys can be surfaced. create() returns the show-once
// ApiKeyCreateResult for the caller to reveal-and-discard — the plaintext
// `api_key` is NEVER held on the model/store (design 05 §6 hygiene). remove()
// THROWS ApiError on failure so the table caller can render the 404-no-oracle
// message; busyId guards the in-flight row. Plain $state class with an
// injectable api so vitest covers the flow without a DOM. This is the COMPLETE
// mutation set as of TK2; FE-M2 (TK7b) adds `update` for the role/active
// delegation (api-key-update) — no other wave extends this file.

import { toApiError, type ApiError } from '../../lib/api'
import { createApiKey, deleteApiKey, listApiKeys, updateApiKey, type ApiKeyCreateSpec } from '../../lib/api/keys'
import type { ApiKeyCreateResult, ApiKeyUpdateSpec, ApiKeyView } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

interface KeysApi {
  list: typeof listApiKeys
  create: typeof createApiKey
  del: typeof deleteApiKey
  update: typeof updateApiKey
}

export class KeysModel {
  keys = $state<ApiKeyView[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** Row id with a table action (revoke) in flight. */
  busyId = $state<string | null>(null)
  /** Last table-action failure (revoke), shown above the table. */
  actionError = $state<string | null>(null)
  /** When true the list includes soft-deleted keys (sends active_only:false). */
  showRevoked = $state(false)

  #api: KeysApi

  constructor(
    api: KeysApi = {
      list: listApiKeys,
      create: createApiKey,
      del: deleteApiKey,
      update: updateApiKey,
    },
  ) {
    this.#api = api
  }

  async load(): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    try {
      const res = await this.#api.list(!this.showRevoked)
      this.keys = res.keys
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  reload = (): Promise<void> => this.load()

  byId(id: string): ApiKeyView | undefined {
    return this.keys.find((k) => k.id === id)
  }

  /**
   * Mint a key — throws ApiError on failure (e.g. firstScopeOutsideTenant 403),
   * returns the show-once ApiKeyCreateResult (carrying the plaintext `api_key`)
   * for the caller to reveal-and-discard, then reloads. The plaintext is NEVER
   * retained on the model (design 05 §6).
   */
  async create(spec: ApiKeyCreateSpec): Promise<ApiKeyCreateResult> {
    const res = await this.#api.create(spec)
    await this.load()
    return res
  }

  /**
   * Revoke (soft-delete) one key — throws on failure (the table caller surfaces
   * the 404-no-oracle "key not found"), reloads on success. busyId guards the
   * in-flight row. The server does NOT exclude the caller's own key; the
   * self-revoke guard is a UI concern (TK5), not enforced here.
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

  /**
   * Promote/demote a key's tenant_role and/or (de)activate it (api-key-update,
   * tierTenantAdmin, design 03-be6-roles). Mirrors remove(): busyId guards the
   * in-flight row, reload on success keeps the live tenant_role/active fields
   * fresh, and a failure (last-owner demote 409, self-lockout / cross-tenant 403)
   * is surfaced on actionError AND rethrown so the table caller can render it. The
   * client-side disable of these controls (last-owner, self-row) is UX only — the
   * server is authoritative.
   */
  async update(spec: ApiKeyUpdateSpec): Promise<void> {
    this.busyId = spec.id
    this.actionError = null
    try {
      await this.#api.update(spec)
      await this.load()
    } catch (err) {
      this.actionError = toApiError(err).message
      throw err
    } finally {
      this.busyId = null
    }
  }

  /** Flip the `show revoked` filter and reload with the new active_only. */
  async toggleRevoked(): Promise<void> {
    this.showRevoked = !this.showRevoked
    await this.load()
  }
}
