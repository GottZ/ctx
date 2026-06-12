// /api/settings client (design 04-§3.3): list + per-key PUT/DELETE. The F2
// mutation contract is one call per key with a `{value}` scalar body — the
// group save loops sequentially so a 422 maps to exactly one field.

import { apiFetch } from '../api'
import type { SettingDeleteResponse, SettingPutResponse, SettingsListResponse } from './types'

export function listSettings(): Promise<SettingsListResponse> {
  return apiFetch<SettingsListResponse>('/api/settings')
}

export function putSetting(key: string, value: string | number | boolean): Promise<SettingPutResponse> {
  return apiFetch<SettingPutResponse>(`/api/settings/${encodeURIComponent(key)}`, {
    method: 'PUT',
    body: JSON.stringify({ value }),
  })
}

export function deleteSetting(key: string): Promise<SettingDeleteResponse> {
  return apiFetch<SettingDeleteResponse>(`/api/settings/${encodeURIComponent(key)}`, {
    method: 'DELETE',
  })
}
