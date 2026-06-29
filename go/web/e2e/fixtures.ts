// Playwright smoke-test fixtures (web-redesign visual verification debt, HANDOVER §8).
//
// The SPA is fully driven by `/api/**` calls; this module mocks every endpoint a
// page touches on initial load with deterministic, scope-/role-shaped payloads so
// the SHELL + per-area LAYOUT MODES (S3–S7), THEME (TH3/TH4) and GRAPH (G1–G3)
// render real content — no live backend, no real key. Wire shapes mirror
// src/lib/api/types.ts + src/lib/graph/api.ts + src/lib/api/blocks.ts verbatim
// (the Go golden tests are the drift anchor; these fixtures are NOT a second one).

import type { Page, Route } from '@playwright/test'

export type Role = 'server-admin' | 'tenant-admin' | 'member'

const KEY = 'smoke-key'

/** WhoamiResponse (types.ts:6) shaped per tier — capabilitiesFor reads admin+role. */
export function whoamiFor(role: Role): Record<string, unknown> {
  const base = {
    success: true,
    label: 'smoke-key',
    home_scope: 'home',
    read_scopes: ['home', 'shared'],
    api_key_id: '0190000000007000800000000000ke7',
    tenant_id: '550e8400-e29b-41d4-a716-446655440aaa',
    tenant_slug: 'acme',
    tenant_display_name: 'Acme Corp',
  }
  if (role === 'server-admin') return { ...base, admin: true, role: 'owner' }
  if (role === 'tenant-admin') return { ...base, admin: false, role: 'owner' }
  return { ...base, admin: false, role: 'member' }
}

/** StatusResponse (types.ts:233) — admin-only; SSE falls back to this GET poll. */
function statusFixture(): Record<string, unknown> {
  return {
    success: true,
    as_of: '2026-06-29T12:00:00Z',
    health: { status: 'ok', services: { db: 'ok', embed: 'ok', chat: 'ok' } },
    backends: [
      {
        id: 'local-llamacpp',
        name: 'llama.cpp (local)',
        trust: 'full-trust',
        locality: 'local',
        roles: ['chat', 'dream'],
        priority: 10,
        enabled: true,
        effective_state: 'active',
        cooldown_remaining_s: 0,
        consecutive_fails: 0,
        last_ok: '2026-06-29T11:59:30Z',
      },
    ],
    dream: {
      mode: 'on',
      throttle_interval_s: 0,
      pickable_now: 4,
      in_cooldown: 1,
      never_dreamed: 12,
      awaiting_embed: 0,
      incoming_1h: 3,
      incoming_6h: 9,
      next_pending_at: null,
      last_cycle_at: '2026-06-29T11:55:00Z',
    },
    llm_24h: [
      {
        backend: 'llama.cpp (local)',
        pipeline: 'chat',
        calls: 42,
        avg_ms: 1230,
        errors: 0,
        prompt_tokens: 51200,
        completion_tokens: 8800,
      },
    ],
    llm_24h_complete: true,
    gaming: { active: false },
    activity: null,
  }
}

/** SettingView[] (types.ts:37) — a few rows so the reading-mode list has body. */
function settingsFixture(): Record<string, unknown>[] {
  return [
    { key: 'pool.default_block_sensitivity', type: 'string', mutability: 'hot', value: 'internal', source: 'db', default: 'credentials' },
    { key: 'dream.enabled', type: 'bool', mutability: 'hot', value: true, source: 'env', default: false },
    { key: 'server.timezone', type: 'timezone', mutability: 'restart', value: 'Europe/Berlin', source: 'db', default: 'UTC' },
  ]
}

