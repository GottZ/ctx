// Live telemetry feed of the status dashboard (S0).
//
// The server collapses ONE tick of llmlog rows into ONE `llmcalls` frame
// (events.go, llmcallsFrameOf) — it no longer pushes a frame per row, which
// overflowed every connection's 16-deep mailbox above ~14 rows and dropped the
// stream. Two shapes ride the name:
//
//   - rows:  {rows: LLMLogEntry[], count}                   — pushed telemetry
//   - bulk:  {kind: 'llmcalls-bulk', count, cursor}         — NO rows: the tick
//     exceeded events.llmcall_coalesce_threshold, so the table refetches over
//     GET /api/llmlog (capped, per-tenant filtered) instead of being pushed.
//
// Split out of StatusPage.svelte as a $state class (mirrors StatusStore) so the
// frame handling is unit-testable DOM-free, and so ONE place decides what an
// unknown frame does: nothing. A stream is a forward-compatible surface — a
// server that learned a new event before this client did must never throw on
// the render path.

import type { LLMLogEntry } from '../../lib/api/types'

/** Rows held in the live list; the fetched page is capped at 50, so this is the
 *  push-side ceiling, not the table's. */
export const LIVE_CAP = 200

export class LlmcallFeed {
  /** Pushed rows, newest first — merged over the table's own fetched history. */
  rows = $state<LLMLogEntry[]>([])
  /** Monotonic refetch signal: every coalesced bulk frame raises it by ONE, so
   *  a 200-row burst costs one refetch, never count-many. */
  refetchToken = $state(0)

  /** Consumes one /api/events frame. Anything that is not a well-formed
   *  `llmcalls` frame — another event name, a malformed payload — is dropped
   *  silently. */
  apply(name: string, data: unknown): void {
    if (name !== 'llmcalls' || data === null || typeof data !== 'object') return
    const frame = data as { kind?: unknown; rows?: unknown }
    if (frame.kind === 'llmcalls-bulk') {
      this.refetchToken++
      return
    }
    if (!Array.isArray(frame.rows) || frame.rows.length === 0) return
    // Server order is created_at ASC; the live list is newest first.
    const pushed = (frame.rows as LLMLogEntry[]).slice().reverse()
    this.rows = [...pushed, ...this.rows].slice(0, LIVE_CAP)
  }
}
