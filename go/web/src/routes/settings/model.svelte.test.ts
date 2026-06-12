// SettingsModel gates: dirty tracking over drafts, the sequential group-save
// flow (one PUT per key, a failed key keeps its inline error while the rest
// still write), PUT-response patching (value/source/warnings) and the
// DELETE-reset path.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type { SettingDeleteResponse, SettingPutResponse, SettingView } from '../../lib/api/types'
import { SettingsModel } from './model.svelte'

function view(partial: Partial<SettingView> & Pick<SettingView, 'key' | 'type'>): SettingView {
  return { mutability: 'hot', value: '', source: 'env', default: '', ...partial }
}

function catalog(): SettingView[] {
  return [
    view({ key: 'rerank.blend_weight', type: 'float', value: 0.5 }),
    view({ key: 'rerank.max_docs', type: 'int', value: 50 }),
    view({ key: 'graph.enabled', type: 'bool', value: true }),
    view({ key: 'server.db', type: 'string', value: 'ctx', mutability: 'restart' }),
    view({ key: 'chat.api_key', type: 'string', value: '(set via env)', sensitive: true }),
  ]
}

interface Call {
  kind: 'put' | 'del'
  key: string
  value?: string | number | boolean
}

/** Scripted api stub: answers per key, records the call order. */
function stubApi(answers: Record<string, SettingPutResponse | SettingDeleteResponse | ApiError> = {}) {
  const calls: Call[] = []
  const answer = <T>(key: string, fallback: T): Promise<T> => {
    const a = answers[key]
    if (a instanceof ApiError) return Promise.reject(a)
    return Promise.resolve((a as T) ?? fallback)
  }
  return {
    calls,
    put: (key: string, value: string | number | boolean): Promise<SettingPutResponse> => {
      calls.push({ kind: 'put', key, value })
      return answer(key, {
        success: true as const,
        key,
        value,
        source: 'db' as const,
        previous: { value: null, source: 'env' as const },
        warnings: [],
      })
    },
    del: (key: string): Promise<SettingDeleteResponse> => {
      calls.push({ kind: 'del', key })
      return answer(key, { success: true as const, key, value: 'reverted', source: 'env' as const })
    },
  }
}

describe('SettingsModel', () => {
  it('initializes drafts from the catalog and tracks dirtiness', () => {
    const m = new SettingsModel(stubApi())
    m.load(catalog())
    expect(m.drafts['rerank.blend_weight']).toBe('0.5')
    expect(m.isDirty('rerank.blend_weight')).toBe(false)

    m.drafts['rerank.blend_weight'] = '0.7'
    expect(m.isDirty('rerank.blend_weight')).toBe(true)
    expect(m.dirtyKeys('rerank')).toEqual(['rerank.blend_weight'])
    expect(m.dirtyKeys('graph')).toEqual([])
  })

  it('never reports a read-only key dirty (restart cannot PUT)', () => {
    const m = new SettingsModel(stubApi())
    m.load(catalog())
    m.drafts['server.db'] = 'other'
    expect(m.isDirty('server.db')).toBe(false)
  })

  it('saves a group sequentially and patches value/source from the response', async () => {
    const api = stubApi()
    const m = new SettingsModel(api)
    m.load(catalog())
    m.drafts['rerank.blend_weight'] = '0.7'
    m.drafts['rerank.max_docs'] = '40'

    await m.saveGroup('rerank')

    expect(api.calls).toEqual([
      { kind: 'put', key: 'rerank.blend_weight', value: 0.7 },
      { kind: 'put', key: 'rerank.max_docs', value: 40 },
    ])
    const s = m.byKey('rerank.blend_weight')
    expect(s?.value).toBe(0.7)
    expect(s?.source).toBe('db')
    expect(m.isDirty('rerank.blend_weight')).toBe(false)
    expect(m.saving).toBe(false)
  })

  it('keeps a 422 inline on its field while later keys still write', async () => {
    const api = stubApi({
      'rerank.blend_weight': new ApiError(422, 'validation', 'validation: rerank.blend_weight must be in [0,1]'),
    })
    const m = new SettingsModel(api)
    m.load(catalog())
    m.drafts['rerank.blend_weight'] = '7'
    m.drafts['rerank.max_docs'] = '40'

    await m.saveGroup('rerank')

    expect(m.errors['rerank.blend_weight']).toContain('must be in [0,1]')
    expect(m.isDirty('rerank.blend_weight')).toBe(true)
    expect(api.calls.map((c) => c.key)).toEqual(['rerank.blend_weight', 'rerank.max_docs'])
    expect(m.errors['rerank.max_docs']).toBeUndefined()
  })

  it('stops a client-side parse failure before the wire', async () => {
    const api = stubApi()
    const m = new SettingsModel(api)
    m.load(catalog())
    m.drafts['rerank.max_docs'] = 'many'

    await m.saveGroup('rerank')

    expect(api.calls).toEqual([])
    expect(m.errors['rerank.max_docs']).toContain('integer')
  })

  it('surfaces PUT warnings on the field', async () => {
    const api = stubApi({
      'rerank.blend_weight': {
        success: true,
        key: 'rerank.blend_weight',
        value: 1,
        source: 'db',
        previous: { value: 0.5, source: 'env' },
        warnings: ['rerank.blend_weight: blend_weight 1.0 with graph expansion enabled'],
      },
    })
    const m = new SettingsModel(api)
    m.load(catalog())
    m.drafts['rerank.blend_weight'] = '1'

    await m.saveGroup('rerank')

    expect(m.warnings['rerank.blend_weight']).toHaveLength(1)
  })

  it('resets via DELETE and adopts the post-revert value', async () => {
    const api = stubApi()
    const m = new SettingsModel(api)
    m.load(catalog())

    await m.reset('rerank.blend_weight')

    expect(api.calls).toEqual([{ kind: 'del', key: 'rerank.blend_weight' }])
    const s = m.byKey('rerank.blend_weight')
    expect(s?.value).toBe('reverted')
    expect(s?.source).toBe('env')
  })

  it('starts sensitive drafts empty and PUTs the secret name', async () => {
    const api = stubApi()
    const m = new SettingsModel(api)
    m.load(catalog())
    expect(m.drafts['chat.api_key']).toBe('')
    expect(m.isDirty('chat.api_key')).toBe(false)

    m.drafts['chat.api_key'] = 'openrouter-main'
    await m.saveGroup('chat')

    expect(api.calls).toEqual([{ kind: 'put', key: 'chat.api_key', value: 'openrouter-main' }])
  })

  it('mirrors the cross-field rules over drafted values', () => {
    const m = new SettingsModel(stubApi())
    m.load(catalog())
    expect(m.issues).toEqual([])

    m.drafts['rerank.blend_weight'] = '1'
    expect(m.issuesFor('rerank.blend_weight')).toHaveLength(1)
    expect(m.issuesFor('rerank.blend_weight')[0].severity).toBe('warn')

    m.revertDraft('rerank.blend_weight')
    expect(m.issues).toEqual([])
  })
})
