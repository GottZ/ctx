// LlmcallFeed gates (S0). The server no longer pushes ONE `llmcall` frame per
// telemetry row — a burst collapsed every open panel's 16-deep mailbox — but ONE
// `llmcalls` frame per tick: rows below the coalesce threshold, a content-free
// {kind:'llmcalls-bulk', count, cursor} refetch signal above it.
//
// RED against Ist: StatusPage.svelte:32-43 handled only the per-row `llmcall`
// name inline (no module, no bulk branch), so a coalesced frame was silently
// dropped and a bulk frame never reached the table.

import { describe, expect, it } from 'vitest'
import type { LLMLogEntry } from '../../lib/api/types'
import { LlmcallFeed, LIVE_CAP } from './llmcall-feed.svelte'

function row(id: string, createdAt = '2026-07-28T10:00:00Z'): LLMLogEntry {
  return {
    id,
    created_at: createdAt,
    pipeline: 'query',
    model: 'qwen3.5:9b',
    backend: 'local',
    duration_ms: 12,
    error: null,
    prompt_tokens: null,
    completion_tokens: null,
    cost_usd: null,
    queue_wait_ms: null,
    dispatch_class: null,
    dispatch_abort: null,
  }
}

describe('LlmcallFeed — rows frame', () => {
  it('fills the live list from a synthetic llmcalls stream, newest first', () => {
    const feed = new LlmcallFeed()
    // Server order is created_at ASC (fetchLLMCalls) — the newest row of the
    // tick must end up at the head of the live list.
    feed.apply('llmcalls', { rows: [row('a'), row('b'), row('c')], count: 3 })
    feed.apply('llmcalls', { rows: [row('d')], count: 1 })
    expect(feed.rows.map((e) => e.id)).toEqual(['d', 'c', 'b', 'a'])
    expect(feed.refetchToken).toBe(0)
  })

  it('caps the live list', () => {
    const feed = new LlmcallFeed()
    const rows = Array.from({ length: LIVE_CAP + 40 }, (_, i) => row(`r${i}`))
    feed.apply('llmcalls', { rows, count: rows.length })
    expect(feed.rows).toHaveLength(LIVE_CAP)
  })
})

describe('LlmcallFeed — bulk frame', () => {
  it('a content-free bulk frame raises the refetch token instead of pushing rows', () => {
    const feed = new LlmcallFeed()
    feed.apply('llmcalls', { rows: [row('a')], count: 1 })
    feed.apply('llmcalls', { kind: 'llmcalls-bulk', count: 64, cursor: '2026-07-28T10:00:00Z|row-063' })
    expect(feed.refetchToken).toBe(1)
    expect(feed.rows.map((e) => e.id)).toEqual(['a']) // no rows pushed by the bulk frame
  })

  it('every bulk frame is one refetch, never count-many', () => {
    const feed = new LlmcallFeed()
    feed.apply('llmcalls', { kind: 'llmcalls-bulk', count: 200, cursor: 'x|y' })
    feed.apply('llmcalls', { kind: 'llmcalls-bulk', count: 200, cursor: 'x|z' })
    expect(feed.refetchToken).toBe(2)
  })
})

describe('LlmcallFeed — tolerance', () => {
  it('drops an unknown event name silently', () => {
    const feed = new LlmcallFeed()
    expect(() => feed.apply('llmcall', row('legacy'))).not.toThrow() // the retired per-row name
    expect(() => feed.apply('quota', { rows: [row('x')], count: 1 })).not.toThrow()
    // The S3 heartbeat lands here too: StatusPage routes every name it does not
    // handle into the feed, so `hb` must be a silent drop until S4 claims it.
    expect(() =>
      feed.apply('hb', { last_good_at: '2026-07-28T10:00:00Z', degraded: false, health: 'ok' }),
    ).not.toThrow()
    expect(feed.rows).toHaveLength(0)
    expect(feed.refetchToken).toBe(0)
  })

  it('drops a malformed payload silently', () => {
    const feed = new LlmcallFeed()
    for (const bad of [null, undefined, 'nope', 42, {}, { rows: 'not-an-array' }, { count: 3 }]) {
      expect(() => feed.apply('llmcalls', bad)).not.toThrow()
    }
    expect(feed.rows).toHaveLength(0)
    expect(feed.refetchToken).toBe(0)
  })
})
