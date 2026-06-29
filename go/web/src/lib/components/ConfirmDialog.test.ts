// Pure confirm-step + slug-gate logic for ConfirmDialog.svelte (wave A2b).
// These live in the component's module script (no DOM) precisely so the
// two-click / Full-Prune gate can be unit-tested in the node-only vitest env
// (design 04-§5.5) — the DOM wiring (showModal, focus, role=status) is the
// thin shell around them and is exercised by the playwright gate, not here.

import { describe, expect, it } from 'vitest'
import { canConfirm, decideConfirm, slugMatches } from './ConfirmDialog.svelte'

describe('slugMatches', () => {
  it('matches an exact slug', () => {
    expect(slugMatches('team-mueller', 'team-mueller')).toBe(true)
  })
  it('rejects a mismatch', () => {
    expect(slugMatches('team-mueller', 'team-muller')).toBe(false)
  })
  it('is case-sensitive', () => {
    expect(slugMatches('team-mueller', 'Team-Mueller')).toBe(false)
  })
  it('trims surrounding whitespace on the typed value', () => {
    expect(slugMatches('team-mueller', '  team-mueller  ')).toBe(true)
  })
  it('rejects a partial prefix (no accidental full-prune on a too-short entry)', () => {
    expect(slugMatches('default', 'def')).toBe(false)
  })
})

describe('canConfirm', () => {
  it('is always allowed when no slug is required (undefined)', () => {
    expect(canConfirm(undefined, '')).toBe(true)
    expect(canConfirm(undefined, 'anything')).toBe(true)
  })
  it('treats an empty requireTyped as no requirement', () => {
    expect(canConfirm('', '')).toBe(true)
  })
  it('gates on the slug when required', () => {
    expect(canConfirm('acme', '')).toBe(false)
    expect(canConfirm('acme', 'acm')).toBe(false)
    expect(canConfirm('acme', 'acme')).toBe(true)
  })
})

describe('decideConfirm', () => {
  it('commits immediately on a benign confirm (no danger, no slug)', () => {
    expect(decideConfirm({ armed: false, typed: '' })).toBe('commit')
  })

  it('arms on the first click of a danger confirm, then commits', () => {
    expect(decideConfirm({ danger: true, armed: false, typed: '' })).toBe('arm')
    expect(decideConfirm({ danger: true, armed: true, typed: '' })).toBe('commit')
  })

  it('arms on the first click of a slug-gated confirm', () => {
    expect(decideConfirm({ requireTyped: 'acme', armed: false, typed: '' })).toBe('arm')
  })

  it('blocks the armed slug confirm until the slug matches exactly', () => {
    expect(decideConfirm({ requireTyped: 'acme', armed: true, typed: '' })).toBe('blocked')
    expect(decideConfirm({ requireTyped: 'acme', armed: true, typed: 'acm' })).toBe('blocked')
    expect(decideConfirm({ requireTyped: 'acme', armed: true, typed: 'acme' })).toBe('commit')
    expect(decideConfirm({ requireTyped: 'acme', armed: true, typed: ' acme ' })).toBe('commit')
  })
})
