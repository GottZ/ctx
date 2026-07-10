// grants.ts binding gates (Wave A1): block-grant create/list/revoke POST the
// exact manage shape block_grant_manage.go reads — create/revoke carry the pair
// in `data`, list narrows by block via a top-level id — and the ownership/opt-in
// 403 surfaces as ApiError. Pure node, fetch stubbed.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import { createBlockGrant, listBlockGrants, revokeBlockGrant } from './grants'

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

const aBlockGrant = {
  id: 'bg1',
  block_id: 'b1',
  grantee_tenant: 't2',
  granted_by: 'k1',
  created_at: '2026-06-29T00:00:00Z',
}

describe('createBlockGrant', () => {
  it('POSTs block-grant-create with the pair in data', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, grant: aBlockGrant }))
    const res = await createBlockGrant({ block_id: 'b1', grantee_tenant: 't2' })
    expect(mock.mock.calls[0]?.[0]).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({
      action: 'block-grant-create',
      data: { block_id: 'b1', grantee_tenant: 't2' },
    })
    expect(res.grant.id).toBe('bg1')
  })

  it('maps the cross-tenant opt-out 403 to ApiError (forbidden)', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'cross-tenant block grant not permitted' }))
    await expect(createBlockGrant({ block_id: 'b1', grantee_tenant: 't2' })).rejects.toMatchObject({
      status: 403,
      code: 'forbidden',
    })
  })
})

describe('listBlockGrants', () => {
  it('POSTs block-grant-list with no id (owner-side, all blocks)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, grants: [aBlockGrant] }))
    const res = await listBlockGrants()
    expect(sentBody(mock)).toEqual({ action: 'block-grant-list' })
    expect(res.grants).toHaveLength(1)
  })

  it('narrows to one block via a top-level id (NOT data)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, grants: [] }))
    await listBlockGrants('b1')
    expect(sentBody(mock)).toEqual({ action: 'block-grant-list', id: 'b1' })
  })
})

describe('revokeBlockGrant', () => {
  it('POSTs block-grant-revoke with the pair in data (NOT a grant id)', async () => {
    const mock = stubFetch(
      jsonResponse(200, { success: true, revoked: { block_id: 'b1', grantee_tenant: 't2' } }),
    )
    const res = await revokeBlockGrant({ block_id: 'b1', grantee_tenant: 't2' })
    expect(sentBody(mock)).toEqual({
      action: 'block-grant-revoke',
      data: { block_id: 'b1', grantee_tenant: 't2' },
    })
    expect(res.revoked.block_id).toBe('b1')
  })

  it('surfaces the 200 {success:false} not-found envelope as ApiError', async () => {
    stubFetch(jsonResponse(200, { success: false, error: 'block grant not found' }))
    await expect(revokeBlockGrant({ block_id: 'b1', grantee_tenant: 't2' })).rejects.toBeInstanceOf(ApiError)
  })
})
