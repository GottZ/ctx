// CorpusModel gates (Wave A7). The load-bearing guarantee is the POLL STOP: the
// status loop must stop once `run.running` flips false (and on error), never
// poll endlessly. We drive a MANUAL timer (no wall-clock, no real setInterval) so
// each tick is deterministic — `fire()` runs the scheduled callback and flushes
// the async tick's microtasks, then `pending` tells us whether another poll was
// scheduled. Stop = no reschedule = `pending` false.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type {
  BlocksAuditStatusResponse,
  BlocksClassifyStatusResponse,
} from '../../lib/api/types'
import { CorpusModel, type Timer } from './corpus.svelte'

/** A manual setTimeout seam: holds the one scheduled callback; `fire()` runs it
 * and flushes the async tick so the next reschedule (if any) is registered. */
function manualTimer() {
  let scheduled: (() => void) | null = null
  const timer: Timer = {
    set(cb) {
      scheduled = cb
      return 1
    },
    clear() {
      scheduled = null
    },
  }
  return {
    timer,
    get pending() {
      return scheduled !== null
    },
    /** Run the pending callback and let the async status round settle. */
    async fire(): Promise<boolean> {
      const cb = scheduled
      scheduled = null
      if (!cb) return false
      cb()
      // Flush the awaited status promise + the state-set/reschedule that follows.
      await Promise.resolve()
      await Promise.resolve()
      await Promise.resolve()
      return true
    },
  }
}

function auditResp(running: boolean, over: Partial<BlocksAuditStatusResponse> = {}): BlocksAuditStatusResponse {
  return {
    success: true,
    scope: 'private',
    pending: 5,
    by_source: {},
    run: {
      running,
      dry_run: true,
      processed: 3,
      kept_credentials: 1,
      to_personal: 0,
      to_internal: 0,
      no_verdict: 0,
      discarded: 0,
      aborted: false,
    },
    ...over,
  }
}

function classifyResp(running: boolean): BlocksClassifyStatusResponse {
  return {
    success: true,
    scope: 'private',
    by_source: {},
    run: { running, dry_run: true, scanned: 4, upgraded: 2, discarded: 0, aborted: false },
  }
}

/** Status fn that walks a fixed sequence (last value sticks), counting calls. */
function seq<T>(values: T[]) {
  let i = 0
  const fn = (): Promise<T> => {
    const v = values[Math.min(i, values.length - 1)]
    i++
    return Promise.resolve(v)
  }
  return {
    fn,
    get calls() {
      return i
    },
  }
}

