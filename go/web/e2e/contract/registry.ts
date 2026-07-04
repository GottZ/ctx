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
import { KEY, SENTINEL, sseRoute, type SseFrame } from '../fixtures'

/**
 * Mobile opt-out for the five PV4-Erstbelegung areas: their mobile BASELINES
 * do not exist yet — the FULL baseline set (incl. re-freezing these five onto
 * mobile + minimal roles) is sequenced after the Achse-05 token stabilization
 * (design 06 §3.1/§9.3). PV7 deliberately does NOT lift this opt-out: lifting
 * it would be a Voll-Refresh of the existing set, which §9.3 forbids before
 * Achse-05 settles. The seven NEW PV7 contracts carry the full mobile
 * dimension from day one (only NEW pages freeze new baselines).
 */
const MOBILE_PV4_EXEMPT = {
  exempt:
    'Mobile-Baselines der PV4-Erstbelegung existieren noch nicht — Voll-Satz (Re-Freeze der Bestands-Areas) ist auf nach der Achse-05-Token-Stabilisierung sequenziert (design 06 §3.1/§9.3); PV7 friert nur die NEUEN Seiten ein.',
} as const

/** Fixture tenant A id (fixtures.ts TENANTS.A) — the /admin/tenants/:id template param. */
const TENANT_A_ID = '550e8400-e29b-41d4-a716-446655440aaa'

// ---------------------------------------------------------------------------
// /chat primaryFlow payload (design 06 §5.6a, wave PV7): the streamed AND
// persisted assistant answer carries the full XSS probe family, so ONE flow
// proves send → stream → persist AND the renderer hardening on the same DOM:
//   - a legitimate ctx: citation      → rewritten to /graph?focus=<id>
//   - raw HTML                        → escaped to text (markdown-it html:false)
//   - [..](javascript:)               → never a javascript: href anywhere
//   - [..](ctx:javascript:alert(1))   → rewrite kante: encodeURIComponent proof
//   - [..](ctx:../../evil)            → path traversal stays URL-encoded
//   - [..](steam:alert)               → DOMPurify URI allowlist strips the href.
//     This is the probe that turns RED when DOMPurify.sanitize is bypassed:
//     markdown-it's own bad-proto list already blocks javascript:/data:, so a
//     scheme OUTSIDE that list but outside DOMPurify's allowlist is the only
//     payload that isolates the SECOND defense line (§5.6a negative gate).
// ---------------------------------------------------------------------------

const CHAT_SESSION_ID = '0190000000007000800000000000se1'
const CHAT_PROMPT = 'Cite the architecture block, then try to break the renderer.'
const CHAT_ANSWER_MD = [
  'Answer with a citation [Core Architecture](ctx:550e8400-e29b-41d4-a716-446655440001).',
  '',
  '<img src=x onerror="alert(1)"> raw HTML must render as text.',
  '',
  '[js-link](javascript:alert(1)) [ctx-js](ctx:javascript:alert(1)) [ctx-trav](ctx:../../evil) [uri-probe](steam:alert)',
].join('\n')

/** Deterministic SSE frames for the /chat primaryFlow (sseRoute, §4.6). */
const CHAT_FRAMES: SseFrame[] = [
  { event: 'session', data: { session_id: CHAT_SESSION_ID, user_seq: 1 } },
  { event: 'backend', data: { backend: 'llama.cpp (local)', model: 'qwen3.5-9b', trust: 'full-trust', tools_active: false, fallback: false } },
  { event: 'delta', data: { text: 'Answer with a citation ' } },
  { event: 'delta', data: { text: CHAT_ANSWER_MD.slice('Answer with a citation '.length) } },
  { event: 'done', data: { finish_reason: 'stop', assistant_seq: 2, iterations: 1, total_ms: 42 } },
]

const CHAT_SESSION_VIEW = {
  id: CHAT_SESSION_ID,
  title: 'Renderer hardening probe',
  scope: 'home',
  max_sensitivity: 'internal',
  created_at: '2026-06-29T11:59:00Z',
  updated_at: '2026-06-29T12:00:00Z',
}

