// Type-registry admin form logic (design 04 §4.7, wave U10) — PURE, no runes /
// no DOM, so vitest covers the whole mechanic in the node env (the U10 gate:
// the 422-draft preservation + the builtin guard are proven here, not only
// through the browser). The .svelte.ts suffix only keeps it beside its page.
//
// Declarative field set (the Settings widgetFor precedent, settings.ts:16): the
// policy fields of BlockTypeConfig are a CLOSED set → one descriptor per field,
// so the form is generated, never hand-wired per key. The write path REPLACES
// the whole config envelope server-side (types_write.go putUpdate: config is set
// wholesale, not merged), so toWriteSpec merges the edited scalars back onto the
// ORIGINAL config — a field this UI does not expose (classify.*, intent_patterns,
// link_classes …) is preserved, never silently dropped on an edit.

import { ApiError } from '../../../lib/api'
import type { BlockTypeConfig, BlockTypeView, BlockTypeWriteSpec } from '../../../lib/api/types'

/** retrieval.policy domain (design 01-§3.3 / types.ts BlockTypeConfig). */
export const RETRIEVAL_POLICIES = ['full-pass', 'excluded', 'damped', 'aggregate-to-parent'] as const
export type RetrievalPolicy = (typeof RETRIEVAL_POLICIES)[number]

/** parent.mode domain (structural-parent relationship, e.g. comment→issue). */
export const PARENT_MODES = ['none', 'optional', 'required'] as const
export type ParentMode = (typeof PARENT_MODES)[number]

/** A builtin type is a shipped '_global' row (source badge OR the builtin column
 * OR the '_global' scope — any one is authoritative; the wire sets all three in
 * lockstep). Builtins are operator-protected: the key + deletion are locked in
 * this UI (design §4.7), policy fields stay editable for the server-admin. */
export function isBuiltin(t: Pick<BlockTypeView, 'source' | 'builtin' | 'scope'>): boolean {
  return t.source === 'builtin' || t.builtin === true || t.scope === '_global'
}

/** Whether this UI offers a delete control for the type. Builtins are never
 * deletable (server 409 ErrBlockTypeBuiltin) — the disabled control is the
 * comfort half of the double-layer guard, the server is the gate (§4.7). */
export function canDeleteType(t: Pick<BlockTypeView, 'source' | 'builtin' | 'scope'>): boolean {
  return !isBuiltin(t)
}

/** Compact policy summary for the list row (the registry is ≪ 100 rows, so the
 * row carries the decision-relevant policy at a glance, not a drill-in). */
export function policySummary(t: Pick<BlockTypeView, 'config'>): string {
  const c = t.config ?? { v: 1 }
  const parts: string[] = [c.retrieval?.policy ?? 'full-pass']
  if (c.guard?.check === false) parts.push('guard off')
  if (c.dream?.linkable === false) parts.push('dream off')
  if (c.parent?.mode && c.parent.mode !== 'none') parts.push(`parent ${c.parent.mode}`)
  return parts.join(' · ')
}

/** The editable form state. Numbers are strings so a BLANK field (unlimited /
 * default) is distinct from '0' — the settings/quota-form precedent. The
 * original config is carried so toWriteSpec can preserve unexposed keys. */
export interface TypeFormFields {
  /** The type key. Immutable on edit (URL identity); required + validated on create. */
  name: string
  displayName: string
  description: string
  retrievalPolicy: RetrievalPolicy
  dampingFactor: string
  guardCheck: boolean
  guardCandidate: boolean
  thresholdDuplicate: string
  thresholdReview: string
  dreamLinkable: boolean
  digestInclude: boolean
  overviewInclude: boolean
  parentMode: ParentMode
  /** True when editing an existing row (name locked, is_default frozen). */
  isEdit: boolean
  /** The stored config, preserved so a wholesale-replace write keeps unexposed keys. */
  original: BlockTypeConfig
}

function policyOrDefault(p: string | undefined): RetrievalPolicy {
  return (RETRIEVAL_POLICIES as readonly string[]).includes(p ?? '') ? (p as RetrievalPolicy) : 'full-pass'
}

function parentOrDefault(m: string | undefined): ParentMode {
  return (PARENT_MODES as readonly string[]).includes(m ?? '') ? (m as ParentMode) : 'none'
}

function numToField(v: number | null | undefined): string {
  return v === null || v === undefined ? '' : String(v)
}

/** Blank empty-form fields for the create flow (all-defaults envelope). */
export function emptyFields(): TypeFormFields {
  return {
    name: '',
    displayName: '',
    description: '',
    retrievalPolicy: 'full-pass',
    dampingFactor: '',
    guardCheck: true,
    guardCandidate: true,
    thresholdDuplicate: '',
    thresholdReview: '',
    dreamLinkable: true,
    digestInclude: true,
    overviewInclude: true,
    parentMode: 'none',
    isEdit: false,
    original: { v: 1 },
  }
}

/** A loaded type → its editable fields (the seed on Edit). Unknown/missing config
 * fields render as their documented default, never crash (forward-compat). */
