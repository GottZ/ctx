// Model API administration client (design/05 §4.2, wave W05-05) — the gateway
// consumers and their ctxm_ keys. Everything goes through the tier-gated
// `POST /api/manage` actions of W01-5 (internal/handler/manage_table.go); there
// is no REST resource. One named function per action, mirroring keys.ts;
// apiFetch raises every {success:false} envelope as ApiError (a 403 tier
// refusal, the 403 "tenant_id is server-admin only", a 400 invalid cursor or
// unknown status). This wave reads only; the mutations (create, deactivate,
// revoke) are W05-06.

import { apiFetch } from '../api'
import type { ConsumerListResponse, ConsumerStatus } from './types'

/** The status vocabulary in the server's order (store.ConsumerStatuses, design/05 §3.2). */
export const CONSUMER_STATUSES: readonly ConsumerStatus[] = ['active', 'expiring', 'rotating', 'no_key', 'inactive']

/**
 * The server's default status set — the four statuses of an ACTIVE consumer
 * (an absent `status` means exactly this, design/05 §3.2; Lead decision 2 of
 * W05-05 shows it as the toolbar's preselection).
 */
export const DEFAULT_CONSUMER_STATUSES: readonly ConsumerStatus[] = ['active', 'expiring', 'rotating', 'no_key']

/**
 * model-consumer-list payload (consumerListRequest). Every field is optional
 * and omitted when unset, so the server applies its own defaults: `status`
 * absent = DEFAULT_CONSUMER_STATUSES, `limit` absent = the page cap
 * gateway.consumer_list_max_limit. `tenant_id` is server-admin only (403
 * otherwise); `cursor` is the opaque next_cursor of the previous page.
 */
export interface ConsumerListParams {
  tenant_id?: string
  status?: ConsumerStatus[] | ['all']
  /** Label prefix (case-insensitive server-side, `%`/`_` literal). */
  q?: string
  cursor?: string
  limit?: number
}

/** One keyset page of model consumers, newest first. The cursor is handed through untouched. */
export function listConsumers(params: ConsumerListParams = {}): Promise<ConsumerListResponse> {
  const data: Record<string, unknown> = {}
  if (params.tenant_id) data.tenant_id = params.tenant_id
  if (params.status !== undefined) data.status = params.status
  if (params.q) data.q = params.q
  if (params.cursor) data.cursor = params.cursor
  if (params.limit !== undefined) data.limit = params.limit
  return apiFetch<ConsumerListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'model-consumer-list', data }),
  })
}
