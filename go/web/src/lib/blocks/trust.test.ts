// trustMarks gates (#295, web part): a block without either field has no mark,
// `untrusted` and `untrusted_origin` each give one in a fixed order, the origin
// statement is the server's text-surface wording verbatim, and a `false` /
// empty value is no mark.

import { describe, expect, it } from 'vitest'
import { trustMarks, untrustedOriginNote } from './trust'

describe('untrustedOriginNote', () => {
  it('is store.UntrustedOriginNote verbatim', () => {
    expect(untrustedOriginNote('session')).toBe(
      'untrusted_origin:session (agent-produced text: re-typed out of a type marked untrusted)',
    )
  })

  it('is empty for an absent or empty record', () => {
    expect(untrustedOriginNote(undefined)).toBe('')
    expect(untrustedOriginNote('')).toBe('')
  })
})

describe('trustMarks', () => {
  it('has no mark for a block without either field', () => {
    expect(trustMarks({})).toEqual([])
    expect(trustMarks({ untrusted: false, untrusted_origin: '' })).toEqual([])
  })

  it('marks a block of a type marked untrusted', () => {
    const marks = trustMarks({ untrusted: true })
    expect(marks.map((m) => [m.key, m.label])).toEqual([['untrusted', 'untrusted']])
    expect(marks[0].title).toContain('observation data')
  })

  it('marks a block re-typed out of such a type with the origin type and the server wording', () => {
    expect(trustMarks({ untrusted_origin: 'checkpoint' })).toEqual([
      {
        key: 'origin',
        label: 'untrusted origin: checkpoint',
        title: 'untrusted_origin:checkpoint (agent-produced text: re-typed out of a type marked untrusted)',
      },
    ])
  })

  it('carries both marks, untrusted first, for a block back in an untrusted type', () => {
    expect(trustMarks({ untrusted: true, untrusted_origin: 'session' }).map((m) => m.key)).toEqual(['untrusted', 'origin'])
  })
})
