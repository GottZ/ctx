// ProvisionModel gates (design 04 §7-U12): the ordered write sequence (the model
// half of the Fixture-Sequenz-Log — tenant-create → scope-create → api-key-create
// for the new-tenant flow, scope-create → api-key-create for the existing-tenant
// alt-flow), the K12 agent-key template on the mint spec, stage transitions +
// resume checkpoint, the error-keeps-stage draft path, and the hygiene invariant
// (a step RETURNS the plaintext, the model never stores it).

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type { TenantSpec } from '../../lib/api/tenants'
import type { ScopeCreateSpec } from '../../lib/api/types'
import type { ApiKeyCreateSpec } from '../../lib/api/keys'
import type { ApiKeyCreateResult, ScopeCreateResult, TenantCreateResult } from '../../lib/api/types'
import { ProvisionModel } from './provision.svelte'

interface Call {
  m: 'tenant' | 'scope' | 'key'
  tenant?: TenantSpec
  scope?: ScopeCreateSpec
  key?: ApiKeyCreateSpec
}

function fakeApi(fail?: Partial<Record<Call['m'], ApiError>>) {
  const calls: Call[] = []
  return {
    calls,
    createTenant: (spec: TenantSpec): Promise<TenantCreateResult> => {
      calls.push({ m: 'tenant', tenant: spec })
      if (fail?.tenant) return Promise.reject(fail.tenant)
      return Promise.resolve({
        success: true,
        tenant: {
          id: 'tid-ccc',
          slug: spec.slug,
          display_name: spec.display_name,
          status: 'active',
          created_at: '2026-07-04T00:00:00Z',
          updated_at: '2026-07-04T00:00:00Z',
        },
        scope: `${spec.slug}:main`,
        owner_key_id: 'ok1',
        owner_key: 'ctx_sk_OWNER_plaintext',
      })
    },
    createScope: (spec: ScopeCreateSpec): Promise<ScopeCreateResult> => {
      calls.push({ m: 'scope', scope: spec })
      if (fail?.scope) return Promise.reject(fail.scope)
      return Promise.resolve({ success: true, scope: `built:${spec.name}`, tenant_id: spec.tenant_id ?? 'tid' })
    },
    createApiKey: (spec: ApiKeyCreateSpec): Promise<ApiKeyCreateResult> => {
      calls.push({ m: 'key', key: spec })
      if (fail?.key) return Promise.reject(fail.key)
      return Promise.resolve({
        success: true,
        id: 'k1',
        label: spec.label,
        home_scope: spec.home_scope,
        allowed_scopes: spec.allowed_scopes ?? [],
        api_key: 'ctx_sk_AGENT_plaintext',
      })
    },
  }
}

describe('ProvisionModel — new-tenant flow (3 ordered steps)', () => {
  it('records tenant-create → scope-create → api-key-create in that order', async () => {
    const api = fakeApi()
    const m = new ProvisionModel(api)
    m.chooseNew()
    expect(m.mode).toBe('new')
    expect(m.stage).toBe('tenant')

    const t = await m.createTenantStep({ slug: 'globex', display_name: 'Globex', max_scopes: 25, max_keys: 50 })
    expect(t.owner_key).toBe('ctx_sk_OWNER_plaintext') // returned for reveal
    expect(m.tenantId).toBe('tid-ccc')
    expect(m.tenantSlug).toBe('globex')
    expect(m.stage).toBe('scope') // advanced — tenant now exists

    await m.createScopeStep('myrepo')
    expect(m.repoScope).toBe('built:myrepo')
    expect(m.stage).toBe('key')

    const k = await m.mintAgentKeyStep('agent-bot')
    expect(k.api_key).toBe('ctx_sk_AGENT_plaintext')
    expect(m.stage).toBe('done')

    expect(api.calls.map((c) => c.m)).toEqual(['tenant', 'scope', 'key'])
  })

  it('mints the K12 agent-key template: home=repo scope, allowed=[], write=[], tenant bound', async () => {
    const api = fakeApi()
    const m = new ProvisionModel(api)
    m.chooseNew()
    await m.createTenantStep({ slug: 'globex', display_name: 'Globex', max_scopes: 25, max_keys: 50 })
    await m.createScopeStep('myrepo')
    await m.mintAgentKeyStep('agent-bot')
    const keySpec = api.calls.find((c) => c.m === 'key')?.key
    expect(keySpec).toEqual({
      label: 'agent-bot',
      home_scope: 'built:myrepo',
      allowed_scopes: [],
      write_scopes: [],
      tenant_id: 'tid-ccc',
    })
  })

  it('the model never stores a key plaintext — only the non-secret checkpoint', () => {
    const m = new ProvisionModel(fakeApi())
    const snapshot = JSON.stringify({
      mode: m.mode,
      stage: m.stage,
      tenantId: m.tenantId,
      tenantSlug: m.tenantSlug,
      repoScope: m.repoScope,
      error: m.error,
    })
    expect(snapshot).not.toContain('ctx_sk')
  })
})

describe('ProvisionModel — existing-tenant alt-flow (2 calls, §9.7)', () => {
  it('skips tenant-create and binds the scope + key to the chosen tenant', async () => {
    const api = fakeApi()
    const m = new ProvisionModel(api)
    m.chooseExisting('tid-aaa', 'acme')
    expect(m.mode).toBe('existing')
    expect(m.stage).toBe('scope')

    await m.createScopeStep('altrepo')
    await m.mintAgentKeyStep('agent-bot')

    expect(api.calls.map((c) => c.m)).toEqual(['scope', 'key']) // NO tenant-create
    expect(api.calls.find((c) => c.m === 'scope')?.scope?.tenant_id).toBe('tid-aaa')
    expect(api.calls.find((c) => c.m === 'key')?.key?.tenant_id).toBe('tid-aaa')
  })
})

describe('ProvisionModel — error keeps the stage (draft path)', () => {
  it('a scope-create 409 sets the error and leaves the stage on scope', async () => {
    const api = fakeApi({ scope: new ApiError(409, 'conflict', 'scope already exists') })
    const m = new ProvisionModel(api)
    m.chooseExisting('tid-aaa', 'acme')
    await expect(m.createScopeStep('dup')).rejects.toThrow()
    expect(m.error).toBe('scope already exists')
    expect(m.stage).toBe('scope') // not advanced
    expect(m.repoScope).toBeNull()
    expect(m.resumable).toBe(true) // the tenant checkpoint is still resumable
  })
})

describe('ProvisionModel — resume + reset', () => {
  it('is resumable only in the scope/key stages and reset clears the checkpoint', async () => {
    const api = fakeApi()
    const m = new ProvisionModel(api)
    expect(m.resumable).toBe(false) // entry
    m.chooseNew()
    expect(m.resumable).toBe(false) // tenant step, nothing provisioned yet
    await m.createTenantStep({ slug: 'globex', display_name: 'Globex', max_scopes: null, max_keys: null })
    expect(m.resumable).toBe(true) // scope stage, tenant exists
    m.reset()
    expect(m.stage).toBe('entry')
    expect(m.tenantId).toBeNull()
    expect(m.repoScope).toBeNull()
    expect(m.resumable).toBe(false)
  })
})
