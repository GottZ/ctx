// FOLGE-Z Z4 — the create dialog without silent values (briefings/FOLGE-Z-INDEX.md
// §1 C-SPA, §3 contract; user 2026-11-08: "ich möchte keine hidden values für
// die api key konfiguration"). Pure and DOM-free like mint.ts, so vitest covers
// it in node (startlimits.test.ts); ConsumerCreateDialog only wires it up.
//
//   - Lane: the server's column default is `bulk` (migration 161) and stays
//     the API default (E3: `urgent` is server-admin only). The dialog never
//     leaves it to that default: it preselects `urgent` for a server-admin
//     (the SPA creates `app` consumers only) and shows `bulk` to a tenant-admin
//     with the reason, and the chosen lane is ALWAYS sent.
//   - Start limits: per op (chat, embed; rerank only when the catalog offers a
//     rerank alias — without one no request can reach that op) the four
//     dimensions a caller tunes first, prefilled with the EFFECTIVE values a new
//     consumer of the target tenant inherits. Source: gateway-limits-get of
//     subject `tenant` (the chosen tenant — a server-admin's target can carry
//     tenant rows, so the instance values would be a wrong prefill; Lead
//     deviation, see the FOLGE-Z-spa report), `instance` only for a server-admin
//     without a tenant id. Only CHANGED fields go out as `limits` (§3 `L`); the
//     row lands at the caller's tier server-side, so a tenant-admin can only
//     narrow (consumer_tenant ≤ the inherited value) — mirrored here, the server
//     stays the gate.
//   - Reveal: the answer's `effective_limits` (§3 `E`) and `admission_lane`;
//     anything the server does not report reads "not reported" — no number is
//     ever derived on the client.

import type {
  ConsumerCreateLimits,
  ConsumerStartLimits,
  ExternalLane,
  GatewayOp,
  ModelAliasView,
} from '../../lib/api/types'
import { isExternalLane } from './lane'

/** The dimensions of the "start limits" block, in display order (all integers). */
export const START_DIMS = ['rpm', 'tpm', 'max_concurrency', 'daily_tokens'] as const
export type StartDim = (typeof START_DIMS)[number]

/** Human labels of the start dimensions (the accessible name is "<op> <label>"). */
export const START_DIM_LABEL: Readonly<Record<StartDim, string>> = {
  rpm: 'rpm',
  tpm: 'tpm',
  max_concurrency: 'concurrency',
  daily_tokens: 'daily tokens',
}

/**
 * Every field of §3 `E` in display order — the reveal shows all of them (review
 * finding 2: a new consumer's allow_external=false or its cost budget is a value
 * it starts with, too).
 */
export const EFFECTIVE_FIELDS = [
  'rpm',
  'tpm',
  'max_concurrency',
  'daily_tokens',
  'monthly_tokens',
  'daily_requests',
  'monthly_cost_usd',
  'allow_external',
  'allow_unmetered_external',
] as const
export type EffectiveField = (typeof EFFECTIVE_FIELDS)[number]

export const EFFECTIVE_LABEL: Readonly<Record<EffectiveField, string>> = {
  ...START_DIM_LABEL,
  monthly_tokens: 'monthly tokens',
  daily_requests: 'daily requests',
  monthly_cost_usd: 'monthly cost',
  allow_external: 'external',
  allow_unmetered_external: 'unmetered external',
}

/** Rates and concurrency are never unbounded (GatewayLimitsEffective); a budget may be (null). */
const BUDGET_DIMS = new Set<string>(['daily_tokens', 'monthly_tokens', 'daily_requests', 'monthly_cost_usd'])
const BOOL_FIELDS = new Set<string>(['allow_external', 'allow_unmetered_external'])

/** One effective value as text: number / $cost, null budget = "unbounded", bool = yes/no, else "not reported". */
function fmtEffective(field: EffectiveField, v: unknown): string {
  if (BOOL_FIELDS.has(field)) return typeof v === 'boolean' ? (v ? 'yes' : 'no') : 'not reported'
  if (positive(v)) return field === 'monthly_cost_usd' ? `$${v}` : v.toLocaleString('en-US')
  return v === null && BUDGET_DIMS.has(field) ? 'unbounded' : 'not reported'
}

/** Per op and dimension: the effective default (number), null = unbounded, absent = not reported. */
export type StartDefaults = Partial<Record<GatewayOp, Partial<Record<StartDim, number | null>>>>

