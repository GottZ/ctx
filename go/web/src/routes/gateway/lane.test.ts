// W05-11 — the lane field of a model consumer (design/05 §4.5 row "Lane",
// §4.8, §5 B-4, V-03-a; RC-D C5). Gates:
//   - the options equal the CLOSED TS type ExternalLane = ['urgent','bulk'] —
//     red against a select that derives its options from the dispatch class
//     list (DispatchBucket.class: interactive / external / external_bulk);
//   - V-03-a mirror: interactive / background / any other value is no lane;
//   - field tier: a tenant-admin may only lower (urgent disabled on a bulk
//     consumer), a server-admin picks both, a ctx-peer row is locked below
//     server-admin.

import { describe, expect, expectTypeOf, it } from 'vitest'
import type { DispatchBucket, ExternalLane } from '../../lib/api/types'
import { LANE_OPTIONS, isExternalLane, laneOptions, type LaneOption } from './lane'

const DISPATCH_CLASSES: DispatchBucket['class'][] = ['interactive', 'external', 'external_bulk']

const byValue = (opts: LaneOption[]): Record<string, LaneOption> => Object.fromEntries(opts.map((o) => [o.value, o]))

describe('W05-11 lane vocabulary (V-03-a)', () => {
  it('W05-11 gate: the lane options are exactly the closed ExternalLane vocabulary, urgent first', () => {
    expectTypeOf<(typeof LANE_OPTIONS)[number]>().toEqualTypeOf<ExternalLane>()
    expect([...LANE_OPTIONS], 'W05-11: options == [urgent, bulk] from the closed TS type ExternalLane').toEqual(['urgent', 'bulk'])
  })

  it('W05-11 gate: no dispatch class is a lane (red against a derivation from DispatchBucket.class)', () => {
    // positive control first: both lanes are lanes
    expect(isExternalLane('urgent'), 'positive control: urgent is a lane').toBe(true)
    expect(isExternalLane('bulk'), 'positive control: bulk is a lane').toBe(true)
    for (const c of DISPATCH_CLASSES) {
      expect(isExternalLane(c), `dispatch class "${c}" must not be a lane`).toBe(false)
      expect(LANE_OPTIONS as readonly string[], `dispatch class "${c}" must not be offered`).not.toContain(c)
    }
  })

  it('V-03-a: interactive / background and every other value are not representable', () => {
    expect(isExternalLane('urgent'), 'positive control: urgent is a lane').toBe(true)
    for (const v of ['interactive', 'background', 'Urgent', 'BULK', ' bulk', '', null, undefined, 0, {}]) {
      expect(isExternalLane(v), `V-03-a: ${JSON.stringify(v)} is no lane`).toBe(false)
    }
  })
})

describe('W05-11 lane field tier (design/05 §4.5, V-01-a mirror)', () => {
  it('W05-11 gate: a tenant-admin on a bulk consumer sees urgent disabled ("server-admin only") and bulk selectable', () => {
    const o = byValue(laneOptions(false, 'bulk', false))
    expect(Object.keys(o), 'W05-11: both lanes are listed').toEqual(['urgent', 'bulk'])
    expect(o.urgent?.disabled, 'W05-11: tenant-admin cannot raise bulk → urgent').toBe(true)
    expect(o.urgent?.note, 'W05-11: the disabled option says why').toBe('server-admin only')
    expect(o.bulk?.disabled, 'W05-11: the current lane stays selectable').toBe(false)
  })

  it('W05-11: a tenant-admin on an urgent consumer may lower it to bulk', () => {
    const o = byValue(laneOptions(false, 'urgent', false))
    expect(o.urgent?.disabled, 'urgent stays selectable (current value)').toBe(false)
    expect(o.bulk?.disabled, 'W05-11: lowering urgent → bulk is allowed to a tenant-admin').toBe(false)
  })

  it('W05-11: a server-admin picks both lanes on any consumer', () => {
    for (const cur of ['bulk', 'urgent']) {
      const opts = laneOptions(true, cur, false)
      expect(opts.map((x) => x.value), `server-admin on ${cur}: both lanes`).toEqual(['urgent', 'bulk'])
      expect(opts.every((x) => !x.disabled), `server-admin on ${cur}: nothing disabled`).toBe(true)
    }
    // the server-admin may raise a peer too (Row-Tier V-01-b only binds below server-admin)
    expect(laneOptions(true, 'bulk', true).every((x) => !x.disabled), 'server-admin on a ctx-peer: nothing disabled').toBe(true)
  })

  it('W05-11: a ctx-peer row is locked for a tenant-admin ("managed by the server-admin")', () => {
    const opts = laneOptions(false, 'urgent', true)
    expect(opts.map((x) => x.value), 'both lanes are listed (read-only)').toEqual(['urgent', 'bulk'])
    for (const x of opts) {
      expect(x.disabled, `peer lane ${x.value} locked`).toBe(true)
      expect(x.note).toBe('managed by the server-admin')
    }
  })

  it('W05-11: an unknown server value grants nothing — a tenant-admin may then only pick bulk (the default lane, lowering)', () => {
    const o = byValue(laneOptions(false, 'interactive', false))
    expect(o.bulk?.disabled, 'bulk (the default lane) is always a lowering').toBe(false)
    expect(o.urgent?.disabled, 'urgent is a raise from an unknown value').toBe(true)
  })
})
