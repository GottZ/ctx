// Shape-compile pins for the self-service wire types (FE-T1, design 05 §1). These
// types carry no runtime — the teeth are at COMPILE time: `npm run check`
// (svelte-check; tsconfig.app includes src/**/*.ts) fails if a frozen shape
// drifts, and each `@ts-expect-error` line fails (TS2578 unused directive) if a
// forbidden shape STOPS being an error. The runtime `expect`s only give vitest a
// suite to run (vitest include = src/**/*.test.ts). The Go JSON golden tests stay
// the cross-language drift anchor; this file pins the TS mirror to its comments.

import { describe, expect, it } from 'vitest'
import type {
  ApiKeyCreateResult,
  ApiKeyUpdateResult,
  ApiKeyUpdateSpec,
  ScopeCreateResult,
  ScopeCreateSpec,
  Tenant,
  TenantCreateResult,
  TenantLimitSpec,
  TenantRole,
  TenantUsageResponse,
  TenantUsageView,
} from './types'

const sampleTenant: Tenant = {
  id: 't-1',
  slug: 'acme',
  display_name: 'Acme',
  status: 'active',
  created_at: '2026-06-30T00:00:00Z',
  updated_at: '2026-06-30T00:00:00Z',
  max_scopes: 10,
  max_keys: null, // null = unlimited (design 02)
}

describe('api-key-update (design 03 §5)', () => {
  it('accepts {id} plus optional tenant_role/active', () => {
    const spec: ApiKeyUpdateSpec = { id: 'k1', tenant_role: 'admin', active: false }
    const idOnly: ApiKeyUpdateSpec = { id: 'k1' }
    expect(spec.tenant_role).toBe('admin')
    expect('tenant_role' in idOnly).toBe(false)
  })

  it('pins tenant_role to the 059 owner|admin|member domain', () => {
    const roles: TenantRole[] = ['owner', 'admin', 'member']
    // @ts-expect-error — an arbitrary string is NOT a valid tenant_role
    const bad: ApiKeyUpdateSpec = { id: 'k1', tenant_role: 'superuser' }
    expect(roles).toHaveLength(3)
    expect(bad.id).toBe('k1')
  })

  it('the update result re-reads the key row (incl. tenant_role, active)', () => {
    const res: ApiKeyUpdateResult = {
      success: true,
      key: {
        id: 'k1',
        label: 'deploy',
        home_scope: 'acme:default',
        allowed_scopes: ['acme:default'],
        active: true,
        created_at: '2026-06-30T00:00:00Z',
        tenant_role: 'owner',
      },
    }
    expect(res.key.tenant_role).toBe('owner')
  })
})

describe('scope-create (design 01 §3 / 05 §1)', () => {
  it('sends the bare name (server prefixes); tenant_id is server-admin-only', () => {
    const spec: ScopeCreateSpec = { name: 'research' }
    const adminSpec: ScopeCreateSpec = { name: 'research', tenant_id: 't-2' }
    expect(spec.name).toBe('research')
    expect(adminSpec.tenant_id).toBe('t-2')
  })

  it('returns the SLIM { success, scope, tenant_id } — NOT ScopeOverview-shaped', () => {
    const res: ScopeCreateResult = { success: true, scope: 'acme:research', tenant_id: 't-1' }
    // @ts-expect-error — slim shape carries NO block_count (06 CD-7 nested shape rejected; 01 wins)
    const nested: ScopeCreateResult = { success: true, scope: 'acme:research', tenant_id: 't-1', block_count: 0 }
    expect(res.scope).toBe('acme:research')
    expect(nested.scope).toBe('acme:research')
  })
})

describe('tenant-create compound (design 03 §6 + 06 fixtures)', () => {
  it('returns a flat owner_key plaintext string (not a nested ApiKeyCreateResult)', () => {
    const res: TenantCreateResult = {
      success: true,
      tenant: sampleTenant,
      scope: 'acme:default',
      owner_key_id: 'k-owner',
      owner_key: 'ctx_sk_reveal_once',
    }
    expect(typeof res.owner_key).toBe('string')
    expect(res.tenant.slug).toBe('acme')
  })
})

describe('tenant limits + usage (design 02 §3 / 05 §1)', () => {
  it('TenantLimitSpec requires BOTH fields; null = unlimited', () => {
    const lim: TenantLimitSpec = { max_scopes: 5, max_keys: null }
    // @ts-expect-error — both max_scopes and max_keys are mandatory (server requirePresence)
    const partial: TenantLimitSpec = { max_scopes: 5 }
    expect(lim.max_keys).toBeNull()
    expect(partial.max_scopes).toBe(5)
  })

  it('TenantUsageView exposes counts + limits; the response wraps it under usage', () => {
    const usage: TenantUsageView = { tenant_id: 't-1', max_scopes: 10, max_keys: null, scope_count: 3, key_count: 2 }
    const resp: TenantUsageResponse = { success: true, usage }
    expect(resp.usage.scope_count).toBe(3)
    expect(resp.usage.max_keys).toBeNull()
  })
})

describe('api-key-create result (design 03 §7) — tenant_role added optional', () => {
  it('compiles with and without the additive tenant_role', () => {
    const withRole: ApiKeyCreateResult = {
      success: true,
      id: 'k1',
      label: 'owner',
      home_scope: 'acme:default',
      allowed_scopes: [],
      api_key: 'ctx_once',
      tenant_role: 'owner',
    }
    const withoutRole: ApiKeyCreateResult = {
      success: true,
      id: 'k2',
      label: 'min',
      home_scope: 'acme:default',
      allowed_scopes: [],
      api_key: 'ctx_once',
    }
    expect(withRole.tenant_role).toBe('owner')
    expect(withoutRole.tenant_role).toBeUndefined()
  })
})
