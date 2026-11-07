// ConsumerDetailModel gates (RC-D C6 S1, W05-09): load reads
// model-consumer-get AND the consumer's calls; a failing calls read never
// hides the consumer; the No-Oracle refusal is the notFound state; stale
// answers are dropped. Red against the S1 stub (load ends in 'error' 503).

import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../lib/api'
import type { ConsumerCallsResponse, ConsumerGetResponse, ConsumerView } from '../../lib/api/types'
import { ConsumerDetailModel } from './consumer.svelte'

function consumer(id: string, label: string): ConsumerView {
  return {
    id,
    tenant_id: 't-a',
    tenant_slug: 'a',
    label,
    description: '',
    kind: 'app',
    tenant_scope: null,
    all_models: false,
    models: [],
    prompt_sensitivity: 'credentials',
    fair_weight: 1,
    admission_lane: 'bulk',
    allowed_cidrs: null,
    allow_plaintext: false,
    active: true,
    status: 'active',
    last_used_at: null,
    created_at: '2026-06-01T10:00:00Z',
    created_by_label: null,
    keys: [],
    keys_total: 0,
    peer: null,
  }
}

function getAnswer(id: string, label: string): ConsumerGetResponse {
  return {
    success: true,
    consumer: consumer(id, label),
    keys_truncated: true,
    audit: [
      {
        entity_type: 'model_consumer',
        id: 'a1',
        entity_key: id,
        scope: '',
        action: 'create',
        old_value: null,
        new_value: { label },
        api_key_id: null,
        actor_label: null,
        metadata: {},
        created_at: '2026-06-01T10:00:00Z',
      },
    ],
  }
}

const CALLS: ConsumerCallsResponse = {
  success: true,
  entries: [
    {
      id: 'L-1',
      created_at: '2026-06-29T11:00:00Z',
      pipeline: 'gateway-chat',
      model: 'chat',
      backend: 'spark-chat',
      consumer_id: 'c1',
      duration_ms: 10,
      error: null,
      prompt_tokens: 1,
      completion_tokens: 1,
      cost_usd: null,
      queue_wait_ms: 0,
    },
  ],
}

describe('ConsumerDetailModel.load (W05-09)', () => {
  it('loads consumer, chain flag, audit and calls of the id', async () => {
    const get = vi.fn().mockResolvedValue(getAnswer('c1', 'ci-chat'))
    const calls = vi.fn().mockResolvedValue(CALLS)
    const m = new ConsumerDetailModel({ get, calls })
    await m.load('c1')
    expect(get, 'gate W05-09: model-consumer-get with the route id').toHaveBeenCalledWith('c1')
    expect(calls, 'gate W05-09: the consumer’s last calls').toHaveBeenCalledWith('c1', undefined)
    expect(m.status).toBe('ready')
    expect(m.consumer?.label).toBe('ci-chat')
    expect(m.keysTruncated).toBe(true)
    expect(m.audit.map((a) => a.id)).toEqual(['a1'])
    expect(m.calls.map((c) => c.id), 'gate W05-09: positive control — seeded call visible').toEqual(['L-1'])
    expect(m.callsStatus).toBe('ready')
  })

  it('a failing calls read keeps the consumer (callsError only)', async () => {
    const m = new ConsumerDetailModel({
      get: vi.fn().mockResolvedValue(getAnswer('c1', 'ci-chat')),
      calls: vi.fn().mockRejectedValue(new ApiError(503, 'service_unavailable', 'llmlog down')),
    })
    await m.load('c1')
    expect(m.status, 'gate W05-09: consumer stays visible').toBe('ready')
    expect(m.consumer?.id).toBe('c1')
    expect(m.callsStatus).toBe('error')
    expect(m.callsError?.status).toBe(503)
  })

  it('the No-Oracle refusal is notFound, with no consumer and no calls fetched result', async () => {
    const m = new ConsumerDetailModel({
      get: vi.fn().mockRejectedValue(new ApiError(404, 'not_found', 'model consumer not found')),
      calls: vi.fn().mockResolvedValue(CALLS),
    })
    await m.load('foreign')
    expect(m.notFound, 'gate W05-09: No-Oracle ⇒ notFound').toBe(true)
    expect(m.consumer).toBeNull()
    expect(m.calls).toEqual([])
  })

  it('a stale answer of a previous id never overwrites the newer one', async () => {
    let releaseFirst: (v: ConsumerGetResponse) => void = () => {}
    const get = vi
      .fn()
      .mockImplementationOnce(() => new Promise<ConsumerGetResponse>((r) => (releaseFirst = r)))
      .mockResolvedValueOnce(getAnswer('c2', 'second'))
    const m = new ConsumerDetailModel({ get, calls: vi.fn().mockResolvedValue({ success: true, entries: [] }) })
    const first = m.load('c1')
    await m.load('c2')
    releaseFirst(getAnswer('c1', 'first'))
    await first
    expect(m.consumer?.label, 'gate W05-09: stale answer dropped').toBe('second')
  })
})
