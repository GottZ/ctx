// ProfilesModel gates (092, U01-W6): load/error, per-profile busy-gating, the
// probe→confirm→write blackout flow (probeActivate returns the target-state
// impact WITHOUT writing; setActive writes + reloads), and CRUD reload + error
// surfacing. DOM-free — the injectable api mirrors the real client shapes.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../../lib/api'
import type {
  DisableProfileImpact,
  DisableProfileListResponse,
  DisableProfileMutateResponse,
  DisableProfileToggleResponse,
  DisableProfileView,
} from '../../../lib/api/profiles'
import { ProfilesModel, profileKey } from './profiles.svelte'

function impact(over: Partial<DisableProfileImpact> = {}): DisableProfileImpact {
  return { backends: [], roles_affected: [], roles_blacked_out: [], ...over }
}

function view(p: Partial<DisableProfileView> & Pick<DisableProfileView, 'name'>): DisableProfileView {
  return {
    scope: '_global',
    label: '',
    description: '',
    active: false,
    reserved: false,
    impact: impact(),
    ...p,
  }
}

interface Call {
  m: 'list' | 'toggle' | 'create' | 'update' | 'del'
  name?: string
  scope?: string
  active?: boolean
  opts?: { confirm?: boolean; dryRun?: boolean }
}

function fakeApi(
  initial: DisableProfileView[],
  opts: {
    fail?: Partial<Record<Call['m'], ApiError>>
    dryImpact?: DisableProfileImpact
  } = {},
) {
  const calls: Call[] = []
  let current = initial
  return {
    calls,
    setList: (l: DisableProfileView[]) => (current = l),
    list: (): Promise<DisableProfileListResponse> => {
      calls.push({ m: 'list' })
      if (opts.fail?.list) return Promise.reject(opts.fail.list)
      return Promise.resolve({ success: true as const, profiles: current })
    },
    toggle: (
      name: string,
      scope: string,
      active: boolean,
      o: { confirm?: boolean; dryRun?: boolean } = {},
    ): Promise<DisableProfileToggleResponse> => {
      calls.push({ m: 'toggle', name, scope, active, opts: o })
      if (opts.fail?.toggle) return Promise.reject(opts.fail.toggle)
      return Promise.resolve({
        success: true as const,
        profile: { name, scope, label: '', description: '', active, reserved: false },
        impact: o.dryRun ? (opts.dryImpact ?? impact()) : impact(),
        as_of: '2026-07-07T00:00:00Z',
        note: 'ok',
        ...(o.dryRun ? { dry_run: true } : {}),
      })
    },
    create: (): Promise<DisableProfileMutateResponse> => {
      calls.push({ m: 'create' })
      if (opts.fail?.create) return Promise.reject(opts.fail.create)
      return Promise.resolve({
        success: true as const,
        profile: view({ name: 'new' }),
        as_of: '2026-07-07T00:00:00Z',
      })
    },
    update: (): Promise<DisableProfileMutateResponse> => {
      calls.push({ m: 'update' })
      if (opts.fail?.update) return Promise.reject(opts.fail.update)
      return Promise.resolve({
        success: true as const,
        profile: view({ name: 'x' }),
        as_of: '2026-07-07T00:00:00Z',
      })
    },
    del: () => {
      calls.push({ m: 'del' })
      if (opts.fail?.del) return Promise.reject(opts.fail.del)
      return Promise.resolve({ success: true as const, deleted: 'x', as_of: '2026-07-07T00:00:00Z' })
    },
  }
}

describe('ProfilesModel load', () => {
  it('populates and reaches ready', async () => {
    const api = fakeApi([view({ name: 'eject' })])
    const m = new ProfilesModel(api)
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.profiles).toHaveLength(1)
  })

  it('surfaces a load error', async () => {
    const api = fakeApi([], { fail: { list: new ApiError(403, 'forbidden', 'admin key required') } })
    const m = new ProfilesModel(api)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError?.message).toBe('admin key required')
  })
})

describe('ProfilesModel blackout probe', () => {
  it('probeActivate returns the target-state impact WITHOUT a write', async () => {
    const api = fakeApi([view({ name: 'eject' })], {
      dryImpact: impact({ roles_blacked_out: ['rerank'], roles_affected: ['chat', 'rerank'] }),
    })
    const m = new ProfilesModel(api)
    const imp = await m.probeActivate('eject', '_global')
    expect(imp.roles_blacked_out).toEqual(['rerank'])
    // exactly one toggle call, and it was a dry-run for active=true — NO real write.
    const toggles = api.calls.filter((c) => c.m === 'toggle')
    expect(toggles).toHaveLength(1)
    expect(toggles[0]).toMatchObject({ active: true, opts: { dryRun: true } })
    expect(api.calls.some((c) => c.m === 'list')).toBe(false)
    expect(m.busyKey).toBeNull()
  })

  it('setActive with confirm sends confirm_role_blackout and reloads', async () => {
    const api = fakeApi([view({ name: 'eject' })])
    const m = new ProfilesModel(api)
    await m.setActive('eject', '_global', true, true)
    const toggle = api.calls.find((c) => c.m === 'toggle')
    expect(toggle).toMatchObject({ active: true, opts: { confirm: true } })
    expect(api.calls.some((c) => c.m === 'list')).toBe(true) // reload-after-mutation
    expect(m.busyKey).toBeNull()
  })
})

describe('ProfilesModel busy-gating + errors', () => {
  it('clears busyKey after a toggle and surfaces the error on failure', async () => {
    const api = fakeApi([view({ name: 'eject' })], {
      fail: { toggle: new ApiError(422, 'validation', 'blackout') },
    })
    const m = new ProfilesModel(api)
    await expect(m.setActive('eject', '_global', true)).rejects.toBeInstanceOf(ApiError)
    expect(m.actionError).toBe('blackout')
    expect(m.busyKey).toBeNull()
  })

  it('create/update/remove each reload the list', async () => {
    const api = fakeApi([view({ name: 'eject' })])
    const m = new ProfilesModel(api)
    await m.create({ name: 'gpu-wartung', label: 'GPU-Wartung' })
    await m.update('gpu-wartung', '_global', { description: 'x' })
    await m.remove('gpu-wartung', '_global')
    expect(api.calls.filter((c) => c.m === 'list')).toHaveLength(3)
  })

  it('remove surfaces the reserved-delete 422 as an actionError', async () => {
    const api = fakeApi([view({ name: 'eject', reserved: true })], {
      fail: { del: new ApiError(422, 'validation', 'Reserviertes Profil — nicht löschbar') },
    })
    const m = new ProfilesModel(api)
    await expect(m.remove('eject', '_global')).rejects.toBeInstanceOf(ApiError)
    expect(m.actionError).toContain('Reserviert')
  })
})

describe('profileKey', () => {
  it('is unique per (scope,name)', () => {
    expect(profileKey('_global', 'eject')).not.toBe(profileKey('acme:home', 'eject'))
  })
})
