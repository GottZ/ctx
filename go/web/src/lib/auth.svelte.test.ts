// Session gates (design 04-§3.4): login probe, sessionStorage lifetime,
// restore semantics and the 401-interceptor wiring. sessionStorage and fetch
// are stubbed — plain node environment. Test keys assembled at runtime.

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Session, session } from './auth.svelte'
import { apiFetch } from './api'

const TEST_KEY = ['dead', 'beef'].join('') // doc-value, assembled at runtime
const STORAGE_KEY = 'ctx.api-key'

const WHOAMI_ADMIN = {
  success: true,
  label: 'example-admin',
  home_scope: 'private',
  read_scopes: ['private', 'shared'],
  admin: true,
}

// Full-shape whoami fixtures per derived tier (N2). Server-admin is role='owner'
// in the default tenant — `admin` is orthogonal and wins (R4).
const WHOAMI_SERVER_ADMIN = {
  success: true,
  label: 'root-operator',
  home_scope: 'private',
  read_scopes: ['private', 'shared'],
  admin: true,
  tenant_id: 'tenant-default',
  role: 'owner',
  api_key_id: 'key-server-admin',
  tenant_slug: 'default',
  tenant_display_name: 'Default Tenant',
}

const WHOAMI_TENANT_ADMIN = {
  success: true,
  label: 'acme-owner',
  home_scope: 'acme',
  read_scopes: ['acme', 'shared'],
  admin: false,
  tenant_id: 'tenant-acme',
  role: 'owner',
  api_key_id: 'key-tenant-admin',
  tenant_slug: 'acme',
  tenant_display_name: 'ACME Corp',
}

const WHOAMI_MEMBER = {
  success: true,
  label: 'acme-member',
  home_scope: 'acme',
  read_scopes: ['acme'],
  admin: false,
  tenant_id: 'tenant-acme',
  role: 'member',
  api_key_id: 'key-member',
  tenant_slug: 'acme',
  tenant_display_name: 'ACME Corp',
}

function memoryStorage(): Storage {
  const store = new Map<string, string>()
  return {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => [...store.keys()][i] ?? null,
    get length() {
      return store.size
    },
  }
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

let storage: Storage

beforeEach(() => {
  vi.unstubAllGlobals()
  storage = memoryStorage()
  vi.stubGlobal('sessionStorage', storage)
  // The module-scope configureApi binds the api client to the singleton —
  // reset it between tests.
  session.logout()
  session.notice = null
})

describe('Session.login', () => {
  it('probes whoami with the entered key and persists it on success', async () => {
    const mock = vi.fn().mockResolvedValueOnce(jsonResponse(200, WHOAMI_ADMIN))
    vi.stubGlobal('fetch', mock)

    const s = new Session()
    await s.login(`  ${TEST_KEY}  `) // trimmed before use

    expect(s.active).toBe(true)
    expect(s.admin).toBe(true)
    expect(s.label).toBe('example-admin')
    expect(storage.getItem(STORAGE_KEY)).toBe(TEST_KEY)
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(new Headers(init.headers).get('Authorization')).toBe(`Bearer ${TEST_KEY}`)
  })

  it('throws on a rejected key and persists nothing', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(jsonResponse(401, { error: 'unauthorized' })))
    const s = new Session()
    await expect(s.login(TEST_KEY)).rejects.toMatchObject({ status: 401, code: 'unauthorized' })
    expect(s.active).toBe(false)
    expect(storage.getItem(STORAGE_KEY)).toBeNull()
  })

  it('reflects admin:false for read-only keys', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValueOnce(jsonResponse(200, { ...WHOAMI_ADMIN, admin: false })),
    )
    const s = new Session()
    await s.login(TEST_KEY)
    expect(s.active).toBe(true)
    expect(s.admin).toBe(false)
  })
})

describe('Session.logout', () => {
  it('clears state and storage', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(jsonResponse(200, WHOAMI_ADMIN)))
    const s = new Session()
    await s.login(TEST_KEY)
    s.logout()
    expect(s.active).toBe(false)
    expect(s.key).toBeNull()
    expect(storage.getItem(STORAGE_KEY)).toBeNull()
  })
})

describe('Session.restore', () => {
  it('re-probes a stored key on tab reload', async () => {
    storage.setItem(STORAGE_KEY, TEST_KEY)
    const mock = vi.fn().mockResolvedValueOnce(jsonResponse(200, WHOAMI_ADMIN))
    vi.stubGlobal('fetch', mock)

    const s = new Session()
    await s.restore()
    expect(s.active).toBe(true)
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(new Headers(init.headers).get('Authorization')).toBe(`Bearer ${TEST_KEY}`)
  })

  it('does nothing in a fresh tab (no stored key)', async () => {
    const mock = vi.fn()
    vi.stubGlobal('fetch', mock)
    const s = new Session()
    await s.restore()
    expect(s.active).toBe(false)
    expect(mock).not.toHaveBeenCalled()
  })

  it('drops a revoked key (401) and leaves a notice', async () => {
    storage.setItem(STORAGE_KEY, TEST_KEY)
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(jsonResponse(401, { error: 'unauthorized' })))
    const s = new Session()
    await s.restore()
    expect(s.active).toBe(false)
    expect(storage.getItem(STORAGE_KEY)).toBeNull()
    expect(s.notice).not.toBeNull()
  })

  it('keeps the stored key on transient failures (network)', async () => {
    storage.setItem(STORAGE_KEY, TEST_KEY)
    vi.stubGlobal('fetch', vi.fn().mockRejectedValueOnce(new TypeError('fetch failed')))
    const s = new Session()
    await s.restore()
    expect(s.active).toBe(false)
    expect(storage.getItem(STORAGE_KEY)).toBe(TEST_KEY)
  })
})

