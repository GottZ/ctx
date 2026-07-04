// Type-Registry read client (design/04 §4 / U03). Consumes the /api/types
// surface frozen by W1 (K5): the wire types (BlockTypeView / TypesListResponse /
// TypeResponse) live in ./types.ts and are pinned by the Go golden
// TestTypesGoldenShape — this module does NOT redeclare them, it only wraps the
// two read endpoints. The write path (PUT/DELETE /api/types/{name}) is Achse-01
// admin territory and is out of U03 scope (§1 Liefert-nicht).

import { apiFetch } from '../api'
import type { BlockTypeWriteSpec, TypeDeleteResponse, TypeResponse, TypesListResponse } from './types'

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

/**
 * PUT /api/types/{name} — upsert a type (workflow W2 write surface, U10). An
 * existing row is updated in its own scope; an unknown name is created in the
 * caller's role-pinned write scope (server-admin → '_global', tenant-admin →
 * own tenant). A 422 (config/caps validation), 403 (a tenant-admin targeting a
 * '_global' type) or 409 is raised as ApiError — the admin form keeps the draft
 * open and surfaces the message at the field (no silent input loss).
 */
export function putType(name: string, spec: BlockTypeWriteSpec): Promise<TypeResponse> {
  return apiFetch<TypeResponse>(`${TYPES_BASE}/${encodeURIComponent(name)}`, {
    method: 'PUT',
    body: JSON.stringify(spec),
  })
}

/**
 * DELETE /api/types/{name} (U10). A builtin type (409 ErrBlockTypeBuiltin) and a
 * still-referenced type (409 + the active/archived count on ApiError.details)
 * are raised as ApiError. The builtin case is ALSO guarded UI-side (delete
 * control disabled), so this call is the server half of the double-layer
 * builtin protection (§4.7): the UI-disable is comfort, the server is the gate.
 */
export function deleteType(name: string): Promise<TypeDeleteResponse> {
  return apiFetch<TypeDeleteResponse>(`${TYPES_BASE}/${encodeURIComponent(name)}`, {
    method: 'DELETE',
  })
}
