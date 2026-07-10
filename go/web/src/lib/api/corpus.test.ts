// corpus.ts binding gates (Wave A1): the audit + classify start/status actions
// POST the exact manage shape (blocks_audit.go / blocks_classify.go). start
// forwards {dry_run,limit} in data (empty object = live full run); status carries
// no data; the audit envelope has `pending`, the classify envelope does not; the
// 503 (scheduler off) surfaces as ApiError. Pure node, fetch stubbed.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import { blocksAuditStatus, blocksClassifyStatus, startBlocksAudit, startBlocksClassify } from './corpus'

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function stubFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const mock = vi.fn()
  for (const res of responses) mock.mockResolvedValueOnce(res)
  vi.stubGlobal('fetch', mock)
  return mock
}

function sentBody(mock: ReturnType<typeof vi.fn>): Record<string, unknown> {
  const init = mock.mock.calls[0]?.[1] as RequestInit
  return JSON.parse(String(init.body)) as Record<string, unknown>
}

beforeEach(() => {
  vi.unstubAllGlobals()
  configureApi({ getCsrfToken: () => null, onUnauthorized: () => {} })
})
afterEach(() => vi.unstubAllGlobals())

const auditStatusBody = {
  success: true,
  scope: 'private',
  pending: 12,
  by_source: { llm: 10, manual: 2 },
  run: {
    running: false,
    dry_run: true,
    processed: 30,
    kept_credentials: 0,
    to_personal: 1,
    to_internal: 2,
    no_verdict: 0,
    discarded: 0,
    aborted: false,
  },
}

const classifyStatusBody = {
  success: true,
  scope: 'private',
  by_source: { llm: 10 },
  run: { running: false, dry_run: false, scanned: 100, upgraded: 3, discarded: 0, aborted: false },
}

describe('startBlocksAudit', () => {
  it('POSTs blocks-audit-start with forwarded dry_run + limit', async () => {
    const mock = stubFetch(jsonResponse(200, auditStatusBody))
    const res = await startBlocksAudit({ dry_run: true, limit: 30 })
    expect(mock.mock.calls[0]?.[0]).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({ action: 'blocks-audit-start', data: { dry_run: true, limit: 30 } })
    expect(res.pending).toBe(12)
    expect(res.run.dry_run).toBe(true)
  })

  it('defaults to an empty data object (live full run)', async () => {
    const mock = stubFetch(jsonResponse(200, auditStatusBody))
    await startBlocksAudit()
    expect(sentBody(mock)).toEqual({ action: 'blocks-audit-start', data: {} })
  })
})

describe('blocksAuditStatus', () => {
  it('POSTs blocks-audit-status with no data', async () => {
    const mock = stubFetch(jsonResponse(200, auditStatusBody))
    await blocksAuditStatus()
    expect(sentBody(mock)).toEqual({ action: 'blocks-audit-status' })
  })

  it('surfaces the 503 (scheduler off) as ApiError', async () => {
    stubFetch(jsonResponse(503, { success: false, error: 'Scheduler not enabled' }))
    await expect(blocksAuditStatus()).rejects.toBeInstanceOf(ApiError)
  })
})

describe('startBlocksClassify', () => {
  it('POSTs blocks-classify-start with forwarded params', async () => {
    const mock = stubFetch(jsonResponse(200, classifyStatusBody))
    const res = await startBlocksClassify({ dry_run: false, limit: 0 })
    expect(sentBody(mock)).toEqual({ action: 'blocks-classify-start', data: { dry_run: false, limit: 0 } })
    // The classify envelope has NO `pending` field (unlike audit).
    expect('pending' in res).toBe(false)
    expect(res.run.scanned).toBe(100)
  })
})

describe('blocksClassifyStatus', () => {
  it('POSTs blocks-classify-status with no data', async () => {
    const mock = stubFetch(jsonResponse(200, classifyStatusBody))
    await blocksClassifyStatus()
    expect(sentBody(mock)).toEqual({ action: 'blocks-classify-status' })
  })
})
