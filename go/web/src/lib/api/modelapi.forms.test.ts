// modelapi.ts — the RC-D C5 wire (W05-07 / W05-08 / W05-13), against the Go
// handlers (internal/handler/model_consumers.go, model_alias_manage.go) and
// the C7 preview contract. Pure node, fetch stubbed (precedent
// modelapi.test.ts). Gates:
//   - model-consumer-update: {action, data:{id, ...patch}} — exactly the patch,
//     never kind / tenant_id / expires_at even when smuggled in;
//   - model-key-rotate: grace ALWAYS on the wire, 0 included (leak);
//   - model-alias-create: `enabled` always on the wire, false included;
//   - model-alias-update: the id TOP-LEVEL, name / kind / scope / tenant_id
//     never in data;
//   - model-alias-preview (C7 contract, RC-D S2 alignment): {alias: <name>,
//     consumer_id} (chain mode, consumer required) or {role} (primary_model);
//   - a {success:false} envelope rejects as ApiError (403, 422 with fields).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import { createModelAlias, poolPrimaryModel, previewAliasChain, rotateModelKey, updateConsumer, updateModelAlias } from './modelapi'
import type { ConsumerUpdateSpec, ModelAliasUpdateSpec } from './types'

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function stubFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const mock = vi.fn()
  for (const res of responses) mock.mockResolvedValueOnce(res)
  vi.stubGlobal('fetch', mock)
  return mock
}

function sentBody(mock: ReturnType<typeof vi.fn>): Record<string, unknown> {
  const init = mock.mock.calls[0]?.[1] as RequestInit | undefined
  if (init === undefined) throw new Error('no request was sent (the call never reached fetch)')
  return JSON.parse(String(init.body)) as Record<string, unknown>
}

/** Await a client call; a rejection becomes an assertion that names the gate. */
async function sent<T>(gate: string, call: () => Promise<T>): Promise<T> {
  try {
    return await call()
  } catch (err) {
    throw new Error(`${gate} — the call failed: ${String(err)}`)
  }
}

beforeEach(() => {
  vi.unstubAllGlobals()
  configureApi({ getCsrfToken: () => null, onUnauthorized: () => {} })
})
afterEach(() => vi.unstubAllGlobals())

describe('W05-07 updateConsumer', () => {
  it('W05-07 gate: POSTs model-consumer-update with exactly {id, ...patch}', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumer: {} }))
    await sent('W05-07: updateConsumer', () => updateConsumer('c-1', { label: 'x', admission_lane: 'bulk', allowed_cidrs: null }))
    expect(mock.mock.calls[0]?.[0]).toBe('/api/manage')
    expect(sentBody(mock), 'W05-07: the wire body').toEqual({ action: 'model-consumer-update', data: { id: 'c-1', label: 'x', admission_lane: 'bulk', allowed_cidrs: null } })
  })

  it('W05-07 gate: kind / tenant_id / expires_at never reach the wire even when smuggled in', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumer: {} }))
    const smuggled = { description: 'd', kind: 'ctx-peer', tenant_id: 't-b', expires_at: '2030-01-01T00:00:00Z' } as unknown as ConsumerUpdateSpec
    await sent('W05-07: updateConsumer', () => updateConsumer('c-1', smuggled))
    expect(sentBody(mock), 'W05-07: explicit field list').toEqual({ action: 'model-consumer-update', data: { id: 'c-1', description: 'd' } })
  })

  it('W05-07 gate: a 403 tier refusal rejects as ApiError (never a success)', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin key required' }))
    const err = await updateConsumer('c-1', { prompt_sensitivity: 'public' }).then(
      () => null,
      (e: unknown) => e,
    )
    expect(err, 'W05-07 gate: the 403 surfaces').toBeInstanceOf(ApiError)
    expect((err as ApiError).status, 'W05-07 gate: the server status, not the stub').toBe(403)
    expect((err as ApiError).message).toBe('admin key required')
  })
})

describe('W05-08 rotateModelKey', () => {
  it('W05-08 gate: grace 0 rides the wire; the answer carries the plaintext once', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, key: { id: 'k-2', hint: 'ctxm_…sist' }, api_key: 'ctxm_TESTKEY' }))
    const res = await sent('W05-08: rotateModelKey', () => rotateModelKey({ consumer_id: 'c-1', grace: 0 }))
    expect(sentBody(mock), 'W05-08 gate: the leak payload').toEqual({ action: 'model-key-rotate', data: { consumer_id: 'c-1', grace: 0 } })
    expect(res.api_key).toBe('ctxm_TESTKEY')
  })

  it('W05-08: a planned change sends grace + an explicit expiry', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, key: {}, api_key: 'x' }))
    await sent('W05-08: rotateModelKey', () => rotateModelKey({ consumer_id: 'c-1', grace: 86_400, expires_at: '2027-01-01T00:00:00Z' }))
    expect(sentBody(mock)).toEqual({ action: 'model-key-rotate', data: { consumer_id: 'c-1', grace: 86_400, expires_at: '2027-01-01T00:00:00Z' } })
  })
})

