// Session gates (design 05 §4.3/§4.4, OAuth R4/05-W5): Key→Cookie-Exchange am
// POST /auth/login, in-memory CSRF-Synchronizer, Cookie-Restore mit EINEM
// stillen /auth/refresh-Fallback und die 401-Interceptor-Verdrahtung. fetch
// ist gestubbt — plain node environment; der rohe Key berührt keinen
// Client-Storage mehr (kein sessionStorage-Stub nötig). Test keys assembled
// at runtime.

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Session, session } from './auth.svelte'
import { apiFetch } from './api'

const TEST_KEY = ['dead', 'beef'].join('') // doc-value, assembled at runtime
const TEST_CSRF = ['c5', '4f', '01'].join('') // doc-value, assembled at runtime

/** Wire-Shape von POST /auth/login (handler/auth_session.go). */
const LOGIN_OK = { success: true, csrf_token: TEST_CSRF }

const WHOAMI_ADMIN = {
  success: true,
  label: 'example-admin',
  home_scope: 'private',
  read_scopes: ['private', 'shared'],
  admin: true,
  csrf_token: TEST_CSRF,
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
  csrf_token: TEST_CSRF,
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
  csrf_token: TEST_CSRF,
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
  csrf_token: TEST_CSRF,
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** Stubbt fetch mit einer geordneten Antwort-Sequenz. */
function stubFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const mock = vi.fn()
  for (const res of responses) mock.mockResolvedValueOnce(res)
  vi.stubGlobal('fetch', mock)
  return mock
}

beforeEach(() => {
  vi.unstubAllGlobals()
  // The module-scope configureApi binds the api client to the singleton —
  // reset it between tests. logout feuert einen best-effort POST /auth/logout,
  // der im node-Env am relativen Pfad scheitert und verschluckt wird.
  session.logout()
  session.notice = null
})

describe('Session.login', () => {
  it('exchanges the key at /auth/login and hydrates whoami over the cookies', async () => {
    const mock = stubFetch(jsonResponse(200, LOGIN_OK), jsonResponse(200, WHOAMI_ADMIN))

    const s = new Session()
    await s.login(`  ${TEST_KEY}  `) // trimmed before use

    expect(s.active).toBe(true)
    expect(s.admin).toBe(true)
    expect(s.label).toBe('example-admin')
    expect(s.csrfToken).toBe(TEST_CSRF)

    const [loginUrl, loginInit] = mock.mock.calls[0] as [string, RequestInit]
    expect(loginUrl).toBe('/auth/login')
    expect(loginInit.method).toBe('POST')
    expect(JSON.parse(loginInit.body as string)).toEqual({ api_key: TEST_KEY })
    // Der rohe Key wandert NUR in den Login-Body — nie in einen Header.
    expect(new Headers(loginInit.headers).get('Authorization')).toBeNull()
    expect(mock.mock.calls[1]?.[0]).toBe('/api/whoami')
  })

  it('throws on a rejected key and stays logged out (no refresh attempt)', async () => {
    const mock = stubFetch(jsonResponse(401, { success: false, error: 'authentication failed' }))
    const s = new Session()
    await expect(s.login(TEST_KEY)).rejects.toMatchObject({ status: 401, code: 'unauthorized' })
    expect(s.active).toBe(false)
    expect(s.csrfToken).toBeNull()
    expect(mock).toHaveBeenCalledTimes(1) // Login besitzt den 401 selbst
  })

  it('reflects admin:false for read-only keys', async () => {
    stubFetch(jsonResponse(200, LOGIN_OK), jsonResponse(200, { ...WHOAMI_ADMIN, admin: false }))
    const s = new Session()
    await s.login(TEST_KEY)
    expect(s.active).toBe(true)
    expect(s.admin).toBe(false)
  })
})

