// Pure-logic gate for the type-registry admin form (design 04 §4.7, wave U10).
// The two U10 gates live here in the node env: the BUILTIN guard (a builtin is
// never deletable) and the 422-DRAFT mechanic (every write error keeps the modal
// open with the input intact — a 422 renders at the field). Plus the wholesale-
// replace safety (an edit never drops config keys this UI does not expose).

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../../lib/api'
import type { BlockTypeView } from '../../../lib/api/types'
import {
  canDeleteType,
  effectHint,
  emptyFields,
  fieldsFromType,
  inUseCounts,
  isBuiltin,
  parseFactor,
  policySummary,
  submitErrorFrom,
  toWriteSpec,
} from './types-admin.svelte'

function typeView(p: Partial<BlockTypeView> = {}): BlockTypeView {
  return {
    id: '77777777-7777-7777-7777-777777777777',
    name: 'issue',
    scope: '_global',
    display_name: 'Issue',
    description: 'A tracked work item',
    builtin: true,
    is_default: false,
    config: { v: 1, retrieval: { policy: 'full-pass' }, parent: { mode: 'none' } },
    created_at: '2026-07-03T00:00:00Z',
    updated_at: '2026-07-03T00:00:00Z',
    source: 'builtin',
    ...p,
  }
}

describe('builtin guard (U10 gate)', () => {
  it('a builtin/_global type is recognised by source, column OR scope', () => {
    expect(isBuiltin(typeView())).toBe(true)
    expect(isBuiltin(typeView({ source: 'tenant', builtin: false, scope: '_global' }))).toBe(true)
    expect(isBuiltin(typeView({ source: 'tenant', builtin: true, scope: 'acme' }))).toBe(true)
  })

  it('a custom tenant type is NOT builtin', () => {
    expect(isBuiltin(typeView({ name: 'sprint', source: 'tenant', builtin: false, scope: 'acme' }))).toBe(false)
  })

  it('canDeleteType is the negation — builtin blocked, custom allowed (the delete-disabled gate)', () => {
    expect(canDeleteType(typeView())).toBe(false) // builtin issue
    expect(canDeleteType(typeView({ name: 'sprint', source: 'tenant', builtin: false, scope: 'acme' }))).toBe(true)
  })
})

describe('422-draft mechanic (U10 gate)', () => {
  it('a 422 keeps the modal open and renders at the FIELD (never a silent input loss)', () => {
    const e = submitErrorFrom(new ApiError(422, 'validation', 'damping_factor must be between 0 and 1'))
    expect(e.keepOpen).toBe(true)
    expect(e.kind).toBe('field')
    expect(e.message).toContain('damping_factor')
  })

  it('a 400 (strict-decode / caps) is also a field-class draft error, modal stays open', () => {
    const e = submitErrorFrom(new ApiError(400, 'bad_request', 'unknown field "scope"'))
    expect(e.kind).toBe('field')
    expect(e.keepOpen).toBe(true)
  })

  it('a 403 / 409 / 5xx / network error keeps the modal open as a form-level banner', () => {
    for (const status of [403, 409, 500, 0]) {
      const e = submitErrorFrom(new ApiError(status, 'x', `err ${status}`))
      expect(e.keepOpen, `status ${status} must keep the modal open`).toBe(true)
      expect(e.kind, `status ${status} is a banner, not a field error`).toBe('form')
    }
  })

  it('a non-ApiError throw still keeps the input (no silent loss)', () => {
    const e = submitErrorFrom(new Error('boom'))
    expect(e.keepOpen).toBe(true)
    expect(e.message).toBe('boom')
  })
})

describe('delete-conflict counts (409 in-use)', () => {
  it('reads the active/archived counts off ApiError.details', () => {
    const e = new ApiError(409, 'conflict', 'type in use', null, { success: false, active: 3, archived: 1 })
    expect(inUseCounts(e)).toEqual({ active: 3, archived: 1 })
  })
  it('is null for a non-409 or a countless envelope', () => {
    expect(inUseCounts(new ApiError(409, 'conflict', 'x', null, { success: false }))).toBeNull()
    expect(inUseCounts(new ApiError(422, 'validation', 'x'))).toBeNull()
  })
})

describe('field round-trip + wholesale-replace safety', () => {
  it('fieldsFromType fills documented defaults for a missing config', () => {
    const f = fieldsFromType(typeView({ config: { v: 1 } }))
    expect(f.retrievalPolicy).toBe('full-pass')
    expect(f.guardCheck).toBe(true)
    expect(f.dreamLinkable).toBe(true)
    expect(f.parentMode).toBe('none')
    expect(f.isEdit).toBe(true)
  })

  it('an unknown retrieval policy degrades to full-pass (forward-compat, never crash)', () => {
    const f = fieldsFromType(typeView({ config: { v: 1, retrieval: { policy: 'future-mode' as never } } }))
    expect(f.retrievalPolicy).toBe('full-pass')
  })

  it('toWriteSpec PRESERVES config keys the form does not expose (no silent drop on edit)', () => {
    const t = typeView({
      config: {
        v: 1,
        retrieval: { policy: 'full-pass', intent_patterns: ['deploy', 'release'] },
        classify: { priority: 5, title_patterns: ['^fix:'] },
      },
    })
    const spec = toWriteSpec(fieldsFromType(t))
    // The exposed field is written …
    expect(spec.config?.retrieval?.policy).toBe('full-pass')
    // … and the UNexposed keys survive the wholesale replace.
    expect(spec.config?.retrieval?.intent_patterns).toEqual(['deploy', 'release'])
    expect(spec.config?.classify).toEqual({ priority: 5, title_patterns: ['^fix:'] })
    expect(spec.config?.v).toBe(1)
  })

  it('an edited scalar reaches the write spec', () => {
    const f = fieldsFromType(typeView())
    f.retrievalPolicy = 'excluded'
    f.guardCheck = false
    f.parentMode = 'required'
    const spec = toWriteSpec(f)
    expect(spec.config?.retrieval?.policy).toBe('excluded')
    expect(spec.config?.guard?.check).toBe(false)
    expect(spec.config?.parent?.mode).toBe('required')
  })

  it('a blank damping/threshold field is null, never 0; out-of-range throws', () => {
    expect(parseFactor('')).toBeNull()
    expect(parseFactor('0.5')).toBe(0.5)
    expect(() => parseFactor('2')).toThrow()
    expect(() => parseFactor('-1')).toThrow()
    expect(() => parseFactor('abc')).toThrow()
  })

  it('emptyFields is a valid all-defaults create seed', () => {
    const spec = toWriteSpec(emptyFields())
    expect(spec.config?.v).toBe(1)
    expect(spec.config?.retrieval?.policy).toBe('full-pass')
  })
})

describe('list-row summary + effect hints', () => {
  it('policySummary is compact and flags the non-default policy', () => {
    expect(policySummary(typeView())).toBe('full-pass')
    expect(
      policySummary(typeView({ config: { v: 1, retrieval: { policy: 'excluded' }, guard: { check: false } } })),
    ).toContain('guard off')
  })

  it('effectHint explains a consequence only for notable settings', () => {
    expect(effectHint(emptyFields())).toEqual([])
    const f = emptyFields()
    f.retrievalPolicy = 'excluded'
    f.guardCheck = false
    expect(effectHint(f).join(' ')).toMatch(/excluded/)
    expect(effectHint(f).join(' ')).toMatch(/guard check off/)
  })
})
