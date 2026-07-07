// Disable-profile client (092, Web-UX U01-W6; design/01 §4.3 + AM-5/AM-7). Like
// backends.ts there is no REST resource — the five profile actions ride the F3
// `POST /api/manage` surface, all tier tenant-admin server-side (the handler +
// store carry the tenant isolation, AM-5 VOLL). One named function per action,
// mirroring the backends.ts / status.ts manage pattern.
//
// Impact (§4.4) is server-computed — ONE source for UI and the blackout gate.
// roles_blacked_out is the fail-closed axis: activating a profile whose
// activation empties a role is a 422 unless confirm_role_blackout is set. The
// UI learns the blackout set from a dry_run:true toggle (a 200 carrying the
// impact) BEFORE the write, so the confirm step shows the roles in cleartext.

import { apiFetch } from '../api'

// One member backend inside a profile impact (§4.4: caller-visible members).
export interface ProfileMemberBackend {
  id: string
  name: string
  scope: string
  roles: string[]
  enabled: boolean
  effective_state: string
}

// impact of a profile AT a given active state. backends + roles_affected are the
// member set (independent of active); roles_blacked_out + embed_degraded depend
// on the evaluated active state (list = current, dry_run/toggle = target).
export interface DisableProfileImpact {
  backends: ProfileMemberBackend[]
  roles_affected: string[]
  roles_blacked_out: string[]
  embed_degraded?: boolean
}

// profileMeta shape (name/scope/label/description/active/reserved). scope is
// _global for operator profiles, the tenant's home scope for tenant profiles.
export interface DisableProfileMeta {
  name: string
  scope: string
  label: string
  description: string
  active: boolean
  reserved: boolean
}

// The list/create/update view: meta + the current-state impact.
export interface DisableProfileView extends DisableProfileMeta {
  impact: DisableProfileImpact
}

export interface DisableProfileListResponse {
  success: true
  profiles: DisableProfileView[]
}

// toggle answers with the flipped profile meta + the target-state impact + the
// as_of merge floor (§4.5) and the running-request note (§5.4). dry_run:true
// carries the same shape without a write (dry_run:true echoed back).
export interface DisableProfileToggleResponse {
  success: true
  profile: DisableProfileMeta
  impact: DisableProfileImpact
  as_of: string
  note: string
  dry_run?: boolean
}

// create/update answer with the profile view (meta + impact) + as_of.
export interface DisableProfileMutateResponse {
  success: true
  profile: DisableProfileView
  as_of: string
}

export interface DisableProfileDeleteResponse {
  success: true
  deleted: string
  as_of: string
}

// The create/update patch. members are backend NAMES (the API surface, §4.3);
// on update an absent members key leaves membership untouched.
export interface DisableProfileSpec {
  name: string
  scope?: string
  label?: string
  description?: string
  members?: string[]
}

export function listDisableProfiles(): Promise<DisableProfileListResponse> {
  return apiFetch<DisableProfileListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'disable-profile-list' }),
  })
}

/**
 * Flip a profile's active state. dryRun learns the blackout set without writing
 * (200 with the target-state impact); confirm carries confirm_role_blackout so
 * a role-emptying activation goes through (else the server answers 422 with
 * roles_blacked_out in the body).
 */
export function toggleDisableProfile(
  name: string,
  scope: string,
  active: boolean,
  opts: { confirm?: boolean; dryRun?: boolean } = {},
): Promise<DisableProfileToggleResponse> {
  const data: Record<string, unknown> = { name, scope, active }
  if (opts.confirm) data.confirm_role_blackout = true
  if (opts.dryRun) data.dry_run = true
  return apiFetch<DisableProfileToggleResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'disable-profile-toggle', data }),
  })
}

export function createDisableProfile(spec: DisableProfileSpec): Promise<DisableProfileMutateResponse> {
  return apiFetch<DisableProfileMutateResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'disable-profile-create', data: spec }),
  })
}

/** Patch label/description/members of a profile. name+scope address it. */
export function updateDisableProfile(
  name: string,
  scope: string,
  patch: { label?: string; description?: string; members?: string[] },
): Promise<DisableProfileMutateResponse> {
  return apiFetch<DisableProfileMutateResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'disable-profile-update', data: { name, scope, ...patch } }),
  })
}

export function deleteDisableProfile(name: string, scope: string): Promise<DisableProfileDeleteResponse> {
  return apiFetch<DisableProfileDeleteResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'disable-profile-delete', data: { name, scope } }),
  })
}
