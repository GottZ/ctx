// Alias catalog state of /gateway/models (design/05 §4.6 store table row
// `gateway/aliases.svelte.ts AliasesModel`; wave W05-13 — RC-D C5). Plain
// $state class with an injectable api (vitest node, precedent
// consumers.svelte.ts). Rules:
//   - load(): model-alias-list (the caller's visible catalog: tenant-admin
//     _global + home scope, server-admin every scope);
//   - create(): `enabled` is ALWAYS on the wire (K-05-2); reload after success;
//   - update(id, patch): never name / kind / scope / tenant_id (the patch type
//     has no field for them); reload after success;
//   - suggestFromPool(): server-admin — per pool role model-alias-preview
//     {role} ⇒ primary_model; a non-empty primary that is not yet a _global
//     alias becomes a DRAFT via model-alias-create {enabled:false}; an empty
//     pool creates nothing (fail-closed, E-02-13);
//   - loadPreview(row, consumerId): model-alias-preview chain mode
//     (server-admin; C7 contract: the alias by NAME through ONE consumer) —
//     the answer per row id in `preview`;
//   - refusals: busyId / actionError + rethrow (the dialog keeps the error),
//     exactly like ConsumersModel.#mutate.

import { toApiError, type ApiError } from '../../lib/api'
import { createModelAlias, listModelAliases, poolPrimaryModel, previewAliasChain, updateModelAlias } from '../../lib/api/modelapi'
import type { ChainPreviewAnswer, ModelAliasCreateSpec, ModelAliasUpdateSpec, ModelAliasView, ModelAliasWriteResult } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'
import { POOL_ROLES, suggestDraft } from './catalog'

export interface AliasesApi {
  list: typeof listModelAliases
  create: typeof createModelAlias
  update: typeof updateModelAlias
  previewChain: typeof previewAliasChain
  rolePrimary: typeof poolPrimaryModel
}

const DEFAULT_API: AliasesApi = {
  list: listModelAliases,
  create: createModelAlias,
  update: updateModelAlias,
  previewChain: previewAliasChain,
  rolePrimary: poolPrimaryModel,
}

/** What suggestFromPool did: the drafts created and the roles skipped (no primary / already present). */
export interface SuggestOutcome {
  created: ModelAliasView[]
  skipped: string[]
}

export class AliasesModel {
  aliases = $state<ModelAliasView[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** The alias id a write is in flight for ('' = a create / suggest). */
  busyId = $state<string | null>(null)
  actionError = $state<string | null>(null)
  /** model-alias-preview chain answer per alias id. */
  preview = $state<Record<string, ChainPreviewAnswer>>({})
  /** A failed preview per alias id (403 / 400 / consumer not found). */
  previewError = $state<Record<string, string>>({})

  #api: AliasesApi
  #seq = 0

  constructor(api: Partial<AliasesApi> = {}) {
    this.#api = { ...DEFAULT_API, ...api }
  }

  /** Load the visible catalog (model-alias-list). A stale answer never overwrites a newer one. */
  async load(): Promise<void> {
    const seq = ++this.#seq
    if (this.status !== 'ready') this.status = 'loading'
    try {
      const res = await this.#api.list()
      if (seq !== this.#seq) return
      this.aliases = res.model_aliases
      this.loadError = null
      this.status = 'ready'
    } catch (err) {
      if (seq !== this.#seq) return
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  /** Create an alias (`enabled` passes through untouched); reload on success. */
  async create(spec: ModelAliasCreateSpec): Promise<ModelAliasWriteResult> {
    return this.#mutate('', () => this.#api.create(spec))
  }

  /** Update an alias by id (the patch type has no name / kind / scope); reload on success. */
  async update(id: string, patch: ModelAliasUpdateSpec): Promise<ModelAliasWriteResult> {
    return this.#mutate(id, () => this.#api.update(id, patch))
  }

  /**
   * Draft DISABLED aliases from the pool's primary models (server-admin): one
   * role preview per pool role, in order; a role without a primary, or whose
   * primary already is a _global alias, is skipped; one reload at the end.
   */
  async suggestFromPool(): Promise<SuggestOutcome> {
    return this.#mutate('', async () => {
      const created: ModelAliasView[] = []
      const skipped: string[] = []
      for (const { role, kind } of POOL_ROLES) {
        const ans = await this.#api.rolePrimary({ role })
        const draft = suggestDraft(role, kind, ans.primary_model)
        if (draft === null || this.aliases.some((a) => a.scope === '_global' && a.name === draft.name)) {
          skipped.push(role)
          continue
        }
        const res = await this.#api.create({ ...draft, enabled: false })
        created.push(res.model_alias)
      }
      return { created, skipped }
    })
  }

  /** Preview the /v1 chain of `row` (by name) through one consumer (server-admin). */
  async loadPreview(row: ModelAliasView, consumerId: string): Promise<void> {
    const { [row.id]: _drop, ...rest } = this.previewError
    this.previewError = rest
    try {
      const ans = await this.#api.previewChain({ alias: row.name, consumer_id: consumerId })
      this.preview = { ...this.preview, [row.id]: ans }
    } catch (err) {
      this.previewError = { ...this.previewError, [row.id]: toApiError(err).message }
    }
  }

  async #mutate<T>(id: string, run: () => Promise<T>): Promise<T> {
    this.busyId = id
    this.actionError = null
    try {
      const res = await run()
      await this.load()
      return res
    } catch (err) {
      this.actionError = toApiError(err).message
      throw err
    } finally {
      this.busyId = null
    }
  }
}
