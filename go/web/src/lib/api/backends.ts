// Backend-pool client (design 04-§3.5, W5). There is no GET/PUT /api/backends —
// the pool is managed through the F3 `POST /api/manage` actions, all admin-gated
// server-side (backend-list/test included). One named function per action,
// mirroring the status.ts manage pattern. The read-only health/state view lives
// in /api/status.backends (status.ts); these are the mutation + list paths.

import { apiFetch } from '../api'
import type {
  BackendDeleteResponse,
  BackendListResponse,
  BackendMutateResponse,
  BackendSpec,
  BackendTestResult,
} from './types'

/**
 * Server-side confirm gates carried inside `data`. confirm_trust_elevation is
 * required when raising trust (create: any non-public; update: rank rose) —
 * else the server answers 400. confirm_data_collection arms an openrouter
 * backend's data collection (off→on) — also 400 without it.
 */
export interface ConfirmFlags {
  confirmTrustElevation?: boolean
  confirmDataCollection?: boolean
}

function dataWithConfirm(spec: BackendSpec, c: ConfirmFlags): Record<string, unknown> {
  const data: Record<string, unknown> = { ...spec }
  if (c.confirmTrustElevation) data.confirm_trust_elevation = true
  if (c.confirmDataCollection) data.confirm_data_collection = true
  return data
}

export function listBackends(): Promise<BackendListResponse> {
  return apiFetch<BackendListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'backend-list' }),
  })
}

export function createBackend(spec: BackendSpec, confirm: ConfirmFlags = {}): Promise<BackendMutateResponse> {
  return apiFetch<BackendMutateResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'backend-create', data: dataWithConfirm(spec, confirm) }),
  })
}

export function updateBackend(
  id: string,
  spec: BackendSpec,
  confirm: ConfirmFlags = {},
): Promise<BackendMutateResponse> {
  return apiFetch<BackendMutateResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'backend-update', id, data: dataWithConfirm(spec, confirm) }),
  })
}

export function deleteBackend(id: string): Promise<BackendDeleteResponse> {
  return apiFetch<BackendDeleteResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'backend-delete', id }),
  })
}

/** Dry-run reachability probe; probeChat=true adds a 1-token chat round-trip. */
export function testBackend(id: string, probeChat = false): Promise<BackendTestResult> {
  const req = probeChat
    ? { action: 'backend-test', id, data: { probe: 'chat' } }
    : { action: 'backend-test', id }
  return apiFetch<BackendTestResult>('/api/manage', {
    method: 'POST',
    body: JSON.stringify(req),
  })
}
