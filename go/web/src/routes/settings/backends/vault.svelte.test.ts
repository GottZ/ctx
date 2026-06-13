// VaultModel gates: load/error, put (create vs rotate) reloads + returns the
// action, delete reloads on success and rethrows the 409-while-referenced.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../../lib/api'
import type { SecretMeta } from '../../../lib/api/types'
import { VaultModel } from './vault.svelte'

interface Call {
  m: 'list' | 'put' | 'del'
  name?: string
  value?: string
}

function fakeApi(initial: SecretMeta[], opts: { putAction?: 'create' | 'rotate'; delFail?: ApiError } = {}) {
  const calls: Call[] = []
  return {
    calls,
    list: () => {
      calls.push({ m: 'list' })
      return Promise.resolve({ success: true as const, secrets: initial })
    },
    put: (name: string, value: string) => {
      calls.push({ m: 'put', name, value })
      return Promise.resolve({ success: true as const, name, action: opts.putAction ?? ('create' as const) })
    },
    del: (name: string) => {
      calls.push({ m: 'del', name })
      return opts.delFail ? Promise.reject(opts.delFail) : Promise.resolve({ success: true as const, name, deleted: true as const })
    },
  }
}

const secret = (name: string): SecretMeta => ({ name, key_version: 1, created_at: 't', referenced_by: [] })

describe('VaultModel', () => {
  it('loads metadata and reaches ready', async () => {
    const m = new VaultModel(fakeApi([secret('or.key')]))
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.has('or.key')).toBe(true)
    expect(m.has('nope')).toBe(false)
  })

  it('surfaces a load error', async () => {
    const api = { list: () => Promise.reject(new ApiError(503, 'server', 'secrets unavailable')), put: () => Promise.reject(new Error()), del: () => Promise.reject(new Error()) }
    const m = new VaultModel(api)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError?.status).toBe(503)
  })

  it('put returns the action and reloads', async () => {
    const api = fakeApi([], { putAction: 'rotate' })
    const m = new VaultModel(api)
    const action = await m.put('or.key', 'sk-secret')
    expect(action).toBe('rotate')
    expect(api.calls.map((c) => c.m)).toEqual(['put', 'list'])
    expect(api.calls[0]).toEqual({ m: 'put', name: 'or.key', value: 'sk-secret' })
    expect(m.busyName).toBeNull()
  })

  it('delete reloads on success', async () => {
    const api = fakeApi([secret('or.key')])
    const m = new VaultModel(api)
    await m.remove('or.key')
    expect(api.calls.map((c) => c.m)).toEqual(['del', 'list'])
  })

  it('delete rethrows the 409-while-referenced', async () => {
    const api = fakeApi([secret('or.key')], { delFail: new ApiError(409, 'conflict', 'referenced by settings') })
    const m = new VaultModel(api)
    await expect(m.remove('or.key')).rejects.toBeInstanceOf(ApiError)
    expect(m.busyName).toBeNull()
  })
})
