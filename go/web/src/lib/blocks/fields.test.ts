// Block-editor 422 field-error extraction (W4). Mirrors lib/backends.test.ts:
// the server's fields[] array rides on ApiError.details; fieldErrors lifts it
// off so the dialog renders each error next to its input, and yields [] for
// any non-422 failure.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../api'
import { fieldErrors } from './fields'

describe('fieldErrors', () => {
  it('extracts the 422 fields array off ApiError.details', () => {
    const err = new ApiError(422, 'validation', 'validation failed', null, {
      success: false,
      error: 'validation failed',
      fields: [{ field: 'title', message: 'title is required' }],
    })
    expect(fieldErrors(err)).toEqual([{ field: 'title', message: 'title is required' }])
  })

  it('returns empty for a failure without fields', () => {
    expect(fieldErrors(new ApiError(400, 'bad_request', 'nope'))).toEqual([])
    expect(fieldErrors(new Error('network'))).toEqual([])
  })
})
