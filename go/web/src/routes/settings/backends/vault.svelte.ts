// Secrets-vault editing state (design 04-§3.5). Holds the secret metadata list
// and its load status; put/delete call the write-only API and reload. Mutations
// THROW ApiError on failure (the form surfaces it per-row). The "in use" /
// "fehlt" derivation is a pure join (lib/backends secretUsage) the form runs
// over secrets × backends × settings — it is not state held here.
//
// Note: the server's DELETE-409 only fires for SETTINGS references; a secret
// used solely by a backend api_key_ref deletes silently (the server's
// referencedBy scans settings overrides only). The form therefore warns from
// the client-side join before deleting, not just on the 409.

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
