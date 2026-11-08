// LimitsModel gates (RC-D C6 S1, W05-12): a field refusal lands at the field
// (fieldErrors["<op>.<field>"]), never as a generic success; the caller's
// values go out without a tier; after a write the view is the server's.
// S2 (Go wins, C1 W06-15): the view is a map per op, a write is
// (consumerId, op, values) with a flat full-row body, the write answers the
// stored row so the model re-reads gateway-limits-get, a field refusal is 400
// {error, param} instead of 422 fields[].

import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../lib/api'
import type { GatewayLimitsGetResponse, GatewayOp, LimitValues, LimitsOpView } from '../../lib/api/types'
import { LimitsModel } from './limits.svelte'

function values(over: Partial<LimitValues> = {}): LimitValues {
  return {
    rpm: null,
    tpm: null,
    max_concurrency: null,
    daily_tokens: null,
    monthly_tokens: null,
    daily_requests: null,
    monthly_cost_usd: null,
    allow_external: null,
    allow_unmetered_external: null,
    ...over,
  }
}

function op(tenant: Partial<LimitValues> | null = null): LimitsOpView {
  return {
    server: null,
    tenant: tenant === null ? null : values(tenant),
    ceiling: values({ rpm: 30, max_concurrency: 2 }),
  }
}

function getAnswer(chatTenant: Partial<LimitValues> | null = null): GatewayLimitsGetResponse {
  return {
    success: true,
    limits: { subject_kind: 'consumer', consumer_id: 'c1', ops: { chat: op(chatTenant), embed: op() } },
  }
}

function tenantOf(m: LimitsModel, o: GatewayOp): LimitValues | null | undefined {
  return m.ops.find((x) => x.op === o)?.view.tenant
}

describe('LimitsModel (W05-12)', () => {
  it('load reads gateway-limits-get for the consumer', async () => {
    const get = vi.fn().mockResolvedValue(getAnswer())
    const m = new LimitsModel({ get })
    await m.load('c1')
    expect(get, 'gate W05-12: gateway-limits-get').toHaveBeenCalledWith('c1')
    expect(m.status).toBe('ready')
    expect(m.ops.map((o) => o.op)).toEqual(['chat', 'embed'])
  })

  it('save sends the values without a tier and the view becomes the server’s (re-read)', async () => {
    const get = vi.fn().mockResolvedValueOnce(getAnswer()).mockResolvedValueOnce(getAnswer({ rpm: 10 }))
    const set = vi.fn().mockResolvedValue({ success: true, limits: { ...values({ rpm: 10 }), subject_kind: 'consumer', op: 'chat', tier: 'tenant' } })
    const m = new LimitsModel({ get, set })
    await m.load('c1')
    await m.save('chat', { rpm: 10 }).catch(() => {})
    expect(set, 'gate W05-12: caller-tier write, no tier').toHaveBeenCalledWith('c1', 'chat', { rpm: 10 })
    expect(tenantOf(m, 'chat')?.rpm).toBe(10)
    expect(get).toHaveBeenCalledTimes(2)
    expect(m.fieldErrors).toEqual({})
  })

  it('a field refusal puts the message at the field and rethrows (never a silent success)', async () => {
    const refusal = new ApiError(400, 'bad_request', 'limit widens: max_concurrency — 5 is above 4, the limit of the levels above', null, {
      success: false,
      error: 'limit widens: max_concurrency — 5 is above 4, the limit of the levels above',
      param: 'max_concurrency',
    })
    const m = new LimitsModel({ get: vi.fn().mockResolvedValue(getAnswer()), set: vi.fn().mockRejectedValue(refusal) })
    await m.load('c1')
    const err = await m.save('chat', { max_concurrency: 9 }).catch((e: unknown) => e)
    expect(m.fieldErrors, 'gate W05-12: Fehler mit Feldnamen erscheint am Feld').toEqual({
      'chat.max_concurrency': 'limit widens: max_concurrency — 5 is above 4, the limit of the levels above',
    })
    expect(err, 'the refusal is rethrown').toBe(refusal)
    expect(tenantOf(m, 'chat'), 'the view keeps the server state').toBeNull()
  })

  it('a refusal without a field lands on actionError, not on a field', async () => {
    const m = new LimitsModel({
      get: vi.fn().mockResolvedValue(getAnswer()),
      set: vi.fn().mockRejectedValue(new ApiError(403, 'forbidden', 'admin key required')),
    })
    await m.load('c1')
    await m.save('chat', { rpm: 1 }).catch(() => {})
    expect(m.actionError, 'gate W05-12: tier refusal shown').toBe('admin key required')
    expect(m.fieldErrors).toEqual({})
  })

  it('clear deletes the caller-tier row of the op and re-reads', async () => {
    const get = vi.fn().mockResolvedValueOnce(getAnswer({ rpm: 10 })).mockResolvedValueOnce(getAnswer())
    const del = vi.fn().mockResolvedValue({ success: true, deleted: true })
    const m = new LimitsModel({ get, del })
    await m.load('c1')
    await m.clear('chat')
    expect(del).toHaveBeenCalledWith('c1', 'chat')
    expect(tenantOf(m, 'chat')).toBeNull()
  })
})
