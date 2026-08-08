// Pure trigger-summary logic for MultiSelect.svelte — lives in the module
// script (no DOM) so it runs in the node-only vitest env (design 04-§5.5),
// same split as ConfirmDialog. The popover open/close/Escape wiring is
// exercised by the playwright focus-stage specs (graph-structural.spec.ts).

import { describe, expect, it } from 'vitest'
import { summarize } from './MultiSelect.svelte'

describe('summarize', () => {
  it('reads "all" when every option is on', () => {
    expect(summarize(7, 7)).toBe('all')
  })
  it('reads on/total when filtered', () => {
    expect(summarize(3, 7)).toBe('3/7')
    expect(summarize(0, 7)).toBe('0/7')
  })
  it('an empty option list never claims "all"', () => {
    expect(summarize(0, 0)).toBe('0/0')
  })
})