describe('W05-13 alias writes', () => {
  it('W05-13 gate: createModelAlias sends enabled:false explicitly', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, model_alias: {} }))
    await sent('W05-13: createModelAlias', () => createModelAlias({ name: 'qwen38-27b', kind: 'chat', role: 'chat', enabled: false }))
    expect(sentBody(mock), 'W05-13 gate: enabled:false on the wire').toEqual({
      action: 'model-alias-create',
      data: { name: 'qwen38-27b', kind: 'chat', role: 'chat', enabled: false },
    })
  })

  it('W05-13: a 422 with fields rejects as ApiError carrying the fields', async () => {
    stubFetch(jsonResponse(422, { success: false, error: 'validation failed', fields: [{ field: 'sensitivity', message: 'below the floor' }] }))
    const err = await createModelAlias({ name: 'chat', kind: 'chat', role: 'chat', enabled: true, sensitivity: 'public' }).then(
      () => null,
      (e: unknown) => e,
    )
    expect((err as ApiError).status, 'W05-13: the server 422, not the stub').toBe(422)
    expect((err as ApiError).details?.fields).toEqual([{ field: 'sensitivity', message: 'below the floor' }])
  })

  it('W05-13 gate: updateModelAlias puts the id top-level and never sends name / kind / scope / tenant_id', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, model_alias: {} }))
    const smuggled = { enabled: false, name: 'renamed', kind: 'rerank', scope: '_global', tenant_id: 't-b' } as unknown as ModelAliasUpdateSpec
    await sent('W05-13: updateModelAlias', () => updateModelAlias('a-1', smuggled))
    expect(sentBody(mock), 'W05-13 gate: id top-level, immutables dropped').toEqual({ action: 'model-alias-update', id: 'a-1', data: { enabled: false } })
  })

  it('W05-13: the confirmed downgrade carries confirm_sensitivity_downgrade', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, model_alias: {}, warnings: ['sensitivity lowered internal → public (confirmed)'] }))
    const res = await sent('W05-13: updateModelAlias', () => updateModelAlias('a-1', { sensitivity: 'public', confirm_sensitivity_downgrade: true }))
    expect(sentBody(mock)).toEqual({ action: 'model-alias-update', id: 'a-1', data: { sensitivity: 'public', confirm_sensitivity_downgrade: true } })
    expect(res.warnings).toEqual(['sensitivity lowered internal → public (confirmed)'])
  })
})

describe('W05-13 preview (C7 contract)', () => {
  // RC-D S2 (W19 decision, contract of C7 RCD-c7-cli-preview-S1 §Übergaben
  // C5, binding by rc-d-s2-common.md point 1): the alias rides by NAME, the
  // chain mode REQUIRES consumer_id, the role mode answers primary_model as a
  // string ('' = nothing offered). The S1 test sent {alias: <id>} and read a
  // single mixed answer — replaced by one test per mode.
  it('W05-13: previewAliasChain sends {alias: <name>, consumer_id} and returns the chain answer', async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        success: true,
        consumer_id: 'c-1',
        alias: { id: 'a-1', name: 'chat', kind: 'chat', role: 'chat', scope: '_global', sensitivity: 'credentials' },
        op: 'chat',
        required_sensitivity: 'credentials',
        scope: 'home',
        lane: 'bulk',
        chain: [{ backend: 'spark-chat', locality: 'lan', provider_class: 'generic', billing: 'free', trust: 'full-trust', priority: 100 }],
        excluded: [],
        notes: ['body_scan_may_raise'],
      }),
    )
    const res = await sent('W05-13: previewAliasChain', () => previewAliasChain({ alias: 'chat', consumer_id: 'c-1' }))
    expect(sentBody(mock), 'W05-13: chain mode request').toEqual({ action: 'model-alias-preview', data: { alias: 'chat', consumer_id: 'c-1' } })
    expect(res.chain[0]?.backend).toBe('spark-chat')
  })

  it('W05-13: poolPrimaryModel sends {role} and returns primary_model', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, role: 'chat', primary_model: 'qwen38-27b' }))
    const res = await sent('W05-13: poolPrimaryModel', () => poolPrimaryModel({ role: 'chat' }))
    expect(sentBody(mock), 'W05-13: role mode request').toEqual({ action: 'model-alias-preview', data: { role: 'chat' } })
    expect(res.primary_model).toBe('qwen38-27b')
  })
})
