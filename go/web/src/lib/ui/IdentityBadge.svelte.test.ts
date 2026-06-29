// Pure projection logic for IdentityBadge.svelte (design 06-role-nav.md §3/§58,
// Welle N7). `identityView` maps a session-like source to the render-ready view;
// it lives in the component's module script (no DOM) so it unit-tests in the
// node-only vitest env, mirroring ThemeToggle.svelte.test. The DOM wiring
// (expanded vs icon render, aria-label attrs) is the thin shell around it and is
// left to the playwright/visual gate, not exercised here.

import { describe, expect, it } from 'vitest'
import type { IdentitySource } from './IdentityBadge.svelte'
import { BADGE_ROLES, identityView } from './IdentityBadge.svelte'

const base: IdentitySource = {
  label: 'ci-key',
  role: 'member',
  tenantDisplayName: 'Acme Corp',
  tenantSlug: 'acme',
  homeScope: 'private',
}

describe('BADGE_ROLES', () => {
  it('covers the three 059 tenant roles, in tier order', () => {
    expect(BADGE_ROLES).toEqual(['owner', 'admin', 'member'])
  })
})

describe('identityView — role badge', () => {
  it.each(['owner', 'admin', 'member'])('keeps a known role %s and derives its initial', (role) => {
    const v = identityView({ ...base, role })
    expect(v.role).toBe(role)
    expect(v.roleInitial).toBe(role.charAt(0).toUpperCase())
  })

  it('drops an unknown role to null (no badge — forward-compat, R3)', () => {
    const v = identityView({ ...base, role: 'superuser' })
    expect(v.role).toBeNull()
    expect(v.roleInitial).toBeNull()
  })

  it('drops an empty/absent role to null', () => {
    expect(identityView({ ...base, role: '' }).role).toBeNull()
    expect(identityView({ ...base, role: null }).role).toBeNull()
  })
})

describe('identityView — tenant', () => {
  it('prefers the display name over the slug', () => {
    expect(identityView(base).tenant).toBe('Acme Corp')
  })

  it('falls back to the slug when only the slug is set', () => {
    expect(identityView({ ...base, tenantDisplayName: '' }).tenant).toBe('acme')
    expect(identityView({ ...base, tenantDisplayName: null }).tenant).toBe('acme')
  })

  it('is null when neither name nor slug is set (omit)', () => {
    expect(identityView({ ...base, tenantDisplayName: '', tenantSlug: '' }).tenant).toBeNull()
    expect(identityView({ ...base, tenantDisplayName: null, tenantSlug: null }).tenant).toBeNull()
  })
})

describe('identityView — label', () => {
  it('passes a non-empty label through', () => {
    expect(identityView(base).label).toBe('ci-key')
  })

  it('drops an empty/absent label to null (omit)', () => {
    expect(identityView({ ...base, label: '' }).label).toBeNull()
    expect(identityView({ ...base, label: null }).label).toBeNull()
  })
})

describe('identityView — read-only', () => {
  it('flags read-only only when home_scope is exactly empty', () => {
    expect(identityView({ ...base, homeScope: '' }).readOnly).toBe(true)
    expect(identityView({ ...base, homeScope: 'private' }).readOnly).toBe(false)
    // null (loading / no whoami) is NOT read-only — mounts only happen post-login.
    expect(identityView({ ...base, homeScope: null }).readOnly).toBe(false)
  })
})

describe('identityView — combined tiers', () => {
  it('projects a server-admin / owner writable key', () => {
    const v = identityView({
      label: 'root',
      role: 'owner',
      tenantDisplayName: 'Default',
      tenantSlug: 'default',
      homeScope: 'shared',
    })
    expect(v).toEqual({
      label: 'root',
      tenant: 'Default',
      role: 'owner',
      roleInitial: 'O',
      readOnly: false,
    })
  })

  it('projects a read-only member with no tenant', () => {
    const v = identityView({
      label: 'guest',
      role: 'member',
      tenantDisplayName: '',
      tenantSlug: '',
      homeScope: '',
    })
    expect(v).toEqual({
      label: 'guest',
      tenant: null,
      role: 'member',
      roleInitial: 'M',
      readOnly: true,
    })
  })
})
