// Pure guard logic for the TK7b role/active controls: owner counting, the
// last-active-owner predicate and the combined disable rule (self-row /
// last-owner / busy). The component wiring + the rendered disabled states are
// covered by the e2e spec (selfservice-key-roles); this pins the arithmetic.

import { describe, expect, it } from 'vitest'
import type { ApiKeyView } from '../../lib/api/types'
import { activeOwnerCount, controlDisabled, isLastActiveOwner } from './role-guards'

function key(p: Partial<ApiKeyView> & Pick<ApiKeyView, 'id'>): ApiKeyView {
  return {
    label: p.id,
    home_scope: 'home',
    allowed_scopes: ['home'],
    active: true,
    created_at: '2026-06-29T00:00:00Z',
    tenant_role: 'member',
    ...p,
  }
}

describe('activeOwnerCount', () => {
  it('counts only ACTIVE owners', () => {
    const keys = [
      key({ id: 'o1', tenant_role: 'owner' }),
      key({ id: 'o2', tenant_role: 'owner', active: false }), // revoked owner — excluded
      key({ id: 'a1', tenant_role: 'admin' }),
      key({ id: 'm1', tenant_role: 'member' }),
    ]
    expect(activeOwnerCount(keys)).toBe(1)
  })

  it('is 0 with no owners', () => {
    expect(activeOwnerCount([key({ id: 'm1', tenant_role: 'member' })])).toBe(0)
  })
})

describe('isLastActiveOwner', () => {
  it('true for the sole active owner (count<=1)', () => {
    expect(isLastActiveOwner(key({ id: 'o1', tenant_role: 'owner' }), 1)).toBe(true)
  })

  it('false when another active owner exists (count>=2)', () => {
    expect(isLastActiveOwner(key({ id: 'o1', tenant_role: 'owner' }), 2)).toBe(false)
  })

  it('false for a non-owner regardless of count', () => {
    expect(isLastActiveOwner(key({ id: 'a1', tenant_role: 'admin' }), 1)).toBe(false)
  })

  it('false for a revoked owner (not active → does not orphan)', () => {
    expect(isLastActiveOwner(key({ id: 'o1', tenant_role: 'owner', active: false }), 1)).toBe(false)
  })
})

describe('controlDisabled', () => {
  const member = key({ id: 'm1', tenant_role: 'member' })
  const lastOwner = key({ id: 'o1', tenant_role: 'owner' })

  it('enabled for an editable member row', () => {
    expect(controlDisabled(member, 1, false, false)).toBe(false)
  })

  it('disabled for the own (self) row', () => {
    expect(controlDisabled(member, 1, true, false)).toBe(true)
  })

  it('disabled for the last active owner', () => {
    expect(controlDisabled(lastOwner, 1, false, false)).toBe(true)
  })

  it('enabled for an owner while a co-owner exists (count 2)', () => {
    expect(controlDisabled(lastOwner, 2, false, false)).toBe(false)
  })

  it('disabled while a mutation on the row is in flight (busy)', () => {
    expect(controlDisabled(member, 1, false, true)).toBe(true)
  })
})