/** The raw text of every start-limit field, keyed `<op>.<dim>`. */
export type StartInputs = Record<string, string>

export function fieldKey(op: GatewayOp, dim: StartDim): string {
  return `${op}.${dim}`
}

const BASE_OPS: readonly GatewayOp[] = ['chat', 'embed']

function isObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v)
}

/** A positive finite number — the only limit value the wire carries (no 0 sentinel). */
function positive(v: unknown): v is number {
  return typeof v === 'number' && Number.isFinite(v) && v > 0
}

// --- lane ---

/** The lane a new app consumer starts on, shown preselected: server-admin urgent, tenant-admin bulk. */
export function createLaneDefault(serverAdmin: boolean): ExternalLane {
  return serverAdmin ? 'urgent' : 'bulk'
}

/** Why a tenant-admin's consumer starts on bulk — said, not hidden; null for a server-admin. */
export function createLaneHint(serverAdmin: boolean): string | null {
  return serverAdmin ? null : 'urgent is server-admin only — a server-admin can raise this consumer later.'
}

/**
 * What bulk means for the limits below (review finding 1, config.go
 * XUReserveRatio / ConsumerConcurrencyDefault): a bulk request is admitted only
 * up to the share 1 − gateway.xu_reserve_ratio of each bucket, concurrency at
 * most max(1, ⌊C·(1 − r)⌋). The ratio is a server setting the SPA does not
 * receive (§3 carries no bulk share) — so it is NAMED, never computed.
 */
export function laneShareNote(lane: ExternalLane): string | null {
  return lane === 'bulk'
    ? 'on bulk a request is admitted only up to the share 1 − gateway.xu_reserve_ratio of each limit below (concurrency at least 1); the rest stays reserved for urgent.'
    : null
}

// --- which ops, which source ---

/** The ops of the start-limits block: chat + embed always, rerank only with an offered (enabled) rerank alias. */
export function startOps(offered: readonly ModelAliasView[]): GatewayOp[] {
  const rerank = offered.some((a) => a.kind === 'rerank' && a.enabled)
  return rerank ? [...BASE_OPS, 'rerank'] : [...BASE_OPS]
}

export type LimitsSubject = { subject_kind: 'tenant'; tenant_id: string } | { subject_kind: 'instance' }

/**
 * The gateway-limits-get subject of the prefill: the target tenant; without a
 * tenant id the instance for a server-admin, nothing for a tenant-admin (the
 * instance is server-admin only — 403).
 */
export function limitsSubject(serverAdmin: boolean, tenantId: string | null): LimitsSubject | null {
  if (typeof tenantId === 'string' && tenantId !== '') return { subject_kind: 'tenant', tenant_id: tenantId }
  return serverAdmin ? { subject_kind: 'instance' } : null
}

// --- prefill ---

/**
 * The effective values per op and start dimension out of a gateway-limits-get
 * answer ({success, limits:{ops:{<op>:{effective:{…}}}}}). A positive number is
 * taken; null only for a budget dimension (unbounded); everything else — 0, a
 * string, a null rate, a missing op — is left out (= not reported).
 */
export function startDefaults(answer: unknown): StartDefaults {
  const ops = isObject(answer) && isObject(answer.limits) ? answer.limits.ops : undefined
  if (!isObject(ops)) return {}
  const out: StartDefaults = {}
  for (const op of ['chat', 'embed', 'rerank'] as const) {
    const view = ops[op]
    const eff = isObject(view) ? view.effective : undefined
    if (!isObject(eff)) continue
    const dims: Partial<Record<StartDim, number | null>> = {}
    for (const dim of START_DIMS) {
      const v = eff[dim]
      if (positive(v)) dims[dim] = v
      else if (v === null && BUDGET_DIMS.has(dim)) dims[dim] = null
    }
    out[op] = dims
  }
  return out
}

/**
 * The dimensions a tenant_tenant row caps (source `tenant_tenant` in the limits
 * answer): it narrows every value below it, a server-admin's consumer row
 * included (gatewaylimits.go levels.effective, `tighter`) — review finding 3.
 */
