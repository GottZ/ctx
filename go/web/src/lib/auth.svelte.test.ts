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