/** SearchResponse (graph/api.ts) — NO success field on the happy path. */
function searchFixture(): Record<string, unknown> {
  const mk = (n: number, cat: string, title: string, sens: string) => ({
    id: `550e8400-e29b-41d4-a716-44665544000${n}`,
    category: cat,
    tags: ['demo', cat],
    title,
    content_preview: `Lorem ipsum preview for ${title} — enough text to fill a master-detail hit row in the split layout.`,
    content_length: 1840,
    scope: 'home',
    updated_at: '2026-06-28T10:00:00Z',
    created_at: '2026-06-01T08:00:00Z',
    sensitivity: sens,
  })
  return {
    count: 3,
    results: [
      mk(1, 'design', 'Core Architecture', 'internal'),
      mk(2, 'reference', 'API Spec', 'public'),
      mk(3, 'learnings', 'Retrieval Findings', 'internal'),
    ],
    next_after: null,
  }
}

/** OverviewResponse (graph/api.ts) — the cluster map for the canvas-mode graph. */
function overviewFixture(): Record<string, unknown> {
  return {
    success: true,
    params: {},
    nodes: [
      { cluster: 0, size: 12, top_categories: ['design', 'code'], repr_id: '550e8400-e29b-41d4-a716-446655440001', repr_title: 'Architecture', scope_mix: ['home'] },
      { cluster: 1, size: 7, top_categories: ['reference'], repr_id: '550e8400-e29b-41d4-a716-446655440002', repr_title: 'API Spec', scope_mix: ['shared'] },
      { cluster: 2, size: 4, top_categories: ['learnings'], repr_id: '550e8400-e29b-41d4-a716-446655440003', repr_title: 'Findings', scope_mix: ['home'] },
    ],
    edges: [
      [0, 1, 3, 0.8],
      [1, 2, 1, 0.4],
    ],
    stats: { nodes: 3, edges: 2, truncated: false, computed_at: '2026-06-29T12:00:00Z', elapsed_ms: 42 },
  }
}

/** EgoResponse (graph/api.ts) — focused ego graph (deep-link ?focus=…). */
function egoFixture(): Record<string, unknown> {
  return {
    success: true,
    focus: '550e8400-e29b-41d4-a716-446655440001',
    params: { hops: 2 },
    rels: ['references', 'links_to', 'supersedes'],
    nodes: [
      { id: '550e8400-e29b-41d4-a716-446655440001', title: 'Architecture', category: 'design', scope: 'home', degree: 5, hop: 0, created_at: '2026-06-01T08:00:00Z' },
      { id: '550e8400-e29b-41d4-a716-446655440002', title: 'API Spec', category: 'reference', scope: 'home', degree: 2, hop: 1, created_at: '2026-06-02T09:30:00Z' },
      { id: '550e8400-e29b-41d4-a716-446655440003', title: 'Findings', category: 'learnings', scope: 'home', degree: 1, hop: 1, created_at: '2026-06-03T10:15:00Z' },
    ],
    edges: [
      [0, 1, 0, 0.95],
      [0, 2, 1, 0.87],
    ],
    stats: { nodes: 3, edges: 2, truncated: false, elapsed_ms: 28 },
  }
}

/** ApiKeyView[] (types.ts:568) for the tenant key table; first row = own key. */
function apiKeysFixture(): Record<string, unknown>[] {
  return [
    { id: '0190000000007000800000000000ke7', label: 'smoke-key', home_scope: 'home', allowed_scopes: ['home', 'shared'], active: true, last_used_at: '2026-06-29T11:00:00Z', created_at: '2026-05-01T08:00:00Z', tenant_role: 'owner' },
    { id: '0190000000007000800000000000ke8', label: 'ci-runner', home_scope: 'home', allowed_scopes: ['home'], active: true, created_at: '2026-06-10T08:00:00Z', tenant_role: 'member' },
  ]
}

