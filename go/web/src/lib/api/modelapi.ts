// Model API administration client (design/05 §4.2, wave W05-05) — the gateway
// consumers and their ctxm_ keys. Everything goes through the tier-gated
// `POST /api/manage` actions of W01-5 (internal/handler/manage_table.go); there
// is no REST resource. One named function per action, mirroring keys.ts;
// apiFetch raises every {success:false} envelope as ApiError (a 403 tier
// refusal, the 403 "tenant_id is server-admin only", a 400 invalid cursor or
// unknown status). W05-05 built the read; W05-06 adds create, deactivate,
// reactivate, key revoke and the alias catalog the offer picker reads.

import { apiFetch } from '../api'
import type {
  AliasChainPreviewRequest,
  AliasRolePreviewRequest,
  ChainPreviewAnswer,
  ConsumerCreateResult,
  ConsumerListResponse,
  ConsumerRevokeResult,
  ConsumerStatus,
  ConsumerUpdateResult,
  ConsumerUpdateSpec,
  ModelAliasCreateSpec,
  ModelAliasListResponse,
  ModelAliasUpdateSpec,
  ModelAliasWriteResult,
  ModelKeyRotateResult,
  ModelKeyRotateSpec,
  RoleSuggestionAnswer,
} from './types'

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

/** Prompt-sensitivity vocabulary, strictest first (store CHECK; `credentials` is the create default). */
export type PromptSensitivity = 'credentials' | 'personal' | 'internal' | 'public'

/**
 * What the create dialog asks for (design/05 §4.6). Deliberately WITHOUT
 * `kind`: the SPA creates `app` consumers only (the server's default) — a peer
 * is made on /admin/peers (W05-14) — so a kind can never reach the wire from
 * here (G3). Every field is optional except `label`; absent = server default:
 * `tenant_scope` absent = _global models only, `models` absent = none,
 * `prompt_sensitivity` absent = credentials, `allowed_cidrs` absent = any
 * source, no expiry field = now + gateway.key_max_lifetime_app.
 */
export interface ConsumerCreateSpec {
  label: string
  description?: string
  /** server-admin only — the target tenant (403 for anyone else). */
  tenant_id?: string
  tenant_scope?: string
  all_models?: boolean
  /** Model alias IDS (W01-9) — never names. */
  models?: string[]
  prompt_sensitivity?: PromptSensitivity
  allowed_cidrs?: string[]
  /** Absolute expiry (RFC 3339). */
  expires_at?: string
  /** Relative expiry in whole days — turned into expires_at below; never on the wire. */
  expires_in_days?: number
}

const DAY_MS = 86_400_000

/**
 * Create a consumer with its first key — model-consumer-create. Returns the
 * reveal-once answer (`api_key` = plaintext) for the caller to show once and
 * drop; this function keeps nothing.
 *
 * The body is built from an explicit field list (never a spread of `spec`),
 * so an extra property on the object — a `kind` above all — is not sent.
 * `expires_in_days` becomes `expires_at = now + days`: the ONE place the
 * gateway SPA reads the client clock, and only to build a request value the
 * server validates against its own clock (`expires_at must lie in the
 * future` / beyond the lifetime ceiling ⇒ 400). Statuses are never derived
 * from this clock (design/05 §5 B-11 — the src/routes/gateway scan holds the
 * surface to that). `now` is injectable for the unit test.
 */
export function createConsumer(spec: ConsumerCreateSpec, now: () => number = Date.now): Promise<ConsumerCreateResult> {
  const data: Record<string, unknown> = { label: spec.label }
  if (spec.description) data.description = spec.description
  if (spec.tenant_id) data.tenant_id = spec.tenant_id
  if (spec.tenant_scope) data.tenant_scope = spec.tenant_scope
  if (spec.all_models === true) data.all_models = true
  if (spec.models !== undefined && spec.models.length > 0) data.models = [...spec.models]
  if (spec.prompt_sensitivity) data.prompt_sensitivity = spec.prompt_sensitivity
  if (spec.allowed_cidrs !== undefined && spec.allowed_cidrs.length > 0) data.allowed_cidrs = [...spec.allowed_cidrs]
  if (spec.expires_at) data.expires_at = spec.expires_at
  else if (spec.expires_in_days !== undefined) data.expires_at = new Date(now() + spec.expires_in_days * DAY_MS).toISOString()
  return apiFetch<ConsumerCreateResult>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'model-consumer-create', data }),
  })
}

/**
 * Deactivate a consumer — model-consumer-delete (soft: active=false, every
 * key revoked server-side). Revoking is always allowed to a tenant-admin of
 * the consumer's tenant (design/01 §4.6).
 */
export function deleteConsumer(id: string): Promise<ConsumerRevokeResult> {
  return apiFetch<ConsumerRevokeResult>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'model-consumer-delete', data: { id } }),
  })
}

/**
 * Reactivate a consumer — model-consumer-update {active:true}. Granting: a
 * tenant-admin only with self-service and never on a ctx-peer (403 otherwise),
 * the tenant's max_model_consumers applies (429). It revives no key (I-K8).
 */
export function reactivateConsumer(id: string): Promise<ConsumerUpdateResult> {
  return apiFetch<ConsumerUpdateResult>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'model-consumer-update', data: { id, active: true } }),
  })
}

/** Revoke one model key — model-key-revoke {key_id}. Always allowed (revoking). */
export function revokeModelKey(keyId: string): Promise<ConsumerRevokeResult> {
  return apiFetch<ConsumerRevokeResult>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'model-key-revoke', data: { key_id: keyId } }),
  })
}