export function tenantCaps(answer: unknown): StartDefaults {
  const ops = isObject(answer) && isObject(answer.limits) ? answer.limits.ops : undefined
  if (!isObject(ops)) return {}
  const out: StartDefaults = {}
  for (const op of ['chat', 'embed', 'rerank'] as const) {
    const view = ops[op]
    if (!isObject(view) || !isObject(view.effective) || !isObject(view.sources)) continue
    const dims: Partial<Record<StartDim, number>> = {}
    for (const dim of START_DIMS) {
      const v = view.effective[dim]
      if (view.sources[dim] === 'tenant_tenant' && positive(v)) dims[dim] = v
    }
    if (Object.keys(dims).length > 0) out[op] = dims
  }
  return out
}

/**
 * The inherited values the block does not edit, as one line per op (review
 * finding 2): monthly tokens, daily requests, monthly cost, external,
 * unmetered external — so nothing a new consumer starts with is unseen.
 */
export function inheritedLines(answer: unknown): Partial<Record<GatewayOp, string>> {
  const ops = isObject(answer) && isObject(answer.limits) ? answer.limits.ops : undefined
  if (!isObject(ops)) return {}
  const out: Partial<Record<GatewayOp, string>> = {}
  for (const op of ['chat', 'embed', 'rerank'] as const) {
    const view = ops[op]
    if (!isObject(view) || !isObject(view.effective)) continue
    const eff = view.effective
    out[op] = EFFECTIVE_FIELDS.filter((f) => !(START_DIMS as readonly string[]).includes(f))
      .map((f) => `${EFFECTIVE_LABEL[f]}: ${fmtEffective(f, eff[f])}`)
      .join(' · ')
  }
  return out
}

/** The inputs of the block, prefilled with the defaults as text (unbounded and unknown stay empty). */
export function prefillInputs(defaults: StartDefaults, ops: readonly GatewayOp[]): StartInputs {
  const out: StartInputs = {}
  for (const op of ops) {
    for (const dim of START_DIMS) {
      const v = defaults[op]?.[dim]
      out[fieldKey(op, dim)] = typeof v === 'number' ? String(v) : ''
    }
  }
  return out
}

export interface StartFieldSpec {
  /** The effective default (number), null = unbounded, undefined = not reported. */
  value: number | null | undefined
  /** The input maximum: a tenant-admin can only narrow (the default); null = no client bound. */
  max: number | null
  /** Not editable (a tenant-admin without a known default — fail-closed). */
  disabled: boolean
  /** What an empty field means. */
  placeholder: string
}

/** How one start field behaves for the caller's tier. */
export function startFieldSpec(defaults: StartDefaults, op: GatewayOp, dim: StartDim, serverAdmin: boolean): StartFieldSpec {
  const value = defaults[op]?.[dim]
  if (value === undefined) {
    return { value, max: null, disabled: !serverAdmin, placeholder: 'not reported' }
  }
  if (value === null) return { value, max: null, disabled: false, placeholder: 'unbounded' }
  return { value, max: serverAdmin ? null : value, disabled: false, placeholder: `inherits ${value.toLocaleString('en-US')}` }
}

// --- the request ---

export interface StartLimitsInput {
  ops: readonly GatewayOp[]
  defaults: StartDefaults
  inputs: StartInputs
  serverAdmin: boolean
}

/**
 * The `limits` of the create request (§3 `L`): only fields of the block's ops
 * whose value differs from the prefill; an empty field inherits. Values are
 * whole numbers > 0; a tenant-admin stays at or under a known default and
 * cannot set a dimension whose default is not reported. null = nothing changed.
 */
export function buildStartLimits(f: StartLimitsInput): { limits: ConsumerCreateLimits | null } | { error: string; field: string } {
  const limits: ConsumerCreateLimits = {}
  for (const op of f.ops) {
    const row: ConsumerStartLimits = {}
    for (const dim of START_DIMS) {
      const key = fieldKey(op, dim)
      const t = (f.inputs[key] ?? '').trim()
      if (t === '') continue
      const name = `${op} ${START_DIM_LABEL[dim]}`
      // plain decimal digits only (review finding 6: Number() takes 0x10, 1e3, and
      // rounds past 2^53 — a value nobody typed would go out)
      if (!/^\d+$/.test(t)) return { error: `${name}: must be a whole number greater than 0`, field: key }
      const n = Number(t)
      if (!Number.isSafeInteger(n) || n <= 0) return { error: `${name}: must be a whole number greater than 0`, field: key }
      const spec = startFieldSpec(f.defaults, op, dim, f.serverAdmin)
      if (typeof spec.value === 'number' && n === spec.value) continue
      if (spec.disabled) return { error: `${name}: no inherited limit known — only a server-admin can set it`, field: key }
      if (spec.max !== null && n > spec.max) return { error: `${name}: at most ${spec.max.toLocaleString('en-US')} (inherited limit; a server-admin can raise it)`, field: key }
      row[dim] = n
    }
    if (Object.keys(row).length > 0) limits[op] = row
  }
  return { limits: Object.keys(limits).length > 0 ? limits : null }
}