/** POST /api/manage dispatch — action-keyed, mirrors the handler envelopes. */
function manageFixture(action: string | undefined, _role: Role): Record<string, unknown> {
  switch (action) {
    case 'list-categories':
      return { success: true, categories: [
        { category: 'design', count: 12 },
        { category: 'reference', count: 7 },
        { category: 'learnings', count: 4 },
      ] }
    case 'list-meta':
      return { success: true, blocks: [
        { id: '550e8400-e29b-41d4-a716-446655440001', category: 'design', title: 'Core Architecture', tags: ['demo'], scope: 'home', updated_at: '2026-06-28T10:00:00Z' },
        { id: '550e8400-e29b-41d4-a716-446655440002', category: 'reference', title: 'API Spec', tags: ['demo'], scope: 'home', updated_at: '2026-06-27T10:00:00Z' },
      ] }
    case 'get':
      return { success: true, block: {
        id: '550e8400-e29b-41d4-a716-446655440001',
        category: 'design',
        tags: ['demo', 'design'],
        title: 'Core Architecture',
        content: 'The ctx graph is the first true canvas-first surface. This mock body fills the reading-measure prose column so the master-detail and floating-window line length stays bounded.',
        scope: 'home',
        sensitivity: 'internal',
        created_at: '2026-06-01T08:00:00Z',
        updated_at: '2026-06-28T10:00:00Z',
      } }
    case 'api-key-list':
      return { success: true, keys: apiKeysFixture() }
    case 'api-key-create':
      return {
        success: true,
        id: '0190000000007000800000000000ke9',
        label: 'new-key',
        home_scope: 'home',
        allowed_scopes: ['home'],
        // Plaintext shown exactly once — the reveal-once dialog must surface it
        // and never persist it (design 05 §6).
        api_key: 'ctx_sk_TESTKEY_reveal_once_do_not_persist',
      }
    case 'api-key-delete':
      return { success: true, deleted: '0190000000007000800000000000ke8' }
    case 'tenant-quota-get':
      return {
        success: true,
        quota: {
          scope: 'home',
          enabled: true,
          daily_cost_usd: 5,
          monthly_cost_usd: 100,
          daily_calls: 1000,
          on_exceed: 'external_off',
        },
      }
    case 'tenant-list':
      return { success: true, tenants: [
        { id: '550e8400-e29b-41d4-a716-446655440aaa', slug: 'acme', display_name: 'Acme Corp', status: 'active', created_at: '2026-05-01T08:00:00Z', updated_at: '2026-06-01T08:00:00Z' },
      ] }
    case 'scope-overview':
      return { success: true, scopes: [
        { scope: 'home', block_count: 128, key_count: 3, tenant_id: '550e8400-e29b-41d4-a716-446655440aaa' },
        { scope: 'shared', block_count: 42, key_count: 8, tenant_id: '550e8400-e29b-41d4-a716-446655440aaa' },
        { scope: 'legacy', block_count: 7, key_count: 1, tenant_id: null },
      ] }
    case 'backend-list':
      return { success: true, backends: [] }
    // A7 corpus maintenance — start kicks off (running), status reports it
    // finished so the poll loop converges to a terminal state on the next tick.
    case 'blocks-audit-start':
      return { success: true, scope: 'home', pending: 30, by_source: { 'llm-audit': 12 }, run: auditRun(true) }
    case 'blocks-audit-status':
      return { success: true, scope: 'home', pending: 30, by_source: { 'llm-audit': 12 }, run: auditRun(false) }
    case 'blocks-classify-start':
      return { success: true, scope: 'home', by_source: { default: 8 }, run: classifyRun(true) }
    case 'blocks-classify-status':
      return { success: true, scope: 'home', by_source: { default: 8 }, run: classifyRun(false) }
    default:
      return { success: true }
  }
}

function auditRun(running: boolean): Record<string, unknown> {
  return running
    ? { running: true, dry_run: true, processed: 0, kept_credentials: 0, to_personal: 0, to_internal: 0, no_verdict: 0, discarded: 0, aborted: false }
    : { running: false, dry_run: true, processed: 30, kept_credentials: 2, to_personal: 5, to_internal: 20, no_verdict: 3, discarded: 0, aborted: false, finished_at: '2026-06-29T12:00:00Z' }
}

