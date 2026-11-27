// LlmlogDetailModel pins (inference-scheduler MW12b / D1b). The teeth are the
// GATE invariants: bodies are fetched ONLY on demand from the per-id endpoint,
// held ONLY while the card is open, and DROPPED on close (no cache) — and a
// superseded fetch never overwrites a newer state. Error states (404 not-found /
// 403 forbidden) and body_state (sealed/evicted) surface distinctly. DOM-free
// via an injected api (Resource/IssueDetailModel pattern).

import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../lib/api'
import type { LLMLogDetail, LLMLogDetailResponse } from '../../lib/api/types'
import { LlmlogDetailModel, type LlmlogDetailApi } from './llmlog-detail.svelte'

function detail(over: Partial<LLMLogDetail> = {}): LLMLogDetail {
  return {
    id: '11111111-1111-1111-1111-111111111111',
    created_at: '2026-07-06T00:00:00Z',
    pipeline: 'chat',
    model: 'qwen',
    backend: 'gpu',
    required_sensitivity: 'internal',
    body_state: 'present',
    request_system: 'you are ctx',
    request_user: 'hello',
    response_content: 'hi',
    ...over,
  }
}

function ok(d: LLMLogDetail): LLMLogDetailResponse {
  return { success: true, detail: d }
}

/** A deferred promise so a test can control resolution ordering. */
function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('LlmlogDetailModel — fetch + happy path', () => {
  it('opens, fetches the body per-id, and exposes body_state present', async () => {
    const api: LlmlogDetailApi = { fetchDetail: vi.fn(async (id) => ok(detail({ id }))) }
    const m = new LlmlogDetailModel(api)
    expect(m.openId).toBeNull()
    expect(m.detail).toBeNull()

    await m.open('abc')

    expect(api.fetchDetail).toHaveBeenCalledWith('abc')
    expect(m.status).toBe('ready')
    expect(m.detail?.request_user).toBe('hello')
    expect(m.openId).toBe('abc')
  })
})

describe('LlmlogDetailModel — body-free-at-rest gate (no cache)', () => {
  it('close() DROPS the fetched bodies immediately', async () => {
    const api: LlmlogDetailApi = { fetchDetail: vi.fn(async () => ok(detail())) }
    const m = new LlmlogDetailModel(api)
    await m.open('a')
    expect(m.detail).not.toBeNull()

    m.close()

    // The gated shadow-corpus must never survive the card. A regression that
    // cached the last detail (kept it on close) turns this red.
    expect(m.detail).toBeNull()
    expect(m.openId).toBeNull()
    expect(m.status).toBe('idle')
  })

  it('opening a different row nulls the previous body BEFORE the new fetch resolves', async () => {
    const first = ok(detail({ id: 'a', request_user: 'FIRST' }))
    const second = deferred<LLMLogDetailResponse>()
    const api: LlmlogDetailApi = {
      fetchDetail: vi.fn(async (id: string) => (id === 'a' ? first : second.promise)),
    }
    const m = new LlmlogDetailModel(api)
    await m.open('a')
    expect(m.detail?.request_user).toBe('FIRST')

    const p = m.open('b') // in-flight
    // While 'b' loads, the old body is already gone (no stale flash).
    expect(m.detail).toBeNull()
    expect(m.status).toBe('loading')

    second.resolve(ok(detail({ id: 'b', request_user: 'SECOND' })))
    await p
    expect(m.detail?.request_user).toBe('SECOND')
  })

  it('a superseded in-flight fetch never overwrites the newer state', async () => {
    const slow = deferred<LLMLogDetailResponse>()
    const api: LlmlogDetailApi = { fetchDetail: vi.fn(async () => slow.promise) }
    const m = new LlmlogDetailModel(api)
    const p = m.open('a')
    m.close() // supersede before 'a' resolves
    slow.resolve(ok(detail({ id: 'a' })))
    await p
    // The late resolution must not resurrect a body into a closed card.
    expect(m.detail).toBeNull()
    expect(m.openId).toBeNull()
  })

  it('clicking the same open row toggles the card closed', async () => {
    const api: LlmlogDetailApi = { fetchDetail: vi.fn(async () => ok(detail())) }
    const m = new LlmlogDetailModel(api)
    await m.open('a')
    await m.open('a') // same id → toggle close
    expect(m.openId).toBeNull()
    expect(m.detail).toBeNull()
    // one real fetch (the open); the toggle-close does not refetch
    expect(api.fetchDetail).toHaveBeenCalledTimes(1)
  })
})

describe('LlmlogDetailModel — error + absent-body states', () => {
  it('maps a 404 to an error state (uniform not-found)', async () => {
    const api: LlmlogDetailApi = {
      fetchDetail: vi.fn(async () => {
        throw new ApiError(404, 'not_found', 'not found')
      }),
    }
    const m = new LlmlogDetailModel(api)
    await m.open('x')
    expect(m.status).toBe('error')
    expect(m.error?.status).toBe(404)
    expect(m.detail).toBeNull()
  })

  it('maps a 403 to an error state (forbidden)', async () => {
    const api: LlmlogDetailApi = {
      fetchDetail: vi.fn(async () => {
        throw new ApiError(403, 'forbidden', 'forbidden')
      }),
    }
    const m = new LlmlogDetailModel(api)
    await m.open('x')
    expect(m.status).toBe('error')
    expect(m.error?.status).toBe(403)
  })

  it('a sealed row is ready but carries null bodies + a reason state', async () => {
    const api: LlmlogDetailApi = {
      fetchDetail: vi.fn(async () =>
        ok(detail({ body_state: 'sealed', request_system: null, request_user: null, response_content: null })),
      ),
    }
    const m = new LlmlogDetailModel(api)
    await m.open('x')
    expect(m.status).toBe('ready')
    expect(m.detail?.body_state).toBe('sealed')
    expect(m.detail?.request_user).toBeNull()
  })

  it('a bodyless row (pipeline never records bodies, llmlog W1) is ready with its own state', async () => {
    const api: LlmlogDetailApi = {
      fetchDetail: vi.fn(async () =>
        ok(detail({ body_state: 'bodyless', request_system: null, request_user: null, response_content: null })),
      ),
    }
    const m = new LlmlogDetailModel(api)
    await m.open('x')
    expect(m.status).toBe('ready')
    expect(m.detail?.body_state).toBe('bodyless')
    expect(m.detail?.response_content).toBeNull()
  })
})
