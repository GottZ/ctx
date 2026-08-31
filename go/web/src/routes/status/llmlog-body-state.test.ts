import { describe, expect, it } from 'vitest'
import { bodyStateReason, unsealedCredentialsNote } from './llmlog-body-state'

// C6-C: the four absent-body reasons plus the one note that must appear when a
// credentials-class row DOES carry bodies. These labels used to live inside
// LlmlogTable.svelte, where no test could reach them — the component is never
// mounted (vitest runs environment:'node', the models are tested instead), so a
// silent rewording of a privacy affordance had no gate at all. Extracting the
// two pure functions is what gives the wording a red/green.
describe('bodyStateReason', () => {
  it('names the reason for each absent-body state', () => {
    expect(bodyStateReason('sealed')).toBe(
      'sealed — credentials-class call, prompt/reply never stored',
    )
    expect(bodyStateReason('evicted')).toBe('evicted — bodies removed by retention')
    expect(bodyStateReason('bodyless')).toBe(
      'bodyless — this pipeline never records prompt/reply (embed, translate, rejection lines)',
    )
  })

  it('says nothing for a present row and for an unknown state', () => {
    expect(bodyStateReason('present')).toBe('')
    expect(bodyStateReason('something-new')).toBe('')
  })
})

describe('unsealedCredentialsNote', () => {
  // The point of the note: a credentials-class row whose bodies ARE shown must
  // not read as harmless. The class stays what it is; only the seal is off.
  it('warns when a credentials-class row is rendered with its bodies', () => {
    expect(unsealedCredentialsNote('present', 'credentials')).toBe(
      'credentials-class · unsealed by tenant devmode',
    )
  })

  it('stays silent for every other class', () => {
    for (const sens of ['personal', 'internal', 'public', '']) {
      expect(unsealedCredentialsNote('present', sens)).toBe('')
    }
  })

  it('stays silent while the row is still sealed', () => {
    for (const state of ['sealed', 'evicted', 'bodyless']) {
      expect(unsealedCredentialsNote(state, 'credentials')).toBe('')
    }
  })
})
