import { test, expect, type Page } from '@playwright/test'
import { runAxeGate } from './contract/axe'
import { THEMES, VIEWPORTS, type ViewportName } from './contract/contract'
import { contracts } from './contract/registry'
import { gotoArea, seedSession, trackPageErrors } from './fixtures'

// Structural edges in the ego graph (plan graph-structural 2026-07-14).
// GA2 (FE-W2) pins the two wave-cut invariants of the INTERIM consumption:
//   (a) mounting /graph with a structural_edges fixture must NOT throw —
//       the merge loop writes NO `type` attribute until the render program
//       is registered (GC1); sigma 3.0.3 throws hard on unregistered types,
//       even on hidden edges (sigma.esm.js:2006/:2736).
//   (b) structural edges are DEFAULT-VISIBLE (user directive: no extra click)
//       — the interim kind-branch in edgeVisible keeps them out of the dream
//       allowlist. GC2 replaces the branch with the blocklist model; GC5 syncs
//       the fixture rels to the real 5-class dream legend and pins EVERY edge
//       default-visible (the N16 drift became a tested invariant).

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

/** The GC2/GC3 checkbox list lives inside the link-class MultiSelect since
 *  the panel rework — open its popover (trigger name = "link class <summary>").
 *  The popover stays open across toggles (multi-select contract). */
async function openEdgeMenu(page: Page): Promise<void> {
  await page.getByRole('button', { name: /^link class/ }).click()
}

/** The focus deep-link opens a detail window that can overlap the panel —
 *  close it before clicking panel controls (pointer-events intercept). */
async function closeFocusWindow(page: Page): Promise<void> {
  const closeBtn = page.getByRole('button', { name: 'close' })
  if ((await closeBtn.count()) > 0) await closeBtn.first().click()
}

