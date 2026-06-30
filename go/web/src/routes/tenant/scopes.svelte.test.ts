// ScopesSelfModel gates (FE-M3): load populates scopes + usage in parallel and
// reaches ready; a load failure maps to ApiError. create sends ONLY the bare name
// (S1 — no client prefix), returns the server-built ScopeCreateResult.scope,
// reloads list + usage, and a 400/429 lands on actionError + rethrows while busy
// clears. Plus the PURE limit logic (atScopeLimit/atKeyLimit): unlimited (null
// max), under, at and over the cap, and null-usage.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type {
  ScopeCreateResult,
  ScopeCreateSpec,
  ScopeOverview,
  ScopeOverviewListResponse,
  TenantUsageView,
} from '../../lib/api/types'
import { atKeyLimit, atScopeLimit, ScopesSelfModel } from './scopes.svelte'

function scope(p: Partial<ScopeOverview> & Pick<ScopeOverview, 'scope'>): ScopeOverview {
  return { block_count: 0, key_count: 0, tenant_id: 't1', ...p }
}

function usageView(p: Partial<TenantUsageView> = {}): TenantUsageView {
  return { tenant_id: 't1', max_scopes: null, max_keys: null, scope_count: 0, key_count: 0, ...p }
}

interface Call {
  m: 'list' | 'usage' | 'create'
  spec?: ScopeCreateSpec
}

function fakeApi(
  scopes: ScopeOverview[],
  usage: TenantUsageView,
  fail?: Partial<Record<Call['m'], ApiError>>,
) {
  const calls: Call[] = []
  let curScopes = scopes
  return {
    calls,
    setScopes: (s: ScopeOverview[]) => (curScopes = s),
    list: (): Promise<ScopeOverviewListResponse> => {
      calls.push({ m: 'list' })
      if (fail?.list) return Promise.reject(fail.list)
      return Promise.resolve({ success: true, scopes: curScopes })
    },
    usage: (): Promise<TenantUsageView> => {
      calls.push({ m: 'usage' })
      if (fail?.usage) return Promise.reject(fail.usage)
      return Promise.resolve(usage)
    },
    create: (spec: ScopeCreateSpec): Promise<ScopeCreateResult> => {
      calls.push({ m: 'create', spec })
      if (fail?.create) return Promise.reject(fail.create)
      return Promise.resolve({ success: true, scope: `acme:${spec.name}`, tenant_id: 't1' })
    },
  }
}

describe('atScopeLimit / atKeyLimit (pure)', () => {
  it('null max = unlimited → never at limit', () => {
    expect(atScopeLimit(usageView({ max_scopes: null, scope_count: 9999 }))).toBe(false)
    expect(atKeyLimit(usageView({ max_keys: null, key_count: 9999 }))).toBe(false)
  })

  it('under the cap → false', () => {
    expect(atScopeLimit(usageView({ max_scopes: 5, scope_count: 4 }))).toBe(false)
    expect(atKeyLimit(usageView({ max_keys: 5, key_count: 4 }))).toBe(false)
  })

  it('exactly at the cap → true', () => {
    expect(atScopeLimit(usageView({ max_scopes: 5, scope_count: 5 }))).toBe(true)
    expect(atKeyLimit(usageView({ max_keys: 5, key_count: 5 }))).toBe(true)
  })

  it('over the cap → true', () => {
    expect(atScopeLimit(usageView({ max_scopes: 5, scope_count: 6 }))).toBe(true)
  })

  it('zero cap → always at limit', () => {
    expect(atScopeLimit(usageView({ max_scopes: 0, scope_count: 0 }))).toBe(true)
  })

  it('null usage (not loaded) → false', () => {
    expect(atScopeLimit(null)).toBe(false)
    expect(atKeyLimit(null)).toBe(false)
  })
})

describe('ScopesSelfModel load', () => {
  it('populates scopes + usage in parallel and reaches ready', async () => {
    const api = fakeApi([scope({ scope: 'acme:main' })], usageView({ max_scopes: 5, scope_count: 1 }))
    const m = new ScopesSelfModel(api)
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.scopes).toHaveLength(1)
    expect(m.usage?.scope_count).toBe(1)
    expect(m.atScopeLimit).toBe(false)
  })

  it('reflects the limit through the model getter', async () => {
    const api = fakeApi([scope({ scope: 'acme:main' })], usageView({ max_scopes: 1, scope_count: 1 }))
    const m = new ScopesSelfModel(api)
    await m.load()
    expect(m.atScopeLimit).toBe(true)
  })

  it('surfaces a 403 load error', async () => {
    const api = fakeApi([], usageView(), { usage: new ApiError(403, 'forbidden', 'tenant-admin required') })
    const m = new ScopesSelfModel(api)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError?.status).toBe(403)
  })
})

describe('ScopesSelfModel create', () => {
  it('sends ONLY the bare name (no client prefix), returns the server scope, reloads', async () => {
    const api = fakeApi([], usageView())
    const m = new ScopesSelfModel(api)
    const res = await m.create('research')
    const create = api.calls.find((c) => c.m === 'create')
    expect(create?.spec).toEqual({ name: 'research' }) // bare name only, no tenant_id
    expect(res.scope).toBe('acme:research') // server-built full name
    // reload after create touches both list + usage
    expect(api.calls.filter((c) => c.m === 'list')).toHaveLength(1)
    expect(api.calls.filter((c) => c.m === 'usage')).toHaveLength(1)
    expect(m.busy).toBe(false)
  })

  it('surfaces a 429 over-limit and rethrows', async () => {
    const api = fakeApi([], usageView({ max_scopes: 1, scope_count: 1 }), {
      create: new ApiError(429, 'too_many', 'scope limit reached'),
    })
    const m = new ScopesSelfModel(api)
    await expect(m.create('research')).rejects.toBeInstanceOf(ApiError)
    expect(m.actionError).toContain('scope limit')
    expect(m.busy).toBe(false)
  })

  it('surfaces a 400 name-charset reject and rethrows', async () => {
    const api = fakeApi([], usageView(), {
      create: new ApiError(400, 'bad_request', 'scope name must not contain ":"'),
    })
    const m = new ScopesSelfModel(api)
    await expect(m.create('x:y')).rejects.toBeInstanceOf(ApiError)
    expect(m.actionError).toContain(':')
    expect(m.busy).toBe(false)
  })
})
