// isEgoFocusNotFound — the graph page's 404 fallback trigger (fix C). The ego
// route answers ONE identical 404 for "does not exist / not visible / not in
// a readable scope" (no existence oracle), so the client must not branch on
// the reason — any 404 from a focus fetch means: drop the focus, show the
// topic map, keep a soft notice. Other statuses (5xx, rate limit, network 0)
// stay hard errors.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../api'
import { egoNotFoundFallback, isEgoFocusNotFound } from './api'

describe('isEgoFocusNotFound', () => {
  it('is true for the graph 404 (the topic-map "Block not found" case)', () => {
    const err = new ApiError(404, 'not_found', 'Block not found')
    expect(isEgoFocusNotFound(err)).toBe(true)
  })

  it('is false for server errors — those stay hard error banners', () => {
    expect(isEgoFocusNotFound(new ApiError(500, 'internal', 'boom'))).toBe(false)
    expect(isEgoFocusNotFound(new ApiError(429, 'rate_limit', 'slow down'))).toBe(false)
  })

  it('is false for network/parse failures (status 0) and non-ApiError throws', () => {
    expect(isEgoFocusNotFound(new ApiError(0, 'internal', 'fetch failed'))).toBe(false)
    expect(isEgoFocusNotFound(new Error('boom'))).toBe(false)
    expect(isEgoFocusNotFound('not an error')).toBe(false)
    expect(isEgoFocusNotFound(undefined)).toBe(false)
  })
})

// PR #48 review: a 404 must only throw the view away when there is no view to
// keep. A SearchBox pick (search has no type filter, so checkpoint/system-meta
// hits show up) or an expand from an established focus keeps focus + windows.
describe('egoNotFoundFallback', () => {
  it('without a focus (deep link, topic-map pick) falls back to the topic map', () => {
    const fb = egoNotFoundFallback(null)
    expect(fb.toOverview).toBe(true)
    expect(fb.notice).toContain('showing the topic map')
  })

  it('with a focus in place keeps the current view and only shows a notice', () => {
    const fb = egoNotFoundFallback('550e8400-e29b-41d4-a716-446655440001')
    expect(fb.toOverview).toBe(false)
    expect(fb.notice).toContain('keeping the current view')
  })
})