test.describe('structural edges (GA2 consumption + GC1 rendering + GC2 filter)', () => {
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

  // GC2 (FE-W4): blocklist checkboxes — unchecking hides EXACTLY that class
  // (reducer level, via the display data), other structural classes stay
  // visible; re-checking restores. Red before the wave: no structural
  // checkbox existed in the panel (locator absent).
  test('structural class checkbox hides exactly that class and restores on re-check (GC2)', async ({ page }) => {
    await seedSession(page, { theme: 'dark' })
    await enterFocusStage(page)

    const visibleByClass = () =>
      page.evaluate(() => {
        const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph!
        const out: Record<string, boolean[]> = {}
        for (const e of g.graph.edges()) {
          if (g.graph.getEdgeAttribute(e, 'kind') !== 'structural') continue
          const cls = String(g.graph.getEdgeAttribute(e, 'rel'))
          const dd = g.renderer.getEdgeDisplayData(e)
          ;(out[cls] ??= []).push(!!dd && dd.hidden !== true)
        }
        return out
      })

    await closeFocusWindow(page)
    await openEdgeMenu(page)

    const referencesBox = page.locator('label.check', { hasText: 'references' }).locator('input[type=checkbox]')
    await expect(referencesBox).toBeChecked()

    await referencesBox.uncheck()
    await expect(referencesBox).not.toBeChecked()
    const afterHide = await visibleByClass()
    expect(afterHide['references'].every((v) => v === false)).toBe(true)
    expect(afterHide['duplicate-of'].every((v) => v === true)).toBe(true) // exakt, nicht pauschal

    await referencesBox.check()
    await expect(referencesBox).toBeChecked()
    const afterRestore = await visibleByClass()
    expect(afterRestore['references'].every((v) => v === true)).toBe(true)
  })

  // GC2 review carry (GB5 unified channel): deselecting a DREAM class must
  // not drop structural from the server mirror — the expand request's
  // link_class carries the loaded structural classes alongside the remaining
  // dream classes ("leere Seite matcht nichts", handler egoLinkClassPartition).
  // Red before the fix: the request carried a dream-only CSV.
  test('dream deselection keeps structural classes in the expand request (GC2/GB5)', async ({ page }) => {
    await seedSession(page, { theme: 'dark' })
    await enterFocusStage(page)

    await closeFocusWindow(page)
    await openEdgeMenu(page)

    // 'causal' ist eine reine dream-Klasse — mit structural hat sie nichts zu tun.
    const causalBox = page.locator('label.check', { hasText: 'causal' }).locator('input[type=checkbox]')
    await causalBox.uncheck()

    const reqPromise = page.waitForRequest((r) => r.url().includes('/api/graph/ego') && r.url().includes('link_class='))
    await page.evaluate(() => {
      const g = (window as unknown as { __ctxGraph: { renderer: { emit(ev: string, p: unknown): void } } }).__ctxGraph
      g.renderer.emit('doubleClickNode', {
        node: '550e8400-e29b-41d4-a716-446655440001',
        event: { preventSigmaDefault: () => {} },
      })
    })
    const req = await reqPromise
    const url = new URL(req.url())
    const classes = (url.searchParams.get('link_class') ?? '').split(',')
    expect(classes).toContain('references')
    expect(classes).toContain('duplicate-of')
    expect(classes).not.toContain('causal')
    expect(classes).toContain('topical')
    expect(url.searchParams.has('struct_class')).toBe(false)
  })

  // GC3 (FE-W5): legend + a11y — the structural checkbox carries the class
  // semantics in its accessible name (visually-hidden suffix), the stats line
  // surfaces the structural count without a click, the onboarding hint names
  // the curved-arrow form. Red before the wave: none of the three existed.
  test('legend semantics: accessible name, stats line, onboarding hint (GC3)', async ({ page }) => {
    await seedSession(page, { theme: 'dark' })
    await enterFocusStage(page)

    // The legend rows live in the link-class popover since the panel rework.
    await closeFocusWindow(page)
    await openEdgeMenu(page)

    // structural checkbox: label text + visually-hidden semantics suffix
    const refBox = page.getByRole('checkbox', { name: /references structural — deterministic reference/ })
    await expect(refBox).toBeVisible()
    // dream checkbox keeps its plain name (no structural suffix)
    await expect(page.getByRole('checkbox', { name: 'topical', exact: true })).toBeVisible()

    // stats line shows the structural count (fixture: 3 structural edges)
    await expect(page.locator('.stats')).toContainText('· 3 structural')

    // onboarding hint names the curved-arrow form language
    await expect(page.locator('.hint')).toContainText('curved arrow = reference')
  })

  // GC5 (FE-W7): EVERY fixture edge — dream AND structural — renders
  // default-visible, no filter touched. Red against the pre-sync fixture
  // legend (['references','links_to','supersedes']): those dream rels are not
  // real dream classes, so the default allowlist silently hid the fixture's
  // dream edges (N16 drift) — mutation-probed by reverting the legend.
  test('all fixture edges render default-visible after the rels sync (GC5)', async ({ page }) => {
    await seedSession(page, { theme: 'dark' })
    await enterFocusStage(page)

    const hidden = await page.evaluate(() => {
      const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph!
      return g.graph.edges().filter((e) => {
        const dd = g.renderer.getEdgeDisplayData(e)
        return !dd || dd.hidden === true
      })
    })
    expect(hidden).toEqual([])
  })

  // GC4 (FE-W6): the detail window carries a "structural links" section —
  // direction glyph + far-node button (focus path) + class/origin badges.
  // Red before the wave: section locator absent.
  test('window shows the structural links section with origin badge; click focuses (GC4)', async ({ page }) => {
    await seedSession(page, { theme: 'dark' })
    await enterFocusStage(page)

    const win = page.getByRole('dialog').first()
    await expect(win).toBeVisible()
    const section = win.locator('section[aria-label="structural links"]')
    await expect(section).toBeVisible()
    // focus node 0 carries 3 outbound structural edges (2× references + 1× duplicate-of)
    await expect(section.locator('li:not(.more)')).toHaveCount(3)
    await expect(section.locator('code.origin').first()).toHaveText('system')
    await expect(section.locator('li', { hasText: 'duplicate-of' })).toHaveCount(1)

    // click a far-node button → focus jumps there (same setFocus path)
    await section.locator('button', { hasText: 'Findings' }).first().click()
    await expect(page.locator('code.focus')).toContainText('550e8400-e29b-41d4-a716-446655440003')
  })

  // GC4 invalidation gate: the graphology instance is deliberately non-reactive
  // — a pure $derived over the graph would freeze the list at window-mount.
  // graphRev (incremented in settle()) re-evaluates it after every merge.
  // Red against the graphRev-less variant (mutation probe, documented in the
  // commit body); the fixture route below serves GROWN structural edges on the
  // second ego call, so the open window's list must grow after Expand.
  test('structural links list grows after expand while the window stays open (GC4)', async ({ page }) => {
    await seedSession(page, { theme: 'dark' })
    // Second-call override: same fixture + one extra node/edge from call 2 on.
    let egoCalls = 0
    await page.route('**/api/graph/ego**', async (route) => {
      egoCalls++
      const base = {
        success: true,
        focus: FOCUS,
        params: { hops: 2 },
        rels: ['topical', 'factual', 'causal', 'recurrent', 'supersedes'],
        nodes: [
          { id: '550e8400-e29b-41d4-a716-446655440001', title: 'Architecture', category: 'design', scope: 'home', degree: 5, hop: 0, created_at: '2026-06-01T08:00:00Z' },
          { id: '550e8400-e29b-41d4-a716-446655440002', title: 'API Spec', category: 'reference', scope: 'home', degree: 2, hop: 1, created_at: '2026-06-02T09:30:00Z' },
          { id: '550e8400-e29b-41d4-a716-446655440003', title: 'Findings', category: 'learnings', scope: 'home', degree: 1, hop: 1, created_at: '2026-06-03T10:15:00Z' },
        ],
        edges: [
          [0, 1, 0, 0.95],
          [0, 2, 1, 0.87],
        ],
        struct_rels: ['references', 'duplicate-of'],
        origins: ['system'],
        structural_edges: [
          [0, 1, 0, 0],
          [0, 2, 0, 0],
          [0, 1, 1, 0],
        ],
        stats: { nodes: 3, edges: 2, structural_edges: 3, truncated: false, elapsed_ms: 28 },
      }
      if (egoCalls >= 2) {
        base.nodes.push({ id: '550e8400-e29b-41d4-a716-446655440004', title: 'Late Issue', category: 'projects', scope: 'home', degree: 1, hop: 1, created_at: '2026-06-04T10:15:00Z' })
        base.structural_edges.push([0, 3, 0, 0])
        base.stats.structural_edges = 4
      }
      await route.fulfill({ json: base })
    })
    await enterFocusStage(page)

    const win = page.getByRole('dialog').first()
    const section = win.locator('section[aria-label="structural links"]')
    await expect(section.locator('li:not(.more)')).toHaveCount(3)

    await win.getByRole('button', { name: 'Expand' }).click()
    await expect(section.locator('li:not(.more)')).toHaveCount(4)
    await expect(section.locator('button', { hasText: 'Late Issue' })).toBeVisible()
  })

  // GC4 negative path: a node with NO structural incidences and NO unloaded-
  // degree badge renders NO section at all ("keine Referenzen" must stay
  // distinguishable from "nicht geladen"). Mutation-probed: an unconditional
  // section render fails this pin.
  test('window of a node without structural links and without badge shows no section (GC4)', async ({ page }) => {
    await seedSession(page, { theme: 'dark' })
    await page.route('**/api/graph/ego**', async (route) => {
      await route.fulfill({
        json: {
          success: true,
          focus: FOCUS,
          params: { hops: 2 },
          rels: ['topical', 'factual', 'causal', 'recurrent', 'supersedes'],
          nodes: [
            { id: FOCUS, title: 'Architecture', category: 'design', scope: 'home', degree: 1, hop: 0, created_at: '2026-06-01T08:00:00Z' },
            { id: '550e8400-e29b-41d4-a716-446655440002', title: 'API Spec', category: 'reference', scope: 'home', degree: 1, hop: 1, created_at: '2026-06-02T09:30:00Z' },
          ],
          edges: [[0, 1, 0, 0.95]],
          struct_rels: [],
          origins: [],
          structural_edges: [],
          stats: { nodes: 2, edges: 1, structural_edges: 0, truncated: false, elapsed_ms: 5 },
        },
      })
    })
    await gotoArea(page, `/graph?focus=${FOCUS}`)
    await page.waitForFunction(
      () => {
        const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph
        return !!g && g.graph.edges().length >= 1
      },
      null,
      { timeout: 10_000 },
    )

    const win = page.getByRole('dialog').first()
    await expect(win).toBeVisible()
    // degree 1 == loadedDeg 1 → badge null; 0 structural → section absent
    await expect(win.locator('section[aria-label="structural links"]')).toHaveCount(0)
  })

  // GC3 axe gate on the FOCUS stage: the registry contract for /graph only
  // mounts the overview (cluster map) — the filter panel (legend swatches,
  // structural checkboxes) never enters that DOM. These free @a11y tests run
  // the SAME axe matrix (runAxeGate: WCAG tags + target-size, /graph ledger
  // context) on the focus stage, over the FULL project matrix (2 themes × 2
  // viewports — target-size bites hardest at 390 px, color-contrast differs
  // per theme). Negative probe (red-proven): removing a structural checkbox's
  // accessible name fails the label rule here.
  for (const theme of THEMES) {
    for (const viewport of Object.keys(VIEWPORTS) as ViewportName[]) {
      test(`focus stage axe matrix ${theme} ${viewport} (GC3)`, { tag: '@a11y' }, async ({ page }, testInfo) => {
        await page.setViewportSize(VIEWPORTS[viewport])
        await seedSession(page, { theme })
        await enterFocusStage(page)
        // Mirror the contract-runner mountState guard: axe must scan against
        // the APPLIED theme — without this assert a light/mobile cell would
        // silently scan whatever theme-boot fell back to.
        await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
        const contract = contracts.find((c) => c.route === '/graph')
        if (!contract) throw new Error('graph contract missing from registry')
        // Pass 1: the stage as it mounts (open detail window + panel triggers).
        await runAxeGate(page, contract, theme, viewport, testInfo)
        // Pass 2: the link-class popover open — the legend checkboxes moved in
        // there with the panel rework, and the negative probe (a structural
        // checkbox losing its accessible name fails the label rule) only bites
        // while they are in the DOM.
        await closeFocusWindow(page)
        await openEdgeMenu(page)
        await expect(page.getByRole('checkbox', { name: 'topical', exact: true })).toBeVisible()
        await runAxeGate(page, contract, theme, viewport, testInfo)
      })
    }
  }

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
