// Policy edit of a model consumer — pure field-tier logic of the
// ConsumerEditDialog (design/05 §4.5 Feld-Tier-UX, §4.2 row "Policy ändern";
// wave W05-07 — RC-D C5). The UI MIRRORS the server rules of
// internal/handler/model_consumers.go (field tiers, consumerPolicy.allowed);
// the server stays the authority (a refusal is a 403 at the dialog).
//
//   field / caller            server-admin   tenant-admin, app          tenant-admin, ctx-peer
//   edit control on the row   yes            yes                        none (managed by server-admin)
//   prompt_sensitivity        every value    ≥ current; lower disabled  —
//                                            "server-admin only"
//   models widen / narrow     yes / yes      mintModelKeys / yes        —
//   all_models false → true   yes            mintModelKeys              —
//   tenant_scope set/change   yes            mintModelKeys (→ null: yes) —
//   allowed_cidrs             free           narrow only; "any source"  —
//                                            and prefixes outside the
//                                            stored list need mintModelKeys
//   lane                      lane.ts        lane.ts (only lower)       —
//
// Sensitivity order (strictest first): credentials > personal > internal >
// public — "lower" = towards public.
//
// Pure module: no DOM, no clock (the B-11 scan covers this directory).

import type { PromptSensitivity } from '../../lib/api/modelapi'
import type { ConsumerUpdateSpec, ConsumerView, ExternalLane } from '../../lib/api/types'
import { isExternalLane } from './lane'
import { parseCidrs } from './mint'
import { canEditPolicy } from './rows'

/** What the caller may do (session.caps). */
export interface EditCaps {
  serverAdmin: boolean
  mintModelKeys: boolean
}

/** One sensitivity option of the edit dialog. */
export interface SensitivityOption {
  value: PromptSensitivity
  disabled: boolean
  /** "server-admin only" on a disabled lower value. */
  note?: string
}

/** The field tiers of one consumer for one caller. */
export interface EditTiers {
  /** No edit control at all (ctx-peer row below server-admin). */
  locked: boolean
  sensitivity: SensitivityOption[]
  /** Unchecked model aliases may be checked (widening). */
  mayWidenModels: boolean
  /** "all offered incl. future" may be switched on. */
  mayEnableAllModels: boolean
  /** tenant_scope may be set or changed (null stays allowed: narrowing). */
  mayChangeScope: boolean
  /** The allowlist may be dropped (null = any source) or get prefixes outside the stored list. */
  mayWidenCidrs: boolean
  /** The lane may be raised bulk → urgent (server-admin only, lane.ts mirrors it per option). */
  mayRaiseLane: boolean
}

/** Strictest first — "lower" = a higher index (credentials > personal > internal > public). */
export const SENSITIVITY_ORDER: readonly PromptSensitivity[] = ['credentials', 'personal', 'internal', 'public']

/** Rank of a sensitivity; an unknown value counts as the strictest (nothing below it is offered). */
function sensRank(s: string): number {
  const i = SENSITIVITY_ORDER.indexOf(s as PromptSensitivity)
  return i < 0 ? 0 : i
}

// canEditPolicy lives in rows.ts (the list chunk; this module and mint.ts stay
// in the lazy edit-dialog chunk) and is re-exported here as part of the
// policy contract.
export { canEditPolicy }

/** The field tiers of `c` for `caps` (the UI mirror of model_consumers.go grant / serverAdminOnly). */
export function editTiers(c: ConsumerView, caps: EditCaps): EditTiers {
  const locked = !canEditPolicy(c, caps)
  const cur = sensRank(c.prompt_sensitivity)
  const sensitivity = SENSITIVITY_ORDER.map((value): SensitivityOption => {
    if (locked) return { value, disabled: true, note: 'managed by the server-admin' }
    if (caps.serverAdmin || sensRank(value) <= cur) return { value, disabled: false }
    return { value, disabled: true, note: 'server-admin only' }
  })
  const grant = !locked && (caps.serverAdmin || caps.mintModelKeys)
  return {
    locked,
    sensitivity,
    mayWidenModels: grant,
    mayEnableAllModels: grant,
    mayChangeScope: grant,
    mayWidenCidrs: grant,
    mayRaiseLane: !locked && caps.serverAdmin,
  }
}

