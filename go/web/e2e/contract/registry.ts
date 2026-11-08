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
import { KEY, REVEAL_MARKER, SENTINEL, modelApiCapabilities, sseRoute, type RecordedCall, type SseFrame } from '../fixtures'
import { PANEL_CALL_ID, PANEL_CONSUMER_A } from '../gateway-panels-fixtures'

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

/** The data payload of the session's model-consumer-list calls, in wire order (W05-05). */
function consumerListBodies(calls: RecordedCall[]): Array<Record<string, unknown>> {
  return calls
    .filter((x) => x.action === 'model-consumer-list')
    .map((x) => ((x.body as { data?: Record<string, unknown> } | undefined)?.data ?? {}))
}

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
      'Operator öffnet die Status-Übersicht und liest den Live-Zustand tile-vollständig: Health-Ampel mit Services, Profil-Quick-Toggle, Dream-Queue-Zahlen, Backend-Pool-Zeile und LLM-Telemetrie tragen die Fixture-DATEN (PV7: erste Inhalts-Asserts auf jede Tile — Inventur §5 hatte keine einzige).',
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
      // Toggles tile: the eject disable-profile quick-toggle (U01-W7, replacing
      // the retired gaming lock) reflects the fixture profile's active=false.
      const toggles = page.locator('section.card[aria-label="toggles"]')
      await expect(toggles.getByRole('button', { name: 'Eject-Modus umschalten' })).toHaveAttribute(
        'aria-pressed',
        'false',
      )
      await expect(toggles).toContainText('no signal') // activity: null
      // Dream tile: queue counters + mode segment + meta rows.
      const dream = page.locator('section.card[aria-label="dream queue"]')
      await expect(dream.locator('.stat', { hasText: 'pickable now' })).toContainText('4')
      await expect(dream.locator('.stat', { hasText: 'never dreamed' })).toContainText('12')
      await expect(dream.locator('.stat', { hasText: 'incoming 6h' })).toContainText('9')
      // Back-off histogram (dream-stats fixture): policy line + one level row
      // per occupied eval count, cooldown rendered through fmtHours.
      await expect(dream).toContainText('re-dream back-off')
      await expect(dream).toContainText('mode=exp')
      const levels = dream.locator('.bo-levels')
      await expect(levels).toContainText('n=0')
      await expect(levels).toContainText('→ 1.3d') // 30.7h at n=3
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
      'Admin editiert eine Einstellung im Katalog (Edit-Roundtrip, §7-PV7): dream-Karte aufklappen (Karten starten eingeklappt) → dream.enabled-Switch kippen → Save der Gruppe → genau EIN PUT /api/settings/dream.enabled mit {value:false} auf dem Draht (postData-Assert) → Echo wendet source=db an und der Dirty-Zähler verschwindet. Danach Fuzzy-Suche als Zweit-Probe: Tippfehler-Query findet den Key über die Levenshtein-Stufe.',
    primaryFlow: async (page, session) => {
      const content = page.locator('main.content')
      // Collapsed index view: the prefixes read as card headers, key fields
      // are not mounted yet (Settings-Rework: cards start collapsed).
      const card = page.locator('section.card[aria-label="dream settings"]')
      await expect(card).toBeVisible()
      await expect(content).toContainText('pool')

      // Edit-Roundtrip (design 06 §4.1: /settings = Edit-Roundtrip, nie der
      // Empty-State). Expand the dream card first; dream.enabled is a hot
      // bool → role=switch checkbox.
      await card.locator('button.disclosure').click()
      const sw = card.locator('[id="dream.enabled"]')
      await expect(sw).toBeChecked() // fixture value true
      // The dream card mounts the back-off curve editor over the six
      // dream.backoff_* fixture keys — the curve renders, not its
      // "curve paused" invalid-draft notice.
      await expect(card.locator('.curve svg')).toBeVisible()
      await sw.click()
      await expect(card).toContainText('1 unsaved')
      await card.getByRole('button', { name: 'Save' }).click()

      // Applied echo: the PUT response flips the source badge env → db and
      // clears the dirty marker (drafts re-sync to the stored value). Scoped
      // to the dream.enabled field — the backoff fixture rows carry their
      // own db badges (strict mode).
      const enabledField = card.locator('.field', { has: page.locator('[id="dream.enabled"]') })
      await expect(enabledField.locator('span.badge.source-db')).toBeVisible()
      await expect(card).not.toContainText('unsaved')
      await expect(sw).not.toBeChecked()

      // postData proof (§7-PV7): exactly one PUT carried {value:false}.
      const puts = session.calls.filter((x) => x.method === 'PUT' && x.path === '/api/settings/dream.enabled')
      expect(puts, 'the group save issues exactly ONE PUT for the one dirty key').toHaveLength(1)
      expect(puts[0].body, 'PUT body carries the toggled scalar').toEqual({ value: false })

      // Fuzzy search (Levenshtein tier: "timezome" is neither substring nor
      // in-order subsequence of "timezone" — only the edit-distance tier
      // reaches it): the typo query still surfaces the key, non-matching
      // groups leave the page.
      await page.getByRole('searchbox', { name: 'search settings' }).fill('timezome')
      await expect(content).toContainText('server.timezone')
      await expect(card).not.toBeVisible()
      await page.getByRole('searchbox', { name: 'search settings' }).fill('')

      // ABSENZ der Legacy-Karte (β9, E11 — Umkehr des Stufe-1-Contracts):
      // the trailing "legacy (superseded)" pseudo-group used to collect the
      // keys the backend pool had replaced. The mechanic was torn down
      // completely (registry marker, API field, PUT 409, CLI hint, FE card),
      // so the page must render NO such card and NO superseded badge — on a
      // page that renders every other settings card fine (asserted above).
      await expect(page.locator('section.card[aria-label="legacy (superseded) settings"]')).toHaveCount(0)
      await expect(page.locator('section.card').filter({ hasText: 'superseded by the backend pool' })).toHaveCount(0)
      await expect(content.locator('span.badge', { hasText: /^superseded$/ })).toHaveCount(0)
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
          // R4: the reject comes from POST /auth/login — the real handler's
          // uniform sessionError string (auth_session.go).
          await expect(page.getByRole('alert')).toContainText('authentication failed')
        },
      },
    ],
    scale: {
      exempt: 'Login-Maske: ein einzelnes Formular ohne Datenliste — es existiert kein 10k-Wachstumspfad.',
    },
    flowDoc:
      'Nutzer fügt den API-Key ein und meldet sich an: POST /auth/login tauscht ihn gegen httpOnly-Cookies (R4) → whoami hydriert → Shell mountet → Member-Landing /home. Der Fehl-Key-Pfad (Fehlerband, NIE Shell) ist der deklarierte error-State + freie Negativ-Probe in smoke.spec.ts.',
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
      {
        name: 'empty',
        seed: { empty: true },
        // Dritter Fall der 1-px-AA-Klasse (nach Q11 und den settings-Shots):
        // eine Kantenzelle wobbelt ±1 LSB zwischen Läufen im selben pinned
        // Container (1 px, light mobile — v4.22.2 Rebaseline-Verify-Läufe).
        // Gleiche Stufe, gleiche Schranke: 4 ≫ 1, ≪ jede echte Regression.
        visualTolerance: {
          maxDiffPixels: 4,
          reason:
            'Edge-pixel AA wobbles ±1 LSB between runs in the same pinned container (1 px, light mobile; v4.22.2 rebaseline verify runs).',
          issue: 'design 06-§4.3 rung 3; PR #12 follow-up (scrollbar stabilizer rebaseline, root run 30718783042)',
        },
      },
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
        // Rung 3 (design 05-§8.E3 ladder / 06 §4.3) — same failure class as the
        // board 'empty' state below: Q11 self-hosted Mono glyph-edge AA wobbles
        // ±1 LSB under parallel-worker load (1 px, YIQ-delta just over the
        // calibrated 3.5) on this sparse filtered-list shot. Locally (12 cores)
        // the CI retry absorbed it; on the 4-vCPU GitHub runner it failed BOTH
        // attempts on three of the four theme/viewport variants (v4.5.0 run
        // 28722080386), the fourth failed locally — the wobble rides the state,
        // not one variant. mask/stylePath do not fit a single AA edge pixel.
        // 4 ≫ the observed 1 yet ≪ any real regression (hundreds of px). NOT a
        // global loosening — every other shot keeps 0.
        visualTolerance: {
          maxDiffPixels: 4,
          reason:
            'Q11 self-hosted Mono: glyph edge AA wobbles ±1 LSB on the sparse search shot under load (1 px, delta ~3.6); failed both CI attempts on the 4-vCPU runner (v4.5.0), local retry absorbed it.',
          issue: 'design 05-§8.E3 (Q11 Font-Determinismus escalation rung 3; CI run 28722080386)',
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
    // Template contract (design 06 §4.2): EIN Kontrakt pro Template, path nötig.
    // U06 wires the real detail. States (design 04 §5.5):
    //  - default      the freeze issue (scope acme:main) — read-only for the
    //                 generic member (home 'home'): the deep-link-read-only
    //                 baseline (no composer, read-only banner).
    //  - dialog-status a WRITABLE issue (scope 'home', prepared via a detail
    //                 override + reload) with the status-transition ConfirmDialog
    //                 OPEN — the Q9 dialog-open Screenshot baseline (name scheme
    //                 dialog-<name>; the matching ARIA snapshot lives in
    //                 issue-detail.spec.ts, the harness snapshots only default).
    route: '/issues/:id',
    name: 'issue-detail',
    role: 'member',
    mode: 'split',
    path: '/issues/550e8400-e29b-41d4-a716-446655440001',
    states: [
      { name: 'default', seed: {} },
      {
        name: 'dialog-status',
        seed: {},
        prepare: async (page) => {
          // Serve a WRITABLE (home-scoped) issue for the detail GET so the
          // status/composer controls render, then reload and open the confirm.
          await page.route('**/api/project/*/issues/*', async (route) => {
            const u = new URL(route.request().url())
            if (route.request().method() !== 'GET' || u.pathname.endsWith('/comments')) return route.fallback()
            return route.fulfill({
              status: 200,
              json: {
                success: true,
                render: 'untrusted',
                comments: [],
                comments_cursor: null,
                issue: {
                  id: '550e8400-e29b-41d4-a716-446655440001',
                  category: 'task',
                  tags: ['bug'],
                  title: 'Writable issue',
                  content: '# Writable\n\nBody markdown.',
                  metadata: { sync_state: 'in_sync' },
                  scope: 'home',
                  type: 'issue',
                  workflow_status: 'open',
                  created_at: '2026-07-01T00:00:00Z',
                  updated_at: '2026-07-03T00:00:00Z',
                },
              },
            })
          })
          await page.reload()
          await expect(page.locator('.shell')).toBeVisible()
          await expect(page.getByRole('heading', { name: 'Writable issue' })).toBeVisible()
          await page.getByRole('combobox', { name: 'Workflow status' }).selectOption('closed')
          await page.getByRole('button', { name: 'Change status' }).click()
          await expect(page.getByRole('dialog')).toBeVisible()
        },
      },
    ],
    scale: {
      // 500-comment thread (design 04 §5.5): the state '10k' seed makes the
      // detail wire return 500 comments; the progressive-reveal cap keeps the
      // rendered thread bounded (<= 150) while the model holds all 500 — the
      // render-budget proof (remove the cap and the page renders 500 <li>).
      name: '10k',
      seed: { state: '10k' },
      domCap: { selector: 'main.content li[data-comment]', max: 150 },
    },
    flowDoc:
      'Deep-Link auf /issues/:id lädt Issue + Kommentare: der Markdown-Body und jeder Kommentar rendern über die sanitizende lib/markdown-Pipeline, das Sync-Badge zeigt den Forge-Zustand, und im schreibbaren Scope erscheinen Composer + Status-/Titel-Mutation (sonst read-only).',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(page.getByRole('heading', { name: 'Example issue' })).toBeVisible()
      // The markdown body rendered through the shared pipeline.
      await expect(content).toContainText('Body markdown')
      // The one freeze comment renders in the thread.
      await expect(content.locator('li[data-comment]')).toHaveCount(1)
      // The freeze issue lives in acme:main; the generic member (home 'home')
      // cannot write it → read-only (no composer), the §5.5 deep-link default.
      await expect(content).toContainText('Read-only in this scope')
      await expect(content.getByRole('textbox', { name: 'Add a comment' })).toHaveCount(0)
    },
  },
  {
    // U07 wires the real read-only board (design 04 §4.2/§5.5). States:
    //  - default the rich board seed (columns open/in_progress/review/done in
    //            wire order; `done` is terminal → starts COLLAPSED). The board
    //            seed ALSO overrides GET /api/types with a workflow config
    //            (states + terminal) — the freeze type-list.json has none.
    //  - empty   a project whose board has zero columns → EmptyState.
    // U09: mobile is the single-column Column-Pager (one column + prev/next);
    // desktop keeps the horizontal multi-column scroll AND opens a card as a
    // floating detail window (lib/windows interop).
    route: '/board',
    name: 'board',
    role: 'member',
    mode: 'board',
    // U09 lifts the U07 mobile opt-out: the 390 Column-Pager now has real
    // baselines (design 04 §7-U09). Mobile visual/aria freeze from here.
    states: [
      { name: 'default', seed: { state: 'board' } },
      {
        name: 'empty',
        seed: { empty: true },
        // Rung 3 (design 05-§8.E3 ladder / 06 §4.3): the Q11 self-hosted Mono
        // (JetBrains Mono) draws the accent-blue glyph edges of the empty board's
        // sparse chrome against the wide surface-0 field; under parallel-worker
        // load the pinned SwiftShader rasteriser wobbles ONE such edge pixel by
        // ±1 LSB in the blue channel (measured: 1 pixel, YIQ-delta ~3.6, just
        // over the calibrated maxDelta 3.5). It is deterministic in isolation and
        // load-induced under contention, and CI retries do NOT clear it on this
        // sparse shot (fails both attempts). mask (rung 1)/stylePath (rung 2) do
        // not fit a single AA edge pixel; per-shot maxDiffPixels is the ladder's
        // rung 3. 4 ≫ the observed 1 yet ≪ any real regression (hundreds of px).
        // Issue anchor: design 05-§8.E3 (Q11 Font-Determinismus). NOT a global
        // loosening — this rides the empty state only; every other shot keeps 0.
        visualTolerance: {
          maxDiffPixels: 4,
          reason:
            'Q11 self-hosted Mono: accent-blue glyph edge AA wobbles ±1 LSB on surface-0 under load (1 px, delta ~3.6); deterministic in isolation, not retry-clearable on this sparse shot.',
          issue: 'design 05-§8.E3 (Q11 Font-Determinismus escalation rung 3)',
        },
      },
    ],
    scale: {
      // 10k×6 board: each column count = 10 000, a bounded first page + a
      // per-column cursor. The per-column windowing keeps the DOM under 300 cards
      // across the open columns (done collapsed renders zero) — remove the
      // windowing (Column.svelte) and the open columns render every loaded card.
      name: 'board-10k',
      seed: { state: 'board-10k' },
      domCap: { selector: 'main.content [data-board-card]', max: 300 },
      flow: async (page) => {
        // The count is the WIRE total (B7), not the loaded card length: an open
        // column shows 10 000 while only a bounded page is hydrated.
        const open = page.locator('[data-board-column][data-status="open"]')
        await expect(open.locator('[data-count]')).toHaveText('10000')
      },
    },
    flowDoc:
      'Deep-Link auf /board rendert die Status-Spalten aus dem Board-Wire (Reihenfolge == Wire-Order == Type-Config), jede mit ihrem Wire-Count; terminale Spalten (registry workflow.terminal) starten eingeklappt, offene zeigen ihre Karten; Desktop-Klick öffnet das Detail als Fenster (lib/windows), Mobile (<SM) blättert einspaltig per Column-Pager (Tap navigiert zu /issues/:id).',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()
      // Lone project auto-selects (0/1/N picker) and writes ?scope=.
      await expect(page).toHaveURL(/scope=acme(%3A|:)main/)
      // Columns render in WIRE order (open, in_progress, review, done).
      const statuses = await content.locator('[data-board-column]').evaluateAll((els) =>
        els.map((e) => e.getAttribute('data-status')),
      )
      expect(statuses).toEqual(['open', 'in_progress', 'review', 'done'])
      // open: count 3 from the wire, its cards render + link to the detail.
      const open = content.locator('[data-board-column][data-status="open"]')
      await expect(open.locator('[data-count]')).toHaveText('3')
      await expect(open.locator('[data-board-card]').first()).toBeVisible()
      // done is terminal → COLLAPSED: count visible, zero cards rendered.
      const done = content.locator('[data-board-column][data-status="done"]')
      await expect(done.locator('[data-count]')).toHaveText('12')
      await expect(done.getByRole('button')).toHaveAttribute('aria-expanded', 'false')
      await expect(done.locator('[data-board-card]')).toHaveCount(0)
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
    states: [
      {
        name: 'default',
        seed: { role: 'server-admin' },
        // Gleiche 1-px-AA-Klasse wie settings-hues (und Q11): eine Kantenzelle
        // wobbelt ±1 LSB zwischen Läufen im SELBEN pinned Container (v4.22.2
        // Scrollbar-Stabilizer-Rebaseline, light mobile). 4 ≫ beobachtete 1,
        // ≪ jede echte Regression; alle anderen Shots behalten 0.
        visualTolerance: {
          maxDiffPixels: 4,
          reason:
            'Edge-pixel AA wobbles ±1 LSB between runs in the same pinned container (1 px, light mobile; v4.22.2 rebaseline).',
          issue: 'design 06-§4.3 rung 3; PR #12 follow-up (scrollbar stabilizer rebaseline, root run 30718783042)',
        },
      },
    ],
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
    route: '/settings/hues',
    name: 'settings-hues',
    // Guard-Wahrheit wie /settings/backends: kein TIER_GATE auf /settings/* —
    // die Fläche ist page-self-gated (session.admin || viewOpsSurfaces =
    // tenant-admin-or-up, = Server-Schreib-Gate W5). Mechanisches Rollen-Minimum
    // = member (erreicht die Route, sieht das Banner statt eines 403-Requests);
    // der Inhalt braucht admin (fixtures: nur server-admin trägt admin:true).
    role: 'member',
    mode: 'reading',
    states: [
      {
        name: 'default',
        seed: { role: 'server-admin' },
        // Hue-Rad-AA: der große SVG-Kreisbogen wobbelt ±1 LSB an einer
        // Kantenzelle — 1 px Diff würfelte über --update- und Verify-Lauf im
        // SELBEN pinned Container verschieden (v4.22.2 Scrollbar-Stabilizer-
        // Rebaseline, light mobile). Gleiche Eskalationsstufe wie Q11:
        // 4 ≫ beobachtete 1 und ≪ jede echte Regression; alle anderen Shots
        // behalten 0.
        visualTolerance: {
          maxDiffPixels: 4,
          reason:
            'SVG hue-wheel arc AA wobbles ±1 LSB on one edge pixel (1 px, mobile); differed between --update and verify runs in the same pinned container (v4.22.2 rebaseline).',
          issue: 'design 06-§4.3 rung 3; PR #12 follow-up (scrollbar stabilizer rebaseline, root run 30718783042)',
        },
      },
    ],
    scale: {
      exempt:
        'Kategorie-Liste ist eine bounded Betreiber-/Tenant-Menge (Block-Kategorien) — die Override-Zeilen skalieren mit der Kategorie-Anzahl, nicht mit Blöcken (kein 10k-Nutzerpfad).',
    },
    flowDoc:
      'Admin öffnet die Farb-Override-Fläche unter dem Settings-Crumb, wählt eine Kategorie und setzt ihren Graph-Hue am Regler (optimistische Vorschau + PUT); der Crumb führt zurück in den Katalog.',
    primaryFlow: async (page, session) => {
      const content = page.locator('main.content')
      await expect(page.getByRole('heading', { name: 'kategorie-farben' })).toBeVisible()
      // Kategorie-Liste aus list-categories (Fixture: design/reference/learnings).
      const cats = content.locator('ul[aria-label="kategorien"] .cat')
      await expect(cats).toHaveCount(3)
      // Kategorie wählen → der Hue-Regler erscheint.
      await content.locator('ul[aria-label="kategorien"] .row').first().click()
      const slider = page.getByRole('slider')
      await expect(slider).toBeVisible()
      // Hue setzen → PUT auf den Draht (die lokale Map + Swatches ziehen optimistisch mit).
      await slider.fill('200')
      await expect
        .poll(() => session.calls.some((c) => c.method === 'PUT' && c.path.startsWith('/api/graph/category-hues/')))
        .toBe(true)
      // Crumb zurück in den Katalog.
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
    states: [
      { name: 'default', seed: {} }, // Rollen-Minimum == Guard-Rolle (server-admin)
      {
        // U12: the project-provisioning wizard OPEN at its entry chooser (design
        // 04 §7-U12 baseline row: dark × Dialog). Deterministic (no async data —
        // the mode chooser + existing-tenant picker), so it freezes byte-stably;
        // the wire/flow/abort/reveal halves live in provision-wizard.spec.ts, the
        // ARIA snapshot is spec-local there (like issue-detail dialog-status).
        name: 'wizard',
        seed: {},
        prepare: async (page) => {
          await page.getByRole('button', { name: '+ Provision project' }).click()
          await expect(page.getByRole('dialog')).toBeVisible()
        },
      },
    ],
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
    adminCalls: ['tenant-get', 'scope-overview', 'tenant-quota-get', 'tenant-grant-list'],
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
    // Tenant-scoped backend pool (design 04 §4 E04-4 / §5.5, wave U11) — the
    // E04-4 rot-fix route. Reuses the settings BackendsPage in its tenant-scoped
    // variant (pool only, no vault). role:'tenant-admin' = the guard truth: the
    // /tenant prefix-guard (guard.ts TIER_GATED → manageTenantKeys) redirects a
    // member; the generated deny test proves the member lands on /home and
    // backend-list never fires. Mobile exempt like the other admin surfaces (the
    // §5.5-Zeile targets dark+light × Desktop); the mobile baseline rides the
    // sequenced full-set re-freeze (design 06 §9.3).
    route: '/tenant/backends',
    name: 'tenant-backends',
    role: 'tenant-admin',
    mode: 'reading',
    adminCalls: ['backend-list'],
    states: [{ name: 'default', seed: {} }],
    scale: {
      exempt:
        'Backend-Pool ist eine bounded Betreiber-Liste (Provider-Backends pro Tenant, server-seitig überschaubar) — kein nutzergetriebener 10k-Pfad (design 04 §5.5-Zeile /tenant/backends).',
    },
    mobile: {
      exempt:
        'Tenant-Verwaltungsfläche, Ziel-Viewport dark+light × Desktop (design 04 §5.5-Zeile /tenant/backends); die Mobile-Baseline landet mit dem sequenzierten Voll-Satz-Re-Freeze (design 06 §9.3), wie /admin/types.',
    },
    flowDoc:
      'Tenant-Admin öffnet den Backend-Pool unter dem Tenant-Crumb: die Pool-Tabelle mountet hinter dem tenant-admin-Self-Gate (Fixture-Empty-State als Positiv-Kontrolle des Ladepfads), der Vault bleibt server-admin-only ausgeblendet, und der Crumb führt zurück auf /tenant.',
    primaryFlow: async (page) => {
      const content = page.locator('main.content')
      await expect(page.getByRole('heading', { name: 'Backend pool', exact: true })).toBeVisible()
      // Pool loaded past the tenant self-gate: the fixture [] renders the empty
      // state (positive control of the reachable, tenant-scoped load path).
      await expect(content).toContainText('no backends — create one to populate the pool')
      // The vault is server-admin only → hidden in the tenant variant.
      await expect(page.locator('section.card[aria-label="secrets vault"]')).toHaveCount(0)
      // Crumb navigates back to the tenant self-service area.
      await page.locator('.crumb').getByRole('link', { name: 'Tenant' }).click()
      await expect(page).toHaveURL(/\/tenant$/)
    },
  },
  {
    // Model API area (design/05 §4.6, waves W05-05 + W05-06): the consumer list
    // and, since W05-06, create with reveal-once, deactivate/reactivate and key
    // revoke. capabilities = gateway ON with self-service, so the tenant-admin
    // holds mintModelKeys (the full Kernauftrag world: create AND revoke); the
    // tier UX without self-service is e2e/gateway-mint.spec.ts (G6).
    // role:'tenant-admin' = the guard truth with the gateway ON — viewModelApi is
    // serverAdmin || (tenantAdminOrUp && model_api.enabled), so the contract-wide
    // `capabilities` seeds the gateway-on world into every generated dimension:
    // the deny test then proves the TIER gate (a member with the gateway on still
    // lands on /home, model-consumer-list never fires) and the leak probe reaches
    // the page as tenant-admin B. The flag half (tenant-admin with enabled:false ⇒
    // /status) is e2e/gateway.spec.ts. Full mobile dimension: a new page freezes
    // its own baselines from day one (PV7 rule).
    route: '/gateway',
    name: 'gateway',
    role: 'tenant-admin',
    mode: 'reading',
    capabilities: modelApiCapabilities(true, true),
    tenantScoped: true,
    // RC-D C5: + the policy edit (W05-07) and the rotation (W05-08) — calls a
    // member session must never fire (generated deny test).
    adminCalls: ['model-consumer-list', 'tenant-list', 'model-consumer-update', 'model-key-rotate'],
    states: [
      { name: 'default', seed: {} },
      // W05-06: the create dialog (template row, scope select, offer picker,
      // expiry, sensitivity, CIDR) — axe + visual of the form view
      {
        name: 'create',
        seed: {},
        prepare: async (page) => {
          await page.getByRole('button', { name: '+ New consumer' }).click()
          await expect(page.locator('[data-testid="models-ready"]')).toBeAttached()
        },
      },
      // W05-06: the revoke confirm of ci-chat's key (danger, typed 4-character gate)
      {
        name: 'revoke',
        seed: {},
        prepare: async (page) => {
          await page.getByRole('button', { name: 'Revoke key ctxm_…a9Fq' }).click()
          await expect(page.getByRole('dialog', { name: 'Revoke key' })).toBeVisible()
        },
      },
      // cross-tenant view: tenant column + tenant filter (tenant-list)
      { name: 'server-admin', seed: { role: 'server-admin' } },
      // no consumer yet: the empty state carries the /v1 base URL hint
      { name: 'empty', seed: { state: 'empty' } },
      // the list read fails: error band with Retry
      { name: 'error', seed: { faults: [{ action: 'model-consumer-list', status: 500 }] } },
    ],
    scale: {
      name: '10k',
      seed: { state: '10k' },
      // Keyset pages of gateway.consumer_list_max_limit (100, the client sends no
      // limit): the first render is ONE page, „Load more" appends exactly one
      // more — 200 = two pages ≪ 10 000. A list without pagination renders 10 000.
      domCap: { selector: 'main.content tr[data-consumer-row]', max: 200 },
      flow: async (page, session) => {
        const rows = page.locator('main.content tr[data-consumer-row]')
        await expect(rows).toHaveCount(100)
        await page.getByRole('button', { name: 'Load more' }).click()
        await expect(rows).toHaveCount(200)
        // the mock resumes strictly after the cursor's row: row 100 = consumer 100
        await expect(rows.nth(100)).toContainText('consumer-00100')
        const bodies = consumerListBodies(session.calls)
        expect(bodies[0]?.cursor, 'page 1 carries no cursor').toBeUndefined()
        expect(typeof bodies[bodies.length - 1]?.cursor, 'load-more re-issues the list WITH the opaque cursor').toBe('string')
      },
    },
    flowDoc:
      'Tenant-Admin öffnet „Model API": die Konsumenten-Liste lädt mit dem Default-Statusfilter (die vier Status eines aktiven Konsumenten), Status-Badges und Key-Hints kommen vom Server; „+ New consumer" mit Vorlage „App · Chat" legt einen Konsumenten an, der Klartext steht genau einmal im Reveal und ist nach „I\'ve stored it — done" aus dem DOM, die Liste zeigt nur den Hint; der Key von ci-chat wird erst nach Tippen seiner 4 Hint-Zeichen widerrufen, der Toast meldet laufende Streams laut Server (W05-06); „inactive" zuschalten erweitert die Liste, die Label-Präfix-Suche engt sie ein.',
    primaryFlow: async (page, session) => {
      const content = page.locator('main.content')
      await expect(page.getByRole('heading', { name: 'Model API', exact: true })).toBeVisible()
      const rows = content.locator('tr[data-consumer-row]')
      // default filter: active, expiring, rotating, no_key — the inactive row stays out
      await expect(rows).toHaveCount(4)
      await expect(content).not.toContainText('legacy-app')
      const ci = rows.filter({ hasText: 'ci-chat' })
      await expect(ci.locator('[data-consumer-status]')).toHaveText('active')
      await expect(ci).toContainText('ctxm_…a9Fq')
      // B-11: the fixture key expired by the page clock, the server says active —
      // the server's verdict is what renders (never the client clock)
      await expect(ci.locator('[data-key-status]')).toHaveText(['active'])
      await expect(rows.filter({ hasText: 'ctx-peer-b' }).locator('[data-consumer-status]')).toHaveText('rotating')
      expect(consumerListBodies(session.calls)[0]?.status).toEqual(['active', 'expiring', 'rotating', 'no_key'])
      // W05-06 — create: template "App · Chat", reveal exactly once, ack wipes it
      await content.getByRole('button', { name: '+ New consumer' }).click()
      const dialog = page.getByRole('dialog', { name: /New model consumer|Consumer created/ })
      await expect(dialog.locator('[data-testid="models-ready"]')).toBeAttached()
      await dialog.getByRole('button', { name: 'App · Chat' }).click()
      await dialog.getByLabel('label', { exact: true }).fill('flow-app')
      await dialog.getByRole('button', { name: 'Create consumer' }).click()
      await expect(dialog.getByRole('textbox', { name: 'new api key — plaintext, shown once' })).toHaveValue(REVEAL_MARKER)
      await dialog.getByRole('button', { name: "I've stored it — done" }).click()
      await expect(dialog).toHaveCount(0)
      await expect(rows).toHaveCount(5)
      await expect(rows.filter({ hasText: 'flow-app' })).toContainText('ctxm_…sist')
      expect(await page.evaluate((m) => document.documentElement.outerHTML.includes(m), REVEAL_MARKER)).toBe(false)
      // RC-D W05-07/W05-08: the own app row carries "Edit policy" and "Rotate key"
      // (tenant-admin with self-service); the ctx-peer row carries neither — peers
      // are managed by the server-admin (design/05 §4.5, V-01-b mirror).
      await expect(ci.getByRole('button', { name: 'Edit policy of ci-chat' }), 'W05-07: own app row has an edit control').toHaveCount(1)
      await expect(ci.getByRole('button', { name: 'Rotate key of ci-chat' }), 'W05-08: own app row has a rotate control').toHaveCount(1)
      const peerRow = rows.filter({ hasText: 'ctx-peer-b' })
      await expect(peerRow.getByRole('button', { name: /edit|rotate/i }), 'W05-07: a ctx-peer row carries no edit / rotate control below server-admin').toHaveCount(0)
      // W05-06 — revoke ci-chat's key behind the typed 4-character gate; toast from the server
      await ci.getByRole('button', { name: 'Revoke key ctxm_…a9Fq' }).click()
      const confirm = page.getByRole('dialog', { name: 'Revoke key' })
      await confirm.getByRole('button', { name: 'Revoke', exact: true }).click()
      await expect(confirm.getByRole('button', { name: 'Revoke', exact: true })).toBeDisabled()
      await confirm.getByRole('textbox').fill('a9Fq')
      await confirm.getByRole('button', { name: 'Revoke', exact: true }).click()
      await expect(confirm).toHaveCount(0)
      await expect(page.locator('[data-testid="gateway-toast"]')).toContainText('Running streams continue until they finish')
      await expect(ci.locator('[data-consumer-status]')).toHaveText('no key')
      // status filter: + inactive ⇒ all five statuses, sent as ["all"]
      await content.getByRole('checkbox', { name: 'inactive', exact: true }).check()
      await expect(rows).toHaveCount(6)
      await expect(content).toContainText('legacy-app')
      expect(consumerListBodies(session.calls).at(-1)?.status).toEqual(['all'])
      // label-prefix search narrows the list (server-side q)
      await content.getByRole('searchbox', { name: 'label prefix' }).fill('ci')
      await content.getByRole('button', { name: 'Search' }).click()
      await expect(rows).toHaveCount(1)
      await expect(rows.first()).toContainText('ci-chat')
      expect(consumerListBodies(session.calls).at(-1)?.q).toBe('ci')
    },
  },
  {
    // Alias catalog /gateway/models (design/05 §3.3/§4.6, wave W05-13 — RC-D
    // C5). Inherits the /gateway prefix guard (viewModelApi), so the contract
    // carries the same gateway-on world. A tenant-admin reads the _global
    // catalog + its home scope and writes only the home scope: `_global` rows
    // carry no write control (inv05 D8 precedent defect). The leak probe reads
    // tenant B's home sentinel alias (disabled, so no offer changes) and must
    // never see tenant A's. Detailed gates: e2e/gateway-models.spec.ts.
    route: '/gateway/models',
    name: 'gateway-models',
    role: 'tenant-admin',
    mode: 'reading',
    capabilities: modelApiCapabilities(true, true),
    tenantScoped: true,
    adminCalls: ['model-alias-list', 'model-alias-create', 'model-alias-update', 'model-alias-preview'],
    states: [
      { name: 'default', seed: {} },
      // every scope, write controls on every row, "Suggest from pool" + preview
      { name: 'server-admin', seed: { role: 'server-admin' } },
      // the catalog read fails: error band with Retry
      { name: 'error', seed: { faults: [{ action: 'model-alias-list', status: 500 }] } },
    ],
    scale: {
      exempt:
        'Alias-Katalog ist eine Betreiber-Liste (Dutzende Aliase je Scope, Admin-Takt, design/05 §6 Zeile Katalog-Reload) — kein nutzergetriebener 10k-Pfad.',
    },
    flowDoc:
      'Tenant-Admin öffnet „Model catalog“ unter /gateway/models: die _global-Aliase stehen ohne Schreib-Controls in der Liste, die Aliase des Heimat-Scopes tragen „Edit“; „+ New alias“ öffnet den Editor mit explizitem enabled-Schalter, niedrigere Sensitivitäten sind gesperrt; im Bearbeiten sind name und kind schreibgeschützt (W05-13).',
    primaryFlow: async (page, session) => {
      const content = page.locator('main.content')
      await expect(page.getByRole('heading', { name: 'Model catalog', exact: true }), 'W05-13: the catalog page mounts').toBeVisible()
      const rows = content.locator('tr[data-alias-row]')
      await expect(rows.filter({ hasText: SENTINEL.A }), 'W05-13: the home-scope alias is listed').toHaveCount(1)
      const globalChat = rows.filter({ has: page.locator('[data-alias-scope="_global"]') }).filter({ hasText: 'chat' }).first()
      await expect(globalChat, 'W05-13: the _global chat alias is listed').toBeVisible()
      await expect(globalChat.getByRole('button'), 'W05-13 gate: a _global row carries no write control for a tenant-admin').toHaveCount(0)
      const homeChat = rows.filter({ has: page.locator('[data-alias-scope="home"]') }).filter({ hasText: 'chat' }).first()
      await expect(homeChat.getByRole('button', { name: 'Edit alias chat' }), 'W05-13: a home-scope row is editable').toHaveCount(1)
      await content.getByRole('button', { name: '+ New alias' }).click()
      const editor = page.getByRole('dialog', { name: 'New model alias' })
      await expect(editor.getByRole('checkbox', { name: 'enabled' }), 'W05-13: enabled is an explicit control').toBeVisible()
      await expect(editor.getByRole('group', { name: 'sensitivity' }).getByRole('radio', { name: 'public' }), 'W05-13: a tenant-admin cannot lower below credentials').toBeDisabled()
      await editor.getByRole('button', { name: 'Cancel' }).click()
      await homeChat.getByRole('button', { name: 'Edit alias chat' }).click()
      const edit = page.getByRole('dialog', { name: 'Edit model alias' })
      await expect(edit.getByLabel('name', { exact: true }), 'W05-13 gate: name is read-only on edit').toBeDisabled()
      await expect(edit.getByLabel('kind', { exact: true }), 'W05-13 gate: kind is read-only on edit').toBeDisabled()
      expect(session.calls.some((c) => c.action === 'model-alias-preview'), 'a tenant-admin never fires the server-admin preview').toBe(false)
    },
  },
  {
    // Consumer detail (RC-D C6, waves W05-09 + W05-10 + W05-12; design/05
    // §4.6): head, key chain (rotated_from), last calls (body-free, no
    // llmlog detail below server-admin), audit trail as a chain, usage panel
    // (null/complete:false = "not recorded", hist:false = marked means, no
    // auth count per consumer, a11y table) and limits card (tenant tier:
    // max = server(d), no "unlimited", 422 at the field). The page inherits
    // the /gateway guard, so the generated deny proves the tier gate with the
    // gateway on (member ⇒ /home, none of the panel calls fires). The tenant
    // leak probe needs a per-tenant path — it is e2e/gateway-panels.spec.ts
    // ("leak: B on A's consumer"), hence tenantScoped stays off here.
    route: '/gateway/consumers/:id',
    name: 'gateway-consumer',
    role: 'tenant-admin',
    mode: 'reading',
    path: `/gateway/consumers/${PANEL_CONSUMER_A}`,
    capabilities: modelApiCapabilities(true, true),
    adminCalls: ['model-consumer-get', 'gateway-limits-get', 'gateway-limits-set', 'gateway-limits-delete', '/api/usage', '/api/llmlog'],
    states: [
      { name: 'default', seed: {} },
      // server-admin: call detail control present, server tier of the limits form
      { name: 'server-admin', seed: { role: 'server-admin' } },
      // the consumer read fails: error band with Retry (calls/usage/limits do not render)
      { name: 'error', seed: { faults: [{ action: 'model-consumer-get', status: 500 }] } },
    ],
    scale: {
      exempt:
        'Server-capped detail: key chain ≤ gateway.consumer_chain_max (50, keys_truncated), audit ≤ 20 rows, one page of calls, ≤ 92-d usage window — no unbounded list on this page; the 10k list dimension is the /gateway contract.',
    },
    flowDoc:
      'Tenant-Admin öffnet einen Konsumenten: Kopf mit Label und Server-Status, Key-Kette neueste zuerst mit „rotated from" je Glied, letzte Calls ohne Body und ohne Link auf /api/llmlog/{id}, Audit-Spur mit der Rotation als Kante „neu ← alt"; das Nutzungs-Panel zeigt nicht erfasste Buckets als „not recorded" (nie 0), ohne Histogramme den Ø-Wert markiert und keine Auth-Ablehnungen je Konsument, daneben die Tabelle für Screenreader; die Limits-Karte bietet je Feld höchstens die Server-Decke an, keinen „unbegrenzt"-Schalter, und ein Wert darüber kommt als 400 mit Feldnamen an genau diesem Feld zurück (W05-09/W05-10/W05-12).',
    primaryFlow: async (page, session) => {
      const content = page.locator('main.content')
      await expect(content.getByRole('heading', { level: 1, name: 'rotated-app' }), 'gate W05-09: the consumer renders').toBeVisible()
      // W05-09 — key chain newest first, each link names its predecessor
      const links = content.locator('ol[aria-label="key chain"] > li[data-key-link]')
      await expect(links.locator('[data-key-hint]')).toHaveText(['ctxm_…CCC3', 'ctxm_…BBB2', 'ctxm_…AAA1'])
      await expect(links.locator('[data-rotated-from]')).toHaveText(['ctxm_…BBB2', 'ctxm_…AAA1', ''])
      // W05-09 — positive control: the seeded call is a row; no detail link for a tenant-admin
      const calls = content.locator('table[aria-label="last calls"] tr[data-call-row]')
      await expect(calls.filter({ has: page.locator(`[data-call-id="${PANEL_CALL_ID}"]`) })).toHaveCount(1)
      await expect(content.locator('a[href*="/api/llmlog"]'), 'gate W05-09: kein Link auf /api/llmlog/{id}').toHaveCount(0)
      await expect(content.getByRole('button', { name: /^Open call/ }), 'gate W05-09: no detail control').toHaveCount(0)
      // the error shows its class (Go llmlogError.class) — never the capped raw detail
      await expect(calls.nth(1)).toContainText('http_502')
      await expect(content, 'gate W05-09: no provider detail in the page').not.toContainText('PROMPT-FRAGMENT-DO-NOT-RENDER')
      // W05-09 — audit trail shows the rotation as one chain edge
      const audit = content.locator('ol[aria-label="audit trail"] > li[data-audit-entry]')
      await expect(audit).toHaveCount(4)
      await expect(audit.first(), 'gate W05-09: Rotation als Kette').toContainText('ctxm_…CCC3 ← ctxm_…BBB2')
      // W05-10 — usage panel: not recorded instead of 0, marked means, no auth count, a11y table
      const usage = content.locator('section[aria-label="usage"]')
      const table = usage.getByRole('table', { name: 'usage table' })
      await expect(table.locator('tbody tr'), 'gate W05-10: a11y table, one row per bucket').toHaveCount(3)
      await expect(table.locator('tbody tr').nth(1).locator('[data-col="calls"]'), 'gate W05-10: complete:false ⇒ not recorded').toHaveText('not recorded')
      await expect(table.locator('tbody tr').nth(2).locator('[data-col="calls"]'), 'gate W05-10: null ⇒ not recorded').toHaveText('not recorded')
      await expect(table.locator('tbody tr').first().locator('[data-col="latency"]'), 'gate W05-10: hist:false ⇒ Ø').toHaveText('Ø 820ms')
      await expect(usage.locator('[data-reject-reason="auth"]'), 'gate W05-10: rejected.auth je Konsument nie als Zahl').toHaveCount(0)
      // review L1: the chart draws the two unrecorded buckets as gaps (never a zero bar), the total is partial
      await expect(usage.locator('[data-bar-gap]'), 'gate W05-10: Lücke statt 0-Balken').toHaveCount(2)
      await expect(usage.locator('[data-bar]')).toHaveCount(1)
      await expect(usage.locator('[data-total="calls"]'), 'gate W05-10: partial total, never 0').toHaveText('≥ 12')
      expect(session.calls.some((c) => c.path === '/api/usage'), 'the panel reads /api/usage').toBe(true)
      // W05-12 — limits: tenant tier, max = server(d), no unlimited, 422 at the field
      const limits = content.locator('section[aria-label="limits"]')
      await expect(limits.getByRole('spinbutton', { name: 'chat rpm' }), 'gate W05-12: Eingabe-Maximum = server(d)').toHaveAttribute('max', '30')
      await expect(limits.getByRole('spinbutton', { name: 'chat max_concurrency' })).toHaveAttribute('max', '4')
      await expect(limits.getByRole('checkbox', { name: /unlimited/i }), 'gate W05-12: kein „unbegrenzt"-Schalter').toHaveCount(0)
      // review L1/L2: the tenant tier can only deny external backends; an unbounded effective budget reads "no limit"
      await expect(limits.getByLabel('chat external backends').locator('option'), 'tenant tier narrows only').toHaveText(['inherit', 'deny'])
      await expect(limits.locator(`[id="lim-${PANEL_CONSUMER_A}-chat.monthly_tokens-eff"]`)).toContainText('effective no limit')
      // over the ceiling: the client hint first, then the server's 400 {param} at the same field
      await limits.getByRole('spinbutton', { name: 'chat max_concurrency' }).fill('5')
      await expect(limits.locator('[data-field-error="chat.max_concurrency"]')).toHaveText('at most 4 (server limit)')
      await limits.getByRole('button', { name: 'Save chat limits' }).click()
      await expect(limits.locator('[data-field-error="chat.max_concurrency"]'), 'gate W05-12: Fehler mit Feldnamen am Feld').toHaveText('limit widens: max_concurrency — 5 is above 4, the limit of the levels above')
      const set = session.calls.filter((c) => c.action === 'gateway-limits-set').at(-1)
      expect(set, 'the write reached the server (it decides)').toBeDefined()
      expect(JSON.stringify(set?.body ?? {}), 'gate W05-12: no tier on the wire').not.toMatch(/"tier"/)
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
export const pendingContracts: PendingContract[] = [
  {
    route: '/guard',
    reason:
      'Guard review queue (needs_review pipeline W4) — page shipped dark-launched; its PageContract (list/pair/resolve flows against a seeded flagged corpus) lands with the guard e2e wave.',
  },
]

/** Sentinel re-export so leak-probe consumers need only the registry. */
export { SENTINEL }
