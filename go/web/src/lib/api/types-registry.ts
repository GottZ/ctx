// Type-Registry read client (design/04 §4 / U03). Consumes the /api/types
// surface frozen by W1 (K5): the wire types (BlockTypeView / TypesListResponse /
// TypeResponse) live in ./types.ts and are pinned by the Go golden
// TestTypesGoldenShape — this module does NOT redeclare them, it only wraps the
// two read endpoints. The write path (PUT/DELETE /api/types/{name}) is Achse-01
// admin territory and is out of U03 scope (§1 Liefert-nicht).

import { apiFetch } from '../api'
import type { TypeResponse, TypesListResponse } from './types'

/**
 * The single path prefix of the type-registry surface. Imported by the e2e
 * fixture namespace matcher alongside ISSUES_BASE (U03) so the /api/types mocks
 * carry their OWN hard-fail namespace and can never drift from the endpoint
 * form. No second hand-written '/api/types' string exists.
 */
export const TYPES_BASE = '/api/types'

/**
 * GET /api/types — the effective registry: the shipped '_global' defaults ∪ the
 * caller's own tenant overlay (tenant rows shadow '_global' by name). Read is
 * ungated-cheap (the registry is ≪ 100 rows); every valid type key here is a
 * badge + a potential board (WorkflowConfig).
 */
export function listTypes(): Promise<TypesListResponse> {
  return apiFetch<TypesListResponse>(TYPES_BASE)
}

/**
 * GET /api/types/{name} — one type incl. its policy config + source badge. An
 * unknown/foreign name is a 404 {success:false} (raised as ApiError, no oracle).
 */
export function getType(name: string): Promise<TypeResponse> {
  return apiFetch<TypeResponse>(`${TYPES_BASE}/${encodeURIComponent(name)}`)
}
