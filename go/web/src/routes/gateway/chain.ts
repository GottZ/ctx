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
  /** model_key rows are 'key', gateway_limits rows 'limits', everything else (model_consumer) 'consumer'. */
  entity: 'consumer' | 'key' | 'limits'
  /** The key's hint for a key row; null for a consumer row or an unknown key. */
  keyHint: string | null
  /** For a key row: the hint of the key it was rotated from (the chain edge). */
  rotatedFromHint: string | null
  /** Names of the changed fields (old_value vs new_value keys), sorted. */
  fields: string[]
}

/** The keys as a chain, newest first, following rotated_from. */
export function keyChain(keys: readonly KeyView[]): KeyChainLink[] {
  const byId = new Map(keys.map((k) => [k.id, k]))
  const successor = new Map<string, string>()
  for (const k of keys) if (k.rotated_from !== null && byId.has(k.rotated_from)) successor.set(k.rotated_from, k.id)
  // Heads = keys no loaded key was rotated from, newest first (one per chain;
  // a consumer has exactly one chain, a second head only after a data fault).
  const heads = keys
    .filter((k) => !successor.has(k.id))
    .sort((a, b) => (a.created_at < b.created_at ? 1 : a.created_at > b.created_at ? -1 : 0))
  const out: KeyChainLink[] = []
  const seen = new Set<string>()
  for (const head of heads) {
    let cur: KeyView | undefined = head
    while (cur !== undefined && !seen.has(cur.id)) {
      seen.add(cur.id)
      const pred: string | null = cur.rotated_from
      out.push({
        key: cur,
        predecessorId: pred,
        successorId: successor.get(cur.id) ?? null,
        truncated: pred !== null && !byId.has(pred),
      })
      cur = pred === null ? undefined : byId.get(pred)
    }
  }
  return out
}

/** The names of the fields a row changed: the union of both sides' keys whose values differ, sorted. */
function changedFields(oldV: unknown, newV: unknown): string[] {
  const o = oldV !== null && typeof oldV === 'object' ? (oldV as Record<string, unknown>) : {}
  const n = newV !== null && typeof newV === 'object' ? (newV as Record<string, unknown>) : {}
  const names = new Set([...Object.keys(o), ...Object.keys(n)])
  return [...names].filter((k) => JSON.stringify(o[k]) !== JSON.stringify(n[k])).sort()
}

/** The audit rows as display entries, newest first, key rows linked into the chain. */
export function auditEntries(audit: readonly ConsumerAuditRow[], keys: readonly KeyView[]): AuditEntry[] {
  const byId = new Map(keys.map((k) => [k.id, k]))
  return audit.map((row) => {
    const key = row.entity_type === 'model_key' ? byId.get(row.entity_key) : undefined
    const from = key?.rotated_from ?? null
    const via = row.metadata?.via
    return {
      id: row.id,
      at: row.created_at,
      action: row.action,
      actor: row.actor_label,
      via: typeof via === 'string' ? via : null,
      entity: row.entity_type === 'model_key' ? 'key' : row.entity_type === 'gateway_limits' ? 'limits' : 'consumer',
      keyHint: key?.hint ?? null,
      // the edge only on the row that CREATED the rotated key — its other rows
      // (revoke, expiry) are events of a link, not the edge itself
      rotatedFromHint: key !== undefined && row.action === 'create' && from !== null ? (byId.get(from)?.hint ?? null) : null,
      fields: changedFields(row.old_value, row.new_value),
    }
  })
}