// --- CIDR containment (mirror of the handler's cidrsNarrow over netip.Prefix) ---

interface Prefix {
  v4: boolean
  addr: bigint
  bits: number
}

function parseV4(a: string): bigint | null {
  const parts = a.split('.')
  if (parts.length !== 4) return null
  let n = 0n
  for (const p of parts) {
    if (!/^\d{1,3}$/.test(p)) return null
    const v = Number(p)
    if (v > 255) return null
    n = (n << 8n) | BigInt(v)
  }
  return n
}

function parseV6(a: string): bigint | null {
  if (!/^[0-9a-fA-F:.]+$/.test(a)) return null
  const dbl = a.split('::')
  if (dbl.length > 2) return null
  const side = (x: string): number[] | null => {
    if (x === '') return []
    const out: number[] = []
    const groups = x.split(':')
    for (let i = 0; i < groups.length; i++) {
      const g = groups[i] ?? ''
      if (g.includes('.')) {
        if (i !== groups.length - 1) return null
        const v4 = parseV4(g)
        if (v4 === null) return null
        out.push(Number(v4 >> 16n), Number(v4 & 0xffffn))
      } else {
        if (!/^[0-9a-fA-F]{1,4}$/.test(g)) return null
        out.push(parseInt(g, 16))
      }
    }
    return out
  }
  const head = side(dbl[0] ?? '')
  const tail = dbl.length === 2 ? side(dbl[1] ?? '') : []
  if (head === null || tail === null) return null
  const fill = 8 - head.length - tail.length
  if (dbl.length === 2 ? fill < 1 : fill !== 0) return null
  const groups = [...head, ...new Array<number>(dbl.length === 2 ? fill : 0).fill(0), ...tail]
  let n = 0n
  for (const g of groups) n = (n << 16n) | BigInt(g)
  return n
}

/** Parse "addr/bits" and mask it (netip.ParsePrefix + Masked); null = not a prefix. */
export function parsePrefix(s: string): Prefix | null {
  const slash = s.lastIndexOf('/')
  if (slash < 0) return null
  const addrText = s.slice(0, slash)
  const bitsText = s.slice(slash + 1)
  if (!/^\d{1,3}$/.test(bitsText)) return null
  const bits = Number(bitsText)
  const v4 = !addrText.includes(':')
  const width = v4 ? 32 : 128
  if (bits > width) return null
  const addr = v4 ? parseV4(addrText) : parseV6(addrText)
  if (addr === null) return null
  const mask = bits === 0 ? 0n : ((1n << BigInt(bits)) - 1n) << BigInt(width - bits)
  return { v4, addr: addr & mask, bits }
}

function contains(outer: Prefix, inner: Prefix): boolean {
  if (outer.v4 !== inner.v4 || outer.bits > inner.bits) return false
  const width = outer.v4 ? 32 : 128
  const mask = outer.bits === 0 ? 0n : ((1n << BigInt(outer.bits)) - 1n) << BigInt(width - outer.bits)
  return (inner.addr & mask) === outer.addr
}

/**
 * True iff `next` widens the stored allowlist `stored` (the handler's
 * widens(): null over a stored list, or a prefix of `next` that lies inside
 * no stored prefix). A list over a stored null narrows; null over null does
 * not widen. An unparsable entry of `next` counts as widening (fail-closed).
 */
export function cidrsWiden(stored: readonly string[] | null, next: readonly string[] | null): boolean {
  if (stored === null) return false
  if (next === null) return true
  const have = stored.map(parsePrefix).filter((p): p is Prefix => p !== null)
  return next.some((n) => {
    const np = parsePrefix(n)
    return np === null || !have.some((o) => contains(o, np))
  })
}

/** The dialog's field values. */
export interface EditFormInput {
  label: string
  description: string
  /** null = only _global. */
  scope: string | null
  allModels: boolean
  /** Alias ids. */
  models: readonly string[]
  sensitivity: PromptSensitivity
  lane: ExternalLane
  /** Raw allowlist text; '' = any source. */
  cidrsText: string
}

