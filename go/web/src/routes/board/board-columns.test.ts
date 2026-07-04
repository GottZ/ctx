// Board classification pins (design 04 §4.2/§6.2, wave U07). PURE logic — the
// order gate, the unmapped negative gate and the closed-collapse seed, all
// DOM-free.
//
// The two negative gates the U07 brief demands (RED-then-GREEN, evidence in the
// return):
//   - UNMAPPED: a wire status outside the registry vocabulary must classify as
//     'unmapped', never be lost/misclassified. RED against a hardcoded status→
//     category map: swap classifyColumns for `{open,done}[status] ?? 'open'` and
//     the on_hold assertion below flips 'unmapped'→'open' → this test fails.
//   - ORDER: the classified order must equal the wire order. RED against a sort:
//     add `.sort((a,b)=>a.status.localeCompare(b.status))` inside classifyColumns
//     and the order assertion below fails (the wire order is permuted).

import { describe, expect, it } from 'vitest'
import type { BlockTypeView, BoardColumn } from '../../lib/api/types'
import { classifyColumns, initialCollapsed, vocabFromTypes } from './board-columns'

function col(status: string, count = 0): BoardColumn {
  return { status, count, issues: [], cursor: null }
}

/** A registry with one workflow type: states open/in_progress/review/done,
 * done terminal. Mirrors the builtin issue type (builtin.go). */
function issueTypes(): BlockTypeView[] {
  return [
    {
      id: '77777777-7777-7777-7777-777777777777',
      name: 'issue',
      scope: '_global',
      display_name: 'Issue',
      description: '',
      builtin: true,
      is_default: false,
      source: 'builtin',
      created_at: '2026-07-03T00:00:00Z',
      updated_at: '2026-07-03T00:00:00Z',
      config: {
        v: 1,
        workflow: { states: ['open', 'in_progress', 'review', 'done'], initial: 'open', terminal: ['done'] },
      },
    },
  ]
}

describe('vocabFromTypes', () => {
  it('unions states + terminal across workflow types (non-workflow types ignored)', () => {
    const types = issueTypes()
    types.push({ ...types[0], name: 'knowledge', config: { v: 1 } }) // no workflow → contributes nothing
    const v = vocabFromTypes(types)
    expect([...v.known].sort()).toEqual(['done', 'in_progress', 'open', 'review'])
    expect([...v.terminal]).toEqual(['done'])
  })

  it('a status listed ONLY in terminal is still known (never unmapped)', () => {
    const types = issueTypes()
    types[0].config.workflow = { states: ['open'], initial: 'open', terminal: ['archived'] }
    const v = vocabFromTypes(types)
    expect(v.known.has('archived')).toBe(true)
    expect(v.terminal.has('archived')).toBe(true)
  })
})

describe('classifyColumns — ORDER gate', () => {
  it('preserves the wire column order verbatim (no sort, no reorder)', () => {
    // Deliberately NOT alphabetical: review before in_progress before open.
    const columns = [col('review'), col('in_progress'), col('open'), col('done')]
    const out = classifyColumns(columns, vocabFromTypes(issueTypes()))
    expect(out.map((c) => c.status)).toEqual(['review', 'in_progress', 'open', 'done'])
  })
})

describe('classifyColumns — category verdict', () => {
  const vocab = vocabFromTypes(issueTypes())

  it('terminal → closed, other known → open', () => {
    const out = classifyColumns([col('open'), col('done')], vocab)
    expect(out.find((c) => c.status === 'open')?.category).toBe('open')
    expect(out.find((c) => c.status === 'done')?.category).toBe('closed')
  })

  it('UNMAPPED gate: a wire status outside the registry → unmapped (not dropped)', () => {
    const out = classifyColumns([col('open'), col('on_hold'), col('done')], vocab)
    // The unknown status survives as its OWN column, badged unmapped.
    expect(out).toHaveLength(3)
    expect(out.find((c) => c.status === 'on_hold')?.category).toBe('unmapped')
  })

  it('carries count/issues/cursor through untouched', () => {
    const columns: BoardColumn[] = [{ status: 'open', count: 42, issues: [], cursor: 'idx-9' }]
    const out = classifyColumns(columns, vocab)
    expect(out[0]).toMatchObject({ status: 'open', count: 42, cursor: 'idx-9', category: 'open' })
  })
})

describe('initialCollapsed — closed-collapse seed', () => {
  it('collapses ONLY the closed columns (open + unmapped stay expanded)', () => {
    const out = classifyColumns([col('open'), col('on_hold'), col('done')], vocabFromTypes(issueTypes()))
    expect(initialCollapsed(out)).toEqual(['done'])
  })
})
