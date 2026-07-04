// Forge sync-state badge model (design 04 §3.1/§5.3/§5.4, wave U06). Pure so
// the ×5-known + unknown-fallback matrix is asserted in node vitest without a
// DOM (the §5.5 Sync-Badge contract line).
//
// IST DEVIATION (design §3.1 vs. the shipped wire): the §3.1 draft carried a
// first-class `sync_state` field on every row. The shipped W6 handler serializes
// the raw block (IssueBlock, store/blocks.go) which has NO sync_state column —
// the contract-freeze golden test (contract_freeze_golden_test.go) documents its
// absence explicitly. The forge sync path records the 3-way content-hash verdict
// (Vision W16, NOT a timestamp) into the block METADATA, so this module reads
// `metadata.sync_state`. When the wire grows a first-class field, only the
// extractor below moves; the badge mapping stays.
//
// Fail-closed by construction (§5.3/§5.4): a value OUTSIDE the closed 5-set —
// including the Fixture 'garbage' probe — maps to the `unknown` badge rendered in
// conflict optics (the loudest state, the documented SIXTH render state), never
// silently dropped and never crashing. An ABSENT value means a ctx-native issue
// that was never involved in a sync → `local` (the union member for exactly that).

/** The closed set of forge sync verdicts (design 04 §3.1). */
export type SyncState = 'local' | 'in_sync' | 'ctx_ahead' | 'forge_ahead' | 'conflict'

/** Semantic tone → the badge's token class (design 04 §5.1 semantic colours). */
export type SyncTone = 'neutral' | 'ok' | 'warn' | 'danger'

export interface SyncBadge {
  /** The raw state key (one of the 5, or 'unknown' for an off-union value). */
  state: SyncState | 'unknown'
  /** Human label rendered in the badge. */
  label: string
  /** Longer description for the title/aria (why this state matters). */
  hint: string
  /** Token-class selector (drives the badge colour). */
  tone: SyncTone
  /** False for the off-union fallback — the conflict-optics unknown state. */
  known: boolean
}

const KNOWN: Record<SyncState, { label: string; hint: string; tone: SyncTone }> = {
  local: { label: 'local', hint: 'ctx-only — not linked to a forge issue', tone: 'neutral' },
  in_sync: { label: 'in sync', hint: 'ctx and the forge issue match', tone: 'ok' },
  ctx_ahead: { label: 'ctx ahead', hint: 'ctx has unpushed changes', tone: 'warn' },
  forge_ahead: { label: 'forge ahead', hint: 'the forge has changes not yet pulled', tone: 'warn' },
  conflict: { label: 'conflict', hint: 'ctx and the forge diverged — both changed', tone: 'danger' },
}

function isSyncState(v: string): v is SyncState {
  return v === 'local' || v === 'in_sync' || v === 'ctx_ahead' || v === 'forge_ahead' || v === 'conflict'
}

/**
 * Badge descriptor for a raw metadata value (`metadata.sync_state`).
 *   - absent / non-string / empty  → `local`   (native, never synced)
 *   - one of the 5 known verdicts   → that badge
 *   - any other string ('garbage')  → `unknown` in conflict optics (§5.4)
 */
export function syncBadgeFor(raw: unknown): SyncBadge {
  if (typeof raw !== 'string' || raw.trim() === '') {
    return { state: 'local', known: true, ...KNOWN.local }
  }
  const v = raw.trim()
  if (isSyncState(v)) return { state: v, known: true, ...KNOWN[v] }
  return {
    state: 'unknown',
    known: false,
    label: 'unknown',
    hint: `unrecognised sync state '${v}' — treated as a conflict`,
    tone: 'danger',
  }
}

/** Extract the raw sync-state value from an issue block's metadata. */
export function syncBadgeForIssue(metadata: Record<string, unknown> | undefined | null): SyncBadge {
  return syncBadgeFor(metadata?.['sync_state'])
}