function classifyRun(running: boolean): Record<string, unknown> {
  return running
    ? { running: true, dry_run: true, scanned: 0, upgraded: 0, discarded: 0, aborted: false }
    : { running: false, dry_run: true, scanned: 40, upgraded: 3, discarded: 0, aborted: false, finished_at: '2026-06-29T12:00:00Z' }
}

/** Empty-corpus variants (N8 empty-state coverage). */
function emptySearchFixture(): Record<string, unknown> {
  return { count: 0, results: [], next_after: null }
}
function emptyOverviewFixture(): Record<string, unknown> {
  return { success: true, params: {}, nodes: [], edges: [], stats: { nodes: 0, edges: 0, truncated: false, computed_at: null, elapsed_ms: 5 } }
}

/**
 * Seed an authenticated session (sessionStorage key, restored on App mount) +
 * a deterministic theme pref (localStorage, read by theme-boot.js before first
 * paint), then install the `/api/**` mocks. Must run BEFORE page.goto().
 */
export async function seedSession(
  page: Page,
  opts: { role: Role; theme: 'light' | 'dark'; empty?: boolean },
): Promise<void> {
  await page.addInitScript(
    ({ key, theme }) => {
      try {
        sessionStorage.setItem('ctx.api-key', key)
        localStorage.setItem('ctx.theme', theme)
      } catch {
        /* storage blocked — test will surface it as a render failure */
      }
    },
    { key: KEY, theme: opts.theme },
  )

  await page.route('**/health', (route: Route) =>
    route.fulfill({ json: { status: 'ok', services: { db: 'ok', embed: 'ok' } } }),
  )

  await page.route('**/api/**', async (route: Route) => {
    const req = route.request()
    const path = new URL(req.url()).pathname
    const method = req.method()

    // SSE telemetry stream → abort so StatusPage uses the GET /api/status poll.
    if (path === '/api/events') return route.abort()

    if (path === '/api/whoami') return route.fulfill({ json: whoamiFor(opts.role) })
    if (path === '/api/status') return route.fulfill({ json: statusFixture() })
    if (path === '/api/llmlog') return route.fulfill({ json: { success: true, entries: [] } })
    if (path === '/api/settings' && method === 'GET') return route.fulfill({ json: { success: true, settings: settingsFixture() } })
    if (path === '/api/secrets' && method === 'GET') return route.fulfill({ json: { success: true, secrets: [] } })
    if (path.startsWith('/api/graph/overview')) return route.fulfill({ json: opts.empty ? emptyOverviewFixture() : overviewFixture() })
    if (path.startsWith('/api/graph/ego')) return route.fulfill({ json: egoFixture() })
    if (path.startsWith('/api/chat/sessions')) return route.fulfill({ json: { success: true, sessions: [] } })
    if (path === '/api/search') return route.fulfill({ json: opts.empty ? emptySearchFixture() : searchFixture() })
    if (path === '/api/manage' && method === 'POST') {
      let action: string | undefined
      try {
        action = (req.postDataJSON() as { action?: string } | null)?.action
      } catch {
        action = undefined
      }
      return route.fulfill({ json: manageFixture(action, opts.role) })
    }

    // Unmapped (write paths not hit on initial load): benign success envelope.
    return route.fulfill({ json: { success: true } })
  })
}

/** Collect uncaught page exceptions; assert this stays empty after a render. */
export function trackPageErrors(page: Page): string[] {
  const errors: string[] = []
  page.on('pageerror', (err) => errors.push(err.message))
  return errors
}

/**
 * Navigate to an SPA area and wait for the routed content to mount. The smoke
 * runs against the production build (vite preview), whose static hashed chunks
 * load deterministically, so a single goto suffices. The content wait also
 * covers the boot-time landing/guard redirect (N4/N5): after a redirect the
 * target area's component mounts under main.content.
 */
export async function gotoArea(page: Page, path: string): Promise<void> {
  await page.goto(path)
  await page.locator('.shell').waitFor({ state: 'visible', timeout: 10_000 })
  await page.locator('main.content > *').first().waitFor({ state: 'attached', timeout: 10_000 })
}
