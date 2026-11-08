// W05-08 — key rotation with the reason question and "End grace period now"
// (design/05 §4.6 RotateDialog + key row "superseded", §5 B-20; RC-D C5):
//   - the preselected reason is "Leak — revoke now" = grace 0 (red against a
//     dialog that preselects the default grace window);
//   - grace 0 rides the wire (red against dropping a falsy grace);
//   - in status `rotating` only the PREDECESSOR carries the end-grace control,
//     and its target is the predecessor's own key id — red against the
//     consumer id or the successor's id;
//   - "Rotate key" mirrors the granting rule (server-admin; tenant-admin with
//     mintModelKeys on an active app consumer; never a ctx-peer below
//     server-admin).

import { describe, expect, it } from 'vitest'
import { MAX_ROTATION_GRACE_S, canRotate, defaultRotateChoice, endGraceTarget, graceOptions, rotateSpec } from './rotate'
import { consumerOf, key, rotatingConsumer } from './forms.fixtures'

describe('W05-08 reason and grace', () => {
  it('W05-08 gate: "Leak — revoke now" (grace 0) is preselected', () => {
    expect(defaultRotateChoice(), 'W05-08: the preselection is the leak case with grace 0').toEqual({ reason: 'leak', graceS: 0 })
  })

  it('W05-08 gate: grace 0 rides the wire (never dropped as falsy)', () => {
    const spec = rotateSpec('c-ci', { reason: 'leak', graceS: 0 })
    expect(spec, 'W05-08: the leak payload').toEqual({ consumer_id: 'c-ci', grace: 0 })
    expect(spec !== null && Object.prototype.hasOwnProperty.call(spec, 'grace'), 'grace key present').toBe(true)
  })

  it('W05-08: a planned change sends its window in seconds; the options are 1 h / 24 h / 7 d', () => {
    const { options, preselected } = graceOptions(86_400)
    expect(options.map((o) => o.seconds), 'W05-08: grace options').toEqual([3_600, 86_400, MAX_ROTATION_GRACE_S])
    expect(preselected, 'planned change preselects rotation_grace_s').toBe(86_400)
    expect(rotateSpec('c-ci', { reason: 'planned', graceS: 86_400 })).toEqual({ consumer_id: 'c-ci', grace: 86_400 })
  })

  it('W05-08: the planned-change preselection is clamped to 7 d; absent server value ⇒ the shortest window', () => {
    expect(graceOptions(30 * 86_400).preselected, 'clamped to the 7-d ceiling').toBe(MAX_ROTATION_GRACE_S)
    expect(graceOptions(null).preselected, 'no rotation_grace_s ⇒ the shortest window (fail-closed)').toBe(3_600)
    expect(MAX_ROTATION_GRACE_S).toBe(604_800)
  })
})

describe('W05-08 end grace period now', () => {
  it('W05-08 gate: in status rotating only the predecessor is the target, by ITS key id', () => {
    const c = rotatingConsumer()
    const t = endGraceTarget(c)
    expect(t?.id, 'W05-08 gate: the predecessor key id').toBe('k-pred-Z9y8')
    expect(t?.id, 'never the consumer id').not.toBe(c.id)
    expect(t?.id, 'never the successor').not.toBe('k-succ-A1b2')
    expect(t?.status).toBe('superseded')
  })

  it('W05-08: the order of keys in the answer does not move the target', () => {
    const c = rotatingConsumer()
    const flipped = { ...c, keys: [...c.keys].reverse() }
    expect(endGraceTarget(flipped)?.id, 'predecessor regardless of order').toBe('k-pred-Z9y8')
  })

  it('W05-08: an ambiguous answer (two superseded keys) yields no target — never a guess (review RV-C)', () => {
    const c = rotatingConsumer()
    const two = { ...c, keys: c.keys.map((k) => ({ ...k, status: 'superseded' as const })) }
    expect(endGraceTarget(c)?.id, 'positive control').toBe('k-pred-Z9y8')
    expect(endGraceTarget(two), 'B-20: no control rather than a guessed key').toBeNull()
  })

  it('W05-08: no target outside status rotating', () => {
    expect(endGraceTarget(rotatingConsumer())?.id, 'positive control').toBe('k-pred-Z9y8')
    expect(endGraceTarget(consumerOf('one')), 'one live key: nothing to end').toBeNull()
    expect(endGraceTarget(consumerOf('none', { status: 'no_key', keys: [] })), 'no key').toBeNull()
    const expiring = consumerOf('exp', { status: 'expiring', keys: [key('k-e', 'expiring', '2026-06-20T09:00:00Z')] })
    expect(endGraceTarget(expiring), 'an expiring single key is no predecessor').toBeNull()
  })
})

describe('W05-08 rotate control', () => {
  it('W05-08: rotate mirrors the granting rule', () => {
    const app = consumerOf('app')
    const peer = consumerOf('peer', { kind: 'ctx-peer' })
    const off = consumerOf('off', { active: false, status: 'inactive', keys: [] })
    expect(canRotate(app, { serverAdmin: false, mintModelKeys: true }), 'positive control: tenant-admin with self-service').toBe(true)
    expect(canRotate(app, { serverAdmin: false, mintModelKeys: false }), 'no self-service ⇒ no rotate').toBe(false)
    expect(canRotate(peer, { serverAdmin: false, mintModelKeys: true }), 'W05-08: never a peer below server-admin').toBe(false)
    expect(canRotate(peer, { serverAdmin: true, mintModelKeys: true }), 'server-admin rotates a peer').toBe(true)
    expect(canRotate(off, { serverAdmin: true, mintModelKeys: true }), 'an inactive consumer cannot rotate (409)').toBe(false)
  })
})
