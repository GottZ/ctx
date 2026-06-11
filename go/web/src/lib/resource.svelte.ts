// Resource<T> (design 04-§2.4): the one loading convention for every data
// path — components render the three-stage {#if} cascade off status instead
// of scattering ad-hoc awaits through templates. W4+ consume this.

import { toApiError, type ApiError } from './api'

export type ResourceStatus = 'idle' | 'loading' | 'ready' | 'error'

export class Resource<T> {
  status = $state<ResourceStatus>('idle')
  data = $state<T | null>(null)
  error = $state<ApiError | null>(null)

  #fetcher: () => Promise<T>
  #seq = 0

  constructor(fetcher: () => Promise<T>) {
    this.#fetcher = fetcher
  }

  /** Load (or reload). Stale in-flight loads are superseded, never applied. */
  async load(): Promise<void> {
    const seq = ++this.#seq
    this.status = 'loading'
    this.error = null
    try {
      const data = await this.#fetcher()
      if (seq !== this.#seq) return
      this.data = data
      this.status = 'ready'
    } catch (err) {
      if (seq !== this.#seq) return
      this.error = toApiError(err)
      this.status = 'error'
    }
  }

  reload = (): Promise<void> => this.load()
}
