// Sync-badge model pins (design 04 §5.3/§5.4, wave U06) — the ×5-known +
// unknown-fallback matrix, plus the metadata extractor. DOM-free.

import { describe, expect, it } from 'vitest'
import { syncBadgeFor, syncBadgeForIssue, type SyncState } from './sync-badge'

const KNOWN: SyncState[] = ['local', 'in_sync', 'ctx_ahead', 'forge_ahead', 'conflict']

describe('syncBadgeFor — the 5 known verdicts', () => {
  for (const s of KNOWN) {
    it(`maps '${s}' to a known badge with a label + tone`, () => {
      const b = syncBadgeFor(s)
      expect(b.known).toBe(true)
      expect(b.state).toBe(s)
      expect(b.label).not.toBe('')
      expect(['neutral', 'ok', 'warn', 'danger']).toContain(b.tone)
    })
  }

  it('conflict carries danger optics (the loudest known state)', () => {
    expect(syncBadgeFor('conflict').tone).toBe('danger')
  })
})

describe('syncBadgeFor — unknown fallback (§5.4, the 6th render state)', () => {
  it("maps the Fixture 'garbage' value to the unknown badge in conflict optics", () => {
    const b = syncBadgeFor('garbage')
    expect(b.known).toBe(false)
    expect(b.state).toBe('unknown')
    expect(b.label).toBe('unknown')
    expect(b.tone).toBe('danger') // conflict optics — never silently dropped
  })

  it('never throws on an off-union string (fail-closed, not crash)', () => {
    expect(() => syncBadgeFor('anything-else')).not.toThrow()
    expect(syncBadgeFor('anything-else').known).toBe(false)
  })
})

describe('syncBadgeFor — absent / non-string → local (native, never synced)', () => {
  it('treats undefined / null / empty as local', () => {
    expect(syncBadgeFor(undefined).state).toBe('local')
    expect(syncBadgeFor(null).state).toBe('local')
    expect(syncBadgeFor('').state).toBe('local')
    expect(syncBadgeFor('   ').state).toBe('local')
  })

  it('treats a non-string value as local', () => {
    expect(syncBadgeFor(42).state).toBe('local')
    expect(syncBadgeFor({}).state).toBe('local')
  })
})

describe('syncBadgeForIssue — reads metadata.sync_state', () => {
  it('extracts the value from the block metadata', () => {
    expect(syncBadgeForIssue({ sync_state: 'conflict' }).state).toBe('conflict')
    expect(syncBadgeForIssue({ sync_state: 'garbage' }).known).toBe(false)
  })

  it('defaults to local when metadata is missing or has no sync_state', () => {
    expect(syncBadgeForIssue(undefined).state).toBe('local')
    expect(syncBadgeForIssue({}).state).toBe('local')
    expect(syncBadgeForIssue({ labels: ['bug'] }).state).toBe('local')
  })
})
