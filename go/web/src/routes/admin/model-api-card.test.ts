// model-api-card.ts gate (RC-D C6 S1, W05-20): the tenant card's counter
// from the first model-consumer-list page — "N", or "N+" when the server
// says there is a next page (never an invented total). Red against the stub ('').

import { describe, expect, it } from 'vitest'
import type { ConsumerView } from '../../lib/api/types'
import { consumerCountText } from './model-api-card'

const C = { id: 'c' } as ConsumerView

describe('consumerCountText (W05-20)', () => {
  it('counts the page', () => {
    expect(consumerCountText({ consumers: [C, C, C], next_cursor: null }), 'gate W05-20: Zähler').toBe('3')
    expect(consumerCountText({ consumers: [], next_cursor: null })).toBe('0')
  })
  it('a next page reads "N+"', () => {
    expect(consumerCountText({ consumers: [C, C], next_cursor: 'opaque' })).toBe('2+')
  })
})
