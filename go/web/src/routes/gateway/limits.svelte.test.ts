// LimitsModel gates (RC-D C6 S1, W05-12): a 422 lands at the field
// (fieldErrors["<op>.<field>"]), never as a generic success; the caller's
// values go out without a tier; the op's view is replaced by the server's
// answer. Red against the S1 stub.

import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../lib/api'
import type { GatewayLimitsGetResponse, LimitValues, LimitsOpView } from '../../lib/api/types'
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

function op(o: LimitsOpView['op'], tenant: Partial<LimitValues> = {}): LimitsOpView {
  return {
    op: o,
    server: values(),
    tenant: values(tenant),
    defaults: values({ rpm: 30, max_concurrency: 2 }),
    effective: values({ rpm: 30, max_concurrency: 2 }),
    binding: 'consumer',
    tenant_envelope: null,
    instance_envelope: null,
  }
}

const GET: GatewayLimitsGetResponse = { success: true, consumer_id: 'c1', ops: [op('chat'), op('embed')] }

describe('LimitsModel (W05-12)', () => {
  it('load reads gateway-limits-get for the consumer', async () => {
    const get = vi.fn().mockResolvedValue(GET)
    const m = new LimitsModel({ get })
    await m.load('c1')
    expect(get, 'gate W05-12: gateway-limits-get').toHaveBeenCalledWith('c1')
    expect(m.status).toBe('ready')
    expect(m.ops.map((o) => o.op)).toEqual(['chat', 'embed'])
  })

  it('save sends the values without a tier and replaces the op view with the answer', async () => {
    const set = vi.fn().mockResolvedValue({ success: true, limits: op('chat', { rpm: 10 }) })
    const m = new LimitsModel({ get: vi.fn().mockResolvedValue(GET), set })
    await m.load('c1')
    await m.save('chat', { rpm: 10 }).catch(() => {})
    expect(set, 'gate W05-12: caller-tier write, no tier').toHaveBeenCalledWith({ consumer_id: 'c1', op: 'chat', values: { rpm: 10 } })
    expect(m.ops.find((o) => o.op === 'chat')?.tenant.rpm).toBe(10)
    expect(m.fieldErrors).toEqual({})
  })

  it('a 422 puts the field message at the field and rethrows (never a silent success)', async () => {
    const refusal = new ApiError(422, 'unprocessable', 'limit exceeds the server limit', null, {
      success: false,
      fields: [{ field: 'max_concurrency', message: 'at most 2 (server limit)' }],
    })
    const m = new LimitsModel({ get: vi.fn().mockResolvedValue(GET), set: vi.fn().mockRejectedValue(refusal) })
    await m.load('c1')
    const err = await m.save('chat', { max_concurrency: 9 }).catch((e: unknown) => e)
    expect(m.fieldErrors, 'gate W05-12: 422 mit Feldnamen erscheint am Feld').toEqual({
      'chat.max_concurrency': 'at most 2 (server limit)',
    })
    expect(err, 'the refusal is rethrown').toBe(refusal)
    expect(m.ops.find((o) => o.op === 'chat')?.tenant.max_concurrency, 'the view keeps the server state').toBeNull()
  })

  it('a non-422 refusal lands on actionError, not on a field', async () => {
    const m = new LimitsModel({
      get: vi.fn().mockResolvedValue(GET),
      set: vi.fn().mockRejectedValue(new ApiError(403, 'forbidden', 'admin key required')),
    })
    await m.load('c1')
    await m.save('chat', { rpm: 1 }).catch(() => {})
    expect(m.actionError, 'gate W05-12: tier refusal shown').toBe('admin key required')
    expect(m.fieldErrors).toEqual({})
  })
})
