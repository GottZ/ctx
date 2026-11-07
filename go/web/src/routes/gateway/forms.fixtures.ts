// Shared fixtures of the RC-D C5 form tests (W05-07 / W05-08 / W05-11 /
// W05-13) — pure data, no DOM, no clock.

import type { ConsumerView, KeyView, ModelAliasView } from '../../lib/api/types'

export function key(id: string, status: KeyView['status'], created: string, p: Partial<KeyView> = {}): KeyView {
  return {
    id,
    hint: `ctxm_…${id.slice(-4)}`,
    status,
    created_at: created,
    expires_at: null,
    revoked_at: null,
    rotated_from: null,
    last_used_at: null,
    ...p,
  }
}

export function consumerOf(label: string, p: Partial<ConsumerView> = {}): ConsumerView {
  return {
    id: `c-${label}`,
    tenant_id: 't-a',
    tenant_slug: 'acme',
    label,
    description: '',
    kind: 'app',
    tenant_scope: 'home',
    all_models: false,
    models: ['m-chat'],
    prompt_sensitivity: 'credentials',
    fair_weight: 1,
    admission_lane: 'bulk',
    allowed_cidrs: null,
    allow_plaintext: false,
    active: true,
    status: 'active',
    last_used_at: null,
    created_at: '2026-06-20T09:00:00Z',
    created_by_label: null,
    keys: [key(`k-${label}-0`, 'active', '2026-06-20T09:00:00Z')],
    keys_total: 1,
    peer: null,
    ...p,
  }
}

/** A consumer in status `rotating`: the successor (newer, active) and the predecessor (older, superseded). */
export function rotatingConsumer(label = 'rot'): ConsumerView {
  return consumerOf(label, {
    status: 'rotating',
    keys: [
      key('k-succ-A1b2', 'active', '2026-06-28T09:00:00Z', { rotated_from: 'k-pred-Z9y8' }),
      key('k-pred-Z9y8', 'superseded', '2026-06-18T09:00:00Z', { expires_at: '2026-06-29T21:00:00Z' }),
    ],
    keys_total: 2,
  })
}

export function aliasOf(id: string, scope: string, name: string, p: Partial<ModelAliasView> = {}): ModelAliasView {
  return {
    id,
    scope,
    name,
    kind: 'chat',
    role: 'chat',
    sensitivity: 'credentials',
    policy: {},
    description: '',
    enabled: true,
    created_at: '2026-06-01T00:00:00Z',
    updated_at: '2026-06-01T00:00:00Z',
    writable: scope !== '_global',
    ...p,
  }
}
