// Write-scope derivation for the workflow surface (design 04 §5.3 / N3, wave
// U06). Pure so the fail-closed matrix is asserted in node vitest.
//
// IST DEVIATION (design §3.1 vs. the shipped wire): §3.1 ordered a first-class
// `writable` flag on B1/B2/B7 (server-computed for the caller-key in the block's
// scope). The shipped W6 detail handler does NOT carry it (the freeze golden
// documents the absent field). §5.3 is explicit that this is the ONE place the
// UI must NOT guess from scope names — but with no wire flag, the honest,
// fail-closed derivation is the SAME authority model the server enforces and the
// shipped block editor already uses (lib/blocks/edit.ts canEdit): a block is
// writable only when its scope is one the caller may WRITE, i.e. the caller's
// home_scope, or 'shared' when the key is granted it (writableBlockScopes,
// internal/handler/context_store.go:276-282). server-admin's _global write-scope
// is deliberately NOT writable-to-shared here — same as the server.
//
// This is UX only: the server stays authoritative. A 403/422 on a mutation
// despite writable:true (a race or a mid-session policy change) is surfaced as an
// error with the selection retained (§4.5) — writable never suppresses the guard.
//
// When the wire grows the real `writable` field, this derivation is replaced by
// reading it; the read-only render path and every gate stay identical.

/** The caller identity slice needed to decide write access (auth.svelte fields). */
export interface WriteIdentity {
  homeScope: string | null
  readScopes: string[]
}

/**
 * True when the caller may WRITE to `scope`. Fail-closed: an empty scope, an
 * unresolved identity (home_scope null before whoami) or a foreign read-only
 * scope all return false. Mirrors writableBlockScopes (N3): home ∪ shared-if-
 * granted.
 */
export function canWriteScope(scope: string | undefined | null, id: WriteIdentity): boolean {
  if (!scope || id.homeScope === null) return false
  if (scope === id.homeScope) return true
  if (scope === 'shared' && id.readScopes.includes('shared')) return true
  return false
}