describe('CorpusModel audit poll', () => {
  it('start sets the run to running from the start response', async () => {
    const mt = manualTimer()
    const m = new CorpusModel(
      {
        startAudit: () => Promise.resolve(auditResp(true)),
        auditStatus: () => Promise.resolve(auditResp(true)),
        startClassify: () => Promise.resolve(classifyResp(false)),
        classifyStatus: () => Promise.resolve(classifyResp(false)),
      },
      { timer: mt.timer },
    )
    await m.startAudit(true)
    expect(m.auditRun?.running).toBe(true)
    expect(m.auditPolling).toBe(true)
    expect(m.auditBusy).toBe(true)
    // A run was scheduled but not yet fired.
    expect(mt.pending).toBe(true)
    m.dispose()
  })

  it('stops the poll once run.running flips false', async () => {
    const status = seq([auditResp(true), auditResp(true), auditResp(false)])
    const mt = manualTimer()
    const m = new CorpusModel(
      {
        startAudit: () => Promise.resolve(auditResp(true)),
        auditStatus: status.fn,
        startClassify: () => Promise.resolve(classifyResp(false)),
        classifyStatus: () => Promise.resolve(classifyResp(false)),
      },
      { timer: mt.timer },
    )
    await m.startAudit(true)
    expect(mt.pending).toBe(true)
    await mt.fire() // poll #1 → running:true → reschedule
    expect(mt.pending).toBe(true)
    await mt.fire() // poll #2 → running:true → reschedule
    expect(mt.pending).toBe(true)
    await mt.fire() // poll #3 → running:false → STOP
    expect(mt.pending).toBe(false)
    expect(m.auditPolling).toBe(false)
    expect(m.auditRun?.running).toBe(false)
    expect(status.calls).toBe(3) // never polled after the false
    expect(m.auditBusy).toBe(false)
  })

  it('stops the poll and records the error when status fails', async () => {
    const mt = manualTimer()
    const m = new CorpusModel(
      {
        startAudit: () => Promise.resolve(auditResp(true)),
        auditStatus: () => Promise.reject(new ApiError(503, 'api_error', 'backend down')),
        startClassify: () => Promise.resolve(classifyResp(false)),
        classifyStatus: () => Promise.resolve(classifyResp(false)),
      },
      { timer: mt.timer },
    )
    await m.startAudit(true)
    await mt.fire() // poll throws → stop
    expect(mt.pending).toBe(false)
    expect(m.auditPolling).toBe(false)
    expect(m.auditError).toBe('backend down')
  })

  it('does not poll when the start response is already finished', async () => {
    const mt = manualTimer()
    const m = new CorpusModel(
      {
        startAudit: () => Promise.resolve(auditResp(false)), // instant dry-run on a tiny corpus
        auditStatus: () => Promise.resolve(auditResp(false)),
        startClassify: () => Promise.resolve(classifyResp(false)),
        classifyStatus: () => Promise.resolve(classifyResp(false)),
      },
      { timer: mt.timer },
    )
    await m.startAudit(true)
    expect(mt.pending).toBe(false)
    expect(m.auditPolling).toBe(false)
    expect(m.audit?.run.running).toBe(false)
  })

  it('passes the dry-run flag through to the start action', async () => {
    let seen: boolean | undefined
    const mt = manualTimer()
    const m = new CorpusModel(
      {
        startAudit: (p) => {
          seen = p?.dry_run
          return Promise.resolve(auditResp(false))
        },
        auditStatus: () => Promise.resolve(auditResp(false)),
        startClassify: () => Promise.resolve(classifyResp(false)),
        classifyStatus: () => Promise.resolve(classifyResp(false)),
      },
      { timer: mt.timer },
    )
    await m.startAudit(true)
    expect(seen).toBe(true)
    await m.startAudit(false)
    expect(seen).toBe(false)
  })

  it('records a start failure without polling', async () => {
    const mt = manualTimer()
    const m = new CorpusModel(
      {
        startAudit: () => Promise.reject(new ApiError(403, 'forbidden', 'server-admin required')),
        auditStatus: () => Promise.resolve(auditResp(true)),
        startClassify: () => Promise.resolve(classifyResp(false)),
        classifyStatus: () => Promise.resolve(classifyResp(false)),
      },
      { timer: mt.timer },
    )
    await m.startAudit(true)
    expect(m.auditError).toBe('server-admin required')
    expect(mt.pending).toBe(false)
    expect(m.auditPolling).toBe(false)
  })
})

describe('CorpusModel classify poll', () => {
  it('stops the poll once run.running flips false', async () => {
    const status = seq([classifyResp(true), classifyResp(false)])
    const mt = manualTimer()
    const m = new CorpusModel(
      {
        startAudit: () => Promise.resolve(auditResp(false)),
        auditStatus: () => Promise.resolve(auditResp(false)),
        startClassify: () => Promise.resolve(classifyResp(true)),
        classifyStatus: status.fn,
      },
      { timer: mt.timer },
    )
    await m.startClassify(true)
    expect(mt.pending).toBe(true)
    await mt.fire() // poll #1 → running:true → reschedule
    expect(mt.pending).toBe(true)
    await mt.fire() // poll #2 → running:false → STOP
    expect(mt.pending).toBe(false)
    expect(m.classifyPolling).toBe(false)
    expect(m.classifyRun?.running).toBe(false)
    expect(status.calls).toBe(2)
  })
})

describe('CorpusModel dispose', () => {
  it('clears the pending timer so no further poll fires', async () => {
    const status = seq([auditResp(true), auditResp(true)])
    const mt = manualTimer()
    const m = new CorpusModel(
      {
        startAudit: () => Promise.resolve(auditResp(true)),
        auditStatus: status.fn,
        startClassify: () => Promise.resolve(classifyResp(false)),
        classifyStatus: () => Promise.resolve(classifyResp(false)),
      },
      { timer: mt.timer },
    )
    await m.startAudit(true)
    expect(mt.pending).toBe(true)
    m.dispose()
    expect(mt.pending).toBe(false) // timer cleared
    expect(m.auditPolling).toBe(false)
    await mt.fire() // nothing scheduled → no-op
    expect(status.calls).toBe(0) // never polled
  })
})
