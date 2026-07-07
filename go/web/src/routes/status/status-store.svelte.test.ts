// StatusStore gates (U01-W7, design/01 §4.5). The load-bearing one is the
// out-of-order guard: a mutation merge raises the as_of floor, and a LATE older
// status answer (SSE frame OR poll) is then discarded so the freshly-toggled
// state survives. RED against Ist: the pre-W7 StatusPage merged every frame
// VORBEHALTLOS (StatusPage.svelte:36-41) — the `applies a stale frame` shape
// below is exactly that overwrite, proven to change state without the floor.

import { describe, expect, it } from 'vitest'
import type { StatusResponse, StatusEvent, StatusProfile } from '../../lib/api/types'
import type { DreamModeResponse } from '../../lib/api/status'
import { StatusStore, asOfMs, acceptAsOf } from './status-store.svelte'

const T0 = '2026-07-07T12:00:00Z'
const T1 = '2026-07-07T12:00:01Z'
const T2 = '2026-07-07T12:00:02Z'
const T3 = '2026-07-07T12:00:03Z'

function profile(over: Partial<StatusProfile> = {}): StatusProfile {
  return { name: 'eject', scope: '_global', label: 'Eject-Modus', active: false, member_count: 2, ...over }
}

function resp(asOf: string, profiles: StatusProfile[]): StatusResponse {
  return {
    success: true,
    as_of: asOf,
    health: { status: 'ok', services: {} },
    backends: [],
    dream: { mode: 'on', throttle_interval_s: 0, pickable_now: 0, in_cooldown: 0, never_dreamed: 0, awaiting_embed: 0, incoming_1h: 0, incoming_6h: 0, next_pending_at: null, last_cycle_at: null },
    llm_24h: [],
    llm_24h_complete: true,
    profiles,
    activity: null,
  }
}

function frame(asOf: string, profiles: StatusProfile[]): StatusEvent {
  const r = resp(asOf, profiles)
  return { as_of: r.as_of, health: r.health, dream: r.dream, llm_24h: r.llm_24h, llm_24h_complete: r.llm_24h_complete, profiles, activity: null }
}

describe('as_of floor helpers', () => {
  it('acceptAsOf rejects older, admits equal/newer, admits unparseable', () => {
    const floor = asOfMs(T2)
    expect(acceptAsOf(T1, floor)).toBe(false)
    expect(acceptAsOf(T2, floor)).toBe(true)
    expect(acceptAsOf(T3, floor)).toBe(true)
    expect(acceptAsOf('', floor)).toBe(true) // unknown freshness → never assume stale
  })
})

describe('StatusStore out-of-order guard', () => {
  it('a late older status frame does NOT overwrite a just-toggled profile', () => {
    const s = new StatusStore(() => Promise.resolve(resp(T0, [profile({ active: false })])))
    // seed as if a first frame landed at T0
    s.applyStatusFrame(frame(T0, [profile({ active: false })]))
    expect(s.data?.profiles?.[0].active).toBe(false)

    // mutation: eject toggled ON, answer stamped T2 → splice + floor rises to T2
    s.spliceProfile({ name: 'eject', scope: '_global', label: 'Eject-Modus', description: '', active: true, reserved: true }, T2)
    expect(s.data?.profiles?.[0].active).toBe(true)
    expect(s.floorMs).toBe(asOfMs(T2))

    // a LATE frame from before the toggle (T1 < T2) still says active:false
    s.applyStatusFrame(frame(T1, [profile({ active: false })]))
    // DISCARDED — state stays fresh
    expect(s.data?.profiles?.[0].active).toBe(true)

    // a newer frame (T3 > floor) is applied normally
    s.applyStatusFrame(frame(T3, [profile({ active: true })]))
    expect(s.data?.profiles?.[0].active).toBe(true)
    expect(s.floorMs).toBe(asOfMs(T3))
  })

  it('a stale poll answer is discarded but a fresh one lands', async () => {
    let next = resp(T0, [profile({ active: false })])
    const s = new StatusStore(() => Promise.resolve(next))
    await s.load() // first load always lands
    expect(s.data?.as_of).toBe(T0)

    s.spliceProfile({ name: 'eject', scope: '_global', label: 'Eject-Modus', description: '', active: true, reserved: true }, T2)

    next = resp(T1, [profile({ active: false })]) // late poll, older than floor
    await s.load()
    expect(s.data?.profiles?.[0].active).toBe(true) // held state survived
    expect(s.data?.as_of).toBe(T0) // stale poll not applied

    next = resp(T3, [profile({ active: true })]) // fresh poll
    await s.load()
    expect(s.data?.as_of).toBe(T3)
  })
})

describe('StatusStore backends buffering + dream merge', () => {
  it('buffers a backends frame that arrives before the first status', () => {
    const s = new StatusStore()
    s.applyBackends([{ id: 'b1', name: 'n', trust: 'full-trust', locality: 'local', roles: ['chat'], priority: 1, enabled: true, effective_state: 'active', cooldown_remaining_s: 0, consecutive_fails: 0 }])
    expect(s.data).toBeNull()
    s.applyStatusFrame(frame(T0, [profile()]))
    expect(s.data?.backends).toHaveLength(1)
  })

  it('applyDream merges mode + throttle and raises the floor', () => {
    const s = new StatusStore()
    s.applyStatusFrame(frame(T0, [profile()]))
    const r: DreamModeResponse = { success: true, mode: 'throttled', interval: 20, as_of: T2 }
    s.applyDream(r)
    expect(s.data?.dream.mode).toBe('throttled')
    expect(s.data?.dream.throttle_interval_s).toBe(20)
    expect(s.floorMs).toBe(asOfMs(T2))
  })
})