/** Persisted truth the store reloads after the stream ends (ChatSessionDetailResponse). */
const CHAT_DETAIL = {
  success: true,
  session: CHAT_SESSION_VIEW,
  messages: [
    { seq: 1, role: 'user', content: CHAT_PROMPT, sensitivity: 'internal', created_at: '2026-06-29T11:59:00Z' },
    { seq: 2, role: 'assistant', content: CHAT_ANSWER_MD, sensitivity: 'internal', backend: 'llama.cpp (local)', created_at: '2026-06-29T12:00:00Z' },
  ],
}

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
      'Operator öffnet die Status-Übersicht und liest den Live-Zustand tile-vollständig: Health-Ampel mit Services, Gaming-Toggle, Dream-Queue-Zahlen, Backend-Pool-Zeile und LLM-Telemetrie tragen die Fixture-DATEN (PV7: erste Inhalts-Asserts auf jede Tile — Inventur §5 hatte keine einzige).',
    primaryFlow: async (page) => {
      // Tile-complete content asserts against statusFixture (PV7, design 06
      // §7-PV7): every tile carries DATA, not just chrome.
      const content = page.locator('main.content')
      // as_of freshness under the fixed clock: FIXED_NOW = as_of + 5 s.
      await expect(content).toContainText('updated 5s ago')
      // Health tile: aggregate + all three service dots.
      const health = page.locator('section.card[aria-label="health"]')
      await expect(health.locator('strong')).toHaveText('ok')
      for (const svc of ['db', 'embed', 'chat']) await expect(health).toContainText(svc)
      // Toggles tile: gaming lock reflects fixture gaming.active=false.
      const toggles = page.locator('section.card[aria-label="toggles"]')
      await expect(toggles.getByRole('button', { name: 'OFF' })).toHaveAttribute('aria-pressed', 'false')
      await expect(toggles).toContainText('no signal') // activity: null
      // Dream tile: queue counters + mode segment + meta rows.
      const dream = page.locator('section.card[aria-label="dream queue"]')
      await expect(dream.locator('.stat', { hasText: 'pickable now' })).toContainText('4')
      await expect(dream.locator('.stat', { hasText: 'never dreamed' })).toContainText('12')
      await expect(dream.locator('.stat', { hasText: 'incoming 6h' })).toContainText('9')
      await expect(dream).toContainText('throttle')
      // Backend pool tile: the one fixture backend, row-complete.
      const pool = page.locator('section.card[aria-label="backend pool"]')
      const row = pool.locator('tbody tr')
      await expect(row).toHaveCount(1)
      await expect(row).toContainText('llama.cpp (local)')
      await expect(row).toContainText('full-trust')
      await expect(row).toContainText('chat, dream')
      await expect(row).toContainText('active')
      // LLM telemetry tile mounts its own /api/llmlog resource (entries []).
      await expect(page.locator('section.card[aria-label="llm telemetry"]')).toContainText('llm calls · 24h sample')
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
      'Nutzer sendet einen Prompt, die Antwort streamt über SSE ins DOM und die Session persistiert in der Sidebar (sseRoute-Kernpfad, §7-PV7) — inklusive der XSS-Probe-Familie §5.6a auf der gerenderten Antwort (ctx:-Citation-Rewrite, escaped Raw-HTML, encodierte ctx:-Payloads, DOMPurify-URI-Allowlist).',
    primaryFlow: async (page) => {
      // Deterministic stream + persisted truth — later page.route registrations
      // override the seedSession defaults exactly for this flow (§4.6).
      await sseRoute(page, '**/api/chat/stream', CHAT_FRAMES)
      await page.route(
        (url) => url.pathname === `/api/chat/sessions/${CHAT_SESSION_ID}`,
        (route) => route.fulfill({ json: CHAT_DETAIL }),
      )
      await page.route(
        (url) => url.pathname === '/api/chat/sessions',
        (route) => route.fulfill({ json: { success: true, sessions: [{ ...CHAT_SESSION_VIEW, message_count: 2 }] } }),
      )
      // No dialog may ever fire (§5.6a: kein pageerror, kein Dialog).
      const dialogs: string[] = []
      page.on('dialog', (d) => {
        dialogs.push(d.message())
        void d.dismiss()
      })

      // Send gate, then the turn: prompt → streamed answer → persisted session.
      const composer = page.getByPlaceholder(/Ask the knowledge store/)
      const send = page.getByRole('button', { name: 'Send' })
      await expect(send).toBeDisabled()
      await composer.fill(CHAT_PROMPT)
      await expect(send).toBeEnabled()
      await send.click()

      // The persisted truth is reloaded after the stream ends: user bubble +
      // rendered assistant markdown, no streaming cursor left behind.
      const md = page.locator('.msg.assistant .body.md')
      await expect(page.locator('.msg.user')).toContainText(CHAT_PROMPT)
      await expect(md).toBeVisible()
      await expect(page.locator('.msg .cursor')).toHaveCount(0)
      // Session persisted in the sidebar (title + the list reload after done).
      await expect(page.locator('#session-list')).toContainText('Renderer hardening probe')

      // ---- XSS probe family (§5.6a) on the rendered answer ----
      // (1) Legitimate ctx: citation → SPA graph route, link text intact.
      const cite = md.locator('a', { hasText: 'Core Architecture' })
      await expect(cite).toHaveAttribute('href', '/graph?focus=550e8400-e29b-41d4-a716-446655440001')
      // (2) Raw HTML is escaped to TEXT (markdown-it html:false): the literal
      // tag is visible text, and no <img> element exists in the bubble.
      await expect(md).toContainText('<img src=x onerror="alert(1)">')
      await expect(md.locator('img')).toHaveCount(0)
      // (3) No javascript: href anywhere in the whole DOM.
      await expect(page.locator('a[href^="javascript" i]')).toHaveCount(0)
      // (4) ctx:-Rewrite-Kante: the payload after ctx: is encodeURIComponent-
      // encoded INTO the focus param — never an executable scheme.
      await expect(md.locator('a', { hasText: 'ctx-js' })).toHaveAttribute('href', '/graph?focus=javascript%3Aalert(1)')
      await expect(md.locator('a', { hasText: 'ctx-trav' })).toHaveAttribute('href', '/graph?focus=..%2F..%2Fevil')
      // (5) DOMPurify URI allowlist (the SECOND defense line, isolated): a
      // scheme markdown-it permits but DOMPurify does not → href is STRIPPED.
      // This is the probe that turns red when sanitize() is bypassed.
      const uriProbe = md.locator('a', { hasText: 'uri-probe' })
      await expect(uriProbe).toBeVisible()
      expect(await uriProbe.getAttribute('href'), 'DOMPurify must strip the non-allowlisted steam: href').toBeNull()
      expect(dialogs, `dialogs fired during the chat turn: ${dialogs.join(' | ')}`).toEqual([])
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
      'Admin editiert eine Einstellung im Katalog (Edit-Roundtrip, §7-PV7): dream.enabled-Switch kippen → Save der Gruppe → genau EIN PUT /api/settings/dream.enabled mit {value:false} auf dem Draht (postData-Assert) → Echo wendet source=db an und der Dirty-Zähler verschwindet.',
    primaryFlow: async (page, session) => {
      const content = page.locator('main.content')
      await expect(content).toContainText('dream.enabled')
      await expect(content).toContainText('pool.default_block_sensitivity')

      // Edit-Roundtrip (design 06 §4.1: /settings = Edit-Roundtrip, nie der
      // Empty-State). dream.enabled is a hot bool → role=switch checkbox.
      const card = page.locator('section.card[aria-label="dream settings"]')
      const sw = card.locator('[id="dream.enabled"]')
      await expect(sw).toBeChecked() // fixture value true
      await sw.click()
      await expect(card).toContainText('1 unsaved')
      await card.getByRole('button', { name: 'Save' }).click()

      // Applied echo: the PUT response flips the source badge env → db and
      // clears the dirty marker (drafts re-sync to the stored value).
      await expect(card.locator('span.badge.source-db')).toBeVisible()
      await expect(card).not.toContainText('unsaved')
      await expect(sw).not.toBeChecked()

      // postData proof (§7-PV7): exactly one PUT carried {value:false}.
      const puts = session.calls.filter((x) => x.method === 'PUT' && x.path === '/api/settings/dream.enabled')
      expect(puts, 'the group save issues exactly ONE PUT for the one dirty key').toHaveLength(1)
      expect(puts[0].body, 'PUT body carries the toggled scalar').toEqual({ value: false })
    },
    mobile: MOBILE_PV4_EXEMPT,
  },

  // -------------------------------------------------------------------------
  // PV7 — Bestands-Lücken (design 06 §7-PV7, seam S13): the seven contracts
  // that empty the pending list. Default states seed each page's ROLE MINIMUM
  // (the named [baseline] decision from the PV4 return) — only the five PV4
  // areas above keep their frozen server-admin defaults until the sequenced
  // full-set re-freeze (§9.3). New pages carry the full mobile dimension.
  // -------------------------------------------------------------------------
  {
    // The ONE pre-router page: the gate mounts before areaRoutes (App.svelte).
    route: 'login',
    name: 'login',
    role: 'member',
    path: '/',
    states: [
      { name: 'default', seed: { anonymous: true } },
      {
        // Fehl-Key error band as a DECLARED state — frozen visually in both
        // themes/viewports; the "never the shell" flow proof lives in
        // smoke.spec.ts (login negative path, PV7 gate).
        name: 'error',
        seed: { anonymous: true },
        prepare: async (page) => {
          await page.getByLabel('API key').fill('wrong-key')
          await page.getByRole('button', { name: 'Sign in' }).click()
          await expect(page.getByRole('alert')).toContainText('invalid or revoked API key')
        },
      },
    ],
    scale: {
      exempt: 'Login-Maske: ein einzelnes Formular ohne Datenliste — es existiert kein 10k-Wachstumspfad.',
    },
    flowDoc:
      'Nutzer fügt den API-Key ein und meldet sich an: whoami-Probe → Shell mountet → Member-Landing /home. Der Fehl-Key-Pfad (Fehlerband, NIE Shell) ist der deklarierte error-State + freie Negativ-Probe in smoke.spec.ts.',
    primaryFlow: async (page) => {
      await page.getByLabel('API key').fill(KEY)
      await page.getByRole('button', { name: 'Sign in' }).click()
      await expect(page.locator('.shell')).toBeVisible()
      // Member landing: `/` canonicalizes via landingFor(member) → /home.
      await expect(page).toHaveURL(/\/home$/)
      await expect(page.locator('main.content')).toContainText('Welcome, smoke-key')
    },
  },
  {
    route: '/home',
    name: 'home',
    role: 'member',
    mode: 'reading',
    // Rollen-Minimum: member (die Zielgruppe der Seite). U04: der Default-State
    // trägt jetzt das viewWorkflow-Flag, damit die Bestands-Shot die neue
    // Workflow-Kachel einfriert (design 04 §4.1.6/§5.5, [baseline]-Update). Die
    // Abwesenheits-Richtung (Kachel weg ohne Flag) probt workflow-nav.spec.ts.
    states: [{ name: 'default', seed: { capabilities: { workflow: true } } }],
    scale: {
      exempt:
        'Capability-Screen mit fixer Kartenzahl aus whoami (Write-Scope, Read-Scopes, Rolle, Tenant) — keine Liste, kein 10k-Pfad.',
    },
    flowDoc:
      'Member landet auf /home, liest seinen Korpus-Zuschnitt (Write-Scope home, Read-Scopes home+shared, Rolle, Tenant), sieht bei viewWorkflow die Workflow-Kachel und springt über „Browse blocks" in die Korpus-Fläche.',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(content).toContainText('Welcome, smoke-key')
      // The four capability cards carry the member whoami DATA.
      await expect(content).toContainText('home, shared') // read access
      await expect(content).toContainText('member') // role
      await expect(content).toContainText('Acme Corp') // tenant display name, never the UUID
      // U04: the workflow tile shows under viewWorkflow (both directions probed
      // in workflow-nav.spec.ts) — its CTA links into the issue surface.
      await expect(content.getByRole('link', { name: 'Open issues →' })).toBeVisible()
      await page.getByRole('link', { name: 'Browse blocks →' }).click()
      await expect(page).toHaveURL(/\/blocks$/)
      await expect(content).toContainText('Core Architecture')
    },
  },
  // -------------------------------------------------------------------------
  // Workflow surface (design 04 §4.1/§5.5, wave U04) — DARK-LAUNCH scaffolds.
  // U04 registers the routes + freezes the EMPTY-state baselines; the data
  // layers land in U05 (/issues), U06 (/issues/:id) and U07 (/board), each of
  // which re-freezes its baseline with real content. role:'member' = the guard
  // truth (the routes are ungated member surfaces; the viewWorkflow flag only
  // drives nav visibility). The scaffolds make ZERO API calls.
  // -------------------------------------------------------------------------
  {
    route: '/issues',
    name: 'issues',
    role: 'member',
    mode: 'split',
    // U05 states (design 04 §5.5): default (single project auto-selected + list),
    // empty (project with no issues → EmptyState), search (q typed → Top-N, no
    // load-more affordance). The 10k DOM-cap proof is the scale dimension.
    states: [
      { name: 'default', seed: {} },
      { name: 'empty', seed: { empty: true } },
      {
        name: 'search',
        seed: {},
        // Type a query + submit ⇒ search mode: the wire returns cursor null and
        // the list renders "Top matches" with NO load-more affordance (§6.1).
        prepare: async (page) => {
          await page.getByRole('searchbox', { name: 'Search issues' }).fill('bug')
          await page.getByRole('button', { name: 'Search' }).click()
          await expect(page.locator('main.content')).toContainText('Top matches')
        },
      },
    ],
    scale: {
      name: '10k',
      seed: { state: '10k' },
      // The virtualisation proof: 10k rows load into the model, virtua keeps the
      // DOM at O(viewport). 200 = the design 04 §5.5 literal; remove the
      // windowing (virtual-window.ts) and the page renders 10k <tr> (RED).
      domCap: { selector: 'main.content tr[data-issue-row]', max: 200 },
      flow: async (page) => {
        // XSS-title-bleibt-Text (§5.5): row 0 carries a <script> title; it renders
        // as literal text (Svelte-escaped), never as an element.
        const first = page.locator('main.content tr[data-issue-row]').first()
        await expect(first).toContainText('<script>alert(')
        expect(await page.locator('main.content tr[data-issue-row] script').count()).toBe(0)
        // Windowing follows the scroll: jump to the bottom, the DOM stays bounded
        // (the runner re-asserts domCap after this flow) and the list still holds.
        await page.locator('main.content .list').evaluate((el) => el.scrollTo(0, el.scrollHeight))
        await expect(page.locator('main.content tr[data-issue-row]').first()).toBeVisible()
      },
    },
    flowDoc:
      'Ein Member öffnet /issues: der einzige Projekt-Scope wird auto-selektiert (Picker), die virtualisierte Liste füllt sich, und der Filter-Zustand (inkl. ?scope=) wandert in die URL (deep-linkbar).',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(page.getByRole('heading', { name: 'Issues' })).toBeVisible()
      // The lone project auto-selects (0/1/N picker, §4.1.5) and writes ?scope=.
      await expect(content).toContainText('Acme Main')
      await expect(page).toHaveURL(/scope=acme(%3A|:)main/)
      // The freeze list row renders through the virtualised table.
      await expect(content.getByRole('link', { name: 'Example issue' })).toBeVisible()
    },
  },
  {
    // Template contract (design 06 §4.2): EIN Kontrakt pro Template, path? nötig.
    route: '/issues/:id',
    name: 'issue-detail',
    role: 'member',
    mode: 'split',
    path: '/issues/550e8400-e29b-41d4-a716-446655440001',
    states: [{ name: 'default', seed: {} }],
    scale: {
      exempt:
        'U04-Scaffold ohne Datenanbindung — der virtualisierte 500-Comments-Thread + das uniform-404-Verhalten landen mit der Detail-Datenschicht in U06 (design 04 §5.5).',
    },
    flowDoc:
      'Deep-Link auf /issues/:id rendert unter Dark-Launch die statische EmptyState (keine API-Calls); Markdown-Body, Comments-Thread, Composer und Sync-Badge landen in U06.',
    primaryFlow: async (page) => {
      await expect(page.getByRole('heading', { name: 'Issue' })).toBeVisible()
      await expect(page.locator('main.content')).toContainText('Issue detail is not wired up yet')
    },
  },
  {
    route: '/board',
    name: 'board',
    role: 'member',
    mode: 'board',
    states: [{ name: 'default', seed: {} }],
    scale: {
      exempt:
        'U04-Scaffold ohne Datenanbindung — die Status-Spalten aus der Type-Config, per-Spalte-Fenster + der 10k×6-DOM-Cap-Beweis (< 300 Karten) landen mit der Board-Datenschicht in U07 (design 04 §5.5).',
    },
    flowDoc:
      'Deep-Link auf /board rendert unter Dark-Launch die statische EmptyState (keine API-Calls); die Status-Spalten, Counts und DnD landen in U07/U08.',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
      await expect(content).toContainText('No board to show yet')
      await expect(content).toContainText('No project selected')
    },
  },
  {
    route: '/settings/backends',
    name: 'settings-backends',
    // Guard truth: kein TIER_GATE auf /settings/* — dieselbe Rail↔Guard-
    // Divergenz wie /settings (PV4-Befund, contract.ts header). Der INHALT ist
    // page-self-gated auf session.admin; das Rollen-Minimum der Inhalts-Fläche
    // ist server-admin (fixtures: nur server-admin trägt admin:true).
    role: 'member',
    mode: 'reading',
    states: [{ name: 'default', seed: { role: 'server-admin' } }],
    scale: {
      exempt:
        'Backend-Pool + Vault sind bounded Betreiber-Listen (Provider-Backends, Secret-Namen) — kein nutzergetriebenes 10k-Wachstum.',
    },
    flowDoc:
      'Admin öffnet den Pool-&-Vault-Editor unter dem Settings-Crumb: Pool-Tabelle (Fixture-Empty-State als Positiv-Kontrolle des Ladepfads) und Secrets-Vault mounten hinter dem admin-Gate.',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(page.getByRole('heading', { name: 'Backend pool & vault' })).toBeVisible()
      // Pool loaded past the spinner: the fixture [] renders the empty state.
      await expect(content).toContainText('no backends — create one to populate the pool')
      // Vault section mounts with its create form.
      await expect(page.locator('section.card[aria-label="secrets vault"]')).toBeVisible()
      await expect(page.getByPlaceholder('name (e.g. openrouter.key)')).toBeVisible()
      // Crumb navigates back to the parent catalog.
      await page.locator('.crumb').getByRole('link', { name: 'Settings' }).click()
      await expect(page).toHaveURL(/\/settings$/)
    },
  },
  {
    route: '/admin',
    name: 'admin',
    role: 'server-admin',
    mode: 'reading',
    adminCalls: ['tenant-list', 'scope-overview'],
    states: [{ name: 'default', seed: {} }], // Rollen-Minimum == Guard-Rolle (server-admin)
    scale: {
      exempt:
        'Tenant-Register + Scope-Map sind Betreiber-Aggregate (Anzahl Tenants/Scopes, server-seitig überschaubar) — der 10k-Korpus-Pfad läuft über /api/search-Flächen, nicht über dieses Register.',
    },
    flowDoc:
      'Server-Admin liest das Tenant-Register (Slug, Status-Ampel), prüft die Scope-Map und steigt über den Slug-Link in die Tenant-Detailseite ein.',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(content).toContainText('Acme Corp')
      const scopeMap = page.locator('section[aria-label="scope map"]')
      await expect(scopeMap).toContainText('home')
      await expect(scopeMap).toContainText('128')
      await expect(scopeMap).toContainText(/unmapped/i)
      await page.getByRole('link', { name: 'acme' }).click()
      await expect(page).toHaveURL(new RegExp(`/admin/tenants/${TENANT_A_ID}$`))
      await expect(page.getByRole('heading', { name: 'tenant detail' })).toBeVisible()
    },
  },
  {
    // Template contract with fixture param (design 06 §4.2: EIN Kontrakt pro
    // Template, path? aus dem PV4-Kontrakt-Modell).
    route: '/admin/tenants/:id',
    name: 'admin-tenant-detail',
    role: 'server-admin',
    mode: 'reading',
    path: `/admin/tenants/${TENANT_A_ID}`,
    adminCalls: ['tenant-get', 'scope-overview', 'tenant-quota-get'],
    states: [{ name: 'default', seed: {} }],
    scale: {
      exempt:
        'Detailseite EINES Tenants: Register-Karte + eine QuotaForm pro eigenem Scope (server-seitig bounded über tenant_limits) — keine 10k-Liste.',
    },
    flowDoc:
      'Server-Admin öffnet die Tenant-Detailseite und setzt die Tages-Kosten-Quota eines Scopes (set → re-get → saved-Marker) — der Verwaltungs-Kernpfad der Seite.',
    primaryFlow: async (page, session) => {
      await expect(page.getByRole('heading', { name: 'tenant detail' })).toBeVisible()
      await expect(page.locator('main.content')).toContainText('Acme Corp')
      const quota = page.locator('form[aria-label="quota for scope home"]')
      await expect(quota).toBeVisible()
      await quota.getByLabel('daily cost (USD)').fill('9')
      await quota.getByRole('button', { name: 'save quota' }).click()
      await expect(quota.getByText('saved')).toBeVisible()
      // Wire proof: the save actually issued the tenant-quota-set action.
      expect(
        session.calls.some((x) => x.action === 'tenant-quota-set'),
        'quota save must reach the wire as tenant-quota-set',
      ).toBe(true)
    },
  },
  {
    // Type-registry admin (design 04 §4.7/§5.5, wave U10). Server-admin only —
    // /admin/types inherits the /admin prefix-guard (guard.ts TIER_GATED, U04
    // pin), so the generated deny test proves a tenant-admin is redirected to
    // /status and /api/types never fires. Rollen-Minimum == Guard-Rolle
    // (server-admin) → the default state seeds no role override.
    route: '/admin/types',
    name: 'admin-types',
    role: 'server-admin',
    mode: 'reading',
    adminCalls: ['/api/types'],
    states: [{ name: 'default', seed: {} }],
    scale: {
      exempt:
        'Type-Registry ist eine bounded, betreiber-definierte Liste (≪ 100 Zeilen: builtin-Defaults ∪ Tenant-Overlays) — kein nutzergetriebener 10k-Pfad (design 04 §5.5-Zeile /admin/types).',
    },
    mobile: {
      exempt:
        'Admin-Verwaltungsfläche, Ziel-Viewport dark+light × Desktop (design 04 §5.5-Zeile /admin/types); die Mobile-Baseline landet mit dem sequenzierten Voll-Satz-Re-Freeze (design 06 §9.3), wie die PV4-Erstbelegung.',
    },
    flowDoc:
      'Server-Admin öffnet die Type-Registry, liest die Typen mit Source-Badge (builtin/tenant) + Policy-Zusammenfassung und öffnet das deklarative Policy-Formular eines Typs (Edit-Kernpfad); builtin-Typen sind nicht löschbar (Delete disabled — die Komfort-Hälfte des Doppel-Schutzes, Server ist das Gate).',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(page.getByRole('heading', { name: 'Types', exact: true })).toBeVisible()
      // The frozen /api/types list renders the builtin 'issue' row + its badge.
      const rows = content.locator('section.card[aria-label="type registry"] tbody tr')
      await expect(rows).toHaveCount(1)
      const row = rows.first()
      await expect(row).toContainText('issue')
      await expect(row.locator('.badge')).toContainText('builtin')
      // Builtin ⇒ Delete disabled (the double-layer comfort half, §4.7).
      await expect(row.getByRole('button', { name: 'Delete' })).toBeDisabled()
      // Edit opens the declarative policy form (Modal) with the builtin note +
      // locked key — the human Edit core path.
      await row.getByRole('button', { name: 'Edit' }).click()
      const dialog = page.getByRole('dialog')
      await expect(dialog).toBeVisible()
      await expect(dialog.getByRole('heading', { name: /Edit issue/ })).toBeVisible()
      await expect(dialog).toContainText('Builtin type')
    },
  },
  {
    route: '/tenant',
    name: 'tenant',
    role: 'tenant-admin', // Rollen-Minimum: manageTenantKeys (tenant-admin), NICHT server-admin
    mode: 'reading',
    adminCalls: ['api-key-list', 'scope-list', 'tenant-usage-get'],
    states: [{ name: 'default', seed: {} }],
    scale: {
      exempt:
        'Key-/Scope-Tabellen sind durch tenant_limits gedeckelt (max_keys/max_scopes, Migration 069) — strukturell kein 10k-Pfad.',
    },
    flowDoc:
      'Tenant-Admin verwaltet die Schlüssel seines Tenants: Tabelle listet die Keys, „+ New key" mintet einen neuen Key mit Reveal-once-Plaintext (Kernpfad der Selbstverwaltung).',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(content).toContainText('smoke-key')
      await expect(content).toContainText('ci-runner')
      // Mint a key: dialog → create → reveal-once → acknowledge.
      await page.getByRole('button', { name: '+ New key' }).click()
      const dialog = page.getByRole('dialog')
      await expect(dialog).toBeVisible()
      await dialog.getByPlaceholder('who/what this key is for').fill('pv7-probe')
      await dialog.getByRole('button', { name: 'Create key' }).click()
      await expect(dialog.getByLabel('new api key — plaintext, shown once')).toHaveValue(
        'ctx_sk_TESTKEY_reveal_once_do_not_persist',
      )
      await dialog.getByRole('button', { name: "I've stored it — done" }).click()
      await expect(dialog).toBeHidden()
    },
  },
  {
    route: '*',
    name: 'notfound',
    role: 'member',
    mode: 'reading',
    path: '/no-such-route',
    states: [{ name: 'default', seed: {} }],
    scale: { exempt: 'Statische 404-Seite ohne Daten — kein 10k-Pfad.' },
    flowDoc:
      'Nutzer landet auf einer unbekannten Route, sieht die 404-Auskunft und kehrt über den Status-Link zurück (Guard-Ist: /status ist member-erreichbar — Rail↔Guard-Divergenz, PV4-Befund).',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(content).toContainText('404')
      await expect(content).toContainText('No such route in this UI.')
      await page.getByRole('link', { name: 'Back to status' }).click()
      await expect(page).toHaveURL(/\/status$/)
      // Member on /status: the read-only degradation renders the public
      // health probe (guard truth, not rail wish — no silent "Behebung").
      await expect(content).toContainText('read-only key')
    },
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
 * Bestands-Lücken (design 06 S13): SINCE PV7 THIS LIST IS EMPTY — the matrix
 * runs green without an exception list ("jede Seite" holds for the whole
 * Ist-Bestand, the PV7 gate). The MECHANIC stays: a future route lands here
 * (with reason + delivering wave) or carries a contract, otherwise the matrix
 * meta-test is red; a pending entry whose contract exists is stale ⇒ red.
 */
export const pendingContracts: PendingContract[] = []

/** Sentinel re-export so leak-probe consumers need only the registry. */
export { SENTINEL }