describe('401 interceptor wiring (singleton)', () => {
  it('tears the session down when the stored key gets revoked mid-session', async () => {
    vi.stubGlobal(
      'fetch',
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, WHOAMI_ADMIN))
        .mockResolvedValueOnce(jsonResponse(401, { error: 'unauthorized' })),
    )

    await session.login(TEST_KEY)
    expect(session.active).toBe(true)

    // Any later call with the stored key → 401 → interceptor → login screen.
    await expect(apiFetch('/api/manage', { method: 'POST', body: '{}' })).rejects.toMatchObject({
      status: 401,
    })
    expect(session.active).toBe(false)
    expect(session.key).toBeNull()
    expect(storage.getItem(STORAGE_KEY)).toBeNull()
    expect(session.notice).not.toBeNull()
  })
})

describe('Session capability deriveds (N2)', () => {
  it('reports tier=loading with no flags before whoami resolves', () => {
    const s = new Session()
    expect(s.tier).toBe('loading')
    expect(s.caps.tier).toBe('loading')
    expect(s.caps.viewCorpus).toBe(false)
    expect(s.caps.viewTenants).toBe(false)
    expect(s.caps.viewOpsSurfaces).toBe(false)
    // Raw identity fields degrade to null/[] until whoami arrives.
    expect(s.role).toBeNull()
    expect(s.tenantId).toBeNull()
    expect(s.homeScope).toBeNull()
    expect(s.readScopes).toEqual([])
    expect(s.tenantSlug).toBeNull()
    expect(s.tenantDisplayName).toBeNull()
    expect(s.apiKeyId).toBeNull()
  })

  it('keeps tier=loading while a boot-time restore probe is in flight (R6)', () => {
    const s = new Session()
    s.restoring = true // whoami not yet set
    expect(s.tier).toBe('loading')
    expect(s.caps.tier).toBe('loading')
  })

  it('derives the server-admin tier + full caps from an admin whoami', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(jsonResponse(200, WHOAMI_SERVER_ADMIN)))
    const s = new Session()
    await s.login(TEST_KEY)

    expect(s.tier).toBe('server-admin')
    expect(s.caps.viewCorpus).toBe(true)
    expect(s.caps.viewTenants).toBe(true)
    expect(s.caps.manageTenantKeys).toBe(true)
    expect(s.caps.viewOpsSurfaces).toBe(true)
    expect(s.role).toBe('owner')
    expect(s.tenantId).toBe('tenant-default')
    expect(s.homeScope).toBe('private')
    expect(s.readScopes).toEqual(['private', 'shared'])
    expect(s.tenantSlug).toBe('default')
    expect(s.tenantDisplayName).toBe('Default Tenant')
    expect(s.apiKeyId).toBe('key-server-admin')
    // Backward-compat deriveds unchanged.
    expect(s.active).toBe(true)
    expect(s.admin).toBe(true)
    expect(s.label).toBe('root-operator')
  })

  it('derives the tenant-admin tier (role=owner, admin=false) without cross-tenant caps', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(jsonResponse(200, WHOAMI_TENANT_ADMIN)))
    const s = new Session()
    await s.login(TEST_KEY)

    expect(s.tier).toBe('tenant-admin')
    expect(s.caps.viewCorpus).toBe(true)
    expect(s.caps.viewTenants).toBe(false)
    expect(s.caps.manageTenantKeys).toBe(true)
    expect(s.caps.manageMembers).toBe(true)
    expect(s.caps.viewTenantBackends).toBe(true)
    expect(s.caps.viewOpsSurfaces).toBe(true)
    expect(s.role).toBe('owner')
    expect(s.tenantId).toBe('tenant-acme')
    expect(s.homeScope).toBe('acme')
    expect(s.readScopes).toEqual(['acme', 'shared'])
    expect(s.tenantSlug).toBe('acme')
    expect(s.tenantDisplayName).toBe('ACME Corp')
    expect(s.apiKeyId).toBe('key-tenant-admin')
    // admin flag is the server-global one — false for a tenant-admin.
    expect(s.admin).toBe(false)
    expect(s.label).toBe('acme-owner')
  })

  it('derives the member tier with corpus-only caps', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(jsonResponse(200, WHOAMI_MEMBER)))
    const s = new Session()
    await s.login(TEST_KEY)

    expect(s.tier).toBe('member')
    expect(s.caps.viewCorpus).toBe(true)
    expect(s.caps.viewTenants).toBe(false)
    expect(s.caps.manageTenantKeys).toBe(false)
    expect(s.caps.manageMembers).toBe(false)
    expect(s.caps.viewTenantBackends).toBe(false)
    expect(s.caps.viewOpsSurfaces).toBe(false)
    expect(s.role).toBe('member')
    expect(s.tenantId).toBe('tenant-acme')
    expect(s.homeScope).toBe('acme')
    expect(s.readScopes).toEqual(['acme'])
    expect(s.admin).toBe(false)
    expect(s.label).toBe('acme-member')
  })

  it('reverts to tier=loading after logout', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(jsonResponse(200, WHOAMI_MEMBER)))
    const s = new Session()
    await s.login(TEST_KEY)
    expect(s.tier).toBe('member')

    s.logout()
    expect(s.tier).toBe('loading')
    expect(s.caps.viewCorpus).toBe(false)
    expect(s.role).toBeNull()
  })
})
