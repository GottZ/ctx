// ConsumersModel — the RC-D C5 mutations (W05-07 policy edit, W05-08
// rotation). Gates: update() forwards (id, patch) once, reloads page 1 and
// hands the answer back; a 403 lands on actionError AND rethrows (the dialog
// shows it — never a success); rotate() hands the reveal-once answer WITH the
// new plaintext to the caller and keeps NOTHING of it (design/05 §5 B-6, the
// second reveal-once path), reloads, and a refusal (409 inactive) rethrows.

import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../lib/api'
import type { ConsumerListResponse, ConsumerUpdateResult, ConsumerUpdateSpec, ModelKeyRotateResult, ModelKeyRotateSpec } from '../../lib/api/types'
import { ConsumersModel, type VisibilitySource } from './consumers.svelte'
import { consumerOf, key } from './forms.fixtures'

const ROTATED = 'ctxm_TESTKEY_rotated_once_do_not_persist'

const hidden: VisibilitySource = { visible: () => false, subscribe: () => () => {} }

function fakeApi() {
  const list = vi.fn(async (): Promise<ConsumerListResponse> => ({ success: true, consumers: [consumerOf('ci-chat')], next_cursor: null }))
  const update = vi.fn(async (_id: string, _patch: ConsumerUpdateSpec): Promise<ConsumerUpdateResult> => ({ success: true, consumer: consumerOf('ci-chat', { label: 'renamed' }) }))
  const rotate = vi.fn(async (_spec: ModelKeyRotateSpec): Promise<ModelKeyRotateResult> => ({
    success: true,
    key: key('k-new-sist', 'active', '2026-06-29T12:00:06Z'),
    api_key: ROTATED,
  }))
  return { api: { list, update, rotate }, list, update, rotate }
}

/** Every own enumerable value of the model, stringified — where a kept plaintext would show. */
function modelDump(m: ConsumersModel): string {
  const seen: string[] = []
  for (const k of Object.keys(m)) seen.push(JSON.stringify((m as unknown as Record<string, unknown>)[k]) ?? '')
  for (const k of ['consumers', 'actionError', 'loadError', 'refreshError', 'moreError', 'busyId', 'filter'] as const) {
    seen.push(JSON.stringify((m as unknown as Record<string, unknown>)[k]) ?? '')
  }
  return seen.join('\n')
}

describe('W05-07 ConsumersModel.update', () => {
  it('W05-07: update() forwards (id, patch) once, reloads page 1 and returns the answer', async () => {
    const f = fakeApi()
    const m = new ConsumersModel(f.api, hidden)
    await m.load()
    const res = await m.update('c-ci-chat', { label: 'renamed' }).catch((e: unknown) => {
      throw new Error(`W05-07: update() must forward the patch — it failed: ${String(e)}`)
    })
    expect(f.update.mock.calls, 'W05-07: one model-consumer-update').toEqual([['c-ci-chat', { label: 'renamed' }]])
    expect(f.list, 'reload after the update').toHaveBeenCalledTimes(2)
    expect(res.consumer.label).toBe('renamed')
    expect(m.busyId).toBeNull()
  })

  it('W05-07 gate: a 403 lands on actionError and rethrows — never a success', async () => {
    const f = fakeApi()
    f.update.mockRejectedValueOnce(new ApiError(403, 'forbidden', 'admin key required'))
    const m = new ConsumersModel(f.api, hidden)
    await m.load()
    await expect(m.update('c-ci-chat', { prompt_sensitivity: 'public' }), 'W05-07 gate: the refusal reaches the dialog').rejects.toMatchObject({ status: 403, message: 'admin key required' })
    expect(f.update, 'positive control: the api was asked').toHaveBeenCalledTimes(1)
    expect(m.actionError).toBe('admin key required')
    expect(f.list, 'no reload after a refusal').toHaveBeenCalledTimes(1)
  })
})

describe('W05-08 ConsumersModel.rotate', () => {
  it('W05-08 gate: rotate() hands the new plaintext to the caller once and keeps nothing (B-6, second reveal-once path)', async () => {
    const f = fakeApi()
    const m = new ConsumersModel(f.api, hidden)
    await m.load()
    const res = await m.rotate({ consumer_id: 'c-ci-chat', grace: 0 }).catch((e: unknown) => {
      throw new Error(`W05-08 gate: rotate() must hand back the reveal-once answer — it failed: ${String(e)}`)
    })
    expect(res.api_key, 'W05-08: the caller receives the plaintext').toBe(ROTATED)
    expect(f.rotate.mock.calls[0]?.[0], 'grace 0 reaches the api').toEqual({ consumer_id: 'c-ci-chat', grace: 0 })
    expect(modelDump(m), 'W05-08 gate: the model holds no plaintext').not.toContain('TESTKEY')
    expect(f.list, 'reload after the rotation').toHaveBeenCalledTimes(2)
  })

  it('W05-08: a refused rotation (409 inactive) lands on actionError and rethrows without a plaintext', async () => {
    const f = fakeApi()
    f.rotate.mockRejectedValueOnce(new ApiError(409, 'conflict', 'model consumer is not active'))
    const m = new ConsumersModel(f.api, hidden)
    await m.load()
    await expect(m.rotate({ consumer_id: 'c-ci-chat', grace: 3600 }), 'W05-08: the server refusal (409) reaches the dialog').rejects.toMatchObject({ status: 409 })
    expect(f.rotate, 'positive control: the api was asked').toHaveBeenCalledTimes(1)
    expect(m.actionError).toBe('model consumer is not active')
    expect(modelDump(m)).not.toContain('TESTKEY')
  })
})
