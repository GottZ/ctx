// Corpus-maintenance manage client (design 04 §3, Wave A1) — the G41 sensitivity
// audit + the G40 credentials classify (blocks_audit.go / blocks_classify.go).
// There is NO SSE for these: start returns the initial status, and the UI polls
// the *-status action while run.running (A7). Both starts take an optional
// {dry_run,limit}; dry_run is the no-write sample gate, limit 0 drains the full
// pick set. start + status share one response envelope per family.

import { apiFetch } from '../api'
import type { BlocksAuditStatusResponse, BlocksClassifyStatusResponse } from './types'

/** start payload (blocks_audit.go:42 / blocks_classify.go:32). Both fields are
 * optional — an empty object is a live run over the full set (dry_run=false,
 * limit=0). The dry-run-default policy lives in the UI (A7), not here. */
export interface CorpusRunParams {
  dry_run?: boolean
  limit?: number
}

export function startBlocksAudit(params: CorpusRunParams = {}): Promise<BlocksAuditStatusResponse> {
  return apiFetch<BlocksAuditStatusResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'blocks-audit-start', data: params }),
  })
}

export function blocksAuditStatus(): Promise<BlocksAuditStatusResponse> {
  return apiFetch<BlocksAuditStatusResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'blocks-audit-status' }),
  })
}

export function startBlocksClassify(params: CorpusRunParams = {}): Promise<BlocksClassifyStatusResponse> {
  return apiFetch<BlocksClassifyStatusResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'blocks-classify-start', data: params }),
  })
}

export function blocksClassifyStatus(): Promise<BlocksClassifyStatusResponse> {
  return apiFetch<BlocksClassifyStatusResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'blocks-classify-status' }),
  })
}
