// Pure helpers for the block editor (block-workbench W4, design plan §W4).
// The sensitivity ranks + downgrade detection mirror backends/trust.go for UX
// only — the server stays authoritative (handler/context_manage.go
// applySensitivityGuard answers 400 without confirm_sensitivity_downgrade). The
// editable draft, the create payload, the update diff (a patch carries ONLY
// changed fields, mirroring store.UpdateBlockData's omitempty/diff semantics)
// and the scope-editability gate all live here so vitest covers them without a
// DOM (lib/backends.ts pattern).

import type { BlockDetail, StoreBlockRequest, UpdateBlockData } from '../api/blocks'

/**
 * The editable surface of the block dialog. category/title/content are required
 * by POST /api/store (context_store.go:66); tags is a set; scope is constrained
 * to home_scope (+'shared' on grant) by the server; sensitivity is one of the
 * four ValidSensitivity levels or '' for "leave to the server default" (create:
 * omit ⇒ pool.default_block_sensitivity; edit: omit ⇒ no sensitivity key).
 */
export interface BlockDraft {
  category: string
  title: string
  content: string
  tags: string[]
  scope: string
  /** '' = unset (omitted on the wire); else credentials|personal|internal|public. */
  sensitivity: string
}

/**
 * Sensitivity levels, most→least sensitive, mirroring backends/trust.go
 * (credentials rank 3 … public rank 0). A DOWNGRADE (rank falls) opens the
 * block to less-trusted backends and is the confirm-gated direction (F3 §3.5).
 */
export interface SensitivityLevel {
  value: string
  rank: number
  tip: string
}
export const SENSITIVITY_LEVELS: SensitivityLevel[] = [
  { value: 'credentials', rank: 3, tip: 'passwords/secrets — only full-trust backends may see this' },
  { value: 'personal', rank: 2, tip: 'personal data — no public cloud backend' },
  { value: 'internal', rank: 1, tip: 'internal material — no credentials/PII' },
  { value: 'public', rank: 0, tip: 'safe to expose publicly — e.g. prompt benchmarks' },
]

const SENSITIVITY_RANK = new Map(SENSITIVITY_LEVELS.map((s) => [s.value, s.rank]))

/**
 * Rank of a sensitivity level; -1 for unknown. The UI never feeds an unknown
 * value to the server (the <select> is closed over SENSITIVITY_LEVELS), so -1
 * is purely a guard — and it makes isSensitivityDowngrade safe for free-text.
 */
export function sensitivityRank(sensitivity: string): number {
  return SENSITIVITY_RANK.get(sensitivity) ?? -1
}

/**
 * True when `next` is LESS sensitive than `current` (rank fell) — the server
 * demands confirm_sensitivity_downgrade then (applySensitivityGuard 400s when
 * newRank < curRank). Equal or rising is free. An empty/unset `next` is never a
 * downgrade: the editor omits the sensitivity key entirely, so the server
 * never re-classifies and the guard never fires.
 */
export function isSensitivityDowngrade(current: string, next: string): boolean {
  if (next === '') return false
  return sensitivityRank(next) < sensitivityRank(current)
}

/**
 * A block is editable (store/update is scope-gated, NOT admin) ONLY when its
 * scope equals the caller's home_scope — a foreign-scope hit reachable via
 * read_scopes is read-only (the server resolves ids in HomeScope only,
 * context_manage.go:442).
 */
export function canEdit(blockScope: string, homeScope: string): boolean {
  return blockScope === homeScope
}

/**
 * Build the POST /api/store body from a create draft. category/title/content
 * always present; tags always sent; scope sent only when non-empty (else the
 * server defaults to home_scope); sensitivity sent only when chosen (else the
 * server applies pool.default_block_sensitivity, source='default').
 */
export function createStoreRequest(draft: BlockDraft): StoreBlockRequest {
  const req: StoreBlockRequest = {
    category: draft.category,
    title: draft.title,
    content: draft.content,
    tags: draft.tags,
  }
  if (draft.scope !== '') req.scope = draft.scope
  if (draft.sensitivity !== '') req.sensitivity = draft.sensitivity
  return req
}

/** Order-insensitive set equality (tags). */
function sameSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false
  const sa = [...a].sort()
  const sb = [...b].sort()
  return sa.every((v, i) => v === sb[i])
}

/**
 * An update patch carrying ONLY the changed fields (store.UpdateBlockData) —
 * unchanged fields are omitted so the server leaves them untouched. tags is an
 * order-insensitive set; an emptied sensitivity is treated as "no change"
 * (never sent as a clear — the server has no "unset" for it). confirm_-
 * sensitivity_downgrade is added by the caller/model, NOT here.
 */
export function blockDiff(original: BlockDraft, draft: BlockDraft): UpdateBlockData {
  const data: UpdateBlockData = {}
  if (draft.category !== original.category) data.category = draft.category
  if (draft.title !== original.title) data.title = draft.title
  if (draft.content !== original.content) data.content = draft.content
  if (!sameSet(draft.tags, original.tags)) data.tags = draft.tags
  if (draft.scope !== original.scope) data.scope = draft.scope
  // An emptied sensitivity is "leave it" — no sensitivity key on the wire.
  if (draft.sensitivity !== '' && draft.sensitivity !== original.sensitivity) {
    data.sensitivity = draft.sensitivity
  }
  return data
}

/** True when an update patch has no field changes (skip the round-trip). */
export function isEmptyDiff(data: UpdateBlockData): boolean {
  return Object.keys(data).length === 0
}

/**
 * Seed the edit dialog's initial draft from a loaded BlockDetail. The W4 dialog
 * needs the block's CURRENT sensitivity pre-filled so the user can lower it and
 * the editor recognizes the downgrade (isSensitivityDowngrade) and adds
 * confirm_sensitivity_downgrade — i.e. so the downgrade-confirm is REACHABLE
 * from the /blocks page at all. Before W6 the detail block carried no
 * sensitivity, so the page seeded '' and the downgrade flow was unreachable.
 *
 * sensitivity falls back to '' only when the server truly omitted it (old/
 * unclassified row) — then the editor leaves the level unset and no
 * re-classification is sent.
 */
export function editInitialFromDetail(block: BlockDetail): Partial<BlockDraft> {
  return {
    category: block.category,
    title: block.title,
    content: block.content,
    tags: block.tags,
    scope: block.scope,
    sensitivity: block.sensitivity ?? '',
  }
}
