// PageContract registry (design 06 §3.4, wave PV4) — THE single source of
// truth for which pages carry which validation contract. Policy as data: a new
// page is a new entry here (+ baselines), never runner code. The matrix
// meta-test (matrix.spec.ts) pins registry ⊇ areaRoutes both ways; smoke.spec.ts
// executes EVERY entry via definePageContract — a registry entry cannot exist
// without running (the loop is the guarantee).
//
// Deliberately free of @playwright/test VALUE imports at module level? No —
// flows use the real `expect`; the import probe (PV4) verified vitest loads
// @playwright/test harmlessly, so the vitest matrix pin can import this file.
//
// Baseline inheritance note (PV4): the 'default' states seed role
// 'server-admin' explicitly — the 13 PV3 baselines were frozen from
// server-admin renders (rail shows Corpus+Server) and stay byte-valid. A
// re-freeze onto each contract's minimal role is a [baseline] decision for
// the PV7 full set, not a silent side effect.

import { expect } from '@playwright/test'
import type { PageContract } from './contract'
import { SENTINEL } from '../fixtures'

/**
 * Mobile opt-out for the PV4 Erstbelegung: mobile BASELINES do not exist yet —
 * the full baseline set is sequenced after the Achse-05 token stabilization
 * (design 06 §3.1/§9.3); the opt-out falls with the PV7/full-set wave.
 */
const MOBILE_PV4_EXEMPT = {
  exempt:
    'Mobile-Baselines existieren noch nicht — Voll-Satz ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); Opt-out fällt mit dem PV7-Voll-Satz.',
} as const

/** Contracts for the five smoke areas (Erstbelegung — consolidates the legacy smoke tests). */
export const contracts: PageContract[] = [
  {
    route: '/status',
    name: 'status',
    role: 'member', // guard truth: TIER_GATED lacks /status — rail hides it for members, the guard does not (named Ist-finding, contract.ts header)
    mode: 'reading',
    states: [{ name: 'default', seed: { role: 'server-admin' } }],
    scale: {
      exempt:
        'Aggregat-Ansicht mit fixer Kachelzahl; die einzigen Listen (backends, llm_24h) sind server-seitig auf die Backend-/Pipeline-Anzahl begrenzt — kein 10k-Wachstumspfad.',
    },
    flowDoc:
      'Operator öffnet die Status-Übersicht und liest den Live-Zustand: Backend-Zeile mit Namen und die Dream-Telemetrie sind inhaltlich gefüllt (erste Inhalts-Asserts; PV7 vertieft auf Tile-Vollständigkeit).',
    primaryFlow: async (page) => {
      // Content asserts against the fixture — the tiles carry DATA, not just chrome.
      await expect(page.locator('main.content')).toContainText('llama.cpp (local)')
      await expect(page.locator('main.content')).toContainText('never dreamed')
    },
    mobile: MOBILE_PV4_EXEMPT,
  },
  {
    route: '/graph',
    name: 'graph',
    role: 'member',
    mode: 'canvas',
    states: [{ name: 'default', seed: { role: 'server-admin' } }],
    scale: {
      exempt:
        'Canvas-Fläche: der Overview-Endpoint aggregiert server-seitig zu Clustern (stats.truncated deckelt), im DOM stehen keine Listen-Knoten — der 10k-DOM-Deckel ist gegenstandslos; Graph-Semantik läuft über den __ctxGraph-Hook (S12).',
    },
    flowDoc:
      'Nutzer öffnet die Cluster-Übersicht des Korpus: die Sigma-Canvas mountet und trägt exakt die drei Fixture-Cluster als Knoten (Semantik über den __ctxGraph-Hook, Pixel bleiben maskiert).',
    primaryFlow: async (page) => {
      await page.waitForFunction(() => '__ctxGraph' in window, null, { timeout: 10_000 })
      const order = await page.evaluate(
        () => (window as unknown as { __ctxGraph: { graph: { order: number } } }).__ctxGraph.graph.order,
      )
      expect(order, 'overview mounts the three fixture clusters').toBe(3)
    },
    maskBudgetOverride: {
      reason:
        'full-bleed sigma canvas (ForceAtlas2 layout is not seed-stable); graph SEMANTICS stay asserted via the __ctxGraph hook (primaryFlow + graph-palette special)',
      issue:
        '.project/plan-workflow-ui-2026-07-02/design/06-playwright-validation.md §4.3 (interim ref until Achse-02 issues exist)',
    },
    mobile: MOBILE_PV4_EXEMPT,
  },
  {
    route: '/blocks',
    name: 'blocks',
    role: 'member',
    mode: 'split',
    tenantScoped: true,
    states: [{ name: 'default', seed: { role: 'server-admin' } }],
    scale: {
      name: '10k',
      seed: { role: 'server-admin', state: '10k' },
      // W7 workbench: keyset pages (server default 10/page) — the ceiling
      // proves the DOM never mirrors the 10k corpus. 300 = the §6.2 literal.
      domCap: { selector: 'main.content ul.results > li', max: 300 },
      flow: async (page, session) => {
        const rows = page.locator('main.content ul.results > li')
        const before = await rows.count()
        // Keyset "Load more": exactly ONE further page is appended …
        await page.getByRole('button', { name: 'Load more' }).click()
        await expect(rows).toHaveCount(before * 2)
        // … and the wire proves the cursor round-trip: the follow-up
        // /api/search body carried the previous page's next_after cursor.
        const searches = session.calls.filter((x) => x.path === '/api/search' && x.body !== undefined)
        const last = searches[searches.length - 1]?.body as { after?: { after_id?: string } } | undefined
        expect(last?.after?.after_id, 'load-more re-issues the search WITH the keyset cursor').toBeTruthy()
      },
    },
    flowDoc:
      'Nutzer durchstöbert den Korpus: die Master-Liste füllt sich aus der Suche, ein Klick auf einen Treffer öffnet das Detail mit dem Block-Inhalt (Lese-Kernpfad des Block-Workbench).',
    primaryFlow: async (page) => {
      const list = page.locator('main.content ul.results')
      await expect(list).toContainText('Core Architecture')
      await list.locator('li button').first().click()
      // manage {action:'get'} fixture body — the detail pane renders real content.
      await expect(page.locator('main.content')).toContainText('canvas-first surface')
    },
    mobile: MOBILE_PV4_EXEMPT,
  },
  {
    route: '/chat',
    name: 'chat',
    role: 'member',
    mode: 'thread',
    states: [{ name: 'default', seed: { role: 'server-admin' } }],
    scale: {
      exempt:
        'Thread-Ansicht rendert nur die aktive Konversation; die Sitzungsliste ist im Ist-Bestand die einzige Liste und ohne 10k-Pfad im Mock — die Scale-Pflicht greift mit den virtualisierten Achse-04-Listenflächen (design 06 §6.2).',
    },
    flowDoc:
      'Nutzer erreicht den Chat mit bedienbarem Composer (Eingabe + Send-Gate). Benannte PV4-Grenze: der eigentliche Kern-Pfad senden→streamen→persistieren ist per §7-PV7 dem sseRoute-primaryFlow der PV7-Welle zugewiesen — dieser Flow deckt den Renderpfad, nicht den Stream.',
    primaryFlow: async (page) => {
      const composer = page.getByPlaceholder(/Ask the knowledge store/)
      await expect(composer).toBeVisible()
      // Send gate: empty composer disables, typed text arms the button.
      const send = page.getByRole('button', { name: 'Send' })
      await expect(send).toBeDisabled()
      await composer.fill('probe')
      await expect(send).toBeEnabled()
    },
    mobile: MOBILE_PV4_EXEMPT,
  },
  {
    route: '/settings',
    name: 'settings',
    role: 'member', // guard truth — same rail/guard divergence note as /status
    mode: 'reading',
    states: [{ name: 'default', seed: { role: 'server-admin' } }],
    scale: {
      exempt:
        'Settings-Katalog ist eine bounded, server-definierte Konfigurationsliste (Dutzende Keys, kein nutzergetriebenes Wachstum) — keine 10k-Dimension.',
    },
    flowDoc:
      'Admin liest den Settings-Katalog: die Katalogzeilen rendern Key + Wert aus dem Server-Fixture. Benannte PV4-Grenze: der Edit-Roundtrip (primaryFlow laut §4.1) ist per §7-PV7 der PV7-Welle zugewiesen.',
    primaryFlow: async (page) => {
      await expect(page.locator('main.content')).toContainText('dream.enabled')
      await expect(page.locator('main.content')).toContainText('pool.default_block_sensitivity')
    },
    mobile: MOBILE_PV4_EXEMPT,
  },
]

