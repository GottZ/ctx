// Block-level read-grant manage client (design 04 §3, Wave A1) — the THIRD grant
// level: a SINGLE block (not a whole scope) shared with a grantee tenant
// (block_grant_manage.go, MT T43). Own file (separate from tenants.ts): a
// different risk/owner semantics — every create/revoke is ownership-gated
// (§5.1), and `block-grant-list` is owner-side only, NOT a global topology oracle
// (block_grant_manage.go:124). One named function per action; apiFetch raises a
// 403 (foreign block / cross-tenant opt-out) as ApiError.

import { apiFetch } from '../api'
import type { BlockGrantListResponse, BlockGrantResponse, BlockGrantRevokeResponse } from './types'

/** create/revoke payload (block_grant_manage.go:26 blockGrantSpec). Both actions
 * carry the pair in `data` — NOT as a top-level id. */
export interface BlockGrantSpec {
  block_id: string
  grantee_tenant: string
}

/** Shares ONE block with a grantee tenant. Fails 403 unless the caller's tenant
 * OWNS the block AND (for a cross-tenant grantee) the per-tenant opt-in is set —
 * the UI maps that 403 to a hint, not a stacktrace (A6). */
export function createBlockGrant(spec: BlockGrantSpec): Promise<BlockGrantResponse> {
  return apiFetch<BlockGrantResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'block-grant-create', data: spec }),
  })
}

/** Lists the grants MADE BY the caller's tenant (owner-side "what have I shared,
 * to whom"). An optional blockId narrows to one block (req.ID filter,
 * block_grant_manage.go:128-129). */
export function listBlockGrants(blockId?: string): Promise<BlockGrantListResponse> {
  const req: Record<string, unknown> = { action: 'block-grant-list' }
  if (blockId !== undefined) req.id = blockId
  return apiFetch<BlockGrantListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify(req),
  })
}

/** Revokes one block grant (ownership-gated; the safe direction needs no opt-in).
 * Takes the pair in `data`, NOT a grant id. */
export function revokeBlockGrant(spec: BlockGrantSpec): Promise<BlockGrantRevokeResponse> {
  return apiFetch<BlockGrantRevokeResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'block-grant-revoke', data: spec }),
  })
}
