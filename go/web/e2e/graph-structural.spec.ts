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
      return !!g && g.graph.edges().length >= 5 // 2 dream + 3 structural (egoFixture, GC1)
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
    expect(counts.structural).toBe(3)
    expect(errors).toEqual([])
  })

  // GC1 (FE-W3): structural edges render as curved arrows in the structural
  // color — `type` selects the EdgeCurvedArrowProgram registered in GraphView,
  // the color is the Teal token, distinct from BOTH dream edge colors. Dream
  // edges stay type-less (defaultEdgeType 'line'). Red before the wave: the
  // GA2 interim loop wrote no `type` and the dream edgeColor.
  test('structural edges carry type=curvedArrow and the structural color (GC1)', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { theme: 'dark' })
    await enterFocusStage(page)

    const probe = await page.evaluate(() => {
      const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph!
      const edges = g.graph.edges()
      const attrs = (e: string, k: string) => g.graph.getEdgeAttribute(e, k)
      const structural = edges.filter((e) => attrs(e, 'kind') === 'structural')
      const dream = edges.filter((e) => attrs(e, 'kind') === 'dream')
      const styles = getComputedStyle(document.documentElement)
      return {
        structTypes: structural.map((e) => String(attrs(e, 'type'))),
        structColors: structural.map((e) => String(attrs(e, 'color'))),
        dreamTypes: dream.map((e) => attrs(e, 'type')),
        dreamColors: dream.map((e) => String(attrs(e, 'color'))),
        tokenStructural: styles.getPropertyValue('--graph-edge-structural').trim(),
        tokenEdge: styles.getPropertyValue('--graph-edge').trim(),
        tokenStrong: styles.getPropertyValue('--graph-edge-strong').trim(),
      }
    })
    expect(probe.structTypes).toHaveLength(3)
    expect(probe.structTypes.every((t) => t === 'curvedArrow')).toBe(true)
    // Farbe = Live-Token, ≠ beide dream-Farben (Wellen-Gate „Farbe ≠ edge/edgeStrong")
    expect(probe.structColors.every((c) => c === probe.tokenStructural)).toBe(true)
    expect(probe.tokenStructural).not.toBe(probe.tokenEdge)
    expect(probe.tokenStructural).not.toBe(probe.tokenStrong)
    expect(probe.dreamTypes.every((t) => t === undefined)).toBe(true)
    expect(probe.dreamColors.every((c) => c === probe.tokenStructural)).toBe(false)
    expect(errors).toEqual([])
  })

  // GC1 §4.2: the struct↔struct parallel pair (references + duplicate-of on
  // the same 0→1 pair) must NOT render pixel-identical — the settle() pass
  // separates them via parallel-index-scaled curvature.
  test('struct↔struct parallel edges get distinct curvature (GC1)', async ({ page }) => {
    await seedSession(page, { theme: 'dark' })
    await enterFocusStage(page)

    const curvatures = await page.evaluate(() => {
      const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph!
      return g.graph
        .edges()
        .filter((e) => g.graph.getEdgeAttribute(e, 'kind') === 'structural')
        .filter((e) => String(e).endsWith('|550e8400-e29b-41d4-a716-446655440001|550e8400-e29b-41d4-a716-446655440002'))
        .map((e) => Number(g.graph.getEdgeAttribute(e, 'curvature')))
    })
    expect(curvatures).toHaveLength(2)
    // nullfrei (0 = gerade Kante läge pixel-identisch unter der dream-line);
    // negatives Vorzeichen = Gegenbogen, legitim getrennt
    expect(curvatures.every((c) => Number.isFinite(c) && c !== 0)).toBe(true)
    expect(new Set(curvatures).size).toBe(2) // getrennte Geometrien
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
    expect(vis).toHaveLength(3)
    expect(vis.every(Boolean)).toBe(true)
  })
})
