// FOLGE-Z Z5 gates (briefings/FOLGE-Z-INDEX.md §1 C-SPA, §3): vision without
// an extra step, made visible.
//   Z5-alias    the catalog row's badge from effective_vision + vision_source:
//               on (backend) / on (policy) / off (policy) / off (no
//               image-capable backend); chat aliases only; absent fields ⇒
//               "not reported", never a guessed capability
//   Z5-policy   the policy form's vision "default" says what default means
//   Z5-backend  the pool row's badge from capabilities.vision
//               (true / false / null = unknown), probed_at as its title

import { describe, expect, it } from 'vitest'
import type { BackendView, ModelAliasView } from '../../lib/api/types'
import { aliasOf } from './forms.fixtures'
import { aliasVisionBadge, backendVisionBadge, policyDefaultLabel } from './vision'

const chat = (p: Partial<ModelAliasView>) => aliasOf('a1', '_global', 'chat', p)

describe('FOLGE-Z Z5-alias: the catalog badge', () => {
  it('Z5-alias gate: the four contract states', () => {
    expect(aliasVisionBadge(chat({ effective_vision: true, vision_source: 'backend' }))?.text).toBe('images: on (backend)')
    expect(aliasVisionBadge(chat({ effective_vision: true, vision_source: 'policy' }))?.text).toBe('images: on (policy)')
    expect(aliasVisionBadge(chat({ effective_vision: false, vision_source: 'policy' }))?.text).toBe('images: off (policy)')
    expect(aliasVisionBadge(chat({ effective_vision: false, vision_source: 'none' }))?.text).toBe('images: off (no image-capable backend)')
  })

  it('Z5-alias: tones follow effective_vision', () => {
    expect(aliasVisionBadge(chat({ effective_vision: true, vision_source: 'backend' }))?.tone).toBe('on')
    expect(aliasVisionBadge(chat({ effective_vision: false, vision_source: 'none' }))?.tone).toBe('off')
  })

  it('Z5-alias gate: fail-closed — a server without the fields reports nothing', () => {
    const b = aliasVisionBadge(chat({}))
    expect(b?.text, 'Z5-alias: no field ⇒ not reported').toBe('images: not reported')
    expect(b?.tone).toBe('unknown')
    expect(aliasVisionBadge(chat({ effective_vision: 'yes' as unknown as boolean, vision_source: 'backend' }))?.text).toBe('images: not reported')
  })

  it('Z5-alias: a pair outside the contract keeps the verdict and drops the claimed source', () => {
    expect(aliasVisionBadge(chat({ effective_vision: true, vision_source: 'none' }))?.text).toBe('images: on (source not reported)')
    expect(aliasVisionBadge(chat({ effective_vision: false, vision_source: 'backend' }))?.text).toBe('images: off (source not reported)')
    expect(aliasVisionBadge(chat({ effective_vision: true, vision_source: 'probe' as never }))?.text).toBe('images: on (source not reported)')
  })

  it('Z5-alias: images are a chat matter — embedding and rerank rows carry no badge', () => {
    expect(aliasVisionBadge(chat({ kind: 'embedding', effective_vision: false, vision_source: 'none' }))).toBeNull()
    expect(aliasVisionBadge(chat({ kind: 'rerank' }))).toBeNull()
  })

  it('Z5-alias: the title explains the source', () => {
    expect(aliasVisionBadge(chat({ effective_vision: true, vision_source: 'backend' }))?.title).toMatch(/backend.*reports/i)
    expect(aliasVisionBadge(chat({ effective_vision: false, vision_source: 'policy' }))?.title).toMatch(/policy\.vision/)
  })
})

describe('FOLGE-Z Z5-policy: the vision default is explained', () => {
  it('Z5-policy gate: vision "default" reads "as the backend reports"; other keys keep "default"', () => {
    expect(policyDefaultLabel('vision')).toMatch(/as the backend reports/)
    expect(policyDefaultLabel('reasoning')).toBe('default')
    expect(policyDefaultLabel('max_n')).toBe('default')
  })
})

describe('FOLGE-Z Z5-backend: the pool row badge', () => {
  const row = (capabilities?: unknown): BackendView => ({ capabilities } as unknown as BackendView)

  it('Z5-backend gate: true / false / null = unknown', () => {
    expect(backendVisionBadge(row({ vision: true, probed_at: '2026-11-08T10:00:00Z', source: 'sglang:/get_model_info has_image_understanding' }))).toMatchObject({ text: 'vision: yes', tone: 'on' })
    expect(backendVisionBadge(row({ vision: false, probed_at: '2026-11-08T10:00:00Z', source: 'llamacpp:/props' }))).toMatchObject({ text: 'vision: no', tone: 'off' })
    expect(backendVisionBadge(row({ vision: null, probed_at: null, source: 'unknown' }))).toMatchObject({ text: 'vision: unknown', tone: 'unknown' })
  })

  it('Z5-backend gate: probed_at is the title (with the source); never probed says so', () => {
    expect(backendVisionBadge(row({ vision: true, probed_at: '2026-11-08T10:00:00Z', source: 'sglang:/get_model_info has_image_understanding' })).title).toBe(
      'probed 2026-11-08T10:00:00Z · sglang:/get_model_info has_image_understanding',
    )
    expect(backendVisionBadge(row({ vision: null, probed_at: null, source: '' })).title).toBe('never probed')
  })

  it('Z5-backend gate: fail-closed — no capabilities (a server before FOLGE-Z) is unknown, never yes', () => {
    expect(backendVisionBadge(row(undefined))).toEqual({ text: 'vision: unknown', tone: 'unknown', title: 'not reported by this server' })
    expect(backendVisionBadge(row({ vision: 'true', probed_at: 5, source: null })).text).toBe('vision: unknown')
  })
})
