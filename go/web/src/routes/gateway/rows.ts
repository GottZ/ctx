// Pure row-action helpers of the /gateway list (wave W05-06, design/05 §4.5/
// §4.6): the typed confirm tail and the reactivate mirror. Kept apart from
// mint.ts (the create dialog's logic) so the list chunk does not carry the
// dialog's offer/CIDR/expiry code — the dialog is a lazy chunk of its own.
// No clock, no DOM (B-11 scan covers this directory).

import type { ConsumerView } from '../../lib/api/types'

/** The last four characters of a key hint ("ctxm_…a9Fq" ⇒ "a9Fq") — the typed revoke gate. */
export function hintTail(hint: string): string {
  return [...hint].slice(-4).join('')
}

/**
 * What a deactivate confirm requires typed: the tail of the youngest live key,
 * else the label — TRIMMED, because ConfirmDialog.slugMatches trims the typed
 * value and the server stores a label with surrounding blanks as sent (only an
 * all-blank label is refused); untrimmed, such a consumer could never be
 * deactivated here (Fremd-Review W05-06 F2).
 */
export function deactivateTyped(c: ConsumerView): string {
  if (c.keys.length === 0) return c.label.trim()
  const newest = [...c.keys].sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at))[0]
  return hintTail(newest.hint)
}

/**
 * Reactivating is granting (model_consumers.go consumerPolicy.allowed): a
 * server-admin always, a tenant-admin only with self-service (mintModelKeys)
 * and never on a ctx-peer row (V-01-b). Only an inactive consumer.
 */
export function canReactivate(c: ConsumerView, caps: { serverAdmin: boolean; mintModelKeys: boolean }): boolean {
  if (c.active) return false
  return caps.serverAdmin || (c.kind !== 'ctx-peer' && caps.mintModelKeys)
}

/**
 * Does the row carry an "Edit policy" control (RC-D C5 W05-07)? A server-admin
 * on every row; below server-admin never on a ctx-peer (peers are managed by
 * the server-admin, V-01-b mirror). Lives here so the list chunk does not pull
 * policy.ts / mint.ts (re-exported from policy.ts).
 */
export function canEditPolicy(c: ConsumerView, caps: { serverAdmin: boolean }): boolean {
  return caps.serverAdmin || c.kind !== 'ctx-peer'
}
