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
//     {role} ⇒ primary_model; a non-null primary that is not yet a _global
//     alias becomes a DRAFT via model-alias-create {enabled:false}; an empty
//     pool creates nothing (fail-closed, E-02-13);
//   - loadPreview(aliasId, consumerId?): model-alias-preview (server-admin) —
//     the chain per row in `preview`;
//   - refusals: busyId / actionError + rethrow (the dialog keeps the error),
//     exactly like ConsumersModel.#mutate.
//
// RC-D S1: contract stub — the store delivers EMPTY data and every mutation
// fails closed. aliases.svelte.test.ts is red against it; S2 implements.

import { ApiError } from '../../lib/api'
import { createModelAlias, listModelAliases, previewModelAlias, updateModelAlias } from '../../lib/api/modelapi'
import type { ModelAliasCreateSpec, ModelAliasPreviewResult, ModelAliasUpdateSpec, ModelAliasView, ModelAliasWriteResult } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

export interface AliasesApi {
  list: typeof listModelAliases
  create: typeof createModelAlias
  update: typeof updateModelAlias
  preview: typeof previewModelAlias
}

const DEFAULT_API: AliasesApi = {
  list: listModelAliases,
  create: createModelAlias,
  update: updateModelAlias,
  preview: previewModelAlias,
}

/** What suggestFromPool did: the drafts created and the roles skipped (no primary / already present). */
export interface SuggestOutcome {
  created: ModelAliasView[]
  skipped: string[]
}

function notWired(what: string): ApiError {
  return new ApiError(503, 'service_unavailable', `${what} is not wired yet (RC-D S1 contract stub)`)
}

export class AliasesModel {
  aliases = $state<ModelAliasView[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** The alias id a write is in flight for ('' = a create / suggest). */
  busyId = $state<string | null>(null)
  actionError = $state<string | null>(null)
  /** model-alias-preview answer per alias id. */
  preview = $state<Record<string, ModelAliasPreviewResult>>({})

  #api: AliasesApi

  constructor(api: AliasesApi = DEFAULT_API) {
    this.#api = api
  }

  /** Load the visible catalog. S1 stub: empty data, never calls the server. */
  async load(): Promise<void> {
    void this.#api
    this.aliases = []
    this.status = 'ready'
  }

  /** Create an alias (enabled always sent). S1 stub: fails closed. */
  async create(_spec: ModelAliasCreateSpec): Promise<ModelAliasWriteResult> {
    throw notWired('model-alias-create')
  }

  /** Update an alias by id (immutable fields never sent). S1 stub: fails closed. */
  async update(_id: string, _patch: ModelAliasUpdateSpec): Promise<ModelAliasWriteResult> {
    throw notWired('model-alias-update')
  }

  /** Draft disabled aliases from the pool's primary models (server-admin). S1 stub: fails closed. */
  async suggestFromPool(): Promise<SuggestOutcome> {
    throw notWired('suggest from pool')
  }

  /** Preview the chain of one alias (optionally through a consumer). S1 stub: no-op. */
  async loadPreview(_aliasId: string, _consumerId?: string): Promise<void> {}
}