/** A route that awaits its contract — visible debt, not a silent gap. */
export interface PendingContract {
  /** areaRoutes key or 'login'. */
  route: string
  /** Why it is pending + which wave delivers it (non-empty, matrix-enforced). */
  reason: string
}

/**
 * Bestands-Lücken (design 06 S13): these routes get their contracts in wave
 * PV7 ("Matrix-Test grün OHNE Ausnahme-Liste" is the PV7 gate — this list must
 * be EMPTY after PV7). The matrix meta-test enforces: every route is contract
 * XOR pending (a pending entry with a contract is stale ⇒ red), and any NEW
 * route without either entry is red — "jede Seite" cannot erode silently.
 */
export const pendingContracts: PendingContract[] = [
  { route: '/home', reason: 'PV7 (design 06 §7-PV7): Member-Landing-Kontrakt' },
  { route: '/settings/backends', reason: 'PV7 (design 06 §7-PV7): Backends-Editor-Kontrakt' },
  { route: '/admin', reason: 'PV7 (design 06 §7-PV7): Server-Admin-Kontrakt inkl. generiertem Deny' },
  { route: '/admin/tenants/:id', reason: 'PV7 (design 06 §7-PV7): Template-Kontrakt mit Fixture-Param' },
  { route: '/tenant', reason: 'PV7 (design 06 §7-PV7): Tenant-Admin-Kontrakt inkl. generiertem Deny' },
  { route: '*', reason: 'PV7 (design 06 §7-PV7): NotFound-Kontrakt' },
  { route: 'login', reason: 'PV7 (design 06 §7-PV7): Login-Gate liegt VOR dem Router (src/App.svelte) — Mock-Variante + Fehl-Key-Pfad' },
]

/** Sentinel re-export so leak-probe consumers need only the registry. */
export { SENTINEL }