/** The form's start values for `c` (what the dialog opens with). */
export function editFormFrom(c: ConsumerView): EditFormInput {
  return {
    label: c.label,
    description: c.description,
    scope: c.tenant_scope,
    allModels: c.all_models,
    models: [...c.models],
    sensitivity: (SENSITIVITY_ORDER as readonly string[]).includes(c.prompt_sensitivity) ? (c.prompt_sensitivity as PromptSensitivity) : 'credentials',
    lane: isExternalLane(c.admission_lane) ? c.admission_lane : 'bulk',
    cidrsText: (c.allowed_cidrs ?? []).join('\n'),
  }
}

function sameSet(a: readonly string[], b: readonly string[]): boolean {
  const x = new Set(a)
  const y = new Set(b)
  return x.size === y.size && [...x].every((v) => y.has(v))
}

function sameList(a: readonly string[] | null, b: readonly string[] | null): boolean {
  if (a === null || b === null) return a === b
  return a.length === b.length && a.every((v, i) => v === b[i])
}

/** The message of a change that is not "nothing changed" — what the dialog shows under the form. */
export const NOTHING_CHANGED = 'nothing changed'

/**
 * The model-consumer-update patch: ONLY the fields that differ from `c`
 * (models compared as sets, sorted ids on the wire; the CIDR text parsed like
 * the create dialog), or the first client-side error — including a change the
 * tiers forbid, so it never reaches the wire. No change ⇒ NOTHING_CHANGED.
 */
export function buildUpdatePatch(c: ConsumerView, f: EditFormInput, tiers: EditTiers): { patch: ConsumerUpdateSpec } | { error: string } {
  if (tiers.locked) return { error: 'this consumer is managed by the server-admin' }
  const patch: ConsumerUpdateSpec = {}

  const label = f.label.trim()
  if (label === '') return { error: 'label is required' }
  if ([...label].length > 100) return { error: 'label must be at most 100 characters' }
  if (label !== c.label) patch.label = label

  const description = f.description.trim()
  if ([...description].length > 1000) return { error: 'description must be at most 1000 characters' }
  if (description !== c.description) patch.description = description

  if (f.scope !== c.tenant_scope) {
    if (f.scope !== null && !tiers.mayChangeScope) return { error: 'setting or changing the scope widens access — self-service is off, ask a server-admin' }
    patch.tenant_scope = f.scope
  }

  if (f.allModels !== c.all_models) {
    if (f.allModels && !tiers.mayEnableAllModels) return { error: 'granting all offered models widens access — self-service is off, ask a server-admin' }
    patch.all_models = f.allModels
  }

  if (!sameSet(f.models, c.models)) {
    const added = f.models.filter((m) => !c.models.includes(m))
    if (added.length > 0 && !tiers.mayWidenModels) return { error: 'granting another model widens access — self-service is off, ask a server-admin' }
    patch.models = [...new Set(f.models)].sort()
  }

  if (f.sensitivity !== c.prompt_sensitivity) {
    const opt = tiers.sensitivity.find((o) => o.value === f.sensitivity)
    if (opt === undefined || opt.disabled) return { error: `lowering the prompt sensitivity to ${f.sensitivity} is server-admin only` }
    patch.prompt_sensitivity = f.sensitivity
  }

  if (f.lane !== c.admission_lane) {
    if (f.lane === 'urgent' && !tiers.mayRaiseLane) return { error: 'raising the lane to urgent is server-admin only' }
    patch.admission_lane = f.lane
  }

  const parsed = parseCidrs(f.cidrsText)
  if (parsed.errors.length > 0) return { error: parsed.errors.join('; ') }
  if (!sameList(parsed.cidrs, c.allowed_cidrs)) {
    if (cidrsWiden(c.allowed_cidrs, parsed.cidrs) && !tiers.mayWidenCidrs) {
      return { error: 'widening the network allowlist needs self-service — ask a server-admin' }
    }
    patch.allowed_cidrs = parsed.cidrs
  }

  return Object.keys(patch).length === 0 ? { error: NOTHING_CHANGED } : { patch }
}