// --- templates ---

/** Template start values (Lead decision): concurrency and RPM of the template's op. */
export const TEMPLATE_START: Readonly<Record<string, { op: GatewayOp; values: Partial<Record<StartDim, number>> }>> = {
  'app-chat': { op: 'chat', values: { max_concurrency: 8, rpm: 120 } },
  'app-embedding': { op: 'embed', values: { max_concurrency: 4, rpm: 240 } },
}

/**
 * Apply a template's start values. A tenant-admin cannot widen: a value above
 * the inherited default is clamped to it and the clamp is said in `notes`
 * (keyed like the inputs); without a known default the field is left alone.
 */
export function applyTemplateStart(
  templateId: string,
  inputs: StartInputs,
  defaults: StartDefaults,
  serverAdmin: boolean,
): { inputs: StartInputs; notes: Record<string, string> } {
  const t = TEMPLATE_START[templateId]
  if (t === undefined) return { inputs, notes: {} }
  const next: StartInputs = { ...inputs }
  const notes: Record<string, string> = {}
  for (const dim of START_DIMS) {
    const v = t.values[dim]
    if (v === undefined) continue
    const key = fieldKey(t.op, dim)
    const def = defaults[t.op]?.[dim]
    if (serverAdmin || def === null) {
      next[key] = String(v)
    } else if (def === undefined) {
      notes[key] = `template asks ${v}; no inherited limit known — left unchanged`
    } else if (v > def) {
      next[key] = String(def)
      notes[key] = `template asks ${v}; your limit allows ${def} — a server-admin can raise it`
    } else {
      next[key] = String(v)
    }
  }
  return { inputs: next, notes }
}

// --- the reveal ---

export interface RevealDim {
  dim: EffectiveField
  text: string
  source: string
}

export type RevealOp = { op: GatewayOp; reported: true; dims: RevealDim[] } | { op: GatewayOp; reported: false }

export type RevealLimits = { reported: false } | { reported: true; ops: RevealOp[] }

/**
 * The answer's effective_limits for the given ops (§3 `E`): EVERY field as text
 * with its source; null on a budget = "unbounded", a permission yes/no;
 * anything else unreadable = "not reported". No object at all ⇒ not reported.
 */
export function revealLimits(effective: unknown, ops: readonly GatewayOp[]): RevealLimits {
  if (!isObject(effective)) return { reported: false }
  return {
    reported: true,
    ops: ops.map((op): RevealOp => {
      const e = effective[op]
      if (!isObject(e)) return { op, reported: false }
      const sources = isObject(e.sources) ? e.sources : {}
      return {
        op,
        reported: true,
        dims: EFFECTIVE_FIELDS.map((dim): RevealDim => ({ dim, text: fmtEffective(dim, e[dim]), source: limitSourceLabel(sources[dim]) })),
      }
    }),
  }
}

const SOURCE_LABEL: Readonly<Record<string, string>> = {
  settings: 'from settings',
  default: 'server default',
  instance: 'instance row',
  tenant_server: 'tenant row (server-admin)',
  tenant_tenant: 'tenant row (tenant-admin)',
  consumer_server: 'consumer row (server-admin)',
  consumer_tenant: 'consumer row (tenant-admin)',
}

/** The §3 source vocabulary in words; a newer value as raw text; nothing = "source not reported". */
export function limitSourceLabel(src: unknown): string {
  if (typeof src !== 'string' || src === '') return 'source not reported'
  return Object.prototype.hasOwnProperty.call(SOURCE_LABEL, src) ? SOURCE_LABEL[src] : `source: ${src}`
}

/** The lane of the answer — the closed vocabulary only, else "not reported". */
export function revealLane(lane: unknown): string {
  return isExternalLane(lane) ? lane : 'not reported'
}
