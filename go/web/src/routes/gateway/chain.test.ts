// chain.ts gates (RC-D C6 S1, W05-09 — design/05 §7 "Audit zeigt Rotation
// als Kette (rotated_from)"). A rotation is ONE chain edge: the new key names
// the key it was rotated from, and the audit trail shows the rotate row of
// the new key with the predecessor's hint. Red against the S1 stubs ([]).

import { describe, expect, it } from 'vitest'
import type { ConsumerAuditRow, KeyView } from '../../lib/api/types'
import { auditEntries, keyChain } from './chain'

function key(id: string, hint: string, status: KeyView['status'], created: string, from: string | null): KeyView {
  return {
    id,
    hint,
    status,
    created_at: created,
    expires_at: null,
    revoked_at: status === 'revoked' ? created : null,
    rotated_from: from,
    last_used_at: null,
  }
}

// k3 ← k2 ← k1 (k3 newest; k2 in grace = superseded; k1 revoked by the first rotation)
const K1 = key('k1', 'ctxm_…AAA1', 'revoked', '2026-06-01T10:00:00Z', null)
const K2 = key('k2', 'ctxm_…BBB2', 'superseded', '2026-06-10T10:00:00Z', 'k1')
const K3 = key('k3', 'ctxm_…CCC3', 'active', '2026-06-20T10:00:00Z', 'k2')

function audit(id: string, type: string, entity: string, action: string, at: string, oldV: unknown, newV: unknown): ConsumerAuditRow {
  return {
    entity_type: type,
    id,
    entity_key: entity,
    scope: '',
    action,
    old_value: oldV,
    new_value: newV,
    api_key_id: null,
    actor_label: 'ops-key',
    metadata: { via: 'manage' },
    created_at: at,
  }
}

describe('keyChain (W05-09: credential chain via rotated_from)', () => {
  it('orders the chain newest first and links predecessor + successor', () => {
    // input order deliberately scrambled — the chain follows rotated_from, not the array
    const chain = keyChain([K1, K3, K2])
    expect(chain.map((l) => l.key.id), 'gate W05-09: chain newest → oldest along rotated_from').toEqual(['k3', 'k2', 'k1'])
    expect(chain[0]).toMatchObject({ predecessorId: 'k2', successorId: null, truncated: false })
    expect(chain[1]).toMatchObject({ predecessorId: 'k1', successorId: 'k3', truncated: false })
    expect(chain[2]).toMatchObject({ predecessorId: null, successorId: 'k2', truncated: false })
  })

  it('marks a link whose rotated_from lies outside the loaded keys as truncated (keys_truncated cut)', () => {
    const chain = keyChain([K3, K2])
    expect(chain.map((l) => l.key.id)).toEqual(['k3', 'k2'])
    expect(chain[1], 'gate W05-09: a cut chain is marked, not silently re-rooted').toMatchObject({
      predecessorId: 'k1',
      truncated: true,
    })
  })

  it('a single key without rotated_from is a one-link chain', () => {
    expect(keyChain([K1])).toEqual([{ key: K1, predecessorId: null, successorId: null, truncated: false }])
  })
})

describe('auditEntries (W05-09: audit trail as chain)', () => {
  const rows: ConsumerAuditRow[] = [
    audit('a4', 'model_key', 'k3', 'create', '2026-06-20T10:00:00Z', null, { status: 'active', rotated_from: 'k2' }),
    audit('a3', 'model_consumer', 'c1', 'update', '2026-06-15T09:00:00Z', { label: 'old', models: [] }, { label: 'new', models: ['m1'] }),
    audit('a2', 'model_key', 'k2', 'create', '2026-06-10T10:00:00Z', null, { status: 'active', rotated_from: 'k1' }),
    audit('a1', 'model_key', 'k1', 'update', '2026-06-10T10:00:01Z', { status: 'active' }, { status: 'revoked' }),
  ]

  it('a key row of a rotated key carries its own hint and the predecessor hint (one edge, not two events)', () => {
    const entries = auditEntries(rows, [K3, K2, K1])
    const rot = entries.find((e) => e.id === 'a4')
    expect(rot, 'gate W05-09: the rotate row of k3 is shown').toBeDefined()
    expect(rot).toMatchObject({ entity: 'key', keyHint: 'ctxm_…CCC3', rotatedFromHint: 'ctxm_…BBB2' })
    expect(entries.find((e) => e.id === 'a2')).toMatchObject({ keyHint: 'ctxm_…BBB2', rotatedFromHint: 'ctxm_…AAA1' })
  })

  it('a consumer row lists the changed field NAMES only (never the raw values)', () => {
    const entries = auditEntries(rows, [K3, K2, K1])
    const pol = entries.find((e) => e.id === 'a3')
    expect(pol, 'gate W05-09: policy change in the trail').toMatchObject({
      entity: 'consumer',
      keyHint: null,
      rotatedFromHint: null,
      fields: ['label', 'models'],
      actor: 'ops-key',
      via: 'manage',
    })
    expect(JSON.stringify(pol), 'no raw old/new value reaches the display entry').not.toMatch(/"old"|"new"|m1/)
  })

  it('keeps the server order (newest first) and drops nothing', () => {
    expect(auditEntries(rows, [K3, K2, K1]).map((e) => e.id), 'gate W05-09: trail complete + ordered').toEqual([
      'a4',
      'a3',
      'a2',
      'a1',
    ])
  })
})
