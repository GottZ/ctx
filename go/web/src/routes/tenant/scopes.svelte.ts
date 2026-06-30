// Tenant-admin self-service scope model (design 05 §3, Wave FE-M3). Holds the
// tenant's OWN scopes (the authoritative `scope-list` rows — server-filtered on
// ar.TenantID) plus its structural usage/limits (`tenant-usage-get`, omitting the
// id so the server pins to the caller's tenant). create() sends ONLY the bare
// `name` (S1 — the server builds the full <slug>:<name> from ar.TenantID and
// echoes it back in ScopeCreateResult.scope; prefix injection is structurally
// impossible client-side), then reloads list + usage so the "X of N scopes"
// count and the limit banner stay fresh. Plain $state class with an injectable
// api so vitest covers the flow without a DOM. The limit visibility (atScopeLimit
// / atKeyLimit) is UX only — the server caps hard (429) and the model renders that.

import { toApiError, type ApiError } from '../../lib/api'
import { createScope, listScopes } from '../../lib/api/scopes'
import { getTenantUsage } from '../../lib/api/tenants'
import type { ScopeCreateResult, ScopeOverview, TenantUsageView } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

/** Injectable seam: the tenant-scoped read actions + the bare-name create. */
export interface ScopesSelfApi {
  list: typeof listScopes
  usage: typeof getTenantUsage
  create: typeof createScope
}

/** True iff the tenant is at/over its scope cap (create should be disabled). A
 * null max_scopes is unlimited → never at limit; a null usage (not loaded yet) is
 * treated as not-at-limit (the server stays authoritative). PURE — no runes. */
export function atScopeLimit(usage: TenantUsageView | null): boolean {
  if (!usage || usage.max_scopes === null) return false
  return usage.scope_count >= usage.max_scopes
}

/** True iff the tenant is at/over its key cap (the key-create CTA, SS2). Same
 * null-unlimited / null-usage rules as atScopeLimit. PURE — no runes. */
export function atKeyLimit(usage: TenantUsageView | null): boolean {
  if (!usage || usage.max_keys === null) return false
  return usage.key_count >= usage.max_keys
}

export class ScopesSelfModel {
  /** The caller's own tenant scopes (scope-list rows; tenant-filtered server-side). */
  scopes = $state<ScopeOverview[]>([])
  /** Usage + structural limits (null max = unlimited); drives the limit banners. */
  usage = $state<TenantUsageView | null>(null)
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** True while a scope-create is in flight. */
  busy = $state(false)
  /** Last create failure (400 charset / 429 over-limit), shown by the form. */
  actionError = $state<string | null>(null)

  #api: ScopesSelfApi

  constructor(
    api: ScopesSelfApi = {
      list: listScopes,
      usage: getTenantUsage,
      create: createScope,
    },
  ) {
    this.#api = api
  }

  /** At/over the scope cap — create disabled + banner. UX only (server 429s). */
  get atScopeLimit(): boolean {
    return atScopeLimit(this.usage)
  }

  /** At/over the key cap — key-create CTA disabled (SS2). UX only (server 429s). */
  get atKeyLimit(): boolean {
    return atKeyLimit(this.usage)
  }

  /** Load (or reload) the tenant scope list AND usage in parallel. Either action
   * 403ing (non-tenant-admin) or any other failure is normalized to ApiError and
   * surfaced as the error state. */
  async load(): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    try {
      const [list, usage] = await Promise.all([this.#api.list(), this.#api.usage()])
      this.scopes = list.scopes
      this.usage = usage
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  reload = (): Promise<void> => this.load()

  /**
   * Provision ONE scope for the caller's tenant. Sends only the bare `name` (S1);
   * the server prepends <slug>: and returns the full name in
   * ScopeCreateResult.scope — the caller renders THAT, never a client-built
   * prefix. Reloads list + usage on success (the count + limit banner refresh);
   * a 400 (charset) / 429 (over max_scopes) / 403 lands on actionError AND is
   * rethrown so the form can render it. busy guards the in-flight create.
   */
  async create(name: string): Promise<ScopeCreateResult> {
    this.busy = true
    this.actionError = null
    try {
      const res = await this.#api.create({ name })
      await this.load()
      return res
    } catch (err) {
      this.actionError = toApiError(err).message
      throw err
    } finally {
      this.busy = false
    }
  }
}
