// ScopesModel gates (Wave A0-FE): load → ready + populated list; load → error
// maps the thrown value to ApiError (the server 403 a non-admin key gets); the
// empty store → ready with an empty list (the ScopeMap renders its Empty state).
// Read-only model — no mutation tests.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type { ScopeOverview, ScopeOverviewListResponse } from '../../lib/api/types'
import { ScopesModel } from './scopes.svelte'

function scope(p: Partial<ScopeOverview> & Pick<ScopeOverview, 'scope'>): ScopeOverview {
  return {
    block_count: 0,
    key_count: 0,
    tenant_id: null,
    ...p,
  }
}

function fakeApi(scopes: ScopeOverview[], fail?: ApiError) {
  let calls = 0
  return {
    get calls() {
      return calls
    },
    overview: (): Promise<ScopeOverviewListResponse> => {
      calls++
      if (fail) return Promise.reject(fail)
      return Promise.resolve({ success: true, scopes })
    },
  }
}

describe('ScopesModel load', () => {
  it('populates and reaches ready', async () => {
    const api = fakeApi([
      scope({ scope: 'private', block_count: 1042, key_count: 3, tenant_id: 't-default' }),
      scope({ scope: '_global', block_count: 0, key_count: 1, tenant_id: null }),
    ])
    const m = new ScopesModel(api)
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.scopes).toHaveLength(2)
    expect(m.scopes[0].scope).toBe('private')
    expect(m.scopes[0].block_count).toBe(1042)
    expect(m.scopes[1].tenant_id).toBeNull()
    expect(m.loadError).toBeNull()
  })

  it('reaches ready with an empty scope list (empty signal)', async () => {
    const api = fakeApi([])
    const m = new ScopesModel(api)
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.scopes).toHaveLength(0)
  })

  it('maps a thrown ApiError to the error state (403 non-admin)', async () => {
    const api = fakeApi([], new ApiError(403, 'forbidden', 'server-admin required'))
    const m = new ScopesModel(api)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError).toBeInstanceOf(ApiError)
    expect(m.loadError?.status).toBe(403)
    expect(m.scopes).toHaveLength(0)
  })

  it('normalizes a non-ApiError throw via toApiError', async () => {
    const api = {
      overview: (): Promise<ScopeOverviewListResponse> => Promise.reject(new Error('boom')),
    }
    const m = new ScopesModel(api)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError).toBeInstanceOf(ApiError)
    expect(m.loadError?.status).toBe(0)
  })

  it('reload re-invokes the overview action', async () => {
    const api = fakeApi([scope({ scope: 'private' })])
    const m = new ScopesModel(api)
    await m.load()
    await m.reload()
    expect(api.calls).toBe(2)
  })
})
