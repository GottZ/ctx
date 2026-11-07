// "Model API" card on /admin/tenants/:id (RC-D C6, wave W05-20, design/05
// §7). The counter reads model-consumer-list {tenant_id} (server-admin,
// default status set = the active consumers) — no new route. One page is
// read; a next_cursor means "more than this page" and renders "N+", never a
// made-up total.
//
// RC-D S1: contract stub — '' (no counter).

import type { ConsumerListResponse } from '../../lib/api/types'

/** The counter text of the card from the first list page ("3", "100+"). */
export function consumerCountText(page: Pick<ConsumerListResponse, 'consumers' | 'next_cursor'>): string {
  void page
  return ''
}
