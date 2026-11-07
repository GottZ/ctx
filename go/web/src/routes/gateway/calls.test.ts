// calls.ts gates (RC-D C6 S1, W05-09 — design/05 §7: "tenant-admin: kein
// Link auf /api/llmlog/{id} — rot gegen Link"; positive control "geseedeter
// Call sichtbar"). The prohibition half is fail-closed in the stub; the
// positive controls (the seeded call is a row, server-admin gets its detail)
// make the suite red until S2. The mutant "detail for every tier" is the
// red proof of the prohibition (overlay skeleton mut/w05-09-llmlog-link.json).

import { describe, expect, it } from 'vitest'
import type { ConsumerCallEntry } from '../../lib/api/types'
import { callDetailAllowed, callRows } from './calls'

const OK: ConsumerCallEntry = {
  id: 'L-0001',
  created_at: '2026-06-29T11:58:00Z',
  pipeline: 'gateway-chat',
  model: 'chat',
  backend: 'spark-chat',
  consumer_id: 'c1',
  duration_ms: 812,
  error: null,
  prompt_tokens: 120,
  completion_tokens: 48,
  cost_usd: null,
  queue_wait_ms: 3,
  dispatch_class: null,
  dispatch_abort: null,
}

const FAILED: ConsumerCallEntry = {
  ...OK,
  id: 'L-0002',
  duration_ms: null,
  prompt_tokens: null,
  completion_tokens: null,
  // S2: the Go shape is llmlogError {class, detail} (C2 keeps it) — the class carries the status (http_502)
  error: { class: 'http_502', detail: 'unexpected status 502: provider said: <prompt fragment SECRET-PROMPT>' },
}

describe('callDetailAllowed (W05-09: no llmlog detail below server-admin)', () => {
  it('server-admin may open a call detail (positive control)', () => {
    expect(callDetailAllowed('server-admin'), 'gate W05-09: server-admin reaches /api/llmlog/{id}').toBe(true)
  })
  it('tenant-admin, member and loading never get a detail link', () => {
    for (const t of ['tenant-admin', 'member', 'loading'] as const) expect(callDetailAllowed(t), t).toBe(false)
  })
})

describe('callRows (W05-09: last calls, body-free)', () => {
  it('the seeded call is a row (positive control) — tenant-admin row has NO detail id', () => {
    const rows = callRows([OK], 'tenant-admin')
    expect(rows, 'gate W05-09: seeded call visible').toHaveLength(1)
    expect(rows[0]).toMatchObject({ id: 'L-0001', model: 'chat', pipeline: 'gateway-chat', outcome: 'ok' })
    expect(rows[0]?.detailId, 'gate W05-09: tenant-admin — kein Link auf /api/llmlog/{id}').toBeNull()
  })

  it('server-admin rows carry the detail id', () => {
    expect(callRows([OK], 'server-admin')[0]?.detailId, 'gate W05-09: server-admin detail').toBe('L-0001')
  })

  it('an error shows its class only (it carries the status), never the raw detail', () => {
    const [row] = callRows([FAILED], 'tenant-admin')
    expect(row?.outcome, 'gate W05-09: error = class (incl. status)').toBe('http_502')
    expect(JSON.stringify(row), 'no provider body fragment in the row').not.toContain('SECRET-PROMPT')
  })

  it('missing duration / tokens read "not recorded", never 0', () => {
    const [row] = callRows([FAILED], 'server-admin')
    expect(row?.durationText, 'gate null ≠ 0').toBe('not recorded')
    expect(row?.tokensText, 'gate null ≠ 0').toBe('not recorded')
  })
})
