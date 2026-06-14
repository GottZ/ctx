// Per-field 422 error extraction for the block editor (W4). Mirrors
// lib/backends.ts fieldErrors: the server carries a `fields[]` array on a 422
// inside ApiError.details (apiFetch keeps the full envelope), letting the
// dialog render each error next to its input. Pure function so vitest covers it
// without a DOM.

import { toApiError } from '../api'

export interface BlockFieldError {
  field: string
  message: string
}

/** Extract the 422 fields[] off ApiError.details; empty for any other failure. */
export function fieldErrors(err: unknown): BlockFieldError[] {
  const f = toApiError(err).details?.fields
  if (!Array.isArray(f)) return []
  return f.filter(
    (x): x is BlockFieldError =>
      typeof x === 'object' && x !== null && typeof (x as { field?: unknown }).field === 'string',
  )
}