export function fieldsFromType(t: BlockTypeView): TypeFormFields {
  const c = t.config ?? { v: 1 }
  return {
    name: t.name,
    displayName: t.display_name ?? '',
    description: t.description ?? '',
    retrievalPolicy: policyOrDefault(c.retrieval?.policy),
    dampingFactor: numToField(c.retrieval?.damping_factor),
    guardCheck: c.guard?.check ?? true,
    guardCandidate: c.guard?.candidate ?? true,
    thresholdDuplicate: numToField(c.guard?.threshold_duplicate),
    thresholdReview: numToField(c.guard?.threshold_review),
    dreamLinkable: c.dream?.linkable ?? true,
    digestInclude: c.digest?.include ?? true,
    overviewInclude: c.overview?.include ?? true,
    parentMode: parentOrDefault(c.parent?.mode),
    isEdit: true,
    original: c,
  }
}

/** Parse a 0..1 factor/threshold text field → number | null. A blank field is
 * `null` (no threshold / server default), NEVER 0. A non-numeric or out-of-range
 * entry throws so the form blocks submit and shows the message; the server 422 is
 * the final authority, this is the client mirror that never round-trips garbage. */
export function parseFactor(raw: string): number | null {
  const t = raw.trim()
  if (t === '') return null
  const n = Number(t)
  if (!Number.isFinite(n) || n < 0 || n > 1) {
    throw new Error('enter a number between 0 and 1, or leave blank for the default')
  }
  return n
}

/** The form fields → the PUT body. The config is the ORIGINAL envelope with the
 * edited scalars merged on top (wholesale-replace-safe: unexposed keys survive).
 * Throws on a bad numeric field (surfaced on the form before any request). */
export function toWriteSpec(f: TypeFormFields): BlockTypeWriteSpec {
  const damping = parseFactor(f.dampingFactor)
  const config: BlockTypeConfig = {
    ...f.original,
    v: 1,
    retrieval: {
      ...f.original.retrieval,
      policy: f.retrievalPolicy,
      ...(damping === null ? {} : { damping_factor: damping }),
    },
    guard: {
      ...f.original.guard,
      check: f.guardCheck,
      candidate: f.guardCandidate,
      threshold_duplicate: parseFactor(f.thresholdDuplicate),
      threshold_review: parseFactor(f.thresholdReview),
    },
    dream: { ...f.original.dream, linkable: f.dreamLinkable },
    digest: { ...f.original.digest, include: f.digestInclude },
    overview: { ...f.original.overview, include: f.overviewInclude },
    parent: { ...f.original.parent, mode: f.parentMode },
  }
  const spec: BlockTypeWriteSpec = {
    display_name: f.displayName.trim(),
    description: f.description.trim(),
    config,
  }
  // is_default is a server-side two-row swap the write path refuses to change on
  // an existing type (loud 422); the form never sends it on edit.
  return spec
}

/** A Wirkungs-Hinweis (§4.7): the UI EXPLAINS a policy consequence, it does not
 * decide it. Returns a note for the notable, non-default settings only. */
export function effectHint(f: TypeFormFields): string[] {
  const hints: string[] = []
  if (f.retrievalPolicy === 'excluded') {
    hints.push('retrieval excluded — blocks of this type never surface in search/ask results')
  } else if (f.retrievalPolicy === 'damped') {
    hints.push('retrieval damped — this type is down-weighted in ranking (set a damping factor)')
  } else if (f.retrievalPolicy === 'aggregate-to-parent') {
    hints.push('aggregate-to-parent — retrieval folds these into their structural parent')
  }
  if (!f.guardCheck) hints.push('guard check off — new blocks of this type skip duplicate detection')
  if (!f.dreamLinkable) hints.push('dream off — the dream pass never links blocks of this type')
  if (f.parentMode === 'required') hints.push('parent required — a block of this type must reference a structural parent')
  return hints
}

/** The submit-error contract (the 422-draft mechanic, §4.7): EVERY write error
 * keeps the modal open and the input intact — nothing is ever silently lost. A
 * 422/400 (validation) is a field-class error rendered at the form; any other
 * status is a form-level banner. `count` carries the 409 in-use active/archived
 * numbers off ApiError.details for a delete conflict. */
export interface SubmitError {
  message: string
  /** 'field' = validation (422/400) shown inline; 'form' = banner (403/409/5xx/network). */
  kind: 'field' | 'form'
  /** Never close the modal or reset the draft on a submit error. Always true. */
  keepOpen: true
}

export function submitErrorFrom(err: unknown): SubmitError {
  if (err instanceof ApiError) {
    const kind: SubmitError['kind'] = err.status === 422 || err.status === 400 ? 'field' : 'form'
    return { message: err.message, kind, keepOpen: true }
  }
  return { message: err instanceof Error ? err.message : String(err), kind: 'form', keepOpen: true }
}

/** The 409 in-use counts off a delete conflict (types_write.go BlockTypeInUseError
 * → {active, archived} on the error envelope), for the delete confirm dialog. */
export function inUseCounts(err: unknown): { active: number; archived: number } | null {
  if (!(err instanceof ApiError) || err.status !== 409 || err.details === null) return null
  const active = err.details.active
  const archived = err.details.archived
  if (typeof active === 'number' || typeof archived === 'number') {
    return { active: typeof active === 'number' ? active : 0, archived: typeof archived === 'number' ? archived : 0 }
  }
  return null
}
