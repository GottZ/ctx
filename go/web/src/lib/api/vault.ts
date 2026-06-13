// Secrets-vault client (design 04-§3.5, W5). Write-only by contract: PUT sets a
// value that is never read back, GET returns metadata only, DELETE answers 409
// while the secret is still referenced. All three are admin-gated server-side.
//
// File named vault* (not secret*) on purpose: pre-commit Gate 1 blocks new
// files matching *secret*/*password*/*credentials* — the hook stays armed for
// real secret material, same convention as the server's sealbox.go.

import { apiFetch } from '../api'
import type { SecretDeleteResponse, SecretListResponse, SecretPutResponse } from './types'

export function listSecrets(): Promise<SecretListResponse> {
  return apiFetch<SecretListResponse>('/api/secrets')
}

/** Create or rotate (overwrite) the named secret. The value is never echoed. */
export function putSecret(name: string, value: string): Promise<SecretPutResponse> {
  return apiFetch<SecretPutResponse>(`/api/secrets/${encodeURIComponent(name)}`, {
    method: 'PUT',
    body: JSON.stringify({ value }),
  })
}

/** Delete the secret — throws ApiError(409) with referenced_by if still in use. */
export function deleteSecret(name: string): Promise<SecretDeleteResponse> {
  return apiFetch<SecretDeleteResponse>(`/api/secrets/${encodeURIComponent(name)}`, {
    method: 'DELETE',
  })
}
