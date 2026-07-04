// @live — REAL SSE transport (design 06 §4.7 probe 4, wave PV10). The mock tier
// aborts /api/events (fixtures.ts) and only proves FRAME SEMANTICS via the
// sseRoute mock; here the ACTUAL transport is exercised against the real hub:
// connect, real server-emitted frames (server timing, not a fulfilled body),
// and a client-driven reconnect. /api/events is server-admin only, so this runs
// as the per-run bootstrap key.
//
// Scope honesty (W21): this proves connect + real frames + client reconnect.
// The server-RESTART reconnect variant (design §4.7) needs an orchestrated
// ctxd bounce mid-test and is documented in README.md as a nightly/manual
// extension, not automated here.

import { test, expect } from '@playwright/test'
import { readState } from './state'
import { loginAs } from './helpers'

const state = readState()

// Reads the real /api/events stream from the browser context (same origin as
// ctxd, Bearer via X-Context-Key — native EventSource cannot set headers, so
// the app and this probe both use fetch + ReadableStream). Resolves with the
// first bytes seen, or '' on timeout.
async function firstSseBytes(page: import('@playwright/test').Page, key: string, ms: number): Promise<string> {
  return page.evaluate(
    async ([apiKey, timeoutMs]) => {
      const ctrl = new AbortController()
      const timer = setTimeout(() => ctrl.abort(), timeoutMs as number)
      try {
        const res = await fetch('/api/events', {
          headers: { 'X-Context-Key': apiKey as string, Accept: 'text/event-stream' },
          signal: ctrl.signal,
        })
        if (!res.ok || !res.body) return `HTTP ${res.status}`
        const reader = res.body.getReader()
        const dec = new TextDecoder()
        let acc = ''
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          acc += dec.decode(value, { stream: true })
          // An SSE frame (event:/data:) or a `:` keepalive ping is enough to
          // prove real transport.
          if (/(^|\n)(event:|data:|:)/.test(acc)) {
            void reader.cancel()
            return acc
          }
        }
        return acc
      } catch {
        return ''
      } finally {
        clearTimeout(timer)
      }
    },
    [key, ms] as const,
  )
}

test('@live sse-transport: real /api/events delivers frames + reconnects', async ({ page }) => {
  await loginAs(page, state.bootstrapKey)

  // Connect #1: the real hub must emit a frame or keepalive.
  const first = await firstSseBytes(page, state.bootstrapKey, 20_000)
  expect(first, `first SSE bytes: ${JSON.stringify(first)}`).toMatch(/(^|\n)(event:|data:|:)/)

  // Client-driven reconnect: a fresh connection against the SAME live server
  // must succeed again (the transport is not single-shot).
  const second = await firstSseBytes(page, state.bootstrapKey, 20_000)
  expect(second, `reconnect SSE bytes: ${JSON.stringify(second)}`).toMatch(/(^|\n)(event:|data:|:)/)
})