/** The alias catalog the caller may read — the offer picker's source (model-alias-list, no payload). */
export function listModelAliases(): Promise<ModelAliasListResponse> {
  return apiFetch<ModelAliasListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'model-alias-list' }),
  })
}

// --- RC-D C5 (W05-07 policy edit, W05-08 rotation, W05-13 alias catalog).
// Every body is built from an EXPLICIT field list — never a spread — so a
// field the server must not see (kind, tenant_id, expires_at on an update;
// name / kind / scope / tenant_id on an alias update) cannot ride along even
// when an object carries it.

function manage<T>(action: string, data: Record<string, unknown>, id?: string): Promise<T> {
  const body: Record<string, unknown> = { action }
  if (id !== undefined) body.id = id
  body.data = data
  return apiFetch<T>('/api/manage', { method: 'POST', body: JSON.stringify(body) })
}

/**
 * Change a consumer's policy — model-consumer-update {data:{id, ...patch}}
 * (W05-07). Sends exactly the fields present in `patch` out of the policy set
 * (label, description, tenant_scope, all_models, models, prompt_sensitivity,
 * admission_lane, allowed_cidrs); a refusal (400 field, 403 "admin key
 * required" tier, 409 label, 429 quota) rejects as ApiError.
 */
export function updateConsumer(id: string, patch: ConsumerUpdateSpec): Promise<ConsumerUpdateResult> {
  const data: Record<string, unknown> = { id }
  if (patch.label !== undefined) data.label = patch.label
  if (patch.description !== undefined) data.description = patch.description
  if (patch.tenant_scope !== undefined) data.tenant_scope = patch.tenant_scope
  if (patch.all_models !== undefined) data.all_models = patch.all_models
  if (patch.models !== undefined) data.models = [...patch.models]
  if (patch.prompt_sensitivity !== undefined) data.prompt_sensitivity = patch.prompt_sensitivity
  if (patch.admission_lane !== undefined) data.admission_lane = patch.admission_lane
  if (patch.allowed_cidrs !== undefined) data.allowed_cidrs = patch.allowed_cidrs === null ? null : [...patch.allowed_cidrs]
  return manage<ConsumerUpdateResult>('model-consumer-update', data)
}

/**
 * Rotate a consumer's key — model-key-rotate {data:{consumer_id, grace,
 * expires_at?}} (W05-08). grace in seconds is ALWAYS on the wire, 0 included
 * (leak: the predecessor is revoked at once). Returns the reveal-once answer
 * with the new plaintext for the caller to show once and drop.
 */
export function rotateModelKey(spec: ModelKeyRotateSpec): Promise<ModelKeyRotateResult> {
  const data: Record<string, unknown> = { consumer_id: spec.consumer_id, grace: spec.grace }
  if (spec.expires_at) data.expires_at = spec.expires_at
  return manage<ModelKeyRotateResult>('model-key-rotate', data)
}

/** Create a model alias — model-alias-create (W05-13). `enabled` is always on the wire. */
export function createModelAlias(spec: ModelAliasCreateSpec): Promise<ModelAliasWriteResult> {
  const data: Record<string, unknown> = { name: spec.name, kind: spec.kind, role: spec.role, enabled: spec.enabled }
  if (spec.scope) data.scope = spec.scope
  if (spec.tenant_id) data.tenant_id = spec.tenant_id
  if (spec.sensitivity !== undefined) data.sensitivity = spec.sensitivity
  if (spec.policy !== undefined) data.policy = spec.policy
  if (spec.description !== undefined) data.description = spec.description
  if (spec.confirm_sensitivity_downgrade === true) data.confirm_sensitivity_downgrade = true
  return manage<ModelAliasWriteResult>('model-alias-create', data)
}

/**
 * Update a model alias — model-alias-update {id, data:{...patch}} (W05-13):
 * the id rides TOP-LEVEL (the handler reads req.ID; an id in data is 400);
 * name / kind / scope / tenant_id never (immutable, 400 immutable_field).
 */
export function updateModelAlias(id: string, patch: ModelAliasUpdateSpec): Promise<ModelAliasWriteResult> {
  const data: Record<string, unknown> = {}
  if (patch.role !== undefined) data.role = patch.role
  if (patch.sensitivity !== undefined) data.sensitivity = patch.sensitivity
  if (patch.policy !== undefined) data.policy = patch.policy
  if (patch.description !== undefined) data.description = patch.description
  if (patch.enabled !== undefined) data.enabled = patch.enabled
  if (patch.confirm_sensitivity_downgrade === true) data.confirm_sensitivity_downgrade = true
  return manage<ModelAliasWriteResult>('model-alias-update', data, id)
}

/**
 * Dry-run the /v1 chain of an alias NAME through one consumer —
 * model-alias-preview {data:{alias, consumer_id}} (W05-04, C7; server-admin).
 */
export function previewAliasChain(req: AliasChainPreviewRequest): Promise<ChainPreviewAnswer> {
  return manage<ChainPreviewAnswer>('model-alias-preview', { alias: req.alias, consumer_id: req.consumer_id })
}

/** The pool's primary model of a role — model-alias-preview {data:{role}} ("Suggest from pool"). */
export function poolPrimaryModel(req: AliasRolePreviewRequest): Promise<RoleSuggestionAnswer> {
  return manage<RoleSuggestionAnswer>('model-alias-preview', { role: req.role })
}
