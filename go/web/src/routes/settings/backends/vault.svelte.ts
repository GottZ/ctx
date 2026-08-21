// Secrets-vault editing state (design 04-§3.5). Holds the secret metadata list
// and its load status; put/delete call the write-only API and reload. Mutations
// THROW ApiError on failure (the form surfaces it per-row). The "fehlt"
// derivation is a pure scan (lib/backends secretUsage) the form runs over
// secrets × backends × settings — it is not state held here.
//
// Note: since 04-W5 the server's DELETE-409 also fires for BACKEND references
// (referencedBy unions context_settings secret_refs with context_backends
// api_key_ref, prefix "backend:"), and the same union is what each secret's
// referenced_by carries. The form therefore RENDERS the server's list rather
// than re-deriving the intact refs client-side; the client scan is left with
// the dangling direction alone, which referenced_by cannot express.

import { toApiError, type ApiError } from '../../../lib/api'
import { deleteSecret, listSecrets, putSecret } from '../../../lib/api/vault'
import type { SecretMeta } from '../../../lib/api/types'
import type { ResourceStatus } from '../../../lib/resource.svelte'

interface VaultApi {
  list: typeof listSecrets
  put: typeof putSecret
  del: typeof deleteSecret
}

export class VaultModel {
  secrets = $state<SecretMeta[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** Secret name with a put/delete in flight. */
  busyName = $state<string | null>(null)

  #api: VaultApi

  constructor(api: VaultApi = { list: listSecrets, put: putSecret, del: deleteSecret }) {
    this.#api = api
  }

  async load(): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    try {
      const res = await this.#api.list()
      this.secrets = res.secrets
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  reload = (): Promise<void> => this.load()

  has(name: string): boolean {
    return this.secrets.some((s) => s.name === name)
  }

  /** Create or rotate — throws ApiError on failure, reloads, returns the action. */
  async put(name: string, value: string): Promise<'create' | 'rotate'> {
    this.busyName = name
    try {
      const res = await this.#api.put(name, value)
      await this.load()
      return res.action
    } finally {
      this.busyName = null
    }
  }

  /** Delete — throws ApiError on failure (409 while referenced), reloads on ok. */
  async remove(name: string): Promise<void> {
    this.busyName = name
    try {
      await this.#api.del(name)
      await this.load()
    } finally {
      this.busyName = null
    }
  }
}
