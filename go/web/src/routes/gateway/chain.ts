// Key chain + audit trail presentation (RC-D C6, wave W05-09, design/05 §3.2
// "Audit-Spur", §7 gate "Audit zeigt Rotation als Kette (rotated_from)").
// Pure and DOM-free so vitest covers it in node.
//
//   keyChain(keys)            the consumer's keys as a chain: newest first,
//                             each link names its predecessor (rotated_from)
//                             and its successor; a key whose rotated_from
//                             points outside the loaded keys is a chain start
//                             with `truncated: true` (keys_truncated cut it).
//   auditEntries(audit, keys) the audit rows as display entries: entity
//                             'consumer' | 'key', the key's hint, and for a
//                             key row the hint of the key it was rotated from
//                             — so a rotation reads as "ctxm_…new ← ctxm_…old",
//                             never as two unrelated key events. Field NAMES
//                             only (design/05 §3.2: never raw values of a key
//                             row).
//
// RC-D S1: contract stubs — both answer [] (fail-closed: nothing shown).

import type { ConsumerAuditRow, KeyView } from '../../lib/api/types'

/** One link of the key chain. */
export interface KeyChainLink {
  key: KeyView
  /** id of the key this one was rotated from (null = first key of the chain). */
  predecessorId: string | null
  /** id of the key rotated from this one (null = newest). */
  successorId: string | null
  /** rotated_from points at a key outside the loaded chain (keys_truncated). */
  truncated: boolean
}

/** One display entry of the audit trail. */
export interface AuditEntry {
  id: string
  at: string
  action: string
  /** actor_label as the server masked it; null = masked / unknown. */
  actor: string | null
  /** metadata.via (manage | cli | web | …) when the server recorded it. */
  via: string | null
  entity: 'consumer' | 'key'
  /** The key's hint for a key row; null for a consumer row or an unknown key. */
  keyHint: string | null
  /** For a key row: the hint of the key it was rotated from (the chain edge). */
  rotatedFromHint: string | null
  /** Names of the changed fields (old_value vs new_value keys), sorted. */
  fields: string[]
}

/** The keys as a chain, newest first, following rotated_from. */
export function keyChain(keys: readonly KeyView[]): KeyChainLink[] {
  void keys
  return []
}

/** The audit rows as display entries, newest first, key rows linked into the chain. */
export function auditEntries(audit: readonly ConsumerAuditRow[], keys: readonly KeyView[]): AuditEntry[] {
  void audit
  void keys
  return []
}
