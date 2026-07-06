// LLM-call detail state (inference-scheduler MW12b / DECISIONS D1b). Injectable
// runes class (Resource/IssueDetailModel pattern) so the fetch / toggle / error-
// state / NO-CACHE logic is vitest-covered without a DOM.
//
// Body-free-at-rest invariant (D1b, the whole point of the gated endpoint): the
// prompt/reply bodies are fetched ONLY on demand from GET /api/llmlog/{id} and
// are held ONLY while the card is open — close() drops them immediately. The
// model NEVER seeds a body from a list row (the list is body-free by contract),
// and it never keeps a body cache across rows: opening a new row nulls the old
// detail before the fetch, and a superseded in-flight fetch is discarded (#seq).

import { toApiError, type ApiError } from '../../lib/api'
import { fetchLLMLogDetail as fetchDetailApi } from '../../lib/api/status'
import type { LLMLogDetail, LLMLogDetailResponse } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

/** The API surface the model needs, injectable for DOM-free tests. */
export interface LlmlogDetailApi {
  fetchDetail: (id: string) => Promise<LLMLogDetailResponse>
}

const DEFAULT_API: LlmlogDetailApi = { fetchDetail: fetchDetailApi }

export class LlmlogDetailModel {
  /** The row id whose detail card is open (null = no card). */
  openId = $state<string | null>(null)
  status = $state<ResourceStatus>('idle')
  /** The fetched bodies — held ONLY while open, never cached past close(). */
  detail = $state<LLMLogDetail | null>(null)
  error = $state<ApiError | null>(null)

  #api: LlmlogDetailApi
  #seq = 0

  constructor(api: LlmlogDetailApi = DEFAULT_API) {
    this.#api = api
  }

  /**
   * Open (fetch) the detail for a row, or toggle-close it if the same row is
   * already open. Always nulls the previous detail BEFORE fetching so a body
   * from another row can never flash — and a stale in-flight fetch (fast row
   * switching) is dropped via the sequence guard.
   */
  async open(id: string): Promise<void> {
    if (this.openId === id) {
      this.close()
      return
    }
    const seq = ++this.#seq
    this.openId = id
    this.detail = null
    this.error = null
    this.status = 'loading'
    try {
      const res = await this.#api.fetchDetail(id)
      if (seq !== this.#seq) return
      this.detail = res.detail
      this.status = 'ready'
    } catch (err) {
      if (seq !== this.#seq) return
      this.error = toApiError(err)
      this.status = 'error'
    }
  }

  /** Close the card and DROP the bodies (no caching — the gated shadow corpus
   * is never retained past the open card). Also supersedes any in-flight fetch. */
  close(): void {
    this.#seq++
    this.openId = null
    this.detail = null
    this.error = null
    this.status = 'idle'
  }
}
