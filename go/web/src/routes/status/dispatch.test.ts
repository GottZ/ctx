// Dispatch presentation + coarsening-gate pins (inference-scheduler MW12b). The
// teeth: the tenant occupancy view must render a COARSE depth bucket, never an
// exact foreign waitQ count (E-A5-6b / F-B3). depthLabel maps only the three
// known buckets and must NEVER echo a raw value — a regression that returned the
// input (so a numeric depth would render as an exact count) turns the coarsening
// assertion red. tenantForeignFields pins the structural side: the tenant target
// shape carries no fair_key / foreign-count field.

import { describe, expect, it } from 'vitest'
import type { DispatchTenantTarget } from '../../lib/api/types'
import { depthClass, depthLabel, fmtMs, fmtTokens, tenantForeignFields } from './dispatch'

describe('depthLabel — coarsening gate', () => {
  it('maps the three coarse buckets to fixed labels', () => {
    expect(depthLabel('leer')).toBe('idle')
    expect(depthLabel('niedrig')).toBe('low')
    expect(depthLabel('hoch')).toBe('high')
  })

  it('NEVER echoes a raw value — a numeric-looking depth is not shown as a count', () => {
    // The whole point of coarsening: a tenant must not see an exact foreign
    // waitQ number. If a regression made depthLabel echo its input, this raw
    // "7" would render as an exact count. The closed-set map forbids it.
    expect(depthLabel('7')).toBe('—')
    expect(depthLabel('anything-else')).toBe('—')
  })

  it('the displayed label is always from the closed presentation set', () => {
    for (const d of ['leer', 'niedrig', 'hoch', '7', '']) {
      expect(['idle', 'low', 'high', '—']).toContain(depthLabel(d))
    }
  })
})

describe('depthClass — closed colour set (Q3)', () => {
  it('maps buckets to a closed class set, unknown → unknown', () => {
    expect(depthClass('leer')).toBe('idle')
    expect(depthClass('niedrig')).toBe('low')
    expect(depthClass('hoch')).toBe('high')
    expect(depthClass('42')).toBe('unknown')
  })
})

describe('tenantForeignFields — structural exposure guard (F-B3)', () => {
  it('a well-formed tenant target exposes no foreign principal / count field', () => {
    const t: DispatchTenantTarget = {
      origin: 'http://gpu:8089',
      busy: true,
      depth: 'hoch',
      own_waiting: 2,
      own_inflight: 1,
      own_oldest_wait_ms: 1200,
    }
    expect(tenantForeignFields(t)).toEqual([])
  })

  it('flags any extra field (a widened cross-tenant load oracle)', () => {
    const leaky = {
      origin: 'http://gpu:8089',
      busy: true,
      depth: 'hoch',
      own_waiting: 2,
      own_inflight: 1,
      own_oldest_wait_ms: 1200,
      // a regression that leaked the exact foreign waitQ / a fair key:
      total_waiting: 17,
      fair_key: 'acme',
    } as unknown as DispatchTenantTarget
    expect(tenantForeignFields(leaky).sort()).toEqual(['fair_key', 'total_waiting'])
  })
})

describe('formatting helpers', () => {
  it('fmtMs renders sub-second in ms and above in seconds; null → —', () => {
    expect(fmtMs(null)).toBe('—')
    expect(fmtMs(340)).toBe('340ms')
    expect(fmtMs(1500)).toBe('1.5s')
  })

  it('fmtTokens groups thousands; null → —', () => {
    expect(fmtTokens(null)).toBe('—')
    expect(fmtTokens(1234567)).toBe('1,234,567')
  })
})
