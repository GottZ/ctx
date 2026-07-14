import { test, expect, type Page } from '@playwright/test'
import { gotoArea, seedSession, trackPageErrors } from './fixtures'

// Structural edges in the ego graph (plan graph-structural 2026-07-14).
// GA2 (FE-W2) pins the two wave-cut invariants of the INTERIM consumption:
//   (a) mounting /graph with a structural_edges fixture must NOT throw —
//       the merge loop writes NO `type` attribute until the render program
//       is registered (GC1); sigma 3.0.3 throws hard on unregistered types,
//       even on hidden edges (sigma.esm.js:2006/:2736).
//   (b) structural edges are DEFAULT-VISIBLE (user directive: no extra click)
//       — the interim kind-branch in edgeVisible keeps them out of the dream
//       allowlist. GC2 replaces the branch with the blocklist model; FE-W7
//       extends this spec with the post-rels-sync default-visibility pin.

const FOCUS = '550e8400-e29b-41d4-a716-446655440001' // egoFixture focus node

interface CtxGraph {
  renderer: { getEdgeDisplayData(id: string): { hidden?: boolean } | undefined }
  graph: {
    edges(): string[]
    getEdgeAttribute(e: string, k: string): unknown
    hasNode(id: string): boolean
  }
}

/** Enter the focus stage and wait until the ego merge (incl. structural loop)
 *  has landed in the __ctxGraph GraphView instance. */
async function enterFocusStage(page: Page): Promise<void> {
  await gotoArea(page, `/graph?focus=${FOCUS}`)
  await page.waitForFunction(
    () => {
      const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph
      return !!g && g.graph.edges().length >= 4 // 2 dream + 2 structural (egoFixture)
    },
    null,
    { timeout: 10_000 },
  )
}

test.describe('structural edges (GA2 interim consumption)', () => {
  test('mount with structural_edges does not throw; edges land kind-tagged', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { theme: 'dark' })
    await enterFocusStage(page)

    const counts = await page.evaluate(() => {
      const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph!
      const kinds = g.graph.edges().map((e) => String(g.graph.getEdgeAttribute(e, 'kind')))
      return {
        dream: kinds.filter((k) => k === 'dream').length,
        structural: kinds.filter((k) => k === 'structural').length,
      }
    })
    expect(counts.dream).toBe(2)
    expect(counts.structural).toBe(2)
    expect(errors).toEqual([])
  })

  test('structural edges render default-visible (no extra click, no filter change)', async ({ page }) => {
    await seedSession(page, { theme: 'dark' })
    await enterFocusStage(page)

    const vis = await page.evaluate(() => {
      const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph!
      const structural = g.graph.edges().filter((e) => g.graph.getEdgeAttribute(e, 'kind') === 'structural')
      return structural.map((e) => {
        const dd = g.renderer.getEdgeDisplayData(e)
        return !!dd && dd.hidden !== true
      })
    })
    expect(vis).toHaveLength(2)
    expect(vis.every(Boolean)).toBe(true)
  })
})