describe('Session.logout', () => {
  it('revokes the server session and clears local state', async () => {
    const mock = stubFetch(
      jsonResponse(200, LOGIN_OK),
      jsonResponse(200, WHOAMI_ADMIN),
      jsonResponse(200, { success: true }), // POST /auth/logout
    )
    const s = new Session()
    await s.login(TEST_KEY)
    s.logout()
    expect(s.active).toBe(false)
    expect(s.csrfToken).toBeNull()
    expect(mock.mock.calls[2]?.[0]).toBe('/auth/logout')
    expect((mock.mock.calls[2]?.[1] as RequestInit).method).toBe('POST')
  })
})

describe('Session.restore', () => {
  it('adopts a living cookie session (whoami 200 with csrf_token)', async () => {
    const mock = stubFetch(jsonResponse(200, WHOAMI_ADMIN))
    const s = new Session()
    await s.restore()
    expect(s.active).toBe(true)
    expect(s.csrfToken).toBe(TEST_CSRF)
    expect(mock.mock.calls[0]?.[0]).toBe('/api/whoami')
  })

  it('rides ONE silent refresh when the access token expired', async () => {
    const mock = stubFetch(
      jsonResponse(401, { success: false, error: 'authentication failed' }), // whoami: Access tot
      jsonResponse(200, { success: true }), // POST /auth/refresh rotiert
      jsonResponse(200, WHOAMI_ADMIN), // whoami über die frischen Cookies
    )
    const s = new Session()
    await s.restore()
    expect(s.active).toBe(true)
    expect(s.csrfToken).toBe(TEST_CSRF)
    expect(mock.mock.calls[1]?.[0]).toBe('/auth/refresh')
  })

  it('stays on the login screen when session AND refresh are dead', async () => {
    const mock = stubFetch(
      jsonResponse(401, { success: false, error: 'authentication failed' }),
      jsonResponse(401, { success: false, error: 'authentication failed' }),
    )
    const s = new Session()
    await s.restore()
    expect(s.active).toBe(false)
    expect(s.csrfToken).toBeNull()
    expect(mock).toHaveBeenCalledTimes(2) // whoami + refresh, KEIN drittes whoami
    // Kein Cookie ↔ abgelaufene Session ist client-seitig ununterscheidbar —
    // ein frischer Besucher bekommt keine Fehlermeldung.
    expect(s.notice).toBeNull()
  })

  it('stays logged out on transient failures (network) without a notice', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('fetch failed')))
    const s = new Session()
    await s.restore()
    expect(s.active).toBe(false)
    expect(s.notice).toBeNull()
    expect(s.restoring).toBe(false)
  })
})

describe('401 interceptor wiring (singleton)', () => {
  it('tears the session down when refresh cannot revive a dead cookie session', async () => {
    stubFetch(
      jsonResponse(200, LOGIN_OK),
      jsonResponse(200, WHOAMI_ADMIN),
      jsonResponse(401, { success: false, error: 'authentication failed' }), // Daten-Request
      jsonResponse(401, { success: false, error: 'authentication failed' }), // Refresh tot
    )

    await session.login(TEST_KEY)
    expect(session.active).toBe(true)

    // Any later call on the dead session → 401 → ein Refresh-Versuch → 401 →
    // interceptor → login screen.
    await expect(apiFetch('/api/manage', { method: 'POST', body: '{}' })).rejects.toMatchObject({
      status: 401,
    })
    expect(session.active).toBe(false)
    expect(session.csrfToken).toBeNull()
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
    stubFetch(jsonResponse(200, LOGIN_OK), jsonResponse(200, WHOAMI_SERVER_ADMIN))
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
    stubFetch(jsonResponse(200, LOGIN_OK), jsonResponse(200, WHOAMI_TENANT_ADMIN))
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
    stubFetch(jsonResponse(200, LOGIN_OK), jsonResponse(200, WHOAMI_MEMBER))
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
    stubFetch(
      jsonResponse(200, LOGIN_OK),
      jsonResponse(200, WHOAMI_MEMBER),
      jsonResponse(200, { success: true }), // POST /auth/logout
    )
    const s = new Session()
    await s.login(TEST_KEY)
    expect(s.tier).toBe('member')

    s.logout()
    expect(s.tier).toBe('loading')
    expect(s.caps.viewCorpus).toBe(false)
    expect(s.role).toBeNull()
  })
})
